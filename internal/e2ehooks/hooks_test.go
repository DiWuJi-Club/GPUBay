package e2ehooks_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"spark-cloud/internal/app"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/e2ehooks"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/pki"
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
	keyFile := filepath.Join(t.TempDir(), "upstream.key") // 显式给上游密钥，免得 dev 下 Build 在 os.TempDir() 留一把
	if err := pki.GenerateUpstreamKey(keyFile); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Env: "dev", Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "mock", KYCDriver: "mock", UploadDir: t.TempDir(),
		AgentHosts: []string{"127.0.0.1"}, AgentDataSizeGiB: 2048, AgentBandwidthMbps: 200, AgentDNSServers: []string{"223.5.5.5"}, UpstreamSSHKeyFile: keyFile}
	a, err := app.Build(cfg, pool, log, app.Options{Clock: clk, KYC: k, DisableRateLimits: true, AgentTick: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = a.Gateway.Serve(ctx, lis) }()
	hooks := &e2ehooks.Server{App: a, Pool: pool, Clock: clk, KYC: k, Log: log, AgentAddr: lis.Addr().String()}
	ts := httptest.NewServer(hooks.Handler())
	t.Cleanup(func() { ts.Close(); hooks.Close(); cancel(); <-done })
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
	ids := e.must("POST", "/hooks/machines", map[string]any{"count": 3, "serial_prefix": "T-"})["ids"].([]any)
	if len(ids) != 3 {
		t.Fatalf("machines = %v", ids)
	}
	mid := int64(ids[0].(float64))
	if m, err := e.a.Fleet.Get(ctx, e.a.Fleet.Pool(), mid); err != nil || !m.AgentOnline || m.State != "idle" || m.Serial != "T-1" {
		t.Fatalf("machine = %+v, %v", m, err)
	}
	if m, _ := e.a.Fleet.Get(ctx, e.a.Fleet.Pool(), mid); !m.Enrolled || !m.Connected {
		t.Fatalf("hooks/machines must enroll and connect: %+v", m)
	}
	// 未指定前缀时多次调用不能撞序列号（随机 6 位曾在全量 e2e 中生日碰撞出 duplicate_serial）
	serials := map[string]bool{}
	for i := 0; i < 100; i++ {
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
	// 步进式：下单后不调 hooks/agent 就一直 creating（fake Agent 不自动执行）
	_, inst3, err := e.a.Rental.Place(ctx, rental.PlaceRequest{UserID: uid, ProductID: dbtest.ProductID(t, e.a.Fleet.Pool()),
		ImageID: dbtest.ImageID(t, e.a.Fleet.Pool()), Duration: catalog.D1d})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if got, _ := e.a.Rental.Get(ctx, inst3.ID); got.State != rental.StateCreating {
		t.Fatalf("stepped agent executed on its own: %s", got.State)
	}
	if out := e.must("POST", "/hooks/agent", map[string]any{"machine_id": inst3.MachineID}); out["completed"].(float64) != 1 {
		t.Fatalf("agent for machine = %v", out)
	}
	st := e.must("GET", fmt.Sprintf("/hooks/agent/status?machine_id=%d", inst3.MachineID), nil)
	if st["connected"] != true {
		t.Fatalf("status = %v", st)
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

// 后台 API 登记（未接入）的机器调 hooks/agent 时自动接入。
func TestAgentHookEnrollsUnknownMachine(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	m, err := e.a.Fleet.Register(ctx, fleet.NewMachine{Serial: "REG-1", LanIP: "10.99.0.9", ProductID: dbtest.ProductID(t, e.a.Fleet.Pool())})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.a.Fleet.SetMaintenance(ctx, m.ID, false); err != nil {
		t.Fatal(err)
	}
	if out := e.must("POST", "/hooks/agent", map[string]any{"machine_id": m.ID}); out["completed"].(float64) != 0 {
		t.Fatalf("agent = %v", out)
	}
	got, _ := e.a.Fleet.Get(ctx, e.a.Fleet.Pool(), m.ID)
	if !got.Enrolled || !got.Connected || !got.AgentOnline || got.LastSeenAt == nil {
		t.Fatalf("machine = %+v", got)
	}
}

// sweep 的 heartbeat 通过 fake Agent 发送：推进 6 分钟后 heartbeat=true 的 sweep 不会判离线。
func TestSweepHeartbeatGoesThroughAgent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ids := e.must("POST", "/hooks/machines", map[string]any{"count": 1})["ids"].([]any)
	mid := int64(ids[0].(float64))
	e.must("POST", "/hooks/clock/advance", map[string]int{"seconds": 360})
	e.must("POST", "/hooks/sweep", map[string]any{})
	m, _ := e.a.Fleet.Get(ctx, e.a.Fleet.Pool(), mid)
	if !m.AgentOnline || !m.LastSeenAt.Equal(e.clk.Now()) || len(m.LastMetrics) == 0 {
		t.Fatalf("after sweep = %+v", m)
	}
	e.must("POST", "/hooks/clock/advance", map[string]int{"seconds": 360})
	e.must("POST", "/hooks/sweep", map[string]any{"heartbeat": false})
	if m, _ = e.a.Fleet.Get(ctx, e.a.Fleet.Pool(), mid); m.AgentOnline {
		t.Fatal("heartbeat=false must let the sweeper mark offline")
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
