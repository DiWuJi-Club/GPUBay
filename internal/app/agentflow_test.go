package app_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentclient "spark-cloud/internal/agent/client"

	"spark-cloud/internal/agent/fakeagent"
	"spark-cloud/internal/agent/metrics"
	"spark-cloud/internal/app"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/pki"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/config"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/rental"
)

const flowTick = 20 * time.Millisecond

type flow struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool
	clk  *clock.Fake
	a    *app.App
	addr string
	ts   *httptest.Server
	seq  int
}

// upstreamKeyFile 生成一把测试用上游密钥：否则 dev 下 Build 会在 os.TempDir() 生成一把永不清理的密钥。
func upstreamKeyFile(t *testing.T) string {
	t.Helper()
	keyFile := filepath.Join(t.TempDir(), "upstream.key")
	if err := pki.GenerateUpstreamKey(keyFile); err != nil {
		t.Fatal(err)
	}
	return keyFile
}

func buildApp(t *testing.T, pool *pgxpool.Pool, clk *clock.Fake) (*app.App, string) {
	t.Helper()
	cfg := config.Config{Env: "dev", UploadDir: t.TempDir(), Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "mock", KYCDriver: "mock",
		AgentAddr: "127.0.0.1:0", AgentHosts: []string{"127.0.0.1"}, AgentDataSizeGiB: 2048, AgentBandwidthMbps: 200, AgentDNSServers: []string{"223.5.5.5"},
		UpstreamSSHKeyFile: upstreamKeyFile(t)}
	a, err := app.Build(cfg, pool, slog.New(slog.NewTextHandler(io.Discard, nil)), app.Options{Clock: clk, AgentTick: flowTick, DisableRateLimits: true})
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
	t.Cleanup(func() { cancel(); <-done })
	return a, lis.Addr().String()
}

func newFlow(t *testing.T) *flow {
	t.Helper()
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	a, addr := buildApp(t, pool, clk)
	ts := httptest.NewServer(a.Handler)
	t.Cleanup(ts.Close)
	return &flow{t: t, ctx: context.Background(), pool: pool, clk: clk, a: a, addr: addr, ts: ts}
}

// enrollAgent 登记一台机器并接入一个 fake Agent；opts 可改 Options（例如顶替类用例给旧 Agent 设 NoReconnect）。
func (f *flow) enrollAgent(serial string, stepped bool, opts ...func(*fakeagent.Options)) (int64, *fakeagent.Agent) {
	f.t.Helper()
	mid := dbtest.CreateMachine(f.t, f.pool, serial)
	_, _ = f.pool.Exec(f.ctx, `UPDATE machines SET agent_online = false, last_seen_at = NULL WHERE id = $1`, mid)
	tok, err := f.a.Fleet.IssueEnrollToken(f.ctx, audit.Actor{}, mid)
	if err != nil {
		f.t.Fatal(err)
	}
	o := fakeagent.Options{Addr: f.addr, CAPEM: f.a.Gateway.CAPEM(), Token: tok.Plain, Stepped: stepped,
		Metrics: metrics.Metrics{GPUTempC: 55, RootDiskTotal: 1000, RootDiskUsed: 400, MemTotal: 128 << 30}}
	for _, opt := range opts {
		opt(&o)
	}
	ag, err := fakeagent.Start(f.ctx, o)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(ag.Stop)
	f.waitFor("first report", func() bool { return f.a.Gateway.ReportCount(mid) >= 1 })
	return mid, ag
}

func (f *flow) renter() *client {
	f.t.Helper()
	f.seq++
	jar, _ := cookiejar.New(nil)
	c := &client{t: f.t, base: f.ts.URL + "/api/v1", http: &http.Client{Jar: jar}}
	phone := fmt.Sprintf("138%08d", f.seq)
	c.do("POST", "/auth/sms-code", map[string]string{"phone": phone})
	c.do("POST", "/auth/login", map[string]string{"phone": phone, "code": "000000"})
	c.do("POST", "/kyc/personal", map[string]string{"name": "李四", "id_number": "110101199001011234"})
	rch := c.do("POST", "/recharges", map[string]any{"amount": 1000000, "channel": "wechat"})
	c.do("POST", "/dev/recharges/"+rch["id"].(string)+"/pay", map[string]any{})
	return c
}

func (f *flow) order(c *client) string {
	f.t.Helper()
	products := c.do("GET", "/products", nil)["items"].([]any)
	images := c.do("GET", "/images", nil)["items"].([]any)
	order := c.do("POST", "/orders", map[string]any{"product_id": products[0].(map[string]any)["id"], "duration": "1d", "image_id": images[0].(map[string]any)["id"]})
	return order["instance"].(map[string]any)["id"].(string)
}

func (f *flow) state(id string) rental.Instance {
	f.t.Helper()
	inst, err := f.a.Rental.Get(f.ctx, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return inst
}

func (f *flow) waitState(id, want string) {
	f.t.Helper()
	f.waitFor("instance "+id+" → "+want, func() bool { return f.state(id).State == want })
}

func (f *flow) waitFor(what string, cond func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatalf("timeout waiting for %s", what)
}

func (f *flow) sweep() {
	f.t.Helper()
	if err := f.a.Rental.Sweep(f.ctx); err != nil {
		f.t.Fatal(err)
	}
}

// syncHeartbeat 推进假时钟后等机器的 last_seen_at 追上当前假时间（自动模式 Agent 每 100ms 心跳；步进模式自己发一次），
// 否则清扫器会把机器判离线、把实例暂停。
func (f *flow) syncHeartbeat(mid int64, ag *fakeagent.Agent, stepped bool) {
	f.t.Helper()
	if stepped {
		if err := ag.Client.SendHeartbeat(f.ctx); err != nil {
			f.t.Fatal(err)
		}
	}
	f.waitFor("heartbeat at current fake time", func() bool {
		m, _ := f.a.Fleet.Get(f.ctx, f.pool, mid)
		return m.LastSeenAt != nil && m.LastSeenAt.Equal(f.clk.Now())
	})
}

func (f *flow) alertCount(kind string) int {
	var n int
	_ = f.pool.QueryRow(f.ctx, `SELECT count(*) FROM alerts WHERE kind = $1`, kind).Scan(&n)
	return n
}

func (f *flow) commandID(instanceID, typ string) string {
	cmds, _ := f.a.Fleet.CommandsForInstance(f.ctx, instanceID, 20)
	for _, c := range cmds {
		if string(c.Type) == typ {
			return c.ID
		}
	}
	return ""
}

// 自动模式 Agent：接入 → 下单 → 开通 → 到期停机 → 续费恢复 → 释放 → 洗机 → 机器回到库存。
func TestAgentFullLifecycle(t *testing.T) {
	f := newFlow(t)
	mid, ag := f.enrollAgent("SN-001", false)
	m, _ := f.a.Fleet.Get(f.ctx, f.pool, mid)
	if !m.Enrolled || !m.Connected || !m.AgentOnline {
		t.Fatalf("machine after enroll = %+v", m)
	}
	c := f.renter()
	id := f.order(c)
	f.waitState(id, rental.StateRunning)
	if ct := ag.Runtime.Containers()[id]; !ct.Running {
		t.Fatalf("container = %+v", ct)
	}
	if _, mounted := ag.Disk.State(id); !mounted {
		t.Fatal("data disk not mounted")
	}
	// 指标到库
	f.waitFor("metrics", func() bool { m, _ := f.a.Fleet.Get(f.ctx, f.pool, mid); return len(m.LastMetrics) > 0 })

	f.clk.Advance(24*time.Hour + time.Second)
	f.syncHeartbeat(mid, ag, false)
	f.sweep()
	f.waitState(id, rental.StateStopped)
	f.waitFor("container stopped", func() bool { return !ag.Runtime.Containers()[id].Running })

	c.do("POST", "/instances/"+id+"/renew", map[string]string{"duration": "1d"})
	f.waitFor("container running again", func() bool { return ag.Runtime.Containers()[id].Running })
	f.waitState(id, rental.StateRunning)

	c.do("POST", "/instances/"+id+"/release", map[string]any{})
	f.waitState(id, rental.StateReleased)
	if ct := ag.Runtime.Containers()[id]; ct.Exists {
		t.Fatal("container not wiped")
	}
	if created, _ := ag.Disk.State(id); created {
		t.Fatal("disk not destroyed")
	}
	f.waitFor("machine idle", func() bool { m, _ := f.a.Fleet.Get(f.ctx, f.pool, mid); return m.State == "idle" })
	var open int
	_ = f.pool.QueryRow(f.ctx, `SELECT count(*) FROM agent_commands WHERE status IN ('pending','sent')`).Scan(&open)
	if open != 0 {
		t.Fatalf("open commands = %d", open)
	}
}

// 断线重连补推：Agent 掉线期间入队的指令在重连后被投递执行。
func TestReconnectDeliversQueuedCommands(t *testing.T) {
	f := newFlow(t)
	mid, ag := f.enrollAgent("SN-001", false)
	ag.Stop()
	f.waitFor("disconnected", func() bool { return !f.a.Gateway.Connected(mid) })
	c := f.renter()
	id := f.order(c) // 机器 5 分钟内仍 agent_online，可被分配（设计 §3.1 第 7 条）
	time.Sleep(5 * flowTick)
	if f.state(id).State != rental.StateCreating {
		t.Fatal("must wait for agent")
	}
	ag2, err := fakeagent.Start(f.ctx, fakeagent.Options{Addr: f.addr, CAPEM: f.a.Gateway.CAPEM(), Creds: ag.Creds(), Dir: ag.Dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ag2.Stop)
	f.waitState(id, rental.StateRunning)
}

// 迟到回报：指令被平台判超时后 Agent 的成功回报被忽略，对账再把实际建出来的容器清掉。
func TestLateResultIgnoredAndOrphanWiped(t *testing.T) {
	f := newFlow(t)
	mid, ag := f.enrollAgent("SN-001", true)
	c := f.renter()
	id := f.order(c)
	f.waitFor("delivered", func() bool { return ag.Client.Received(f.commandID(id, "create_instance")) })
	f.clk.Advance(rental.CreateCommandTimeout + time.Second)
	f.syncHeartbeat(mid, ag, true) // 机器仍在线，指令超时才会计时
	f.sweep()
	f.waitState(id, rental.StateFailed)
	if _, err := ag.Client.Step(f.ctx, nil); err != nil { // Agent 现在才执行：容器真的建出来了
		t.Fatal(err)
	}
	f.waitFor("orphan wipe enqueued", func() bool { return f.alertCount("orphan_container") == 1 })
	if f.state(id).State != rental.StateFailed {
		t.Fatal("late success revived instance")
	}
	f.waitFor("wipe delivered", func() bool { return ag.Client.Received(f.commandID(id, "wipe_instance")) })
	if _, err := ag.Client.Step(f.ctx, nil); err != nil {
		t.Fatal(err)
	}
	f.waitFor("container gone", func() bool { return !ag.Runtime.Containers()[id].Exists })
	m, _ := f.a.Fleet.Get(f.ctx, f.pool, mid)
	if m.State != "faulty" { // 开通失败照旧隔离机器
		t.Fatalf("machine = %s", m.State)
	}
}

// 离线恢复：容器不在 → 保持暂停并告警；容器在 → 恢复计时。
func TestOfflineResumeDependsOnContainer(t *testing.T) {
	f := newFlow(t)
	mid, ag := f.enrollAgent("SN-001", true)
	c := f.renter()
	id := f.order(c)
	f.waitFor("delivered", func() bool { return ag.Client.Received(f.commandID(id, "create_instance")) })
	if _, err := ag.Client.Step(f.ctx, nil); err != nil {
		t.Fatal(err)
	}
	f.waitState(id, rental.StateRunning)
	f.clk.Advance(time.Hour)
	f.syncHeartbeat(mid, ag, true)
	f.clk.Advance(rental.OfflineTimeout + time.Minute)
	f.sweep()
	if inst := f.state(id); !inst.Suspended() {
		t.Fatalf("not suspended: %+v", inst)
	}
	ag.Runtime.Exit(id) // 失联期间容器退出了
	if err := ag.Client.SendHeartbeat(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := ag.Client.SendStateReport(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.waitFor("container_down alert", func() bool { return f.alertCount("container_down") == 1 })
	if !f.state(id).Suspended() {
		t.Fatal("resumed although container is down")
	}
	_ = ag.Runtime.Start(f.ctx, id) // 管理员手工拉起
	if err := ag.Client.SendStateReport(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.waitFor("resumed", func() bool { return !f.state(id).Suspended() })
}

// 新连接顶掉旧连接；指纹不符被拒。旧 Agent 用 NoReconnect，否则它会在退避后重新连上再把新 Agent 顶掉（乒乓）。
func TestReplaceAndRejectedFingerprint(t *testing.T) {
	f := newFlow(t)
	noReconnect := func(o *fakeagent.Options) { o.NoReconnect = true }
	mid, ag := f.enrollAgent("SN-001", true, noReconnect)
	ag2, err := fakeagent.Start(f.ctx, fakeagent.Options{Addr: f.addr, CAPEM: f.a.Gateway.CAPEM(), Creds: ag.Creds(), Stepped: true, NoReconnect: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ag2.Stop)
	f.waitFor("old stream replaced", func() bool { return !ag.Client.Connected() })
	f.waitFor("new stream active", func() bool { return ag2.Client.Connected() && f.a.Gateway.Connected(mid) })
	// 重新接入签出新证书：ag2 的流在下一个 tick 被断，旧凭据再连被拒
	tok, _ := f.a.Fleet.IssueEnrollToken(f.ctx, audit.Actor{}, mid)
	ag3, err := fakeagent.Start(f.ctx, fakeagent.Options{Addr: f.addr, CAPEM: f.a.Gateway.CAPEM(), Token: tok.Plain, Stepped: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ag3.Stop)
	f.waitFor("ag2 aborted", func() bool { return !ag2.Client.Connected() })
	ctx, cancel := context.WithTimeout(f.ctx, 2*time.Second)
	defer cancel()
	_, err = fakeagent.Start(ctx, fakeagent.Options{Addr: f.addr, CAPEM: f.a.Gateway.CAPEM(), Creds: ag.Creds(), Stepped: true, NoReconnect: true})
	if status.Code(err) != grpccodes.Unauthenticated || !strings.Contains(err.Error(), "certificate not recognised") {
		t.Fatalf("old certificate: err = %v, want Unauthenticated「certificate not recognised」", err)
	}
	f.waitFor("ag3 still active", func() bool { return ag3.Client.Connected() && f.a.Gateway.Connected(mid) })
}

// DisableRateLimits 同样放开 Enroll 按 IP 限流（e2e 一次从 127.0.0.1 接入几十台机器）；未设置时第 11 次被限流。
func TestDisableRateLimitsLiftsEnrollLimit(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		cfg := config.Config{UpstreamSSHKeyFile: upstreamKeyFile(t), Env: "dev", Domain: "spark.test", SecretKey: dbtest.Key,
			SMSDriver: "mock", KYCDriver: "mock", UploadDir: t.TempDir(), AgentHosts: []string{"127.0.0.1"}}
		a, err := app.Build(cfg, dbtest.New(t), discardLog(), app.Options{Clock: clock.NewFake(dbtest.T0), DisableRateLimits: disabled})
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
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		var got []grpccodes.Code
		for range 11 {
			_, _, err := agentclient.Enroll(ctx, lis.Addr().String(), "spk1_bogus", a.Gateway.CAFingerprint(), key)
			got = append(got, status.Code(err))
		}
		cancel()
		<-done
		last := got[len(got)-1]
		if disabled && last != grpccodes.PermissionDenied || !disabled && last != grpccodes.ResourceExhausted {
			t.Fatalf("disabled=%v: codes = %v", disabled, got)
		}
	}
}

// 上游 SSH 密钥：非生产环境未配置时在 os.TempDir() 自动生成；生产环境未配置则 Build 失败。
func TestBuildUpstreamKeyFallbackOnlyOutsideProd(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	cfg := config.Config{Env: "dev", Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "mock", KYCDriver: "mock", UploadDir: t.TempDir()}
	if _, err := app.Build(cfg, dbtest.New(t), discardLog(), app.Options{}); err != nil {
		t.Fatal(err)
	}
	if keys, _ := filepath.Glob(filepath.Join(tmp, "spark-upstream-*.key")); len(keys) != 1 {
		t.Fatalf("generated keys = %v", keys)
	}

	prod := config.Config{Env: "prod", Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "aliyun", KYCDriver: "aliyun",
		PayDriver: "real", PDUDriver: "snmp", UploadDir: t.TempDir(), AgentHosts: []string{"agent.spark.test"}}
	prodPool := dbtest.New(t)
	if _, err := app.Build(prod, prodPool, discardLog(), app.Options{}); err == nil || !strings.Contains(err.Error(), "SPARK_UPSTREAM_SSH_KEY_FILE") {
		t.Fatalf("err = %v, want 生产环境必须设置 SPARK_UPSTREAM_SSH_KEY_FILE", err)
	}
	var cas int // 配置错误先于 EnsureCA 报出，不留数据库写入
	if err := prodPool.QueryRow(context.Background(), `SELECT count(*) FROM platform_pki`).Scan(&cas); err != nil || cas != 0 {
		t.Fatalf("platform_pki rows = %d, %v", cas, err)
	}
	if keys, _ := filepath.Glob(filepath.Join(tmp, "spark-upstream-*.key")); len(keys) != 1 {
		t.Fatalf("prod must not generate a key: %v", keys)
	}
	prod.UpstreamSSHKeyFile = upstreamKeyFile(t)
	if _, err := app.Build(prod, dbtest.New(t), discardLog(), app.Options{}); err != nil {
		t.Fatal(err)
	}
}

// 内置 CA 的有效期按墙上时钟：注入的假时钟领先真实时间时，CA 也必须已经生效。
func TestCAValidityFollowsWallClock(t *testing.T) {
	cfg := config.Config{UpstreamSSHKeyFile: upstreamKeyFile(t), Env: "dev", Domain: "spark.test", SecretKey: dbtest.Key,
		SMSDriver: "mock", KYCDriver: "mock", UploadDir: t.TempDir()}
	future := clock.NewFake(time.Now().AddDate(1, 0, 0))
	a, err := app.Build(cfg, dbtest.New(t), discardLog(), app.Options{Clock: future})
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(a.CA.CertDER())
	if err != nil {
		t.Fatal(err)
	}
	if now := time.Now(); now.Before(ca.NotBefore) || now.After(ca.NotAfter) {
		t.Fatalf("CA validity %v..%v does not cover wall time %v", ca.NotBefore, ca.NotAfter, now)
	}
}
