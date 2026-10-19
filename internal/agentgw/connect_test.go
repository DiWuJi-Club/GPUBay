package agentgw_test

import (
	"math"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"spark-cloud/internal/agentpb"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/rental"
)

func TestConnectRequiresValidCert(t *testing.T) {
	e := newEnv(t)
	mid := e.machine("SN-001")
	// 没有客户端证书
	a := e.connect(nil)
	a.hello()
	if err := a.waitErr(2 * time.Second); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("no cert = %v", err)
	}
	old := e.enroll(mid)
	fresh := e.enroll(mid) // 重新接入：旧证书失效
	a = e.connect(old)
	a.hello()
	if err := a.waitErr(2 * time.Second); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("old cert = %v", err)
	}
	a = e.connect(fresh)
	a.hello()
	if m := a.next(2 * time.Second); m == nil || m.GetConfig() == nil {
		t.Fatalf("fresh cert must get config, got %v", m)
	}
	// 撤销接入后同一证书被拒
	if err := e.fleet.RevokeEnrollment(e.ctx, audit.Actor{AdminID: 1}, mid); err != nil {
		t.Fatal(err)
	}
	a2 := e.connect(fresh)
	a2.hello()
	if err := a2.waitErr(2 * time.Second); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("revoked = %v", err)
	}
	// 第一条不是 Hello → InvalidArgument
	creds := e.enroll(mid)
	a3 := e.connect(creds)
	a3.report()
	if err := a3.waitErr(2 * time.Second); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("no hello = %v", err)
	}
}

func TestConnectConfigHeartbeatMetricsAndAlerts(t *testing.T) {
	e := newEnv(t)
	mid := e.machine("SN-001")
	_, _ = e.pool.Exec(e.ctx, `UPDATE machines SET agent_online = false, last_seen_at = NULL WHERE id = $1`, mid)
	creds := e.enroll(mid)
	a := e.connect(creds)
	a.hello("10.0.0.10", "192.168.1.5")
	cfg := a.next(2 * time.Second).GetConfig()
	if cfg.GetLanIp() != "10.0.0.10" || cfg.GetHeartbeatIntervalS() != 10 || cfg.GetDnsServers()[0] != "223.5.5.5" ||
		cfg.GetAllowCidrs()[0] != "10.0.9.0/24" || cfg.GetBandwidthMbps() != 200 || cfg.GetDataSizeGib() != 2048 {
		t.Fatalf("config = %v", cfg)
	}
	e.waitFor("online", func() bool { m, _ := e.fleet.Get(e.ctx, e.pool, mid); return m.AgentOnline && m.Connected })
	m, _ := e.fleet.Get(e.ctx, e.pool, mid)
	if m.AgentConnectedAt == nil || m.LastSeenAt == nil || m.AgentVersion != "test-0.1" || e.alertCount("lan_ip_mismatch") != 0 {
		t.Fatalf("after hello = %+v alerts=%d", m, e.alertCount("lan_ip_mismatch"))
	}
	before := e.srv.HeartbeatCount(mid)
	a.heartbeat(&agentpb.Metrics{GpuUtil: 12, GpuTempC: 95, GpuPowerW: 30, MemTotal: 128 << 30, MemUsed: 8 << 30, RootDiskTotal: 1000, RootDiskUsed: 950, Load1: 0.5})
	e.waitFor("heartbeat", func() bool { return e.srv.HeartbeatCount(mid) > before })
	m, _ = e.fleet.Get(e.ctx, e.pool, mid)
	if string(m.LastMetrics) == "" || !containsJSON(string(m.LastMetrics), `"gpu_temp_c": 95`) {
		t.Fatalf("metrics = %s", m.LastMetrics)
	}
	for i := 0; i < 2; i++ { // 同样的越限心跳只告警一次
		before = e.srv.HeartbeatCount(mid)
		a.heartbeat(&agentpb.Metrics{GpuTempC: 96, RootDiskTotal: 1000, RootDiskUsed: 960})
		e.waitFor("heartbeat", func() bool { return e.srv.HeartbeatCount(mid) > before })
	}
	if e.alertCount("gpu_temp_high") != 1 || e.alertCount("root_disk_high") != 1 {
		t.Fatalf("threshold alerts gpu=%d disk=%d", e.alertCount("gpu_temp_high"), e.alertCount("root_disk_high"))
	}
	var msg string
	_ = e.pool.QueryRow(e.ctx, `SELECT message FROM alerts WHERE kind = 'root_disk_high'`).Scan(&msg)
	if msg != "机器 SN-001 根分区使用率超过 90%" {
		t.Fatalf("message = %q", msg)
	}
}

func TestHeartbeatSanitizesNonFiniteMetrics(t *testing.T) {
	e := newEnv(t)
	mid := e.machine("SN-001")
	a := e.up(e.enroll(mid), mid)
	before := e.srv.HeartbeatCount(mid)
	a.heartbeat(&agentpb.Metrics{GpuUtil: math.NaN(), GpuTempC: math.Inf(1), Load1: 1.5})
	e.waitFor("heartbeat", func() bool { return e.srv.HeartbeatCount(mid) > before })
	m, _ := e.fleet.Get(e.ctx, e.pool, mid)
	if !containsJSON(string(m.LastMetrics), `"load1": 1.5`) || !containsJSON(string(m.LastMetrics), `"gpu_util": 0`) {
		t.Fatalf("metrics = %s", m.LastMetrics)
	}
	if err := a.waitErr(200 * time.Millisecond); err != nil {
		t.Fatalf("stream dropped: %v", err)
	}
}

func TestLanIPMismatchAlert(t *testing.T) {
	e := newEnv(t)
	for i, ips := range [][]string{{"192.168.9.9"}, {}} {
		mid := e.machine("SN-00" + string(rune('1'+i)))
		a := e.connect(e.enroll(mid))
		a.hello(ips...)
		if a.next(2*time.Second) == nil {
			t.Fatal("no config")
		}
		e.waitFor("alert", func() bool { return e.alertCount("lan_ip_mismatch") == i+1 })
	}
	var msg string
	_ = e.pool.QueryRow(e.ctx, `SELECT message FROM alerts WHERE kind = 'lan_ip_mismatch' ORDER BY id LIMIT 1`).Scan(&msg)
	if msg != "机器 SN-001 登记的内网 IP 与 Agent 上报的地址不一致" {
		t.Fatalf("message = %q", msg)
	}
}

func TestStateReportReconcilesAndResumes(t *testing.T) {
	e := newEnv(t)
	mid := e.machine("SN-001")
	uid := dbtest.CreateUser(t, e.pool, "13600000001", true)
	dbtest.Fund(t, e.pool, uid, 1_000_000)
	_, inst, err := e.rental.Place(e.ctx, rental.PlaceRequest{UserID: uid, ProductID: dbtest.ProductID(t, e.pool), ImageID: dbtest.ImageID(t, e.pool), Duration: catalog.D1d})
	if err != nil {
		t.Fatal(err)
	}
	cmds, _ := e.fleet.PendingCommands(e.ctx, mid)
	if err := e.rental.HandleCommandResult(e.ctx, cmds[0].ID, true, "ok"); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(time.Hour)
	if _, err := e.rental.SuspendMachines(e.ctx, []int64{mid}); err != nil {
		t.Fatal(err)
	}
	creds := e.enroll(mid)
	// 报告里没有该实例 → 保持暂停并告警；报告 running → 恢复
	a := e.up(creds, mid)
	got, _ := e.rental.Get(e.ctx, inst.ID)
	if !got.Suspended() || e.alertCount("container_down") != 1 {
		t.Fatalf("after empty report: suspended=%v alerts=%d", got.Suspended(), e.alertCount("container_down"))
	}
	before := e.srv.ReportCount(mid)
	a.report(running(inst.ID))
	e.waitFor("second report", func() bool { return e.srv.ReportCount(mid) > before })
	if got, _ = e.rental.Get(e.ctx, inst.ID); got.Suspended() {
		t.Fatal("not resumed after running report")
	}
	m, _ := e.fleet.Get(e.ctx, e.pool, mid)
	if m.StateReportedAt == nil || !containsJSON(string(mustReport(e, mid)), inst.ID) {
		t.Fatalf("state report not stored: %+v", m)
	}
}

func TestNewConnectionReplacesOldAndDisconnectClearsOwnConn(t *testing.T) {
	e := newEnv(t)
	mid := e.machine("SN-001")
	creds := e.enroll(mid)
	a := e.up(creds, mid)
	b := e.up(creds, mid)
	if err := a.waitErr(2 * time.Second); status.Code(err) != codes.Aborted {
		t.Fatalf("old stream = %v", err)
	}
	if !e.srv.Connected(mid) {
		t.Fatal("new stream must be registered")
	}
	m, _ := e.fleet.Get(e.ctx, e.pool, mid)
	if !m.Connected {
		t.Fatal("old stream detach must not clear new conn id")
	}
	_ = b.stream.CloseSend()
	e.waitFor("conn cleared", func() bool { m, _ := e.fleet.Get(e.ctx, e.pool, mid); return !m.Connected && !e.srv.Connected(mid) })
	if m, _ := e.fleet.Get(e.ctx, e.pool, mid); !m.AgentOnline {
		t.Fatal("disconnect must not mark offline (sweeper does after 5 minutes)")
	}
}

func TestUnknownCommandResultIsIgnored(t *testing.T) {
	e := newEnv(t)
	mid := e.machine("SN-001")
	a := e.up(e.enroll(mid), mid)
	a.result("no-such-command", true, "ok")
	before := e.srv.HeartbeatCount(mid)
	a.heartbeat(&agentpb.Metrics{Load1: 1})
	e.waitFor("heartbeat after bogus result", func() bool { return e.srv.HeartbeatCount(mid) > before })
	if err := a.waitErr(100 * time.Millisecond); err != nil {
		t.Fatalf("stream dropped: %v", err)
	}
}
