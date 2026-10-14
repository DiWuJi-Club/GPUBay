package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/api"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/identity"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/secretbox"
	"spark-cloud/internal/plugins/kyc"
	"spark-cloud/internal/plugins/sms"
	"spark-cloud/internal/rental"
	"spark-cloud/internal/wallet"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

type rentalNoop struct{}

func (rentalNoop) NotifyExpiry(context.Context, int64, string, time.Duration) error { return nil }

type testEnv struct {
	t      *testing.T
	pool   *pgxpool.Pool
	ts     *httptest.Server
	wallet *wallet.Service
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	box, _ := secretbox.New(dbtest.Key)
	ids := identity.NewService(identity.Deps{Pool: pool, Clock: clk, SMS: &sms.Mock{}, KYC: kyc.Mock{}, Box: box, FixedCode: "000000"})
	w := wallet.NewService(pool, clk)
	c := catalog.NewService(pool)
	f := fleet.NewService(pool, clk)
	r := rental.NewService(rental.Deps{Pool: pool, Clock: clk, Box: box, Users: ids, Wallet: w, Catalog: c, Fleet: f, Notifier: rentalNoop{}})
	srv := &api.Server{Identity: ids, Wallet: w, Catalog: c, Fleet: f, Rental: r, Clock: clk,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Domain: "spark.test"}
	ts := httptest.NewServer(srv.Router())
	t.Cleanup(ts.Close)
	return &testEnv{t: t, pool: pool, ts: ts, wallet: w}
}

type client struct {
	t    *testing.T
	base string
	http *http.Client
}

func (e *testEnv) client() *client {
	jar, _ := cookiejar.New(nil)
	return &client{t: e.t, base: e.ts.URL + "/api/v1", http: &http.Client{Jar: jar}}
}

func (c *client) do(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (c *client) login(phone string) map[string]any {
	c.t.Helper()
	if code, out := c.do("POST", "/auth/sms-code", map[string]string{"phone": phone}); code != 200 {
		c.t.Fatalf("sms-code: %d %v", code, out)
	}
	code, out := c.do("POST", "/auth/login", map[string]string{"phone": phone, "code": "000000"})
	if code != 200 {
		c.t.Fatalf("login: %d %v", code, out)
	}
	return out
}

func TestLoginSessionAndLogout(t *testing.T) {
	e := newTestEnv(t)
	c := e.client()
	if code, out := c.do("GET", "/me", nil); code != 401 || out["code"] != "unauthorized" {
		t.Fatalf("anonymous /me = %d %v", code, out)
	}
	user := c.login("13800000001")
	if user["phone"] != "13800000001" {
		t.Fatalf("login user = %v", user)
	}
	if code, out := c.do("GET", "/me", nil); code != 200 || out["phone"] != "13800000001" {
		t.Fatalf("/me = %d %v", code, out)
	}
	if code, _ := c.do("POST", "/auth/logout", map[string]any{}); code != 200 {
		t.Fatalf("logout = %d", code)
	}
	if code, _ := c.do("GET", "/me", nil); code != 401 {
		t.Fatalf("/me after logout = %d", code)
	}
}

func TestWritesRequireJSONAndRejectUnknownFields(t *testing.T) {
	e := newTestEnv(t)
	resp, err := http.Post(e.ts.URL+"/api/v1/auth/sms-code", "text/plain", bytes.NewReader([]byte(`{"phone":"13800000001"}`)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("text/plain status = %d, want 400", resp.StatusCode)
	}
	c := e.client()
	if code, out := c.do("POST", "/auth/sms-code", map[string]string{"phone": "13800000001", "x": "1"}); code != 400 || out["code"] != "bad_request" {
		t.Fatalf("unknown field = %d %v", code, out)
	}
}

func TestOrderRequiresKYC(t *testing.T) {
	e := newTestEnv(t)
	c := e.client()
	c.login("13800000002")
	code, out := c.do("POST", "/orders", map[string]any{"product_id": dbtest.ProductID(t, e.pool), "duration": "1d", "image_id": dbtest.ImageID(t, e.pool)})
	if code != 403 || out["code"] != "kyc_required" {
		t.Fatalf("order without kyc = %d %v", code, out)
	}
}

func TestOrderAndInstanceEndpoints(t *testing.T) {
	e := newTestEnv(t)
	dbtest.CreateMachine(t, e.pool, "SN001")
	c := e.client()
	user := c.login("13800000003")
	if code, out := c.do("POST", "/kyc/personal", map[string]string{"name": "张三", "id_number": "11010119900101123X"}); code != 200 || out["kyc_status"] != "verified" {
		t.Fatalf("kyc = %d %v", code, out)
	}
	uid := int64(user["id"].(float64))
	if _, err := e.wallet.AdminAdjust(context.Background(), 1, uid, 100000, "测试充值", ""); err != nil {
		t.Fatal(err)
	}

	code, out := c.do("GET", "/products", nil)
	items := out["items"].([]any)
	p := items[0].(map[string]any)
	if code != 200 || p["idle_count"].(float64) != 1 || len(p["prices"].([]any)) != 3 {
		t.Fatalf("products = %d %v", code, out)
	}

	code, out = c.do("POST", "/orders", map[string]any{"product_id": p["id"], "duration": "1d", "image_id": dbtest.ImageID(t, e.pool)})
	if code != 201 {
		t.Fatalf("order = %d %v", code, out)
	}
	inst := out["instance"].(map[string]any)
	id := inst["id"].(string)
	if inst["state"] != "creating" || inst["pending_seconds"].(float64) != 86400 {
		t.Fatalf("instance = %v", inst)
	}

	if code, out := c.do("GET", "/instances", nil); code != 200 || len(out["items"].([]any)) != 1 {
		t.Fatalf("instances = %d %v", code, out)
	}
	if code, out := c.do("GET", "/instances/"+id+"/password", nil); code != 200 || len(out["password"].(string)) != 16 {
		t.Fatalf("password = %d %v", code, out)
	}
	if code, out := c.do("GET", "/wallet", nil); code != 200 || out["balance"].(float64) != 100000-6900 {
		t.Fatalf("wallet = %d %v", code, out)
	}
	if code, out := c.do("GET", "/wallet/ledger?limit=10", nil); code != 200 || len(out["items"].([]any)) != 2 {
		t.Fatalf("ledger = %d %v", code, out)
	}
	if code, out := c.do("GET", "/orders", nil); code != 200 || len(out["items"].([]any)) != 1 {
		t.Fatalf("orders = %d %v", code, out)
	}
	if code, out := c.do("POST", "/instances/"+id+"/release", map[string]any{}); code != 409 || out["code"] != "invalid_state" {
		t.Fatalf("release while creating = %d %v", code, out)
	}

	other := e.client()
	other.login("13800000004")
	if code, _ := other.do("GET", "/instances/"+id, nil); code != 404 {
		t.Fatalf("other user's instance = %d, want 404", code)
	}
	if code, out := other.do("GET", "/instances", nil); code != 200 || len(out["items"].([]any)) != 0 {
		t.Fatalf("other user's list = %d %v (items must be [] not null)", code, out)
	}
}

func TestSMSIPLimitIgnoresForwardedForWithoutTrustProxy(t *testing.T) {
	e := newTestEnv(t)
	var last int
	for i := 0; i < 31; i++ {
		b, _ := json.Marshal(map[string]string{"phone": fmt.Sprintf("1380000%04d", i)})
		req, _ := http.NewRequest("POST", e.ts.URL+"/api/v1/auth/sms-code", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("10.0.0.%d", i+1))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		last = resp.StatusCode
		if i < 30 && last != 200 {
			t.Fatalf("request %d = %d, want 200", i, last)
		}
	}
	if last != 429 {
		t.Fatalf("31st request = %d, want 429", last)
	}
}

func TestAuthenticatedResponsesAreNoStore(t *testing.T) {
	e := newTestEnv(t)
	c := e.client()
	c.login("13800000005")
	req, _ := http.NewRequest("GET", c.base+"/me", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("/me = %d, Cache-Control %q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
}
