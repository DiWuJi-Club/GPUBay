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
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/api"
	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/billing"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/identity"
	"spark-cloud/internal/ingress"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/secretbox"
	"spark-cloud/internal/plugins/kyc"
	"spark-cloud/internal/plugins/pay"
	"spark-cloud/internal/plugins/sms"
	"spark-cloud/internal/referral"
	"spark-cloud/internal/rental"
	"spark-cloud/internal/storage"
	"spark-cloud/internal/wallet"
	"spark-cloud/internal/web"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

type rentalNoop struct{}

func (rentalNoop) NotifyExpiry(context.Context, int64, string, time.Duration) error    { return nil }
func (rentalNoop) NotifyAutoRenewFailed(context.Context, int64, string, string) error  { return nil }
func (rentalNoop) NotifyBookingCancelled(context.Context, int64, string, string) error { return nil }

type testEnv struct {
	t       *testing.T
	pool    *pgxpool.Pool
	ts      *httptest.Server
	wallet  *wallet.Service
	pay     *pay.Mock
	clk     *clock.Fake
	rental  *rental.Service
	fleet   *fleet.Service
	ingress *ingress.Auth
	apps    *appdeploy.Service // 只有 newAppsTestEnv 组装；其余为 nil（Server.Apps 也为 nil）
	appCat  *appdeploy.Catalog
	appLog  *lockedBuffer // 应用部署服务的日志（newAppsTestEnv）
}

// lockedBuffer 是可并发写的日志缓冲（服务端处理请求的 goroutine 写、测试读）。
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newTestEnv(t *testing.T) *testEnv { return newTestEnvWith(t, nil) }

// newTestEnvWith 在构建路由前允许调整 Server 字段（例如 Static）。
func newTestEnvWith(t *testing.T, tweak func(*api.Server)) *testEnv {
	t.Helper()
	return buildTestEnv(t, false, tweak)
}

// newAppsTestEnv 额外组装应用部署服务（Server.Apps + rental 钩子）。
func newAppsTestEnv(t *testing.T, tweak func(*api.Server)) *testEnv {
	t.Helper()
	return buildTestEnv(t, true, tweak)
}

func buildTestEnv(t *testing.T, withApps bool, tweak func(*api.Server)) *testEnv {
	t.Helper()
	var setup func(*api.Server, *pgxpool.Pool)
	if tweak != nil {
		setup = func(srv *api.Server, _ *pgxpool.Pool) { tweak(srv) }
	}
	return buildTestEnvPool(t, withApps, setup)
}

// buildTestEnvPool 同 buildTestEnv，调整函数额外拿到连接池（组装需要直连数据库的服务，例如 AI 管家）。
func buildTestEnvPool(t *testing.T, withApps bool, tweak func(*api.Server, *pgxpool.Pool)) *testEnv {
	t.Helper()
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	box, _ := secretbox.New(dbtest.Key)
	ids := identity.NewService(identity.Deps{Pool: pool, Clock: clk, SMS: &sms.Mock{}, KYC: kyc.Mock{}, Box: box, FixedCode: "000000"})
	w := wallet.NewService(pool, clk)
	c := catalog.NewService(pool)
	f := fleet.NewService(pool, clk)
	r := rental.NewService(rental.Deps{Pool: pool, Clock: clk, Box: box, Users: ids, Wallet: w, Catalog: c, Fleet: f, Notifier: rentalNoop{}, Keys: ids})
	disk, err := storage.NewLocalDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	uploads := storage.NewService(pool, clk, disk)
	payMock := pay.NewMock("test-secret")
	bill := billing.NewService(billing.Deps{
		Pool: pool, Clock: clk, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Wallet: w, Users: ids, Alerts: f,
		Providers:     map[string]pay.Provider{pay.ChannelWechat: payMock, pay.ChannelAlipay: payMock},
		NotifyBaseURL: "https://spark.test",
	})
	ref := referral.NewService(referral.Deps{Pool: pool, Clock: clk, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Wallet: w, Users: ids, Orders: r})
	ids.SetReferral(ref)
	auth, err := ingress.NewAuth(dbtest.Key, "http://spark.test", "spark.test", false, clk)
	if err != nil {
		t.Fatal(err)
	}
	srv := &api.Server{Identity: ids, Wallet: w, Catalog: c, Fleet: f, Rental: r, Uploads: uploads, Clock: clk,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Domain: "spark.test", SSHHost: "ssh.spark.test", PublicBaseURL: "http://spark.test",
		Billing: bill, Referral: ref, DevPay: true, Ingress: auth}
	var apps *appdeploy.Service
	var appCat *appdeploy.Catalog
	appLog := &lockedBuffer{}
	if withApps {
		quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
		appCat = appdeploy.NewCatalog(pool, clk, f, c, quiet)
		apps = appdeploy.NewService(appdeploy.Deps{Pool: pool, Clock: clk, Box: box, Log: slog.New(slog.NewTextHandler(appLog, nil)), Catalog: appCat, Images: c, Rental: r, Fleet: f,
			PortURL: auth.PortURL})
		r.SetAppHooks(apps)
		srv.Apps = apps
	}
	if tweak != nil {
		tweak(srv, pool)
	}
	ts := httptest.NewServer(srv.Router())
	t.Cleanup(ts.Close)
	return &testEnv{t: t, pool: pool, ts: ts, wallet: w, pay: payMock, clk: clk, rental: r, fleet: f, ingress: auth, apps: apps, appCat: appCat, appLog: appLog}
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
	return c.doH(method, path, body, nil)
}

func (c *client) doH(method, path string, body any, headers map[string]string) (int, map[string]any) {
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
	for k, v := range headers {
		req.Header.Set(k, v)
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

func TestPlaceOrderHonorsIdempotencyKey(t *testing.T) {
	e := newTestEnv(t)
	dbtest.CreateMachine(t, e.pool, "SN001")
	dbtest.CreateMachine(t, e.pool, "SN002")
	c := e.client()
	user := c.login("13800000006")
	if code, out := c.do("POST", "/kyc/personal", map[string]string{"name": "张三", "id_number": "11010119900101123X"}); code != 200 {
		t.Fatalf("kyc = %d %v", code, out)
	}
	uid := int64(user["id"].(float64))
	if _, err := e.wallet.AdminAdjust(context.Background(), 1, uid, 100000, "测试充值", ""); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"product_id": dbtest.ProductID(t, e.pool), "duration": "1d", "image_id": dbtest.ImageID(t, e.pool)}
	key := map[string]string{"Idempotency-Key": "click-1"}
	code, first := c.doH("POST", "/orders", body, key)
	if code != 201 {
		t.Fatalf("first order = %d %v", code, first)
	}
	code, second := c.doH("POST", "/orders", body, key)
	if code != 201 {
		t.Fatalf("retried order = %d %v", code, second)
	}
	id1 := first["order"].(map[string]any)["id"]
	if id2 := second["order"].(map[string]any)["id"]; id1 != id2 {
		t.Fatalf("order ids %v != %v", id1, id2)
	}
	if code, out := c.do("GET", "/wallet", nil); code != 200 || out["balance"].(float64) != 100000-6900 {
		t.Fatalf("wallet = %d %v, want charged once", code, out)
	}
	instID := first["instance"].(map[string]any)["id"].(string)
	if code, out := c.doH("POST", "/instances/"+instID+"/renew", map[string]string{"duration": "1d"}, key); code != 409 ||
		out["code"] != "idempotency_conflict" {
		t.Fatalf("renew with order key = %d %v", code, out)
	}
}

func TestInstanceViewIncludesProductAndImageNames(t *testing.T) {
	e := newTestEnv(t)
	dbtest.CreateMachine(t, e.pool, "SN-NAMES")
	c, uid := e.verifiedClient("13800000021")
	if _, err := e.wallet.AdminAdjust(context.Background(), 1, uid, 100000, "测试充值", ""); err != nil {
		t.Fatal(err)
	}
	iid := dbtest.ImageID(t, e.pool)
	code, out := c.do("POST", "/orders", map[string]any{"product_id": dbtest.ProductID(t, e.pool), "duration": "1d", "image_id": iid})
	if code != 201 {
		t.Fatalf("order = %d %v", code, out)
	}
	inst := out["instance"].(map[string]any)
	if inst["product_name"] != "DGX Spark 整机" || inst["image_name"] != "Ubuntu 24.04 + CUDA" {
		t.Fatalf("names on create = %v", inst)
	}
	// 镜像停用后仍能显示名称
	if _, err := e.pool.Exec(context.Background(), `UPDATE images SET enabled = false WHERE id = $1`, iid); err != nil {
		t.Fatal(err)
	}
	id := inst["id"].(string)
	if code, out := c.do("GET", "/instances/"+id, nil); code != 200 || out["image_name"] != "Ubuntu 24.04 + CUDA" {
		t.Fatalf("detail = %d %v", code, out)
	}
	if code, out := c.do("GET", "/instances", nil); code != 200 || out["items"].([]any)[0].(map[string]any)["product_name"] != "DGX Spark 整机" {
		t.Fatalf("list = %d %v", code, out)
	}
}

func TestMyKYCRecord(t *testing.T) {
	e := newTestEnv(t)
	c := e.client()
	c.login("13800000022")
	if code, out := c.do("GET", "/kyc", nil); code != 200 || out["record"] != nil {
		t.Fatalf("empty kyc = %d %v", code, out)
	}
	if code, out := c.do("POST", "/kyc/personal", map[string]string{"name": "欧阳娜娜", "id_number": "11010119900101123X"}); code != 200 {
		t.Fatalf("kyc = %d %v", code, out)
	}
	code, out := c.do("GET", "/kyc", nil)
	rec, _ := out["record"].(map[string]any)
	if code != 200 || rec == nil || rec["kind"] != "personal" || rec["result"] != "verified" || rec["real_name"] != "欧***" {
		t.Fatalf("kyc record = %d %v", code, out)
	}
	for _, k := range []string{"id_number_masked", "license_file", "user_id"} {
		if _, ok := rec[k]; ok {
			t.Fatalf("%s must not be exposed: %v", k, rec)
		}
	}
	if code, _ := e.client().do("GET", "/kyc", nil); code != 401 {
		t.Fatalf("anonymous kyc = %d, want 401", code)
	}
}

func TestRouterServesConsoleAndJSON404(t *testing.T) {
	index := fstest.MapFS{"index.html": {Data: []byte("<!doctype html><title>console</title>")}}
	e := newTestEnvWith(t, func(s *api.Server) { s.Static = web.NewHandler(index, "/") })
	get := func(path string) *http.Response {
		resp, err := http.Get(e.ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}
	for _, p := range []string{"/", "/market", "/instances/ins-abc12345", "/wallet/recharge/rch-x"} {
		resp := get(p)
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || !strings.Contains(string(body), "console") || resp.Header.Get("Cache-Control") != "no-cache" ||
			resp.Header.Get("Content-Security-Policy") == "" {
			t.Fatalf("%s: %d %q cache=%q", p, resp.StatusCode, body, resp.Header.Get("Cache-Control"))
		}
	}
	resp := get("/api/v1/nope")
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 404 || out["code"] != "not_found" {
		t.Fatalf("unknown api = %d %v", resp.StatusCode, out)
	}
	if resp := get("/api/nope"); resp.StatusCode != 404 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("/api/nope = %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if resp := get("/api/v1/wallet"); resp.StatusCode != 401 || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("anonymous wallet = %d cache=%q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
}

func TestSessionCookieNameAndSameSiteGuard(t *testing.T) {
	e := newTestEnvWith(t, func(s *api.Server) { s.SecureCookie = true })
	raw := &http.Client{}
	post := func(path, body string, headers map[string]string) *http.Response {
		req, _ := http.NewRequest("POST", e.ts.URL+"/api/v1"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := raw.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	post("/auth/sms-code", `{"phone":"13800009999"}`, nil)
	resp := post("/auth/login", `{"phone":"13800009999","code":"000000"}`, nil)
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "__Host-sc_session" {
			cookie = c
		}
	}
	if cookie == nil || !cookie.Secure || cookie.Path != "/" || cookie.Domain != "" || !cookie.HttpOnly {
		t.Fatalf("cookie = %+v", resp.Cookies())
	}
	me := func() int {
		req, _ := http.NewRequest("GET", e.ts.URL+"/api/v1/me", nil)
		req.AddCookie(&http.Cookie{Name: cookie.Name, Value: cookie.Value})
		r, err := raw.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	if code := me(); code != 200 {
		t.Fatalf("me with __Host- cookie = %d", code)
	}
	// 同站（兄弟子域）写请求被拒且会话仍有效；same-origin 放行且会话真正注销
	sessionHeader := cookie.Name + "=" + cookie.Value
	if r := post("/auth/logout", `{}`, map[string]string{"Sec-Fetch-Site": "same-site", "Cookie": sessionHeader}); r.StatusCode != 403 {
		t.Fatalf("same-site logout = %d", r.StatusCode)
	}
	if code := me(); code != 200 {
		t.Fatalf("me after rejected logout = %d, want 200", code)
	}
	if r := post("/auth/logout", `{}`, map[string]string{"Sec-Fetch-Site": "same-origin", "Cookie": sessionHeader}); r.StatusCode != 200 {
		t.Fatalf("same-origin logout = %d", r.StatusCode)
	}
	if code := me(); code != 401 {
		t.Fatalf("me after logout = %d, want 401", code)
	}
	// 支付回调不受同站限制（这里渠道会因验签失败返回非 403）
	if r := post("/pay/notify/wechat", `{}`, map[string]string{"Sec-Fetch-Site": "cross-site"}); r.StatusCode == 403 {
		t.Fatal("payment notify blocked by same-site guard")
	}
	if r := post("/pay/notify/alipay", `{}`, map[string]string{"Origin": "https://openapi.alipay.com"}); r.StatusCode == 403 {
		t.Fatal("payment notify with foreign Origin blocked by same-site guard")
	}
	// dev（SecureCookie=false）仍叫 sc_session
	dev := newTestEnv(t)
	c := dev.client()
	c.login("13800009998")
	for _, ck := range c.http.Jar.Cookies(mustURL(dev.ts.URL)) {
		if ck.Name == "sc_session" {
			return
		}
	}
	t.Fatal("dev cookie not named sc_session")
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}
