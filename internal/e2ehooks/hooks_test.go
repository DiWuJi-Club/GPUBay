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
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/access/sshtest"
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
	"spark-cloud/internal/referral"
	"spark-cloud/internal/rental"
	"spark-cloud/internal/sshgw"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

type env struct {
	t   *testing.T
	a   *app.App
	clk *clock.Fake
	ts  *httptest.Server
	up  *sshtest.Server
}

func newEnv(t *testing.T) *env { t.Helper(); return newEnvWith(t, false) }

// newEnvWith(apps = true) 与 cmd/e2e-server 的应用部署配置相同：假容器 sshd 用 shell 模式、每实例一个应用根目录、
// 健康检查 / 应用 Web 打到一个 httptest；结束时先停掉容器里 setsid 出去的 fake 服务进程。
func newEnvWith(t *testing.T, apps bool) *env {
	t.Helper()
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	k := &e2ehooks.SwitchableKYC{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	keyFile := filepath.Join(t.TempDir(), "upstream.key") // 显式给上游密钥，免得 dev 下 Build 在 os.TempDir() 留一把
	if err := pki.GenerateUpstreamKey(keyFile); err != nil {
		t.Fatal(err)
	}
	// 与 cmd/e2e-server 相同：进程内假容器 sshd 作为所有实例的上游（共用一把主机密钥），SSH 网关监听随机端口
	upSigner, err := pki.LoadUpstreamSigner(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	hostPub, hostPEM, err := pki.GenerateInstanceHostKey()
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.ParsePrivateKey(hostPEM)
	if err != nil {
		t.Fatal(err)
	}
	upOpts := sshtest.Options{AuthorizedKey: upSigner.PublicKey(), HostKey: hostSigner}
	if apps {
		upOpts.Shell, upOpts.Root = true, t.TempDir()
	}
	up, err := sshtest.New(upOpts)
	if err != nil {
		t.Fatal(err)
	}
	opt := app.Options{Clock: clk, KYC: k, DisableRateLimits: true, AgentTick: 20 * time.Millisecond,
		HostKeyGen: func() (string, []byte, error) { return hostPub, hostPEM, nil }}
	healthAddr := ""
	if apps {
		health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, "spark-app-ok|path=%s", r.URL.Path) }))
		t.Cleanup(health.Close)
		healthAddr = strings.TrimPrefix(health.URL, "http://")
		root := up.Root()
		opt.AppHealthAddr = func(int) string { return healthAddr }
		opt.AppRootFor = func(id string) string { return filepath.Join(root, id, "root", "data") }
		t.Cleanup(func() { e2ehooks.StopFakeApps(root) })
	}
	cfg := config.Config{Env: "dev", Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "mock", KYCDriver: "mock", UploadDir: t.TempDir(),
		AgentHosts: []string{"127.0.0.1"}, AgentDataSizeGiB: 2048, AgentBandwidthMbps: 200, AgentDNSServers: []string{"223.5.5.5"}, UpstreamSSHKeyFile: keyFile,
		UpstreamSSHPort: up.Port()}
	a, err := app.Build(cfg, pool, log, opt)
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sshLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = a.Gateway.Serve(ctx, lis) }()
	sshDone := make(chan struct{})
	go func() { defer close(sshDone); _ = a.SSH.Serve(ctx, sshLis) }()
	hooks := &e2ehooks.Server{App: a, Pool: pool, Clock: clk, KYC: k, Log: log, AgentAddr: lis.Addr().String(), SSHAddr: sshLis.Addr().String(), Upstream: up,
		HealthAddr: healthAddr}
	ts := httptest.NewServer(hooks.Handler())
	t.Cleanup(func() {
		ts.Close()
		hooks.Close()
		cancel()
		<-done
		<-sshDone
		a.Upstream.Close()
		up.Close()
	})
	return &env{t: t, a: a, clk: clk, ts: ts, up: up}
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
	// sweep 也跑 Referral.Settle：扫描水位离开迁移写入的初值 2000-01-01，推进到 now − ScanLag
	var watermark time.Time
	if err := e.a.Fleet.Pool().QueryRow(ctx, `SELECT watermark FROM referral_state WHERE id = 1`).Scan(&watermark); err != nil ||
		!watermark.Equal(e.clk.Now().Add(-referral.ScanLag)) {
		t.Fatalf("referral watermark = %v %v, want sweep to run Referral.Settle", watermark, err)
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

// 造的机器 lan_ip 为 127.0.0.1（网关与 Web 入口拨进程内假容器 sshd）；/hooks/ssh 经真实网关登录：
// 不存在的实例 → error 非空、output 带 Banner；正确密码 → 执行命令；错误密码 → error 非空。
// /hooks/ingress-upstream 登记回显服务并立即刷新路由表。
func TestSSHAndIngressUpstreamHooks(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ids := e.must("POST", "/hooks/machines", map[string]any{"count": 1})["ids"].([]any)
	m, err := e.a.Fleet.Get(ctx, e.a.Fleet.Pool(), int64(ids[0].(float64)))
	if err != nil || m.LanIP != "127.0.0.1" {
		t.Fatalf("machine lan_ip = %q, %v", m.LanIP, err)
	}
	out := e.must("POST", "/hooks/ssh", map[string]any{"instance_id": "ins-zzzzzzzz", "password": "x", "command": "echo hi"})
	if out["error"] == "" || !strings.Contains(out["output"].(string), sshgw.BannerUnknown) || out["exit_status"].(float64) != -1 {
		t.Fatalf("unknown instance = %v", out)
	}
	if code, _ := e.call("POST", "/hooks/ssh", map[string]any{"instance_id": "ins-zzzzzzzz", "key_pem": "garbage", "command": "echo"}); code != 400 {
		t.Fatalf("bad key_pem = %d, want 400", code)
	}

	u := e.must("POST", "/hooks/users", map[string]any{"verified": true, "balance": 100000})
	uid := int64(u["id"].(float64))
	_, inst, err := e.a.Rental.Place(ctx, rental.PlaceRequest{UserID: uid, ProductID: dbtest.ProductID(t, e.a.Fleet.Pool()),
		ImageID: dbtest.ImageID(t, e.a.Fleet.Pool()), Duration: catalog.D1d})
	if err != nil {
		t.Fatal(err)
	}
	e.must("POST", "/hooks/agent", map[string]any{})
	pw, err := e.a.Rental.ResetPassword(ctx, uid, inst.ID, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if out := e.must("POST", "/hooks/ssh", map[string]any{"instance_id": inst.ID, "password": pw, "command": "echo hi"}); out["error"] != "" || out["output"] != "hi\n" || out["exit_status"].(float64) != 0 {
		t.Fatalf("password login = %v", out)
	}
	if out := e.must("POST", "/hooks/ssh", map[string]any{"instance_id": inst.ID, "password": "wrong", "command": "echo hi"}); out["error"] == "" {
		t.Fatalf("wrong password accepted: %v", out)
	}

	if _, err := e.a.Rental.AddPort(ctx, uid, inst.ID, 8888, "public", "", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.a.Routes.Lookup(inst.ID, 8888); ok {
		t.Fatal("route present before refresh (test premise)")
	}
	e.must("POST", "/hooks/ingress-upstream", map[string]any{"port": 8888, "body": "first"})
	if _, ok := e.a.Routes.Lookup(inst.ID, 8888); !ok {
		t.Fatal("ingress-upstream hook must refresh the route table")
	}
	e.must("POST", "/hooks/ingress-upstream", map[string]any{"port": 8888, "body": "second"}) // 再次登记同一端口：替换回显内容
	front := httptest.NewServer(e.a.Handler)
	defer front.Close()
	req, _ := http.NewRequest("GET", front.URL+"/x?y=1", nil)
	req.Host = inst.ID + "-8888.spark.test"
	req.Header.Set("Cookie", "app=1; sc_ingress=forged")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if want := "second|host=" + inst.ID + "-8888.spark.test|path=/x?y=1|cookie=app=1"; resp.StatusCode != 200 || string(b) != want {
		t.Fatalf("ingress = %d %q, want %q", resp.StatusCode, b, want)
	}
	if code, _ := e.call("POST", "/hooks/ingress-upstream", map[string]any{"port": 0, "body": "x"}); code != 400 {
		t.Fatalf("port 0 = %d, want 400", code)
	}
}

// POST /hooks/groups 建机组（visible 缺省 true）；POST /hooks/machines 带 group_id 时登记后入组。
func TestGroupsHookAndMachinesInGroup(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	gid := int64(e.must("POST", "/hooks/groups", map[string]any{"name": "A"})["id"].(float64))
	hidden := int64(e.must("POST", "/hooks/groups", map[string]any{"name": "留池", "visible": false})["id"].(float64))
	groups, err := e.a.Fleet.ListGroups(ctx, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	vis := map[int64]bool{}
	for _, g := range groups {
		vis[g.ID] = g.Visible
	}
	if v, ok := vis[gid]; !ok || !v {
		t.Fatalf("group A visible = %v (present %v)", v, ok)
	}
	if v, ok := vis[hidden]; !ok || v {
		t.Fatalf("hidden group visible = %v (present %v)", v, ok)
	}
	ids := e.must("POST", "/hooks/machines", map[string]any{"count": 1, "group_id": gid})["ids"].([]any)
	m, err := e.a.Fleet.Get(ctx, e.a.Fleet.Pool(), int64(ids[0].(float64)))
	if err != nil || m.GroupID == nil || *m.GroupID != gid {
		t.Fatalf("machine group = %+v, %v", m.GroupID, err)
	}
	ids = e.must("POST", "/hooks/machines", map[string]any{"count": 1})["ids"].([]any)
	if m, _ := e.a.Fleet.Get(ctx, e.a.Fleet.Pool(), int64(ids[0].(float64))); m.GroupID != nil {
		t.Fatalf("machine without group_id = %v", *m.GroupID)
	}
}

// /hooks/users 带 invite_code / device_id / ip 时走真实注册路径（SendLoginCode + LoginWith）：新用户被绑定、设备被记录，
// 余额与实名照常写入；对已存在的手机号只是再登录一次（记设备），不重置实名、不重复入账（裁定 Q9）。
func TestUsersHookRealRegistrationWithInvite(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	pool := e.a.Fleet.Pool()
	inviter := e.must("POST", "/hooks/users", map[string]any{"phone": "13900000011", "verified": true, "balance": 1000})
	inviterID := int64(inviter["id"].(float64))
	acc, err := e.a.Referral.EnsureCode(ctx, inviterID)
	if err != nil {
		t.Fatal(err)
	}
	dev := "6f1c2a3b-4d5e-4f60-8a7b-9c0d1e2f3a4b"
	out := e.must("POST", "/hooks/users", map[string]any{"phone": "13900000012", "verified": true, "balance": 5000, "ledger_rows": 2,
		"invite_code": strings.ToLower(acc.Code), "device_id": dev, "ip": "10.1.2.3"})
	inviteeID := int64(out["id"].(float64))
	inv, _ := out["invite"].(map[string]any)
	if inv == nil || inv["bound"] != true || inv["reason"] != "ok" || inv["inviter"] != "139****0011" || out["phone"] != "13900000012" {
		t.Fatalf("users with invite_code = %v", out)
	}
	var n int
	var gotInviter int64
	var ip, status string
	if err := pool.QueryRow(ctx, `SELECT count(*), max(inviter_id), max(ip), max(status) FROM referral_bindings WHERE invitee_id = $1`, inviteeID).
		Scan(&n, &gotInviter, &ip, &status); err != nil || n != 1 || gotInviter != inviterID || ip != "10.1.2.3" || status != "active" {
		t.Fatalf("binding = %d %d %q %q %v", n, gotInviter, ip, status, err)
	}
	if user, err := e.a.Identity.GetUser(ctx, inviteeID); err != nil || user.KYCStatus != "verified" || user.Kind != "personal" {
		t.Fatalf("invitee = %+v, %v", user, err)
	}
	if bal, _ := e.a.Wallet.Balance(ctx, inviteeID); bal != 5000 {
		t.Fatalf("invitee balance = %d, want 5000", bal)
	}
	if rows, _ := e.a.Wallet.ListLedger(ctx, inviteeID, 0, 100); len(rows) != 3 {
		t.Fatalf("invitee ledger rows = %d, want 3", len(rows))
	}

	// 已存在的手机号：只登录（记设备 D），不重置实名、不重复入账
	again := e.must("POST", "/hooks/users", map[string]any{"phone": "13900000011", "device_id": dev})
	if int64(again["id"].(float64)) != inviterID {
		t.Fatalf("existing phone id = %v, want %d", again["id"], inviterID)
	}
	if user, _ := e.a.Identity.GetUser(ctx, inviterID); user.KYCStatus != "verified" {
		t.Fatalf("existing user kyc reset to %q", user.KYCStatus)
	}
	if bal, _ := e.a.Wallet.Balance(ctx, inviterID); bal != 1000 {
		t.Fatalf("existing user balance = %d, want 1000 (no re-seed)", bal)
	}
	var devices int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM user_devices WHERE user_id = $1 AND device_id = $2`, inviterID, dev).Scan(&devices); err != nil || devices != 1 {
		t.Fatalf("inviter device = %d, %v", devices, err)
	}
	// 与邀请人同设备注册 → 绑定被搁置
	held := e.must("POST", "/hooks/users", map[string]any{"phone": "13900000013", "invite_code": acc.Code, "device_id": dev})
	if err := pool.QueryRow(ctx, `SELECT status FROM referral_bindings WHERE invitee_id = $1`, int64(held["id"].(float64))).Scan(&status); err != nil || status != "held" {
		t.Fatalf("same-device binding = %q, %v", status, err)
	}
	// 老用户带码：不绑定
	old := e.must("POST", "/hooks/users", map[string]any{"phone": "13900000012", "invite_code": acc.Code})
	if inv, _ := old["invite"].(map[string]any); inv == nil || inv["bound"] != false || inv["reason"] != "existing_user" {
		t.Fatalf("existing user with code = %v", old)
	}
	// 不带三者时仍是直接插入：没有 invite 字段
	if plain := e.must("POST", "/hooks/users", map[string]any{}); plain["invite"] != nil {
		t.Fatalf("plain users invite = %v", plain["invite"])
	}
}

// /hooks/referral/config 插一版费率：未给的字段沿用当前版本，立即生效。
func TestReferralConfigHook(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	before, err := e.a.Referral.CurrentConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out := e.must("POST", "/hooks/referral/config", map[string]any{"hold_days": 0})
	cur, err := e.a.Referral.CurrentConfig(ctx)
	if err != nil || cur.HoldDays != 0 || cur.RateBP != before.RateBP || cur.MaxPerInvitee != before.MaxPerInvitee ||
		cur.MaxPerInviter30d != before.MaxPerInviter30d || cur.PeriodDays != before.PeriodDays || int64(out["id"].(float64)) != cur.ID {
		t.Fatalf("after hold_days 0: %+v (before %+v) %v, out %v", cur, before, err, out)
	}
	e.must("POST", "/hooks/referral/config", map[string]any{"rate_bp": 1500})
	if cur, _ = e.a.Referral.CurrentConfig(ctx); cur.RateBP != 1500 || cur.HoldDays != 0 {
		t.Fatalf("after rate 1500: %+v", cur)
	}
	if code, _ := e.call("POST", "/hooks/referral/config", map[string]any{"rate_bp": 9999}); code == 200 {
		t.Fatal("invalid rate accepted")
	}
}

// /hooks/sweep 与生产清扫器一样持有清扫咨询锁（rental.SweeperLockKey），并发的钩子清扫也逐个执行：
// 两轮 Rental.Sweep 交叠会在 fleet.MarkOfflineStale 的多行 UPDATE 上互相死锁。
func TestSweepHookTakesSweeperLock(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.must("POST", "/hooks/machines", map[string]any{"count": 3})
	conn, err := e.a.Fleet.Pool().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, rental.SweeperLockKey); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		code, _ := e.call("POST", "/hooks/sweep", map[string]any{})
		done <- code
	}()
	select {
	case code := <-done:
		t.Fatalf("sweep finished (%d) while another sweeper held the lock", code)
	case <-time.After(500 * time.Millisecond):
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, rental.SweeperLockKey); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 200 {
			t.Fatalf("sweep after unlock = %d", code)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("sweep still blocked after unlock")
	}
	// 并发调用全部成功（逐个执行，不死锁、不耗尽连接池）
	e.must("POST", "/hooks/clock/advance", map[string]int{"seconds": 360})
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			code, out := e.call("POST", "/hooks/sweep", map[string]any{"heartbeat": i%2 == 0})
			if code != 200 {
				errs <- fmt.Errorf("sweep = %d %v", code, out)
				return
			}
			errs <- nil
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}
