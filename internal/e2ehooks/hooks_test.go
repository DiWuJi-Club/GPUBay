package e2ehooks_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"spark-cloud/internal/app"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/e2ehooks"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/config"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/rental"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

type env struct {
	t   *testing.T
	a   *app.App
	clk *clock.Fake
	ts  *httptest.Server
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	k := &e2ehooks.SwitchableKYC{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{Env: "dev", Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "mock", KYCDriver: "mock", UploadDir: t.TempDir()}
	a, err := app.Build(cfg, pool, log, app.Options{Clock: clk, KYC: k, DisableRateLimits: true})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer((&e2ehooks.Server{App: a, Pool: pool, Clock: clk, KYC: k, Log: log}).Handler())
	t.Cleanup(ts.Close)
	return &env{t: t, a: a, clk: clk, ts: ts}
}

func (e *env) call(method, path string, body any) (int, map[string]any) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.ts.URL+path, rd)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (e *env) must(method, path string, body any) map[string]any {
	e.t.Helper()
	code, out := e.call(method, path, body)
	if code != 200 {
		e.t.Fatalf("%s %s = %d %v", method, path, code, out)
	}
	return out
}

func TestClockHooks(t *testing.T) {
	e := newEnv(t)
	if code, out := e.call("GET", "/healthz", nil); code != 200 || out["ok"] != true {
		t.Fatalf("healthz = %d %v", code, out)
	}
	now, _ := time.Parse(time.RFC3339Nano, e.must("GET", "/hooks/clock", nil)["now"].(string))
	if !now.Equal(dbtest.T0) {
		t.Fatalf("clock = %v, want %v", now, dbtest.T0)
	}
	after, _ := time.Parse(time.RFC3339Nano, e.must("POST", "/hooks/clock/advance", map[string]int{"seconds": 90})["now"].(string))
	if !after.Equal(dbtest.T0.Add(90 * time.Second)) {
		t.Fatalf("advanced = %v", after)
	}
	if code, _ := e.call("POST", "/hooks/clock/advance", map[string]int{"seconds": 0}); code != 400 {
		t.Fatalf("advance 0 = %d, want 400", code)
	}
	if code, _ := e.call("POST", "/hooks/clock/advance", map[string]int{"seconds": -5}); code != 400 {
		t.Fatalf("advance -5 = %d, want 400", code)
	}
}

func TestMachinesUsersAgentAndSweep(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ids := e.must("POST", "/hooks/machines", map[string]any{"count": 2, "serial_prefix": "T-"})["ids"].([]any)
	if len(ids) != 2 {
		t.Fatalf("machines = %v", ids)
	}
	mid := int64(ids[0].(float64))
	if m, err := e.a.Fleet.Get(ctx, e.a.Fleet.Pool(), mid); err != nil || !m.AgentOnline || m.State != "idle" || m.Serial != "T-1" {
		t.Fatalf("machine = %+v, %v", m, err)
	}
	// 未指定前缀时多次调用不能撞序列号（随机 6 位曾在全量 e2e 中生日碰撞出 duplicate_serial）
	serials := map[string]bool{}
	for i := 0; i < 300; i++ {
		got := e.must("POST", "/hooks/machines", map[string]any{"count": 1})["ids"].([]any)
		m, err := e.a.Fleet.Get(ctx, e.a.Fleet.Pool(), int64(got[0].(float64)))
		if err != nil || serials[m.Serial] {
			t.Fatalf("default serial #%d = %q, %v", i, m.Serial, err)
		}
		serials[m.Serial] = true
	}
	u := e.must("POST", "/hooks/users", map[string]any{"phone": "13900000001", "verified": true, "balance": 100000, "ledger_rows": 3})
	uid := int64(u["id"].(float64))
	if user, err := e.a.Identity.GetUser(ctx, uid); err != nil || user.KYCStatus != "verified" || user.Phone != "13900000001" {
		t.Fatalf("user = %+v, %v", user, err)
	}
	if bal, _ := e.a.Wallet.Balance(ctx, uid); bal != 100000 {
		t.Fatalf("balance = %d, want 100000", bal)
	}
	if rows, _ := e.a.Wallet.ListLedger(ctx, uid, 0, 100); len(rows) != 4 {
		t.Fatalf("ledger rows = %d, want 4 (3 seed + 1 balance)", len(rows))
	}
	if code, out := e.call("POST", "/hooks/users", map[string]any{}); code != 200 || len(out["phone"].(string)) != 11 {
		t.Fatalf("default user = %d %v", code, out)
	}

	_, inst, err := e.a.Rental.Place(ctx, rental.PlaceRequest{UserID: uid, ProductID: dbtest.ProductID(t, e.a.Fleet.Pool()),
		ImageID: dbtest.ImageID(t, e.a.Fleet.Pool()), Duration: catalog.D1d})
	if err != nil {
		t.Fatal(err)
	}
	if out := e.must("POST", "/hooks/agent", map[string]any{}); out["completed"].(float64) != 1 {
		t.Fatalf("agent = %v", out)
	}
	if got, _ := e.a.Rental.Get(ctx, inst.ID); got.State != rental.StateRunning {
		t.Fatalf("after agent = %s, want running", got.State)
	}
	e.must("POST", "/hooks/clock/advance", map[string]int{"seconds": 86400})
	e.must("POST", "/hooks/sweep", map[string]any{})
	if got, _ := e.a.Rental.Get(ctx, inst.ID); got.State != rental.StateStopped {
		t.Fatalf("after sweep = %s, want stopped (sweep must heartbeat first)", got.State)
	}

	// 开通失败：第二台机器上下单后 Agent 回报失败 → failed + create_failed 告警
	u2 := e.must("POST", "/hooks/users", map[string]any{"verified": true, "balance": 100000})
	_, inst2, err := e.a.Rental.Place(ctx, rental.PlaceRequest{UserID: int64(u2["id"].(float64)), ProductID: dbtest.ProductID(t, e.a.Fleet.Pool()),
		ImageID: dbtest.ImageID(t, e.a.Fleet.Pool()), Duration: catalog.D1d})
	if err != nil {
		t.Fatal(err)
	}
	e.must("POST", "/hooks/agent", map[string]any{"fail": true})
	if got, _ := e.a.Rental.Get(ctx, inst2.ID); got.State != rental.StateFailed {
		t.Fatalf("after failed agent = %s, want failed", got.State)
	}
	unresolved := false
	alerts, _, err := e.a.Fleet.ListAlerts(ctx, &unresolved, paging.Page{Number: 1, Size: 20})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, al := range alerts {
		if al.Kind == "create_failed" && al.InstanceID == inst2.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("create_failed alert missing: %+v", alerts)
	}
}

func TestSweepWithoutHeartbeatSuspends(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.must("POST", "/hooks/machines", map[string]any{"count": 1})
	u := e.must("POST", "/hooks/users", map[string]any{"verified": true, "balance": 100000})
	_, inst, err := e.a.Rental.Place(ctx, rental.PlaceRequest{UserID: int64(u["id"].(float64)), ProductID: dbtest.ProductID(t, e.a.Fleet.Pool()),
		ImageID: dbtest.ImageID(t, e.a.Fleet.Pool()), Duration: catalog.D1d})
	if err != nil {
		t.Fatal(err)
	}
	e.must("POST", "/hooks/agent", map[string]any{})
	e.must("POST", "/hooks/clock/advance", map[string]int{"seconds": 360})
	e.must("POST", "/hooks/sweep", map[string]any{"heartbeat": false})
	if got, _ := e.a.Rental.Get(ctx, inst.ID); !got.Suspended() || got.State != rental.StateRunning {
		t.Fatalf("after offline sweep = %+v, want suspended running", got)
	}
}

func TestAdminsHook(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	out := e.must("POST", "/hooks/admins", map[string]any{"username": "e2e_root", "role": "super", "bind_totp": true})
	if out["password"] != "e2e-password-123456" || out["totp_secret"] == "" || len(out["recovery_codes"].([]any)) != 8 {
		t.Fatalf("admins = %v", out)
	}
	a, err := e.a.Admins.FindByUsername(ctx, "e2e_root")
	if err != nil || !a.TOTPConfirmed || string(a.Role) != "super" {
		t.Fatalf("admin = %+v, %v", a, err)
	}
	res, err := e.a.Admins.Login(ctx, "e2e_root", "e2e-password-123456", "127.0.0.1")
	if err != nil || res.NeedTOTPSetup {
		t.Fatalf("login = %+v, %v", res, err)
	}
	out = e.must("POST", "/hooks/admins", map[string]any{"username": "e2e_ops", "role": "ops", "bind_totp": false})
	if _, ok := out["totp_secret"]; ok {
		t.Fatalf("unbound admin must not return secret: %v", out)
	}
	if a, _ := e.a.Admins.FindByUsername(ctx, "e2e_ops"); a.TOTPConfirmed {
		t.Fatal("ops must not be bound")
	}
	if code, _ := e.call("POST", "/hooks/admins", map[string]any{"username": "e2e_root", "role": "super", "bind_totp": false}); code != 500 {
		t.Fatalf("duplicate username = %d, want 500", code)
	}
}

func TestKYCHook(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	u := e.must("POST", "/hooks/users", map[string]any{"verified": false})
	uid := int64(u["id"].(float64))
	e.must("POST", "/hooks/kyc", map[string]bool{"reject": true})
	if _, err := e.a.Identity.SubmitPersonalKYC(ctx, uid, "张三", "11010119900101123X"); apperr.As(err) == nil || apperr.As(err).Code != "kyc_rejected" {
		t.Fatalf("rejected kyc = %v", err)
	}
	e.must("POST", "/hooks/kyc", map[string]bool{"reject": false})
	if user, err := e.a.Identity.SubmitPersonalKYC(ctx, uid, "张三", "11010119900101123X"); err != nil || user.KYCStatus != "verified" {
		t.Fatalf("passed kyc = %+v, %v", user, err)
	}
}

func TestLoopbackOnly(t *testing.T) {
	e := newEnv(t)
	h := (&e2ehooks.Server{App: e.a, Clock: e.clk, KYC: &e2ehooks.SwitchableKYC{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.RemoteAddr = "10.0.0.5:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-loopback = %d, want 403", rec.Code)
	}
}

func TestUsersHookRejectsLedgerRowsAboveBalance(t *testing.T) {
	e := newEnv(t)
	// 余额 0 却要 3 条 +1 分流水：最终余额无法等于 balance，必须 400 而不是悄悄变成 3 分
	if code, _ := e.call("POST", "/hooks/users", map[string]any{"balance": 0, "ledger_rows": 3}); code != 400 {
		t.Fatalf("ledger_rows > balance = %d, want 400", code)
	}
	if code, _ := e.call("POST", "/hooks/users", map[string]any{"balance": -1}); code != 400 {
		t.Fatalf("negative balance = %d, want 400", code)
	}
}
