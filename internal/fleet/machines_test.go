package fleet_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/agent/features"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/paging"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

func newEnv(t *testing.T) (*fleet.Service, *pgxpool.Pool, *clock.Fake) {
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	return fleet.NewService(pool, clk), pool, clk
}

func TestRegisterValidatesAndRejectsDuplicates(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	pid := dbtest.ProductID(t, pool)

	m, err := svc.Register(ctx, fleet.NewMachine{Serial: "SN001", Rack: "A01", Slot: "03", LanIP: "10.0.1.3", ProductID: pid})
	if err != nil {
		t.Fatal(err)
	}
	if m.State != fleet.StateMaintenance || m.AgentOnline {
		t.Fatalf("new machine = %+v", m)
	}
	_, err = svc.Register(ctx, fleet.NewMachine{Serial: "SN001", LanIP: "10.0.1.4", ProductID: pid})
	if ae := apperr.As(err); ae == nil || ae.Code != "duplicate_serial" {
		t.Fatalf("want duplicate_serial, got %v", err)
	}
	_, err = svc.Register(ctx, fleet.NewMachine{Serial: "SN002", LanIP: "not-an-ip", ProductID: pid})
	if ae := apperr.As(err); ae == nil || ae.Code != "bad_request" {
		t.Fatalf("want bad_request, got %v", err)
	}
}

func TestMaintenanceTransitions(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	m, _ := svc.Register(ctx, fleet.NewMachine{Serial: "SN001", LanIP: "10.0.1.3", ProductID: dbtest.ProductID(t, pool)})

	if err := svc.SetMaintenance(ctx, m.ID, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.Get(ctx, pool, m.ID); got.State != fleet.StateIdle {
		t.Fatalf("state = %s, want idle", got.State)
	}
	if err := svc.SetMaintenance(ctx, m.ID, false); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("idle->idle want invalid state, got %v", err)
	}
	if err := svc.SetMaintenance(ctx, m.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetMaintenance(ctx, 9999, true); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("missing machine want not found, got %v", err)
	}
}

func TestHeartbeatAndOfflineDetection(t *testing.T) {
	svc, pool, clk := newEnv(t)
	ctx := context.Background()
	id := dbtest.CreateMachine(t, pool, "SN001")

	if came, err := svc.Heartbeat(ctx, id); err != nil || came {
		t.Fatalf("heartbeat on online machine: came=%v err=%v", came, err)
	}
	clk.Advance(5*time.Minute + time.Second)
	offline, err := svc.MarkOfflineStale(ctx, 5*time.Minute)
	if err != nil || len(offline) != 1 || offline[0] != id {
		t.Fatalf("offline = %v, %v", offline, err)
	}
	if again, _ := svc.MarkOfflineStale(ctx, 5*time.Minute); len(again) != 0 {
		t.Fatalf("already offline machines reported again: %v", again)
	}
	if came, err := svc.Heartbeat(ctx, id); err != nil || !came {
		t.Fatalf("heartbeat after offline: came=%v err=%v", came, err)
	}
	if _, err := svc.Heartbeat(ctx, 9999); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("unknown machine want not found, got %v", err)
	}
}

// MarkOfflineStale 不等别的事务锁着的机器行（SKIP LOCKED）：多行 UPDATE 不定序加锁会与锁机器行的事务死锁。
// 被跳过的机器在锁释放后的下一轮判定。
func TestMarkOfflineStaleSkipsLockedRows(t *testing.T) {
	svc, pool, clk := newEnv(t)
	ctx := context.Background()
	a := dbtest.CreateMachine(t, pool, "SN-A")
	b := dbtest.CreateMachine(t, pool, "SN-B")
	clk.Advance(5*time.Minute + time.Second)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT id FROM machines WHERE id = $1 FOR UPDATE`, b); err != nil {
		t.Fatal(err)
	}
	quick, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	offline, err := svc.MarkOfflineStale(quick, 5*time.Minute)
	if err != nil || len(offline) != 1 || offline[0] != a {
		t.Fatalf("with B locked: offline = %v, %v; want [%d] without waiting", offline, err, a)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if offline, err = svc.MarkOfflineStale(ctx, 5*time.Minute); err != nil || len(offline) != 1 || offline[0] != b {
		t.Fatalf("after unlock: offline = %v, %v; want [%d]", offline, err, b)
	}
}

func TestMarkFaultyAndAlert(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	id := dbtest.CreateMachine(t, pool, "SN001")
	if err := svc.MarkFaulty(ctx, pool, id); err != nil {
		t.Fatal(err)
	}
	if m, _ := svc.Get(ctx, pool, id); m.State != fleet.StateFaulty {
		t.Fatalf("state = %s", m.State)
	}
	if err := svc.RaiseAlert(ctx, pool, fleet.Alert{Level: "critical", Kind: "wipe_failed", MachineID: id, Message: "disk error"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.RaiseAlert(ctx, pool, fleet.Alert{Level: "warning", Kind: "system", Message: "no machine"}); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM alerts`).Scan(&n)
	if n != 2 {
		t.Fatalf("alerts = %d", n)
	}
}

func TestOfflineMachineIDsAndOnlineAmong(t *testing.T) {
	svc, pool, clk := newEnv(t)
	ctx := context.Background()
	a := dbtest.CreateMachine(t, pool, "SN001")
	b := dbtest.CreateMachine(t, pool, "SN002")

	if got, err := svc.OnlineAmong(ctx, nil); err != nil || got != nil {
		t.Fatalf("empty input = %v, %v", got, err)
	}
	if got, _ := svc.OfflineMachineIDs(ctx); len(got) != 0 {
		t.Fatalf("offline = %v", got)
	}
	clk.Advance(6 * time.Minute)
	if _, err := svc.Heartbeat(ctx, b); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.MarkOfflineStale(ctx, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if got, err := svc.OfflineMachineIDs(ctx); err != nil || len(got) != 1 || got[0] != a {
		t.Fatalf("offline = %v, %v; want [%d]", got, err, a)
	}
	if got, err := svc.OnlineAmong(ctx, []int64{a, b}); err != nil || len(got) != 1 || got[0] != b {
		t.Fatalf("online among = %v, %v; want [%d]", got, err, b)
	}
}

func TestRegisterUnknownProductIsBadRequest(t *testing.T) {
	svc, _, _ := newEnv(t)
	_, err := svc.Register(context.Background(), fleet.NewMachine{Serial: "SN009", LanIP: "10.0.1.9", ProductID: 9999})
	if ae := apperr.As(err); ae == nil || ae.Code != "bad_request" || ae.Message != "商品不存在" {
		t.Fatalf("want bad_request 商品不存在, got %v", err)
	}
}

// 计划 13：Hello / StateReport 的能力位与网卡探测落库，就绪列随之变化；相同 JSON 只刷新探测时间。
func TestRecordHelloAndNetInfo(t *testing.T) {
	svc, pool, clk := newEnv(t)
	ctx := context.Background()
	f, err := svc.CreateFabric(ctx, opsActor, lanInput("办公网"))
	if err != nil {
		t.Fatal(err)
	}
	id := dbtest.CreateMachine(t, pool, "SN1")
	m, err := svc.UpdateMachine(ctx, opsActor, id, fleet.MachineUpdate{SetFabric: true, FabricID: &f.ID})
	if err != nil || m.ClusterReady || m.ClusterReason != "Agent 不支持集群（请升级）" || m.NetInfo != nil || m.FabricName != "办公网" {
		t.Fatalf("joined before hello = %+v, %v", m, err)
	}
	// 旧 Agent：features = 0、无 net
	if err := svc.RecordHello(ctx, id, features.SharedSlots, nil); err != nil {
		t.Fatal(err)
	}
	if m, _ = svc.Get(ctx, pool, id); m.ClusterReady || m.AgentFeatures != features.SharedSlots || m.NetInfoAt != nil {
		t.Fatalf("old agent = %+v", m)
	}
	// 新 Agent 建流：Hello 在 AgentConfig 之前发，LanIface 为空，按 lan_ip 认管理网卡 → 就绪
	n := testNet("10.0.0.10/24")
	if err := svc.RecordHello(ctx, id, features.SharedSlots|features.Cluster, &n); err != nil {
		t.Fatal(err)
	}
	m, _ = svc.Get(ctx, pool, id)
	if !m.ClusterReady || m.ClusterRDMA || m.ClusterNet != "10.0.0.0/24" || m.ClusterReason != "" || m.NetInfo == nil ||
		len(m.NetInfo.Ifaces) != 2 || m.NetInfoAt == nil || !m.NetInfoAt.Equal(dbtest.T0) || m.AgentFeatures != features.SharedSlots|features.Cluster {
		t.Fatalf("after hello = %+v", m)
	}
	// 重连：Hello 不带 net（不覆盖已有探测，判定不翻转）
	clk.Advance(time.Minute)
	if err := svc.RecordHello(ctx, id, features.SharedSlots|features.Cluster, nil); err != nil {
		t.Fatal(err)
	}
	if m, _ = svc.Get(ctx, pool, id); !m.ClusterReady || m.NetInfo == nil || !m.NetInfoAt.Equal(dbtest.T0) {
		t.Fatalf("hello without net = %+v", m)
	}
	// StateReport 发来完全相同的 JSON：只刷新 net_info_at，不动 updated_at
	var updatedBefore time.Time
	_ = pool.QueryRow(ctx, `SELECT updated_at FROM machines WHERE id = $1`, id).Scan(&updatedBefore)
	clk.Advance(time.Minute)
	if err := svc.RecordNetInfo(ctx, id, n); err != nil {
		t.Fatal(err)
	}
	var updatedAfter time.Time
	_ = pool.QueryRow(ctx, `SELECT updated_at FROM machines WHERE id = $1`, id).Scan(&updatedAfter)
	if m, _ = svc.Get(ctx, pool, id); !m.NetInfoAt.Equal(dbtest.T0.Add(2*time.Minute)) || !updatedAfter.Equal(updatedBefore) {
		t.Fatalf("same json: net_info_at=%v updated %v → %v", m.NetInfoAt, updatedBefore, updatedAfter)
	}
	// 收到 AgentConfig 之后 LanIface 有值：判定不变
	n.LanIface = "enP7s7"
	if err := svc.RecordNetInfo(ctx, id, n); err != nil {
		t.Fatal(err)
	}
	if m, _ = svc.Get(ctx, pool, id); !m.ClusterReady || m.NetInfo.LanIface != "enP7s7" {
		t.Fatalf("with lan iface = %+v", m)
	}
	// 管理网卡掉线 → 未就绪，原因来自 ResolveIface
	n.Ifaces[0].Up = false
	if err := svc.RecordNetInfo(ctx, id, n); err != nil {
		t.Fatal(err)
	}
	if m, _ = svc.Get(ctx, pool, id); m.ClusterReady || m.ClusterNet != "" || m.ClusterReason != "网卡 enP7s7 未连接" {
		t.Fatalf("link down = %+v", m)
	}
	// 移出 fabric → 未就绪
	n.Ifaces[0].Up = true
	_ = svc.RecordNetInfo(ctx, id, n)
	m, err = svc.UpdateMachine(ctx, opsActor, id, fleet.MachineUpdate{SetFabric: true})
	if err != nil || m.FabricID != nil || m.ClusterReady || m.ClusterReason != "未加入集群网络" || m.FabricName != "" {
		t.Fatalf("leave fabric = %+v, %v", m, err)
	}
	if err := svc.RecordNetInfo(ctx, 99999, n); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("missing machine = %v", err)
	}
	if err := svc.RecordHello(ctx, 99999, 0, nil); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("missing machine hello = %v", err)
	}
	// 按 fabric 筛选
	_, _ = svc.UpdateMachine(ctx, opsActor, id, fleet.MachineUpdate{SetFabric: true, FabricID: &f.ID})
	dbtest.CreateMachine(t, pool, "SN2")
	list, total, err := svc.ListMachines(ctx, fleet.MachineFilter{FabricID: f.ID}, paging.Page{})
	if err != nil || total != 1 || list[0].ID != id || !list[0].ClusterReady {
		t.Fatalf("filter by fabric = %+v %d %v", list, total, err)
	}
}

// 计划 13：机器上有集群成员实例时不能改 fabric / 集群网卡；其它字段照改。审计 diff 带 fabric_id / cluster_iface。
func TestUpdateMachineFabricGuard(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	f, _ := svc.CreateFabric(ctx, opsActor, lanInput("办公网"))
	g, _ := svc.CreateFabric(ctx, opsActor, lanInput("另一网"))
	id := readyMachine(t, svc, pool, "SN1")
	if _, err := svc.UpdateMachine(ctx, opsActor, id, fleet.MachineUpdate{SetFabric: true, FabricID: &f.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateMachine(ctx, opsActor, id, fleet.MachineUpdate{ClusterIface: strp("bad name!")}); codeOf(err) != "bad_request" {
		t.Fatalf("bad iface = %v", err)
	}
	insertActiveCluster(t, pool, "clu-aaaaaa0003", f.ID, 3)
	if _, err := pool.Exec(ctx, `INSERT INTO instances (id, user_id, machine_id, product_id, image_id, state, ssh_password_enc, password_auth, created_at, updated_at, cluster_id, cluster_rank)
		SELECT 'ins-aaaaaaaa', c.user_id, m.id, m.product_id, $2, 'running', '\x00', false, $3, $3, c.id, 0
		FROM machines m, instance_clusters c WHERE m.id = $1 AND c.id = 'clu-aaaaaa0003'`, id, dbtest.ImageID(t, pool), dbtest.T0); err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string]fleet.MachineUpdate{
		"换 fabric":  {SetFabric: true, FabricID: &g.ID},
		"移出 fabric": {SetFabric: true},
		"改集群网卡":     {ClusterIface: strp("enp1s0f0np0")},
	} {
		_, err := svc.UpdateMachine(ctx, opsActor, id, in)
		if ae := apperr.As(err); ae == nil || ae.Code != apperr.ErrInvalidState.Code || ae.Message != "机器上有集群成员，不能修改集群网络" {
			t.Fatalf("%s = %v", name, err)
		}
	}
	// 不变的值、其它字段可以改
	name := "A-01"
	m, err := svc.UpdateMachine(ctx, opsActor, id, fleet.MachineUpdate{SetFabric: true, FabricID: &f.ID, ClusterIface: strp(""), DisplayName: &name})
	if err != nil || m.DisplayName != "A-01" || m.FabricID == nil || *m.FabricID != f.ID {
		t.Fatalf("no-op net change = %+v, %v", m, err)
	}
	// 成员已结束（released）→ 可以改
	if _, err := pool.Exec(ctx, `UPDATE instances SET state = 'released' WHERE id = 'ins-aaaaaaaa'`); err != nil {
		t.Fatal(err)
	}
	if m, err = svc.UpdateMachine(ctx, opsActor, id, fleet.MachineUpdate{SetFabric: true, FabricID: &g.ID, ClusterIface: strp("enP7s7")}); err != nil ||
		*m.FabricID != g.ID || m.ClusterIface != "enP7s7" || !m.ClusterReady {
		t.Fatalf("after member ended = %+v, %v", m, err)
	}
	var diff string
	_ = pool.QueryRow(ctx, `SELECT diff::text FROM audit_logs WHERE action = 'machine.update' ORDER BY id DESC LIMIT 1`).Scan(&diff)
	if !strings.Contains(diff, `"cluster_iface": "enP7s7"`) || !strings.Contains(diff, `"fabric_id": `+strconv.FormatInt(g.ID, 10)) {
		t.Fatalf("audit diff = %s", diff)
	}
}
