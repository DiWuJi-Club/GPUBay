package rental_test

import (
	"testing"
	"time"

	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/rental"
)

func (e *env) clusterView(id string) rental.ClusterView {
	e.t.Helper()
	v, err := e.svc.GetClusterView(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return v
}

func TestClusterViewStatus(t *testing.T) {
	e := newEnv(t, 0)
	f := e.fabric("Lab", "lan", "routed", false)
	fm := e.lanMachines(f, "192.168.1", 2)
	if _, err := e.pool.Exec(e.ctx, `UPDATE machines SET display_name = 'A-01' WHERE id = $1`, fm[0]); err != nil {
		t.Fatal(err)
	}
	res := e.placeCluster(2)
	id := res.Cluster.ID
	ranked := byRank(t, res.Instances)

	v := e.clusterView(id)
	if v.Status != rental.ClusterForming || v.Network != "ethernet" || v.FabricName != "Lab" || v.FabricKind != "lan" ||
		v.Subnet != "10.210.0.0/24" || v.HeadIP != "10.210.0.11" || len(v.Members) != 2 {
		t.Fatalf("forming view = %+v", v)
	}
	m0 := v.Members[0]
	if m0.Rank != 0 || m0.InstanceID != ranked[0].ID || m0.State != rental.StateCreating || m0.IP != "10.210.0.11" ||
		m0.MachineID != fm[0] || m0.MachineLabel != "A-01" || m0.NetStatus != "" {
		t.Fatalf("member 0 = %+v", m0)
	}
	if v.Members[1].MachineLabel == "" || v.Members[1].IP != "10.210.0.12" {
		t.Fatalf("member 1 = %+v", v.Members[1])
	}

	// 全员 running（Create 成功）、集群网络 ok、ready_at 已置 → ready
	e.ack(true)
	if _, err := e.pool.Exec(e.ctx, `UPDATE instance_clusters SET ready_at = $2 WHERE id = $1`, id, e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	for _, inst := range ranked {
		e.reportCluster(inst.MachineID, inst.ID, fleet.PhaseRunning, "ok")
	}
	if v := e.clusterView(id); v.Status != rental.ClusterReady || v.Members[1].NetStatus != "ok" || v.Members[0].ExpiresAt == nil {
		t.Fatalf("ready view = %+v", v)
	}
	// 一个成员网络未就绪 → degraded
	e.reportCluster(ranked[1].MachineID, ranked[1].ID, fleet.PhaseRunning, "error: veth")
	if v := e.clusterView(id); v.Status != rental.ClusterDegraded || v.Members[1].NetStatus != "error: veth" {
		t.Fatalf("net error view = %+v", v)
	}
	e.reportCluster(ranked[1].MachineID, ranked[1].ID, fleet.PhaseRunning, "ok")
	// 成员暂停 → degraded
	if _, err := e.pool.Exec(e.ctx, `UPDATE instances SET suspended_at = $2 WHERE id = $1`, ranked[0].ID, e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if v := e.clusterView(id); v.Status != rental.ClusterDegraded || !v.Members[0].Suspended {
		t.Fatalf("suspended view = %+v", v)
	}
	if _, err := e.pool.Exec(e.ctx, `UPDATE instances SET suspended_at = NULL WHERE id = $1`, ranked[0].ID); err != nil {
		t.Fatal(err)
	}
	// 成员缺位 → degraded
	if _, err := e.pool.Exec(e.ctx, `UPDATE instances SET state = 'wiping' WHERE id = $1`, ranked[1].ID); err != nil {
		t.Fatal(err)
	}
	if v := e.clusterView(id); v.Status != rental.ClusterDegraded || len(v.Members) != 1 {
		t.Fatalf("missing member view = %+v", v)
	}
	// 非法 last_state_report（instances 不是数组）不报错
	if _, err := e.pool.Exec(e.ctx, `UPDATE machines SET last_state_report = '{"instances": null}' WHERE id = $1`, ranked[0].MachineID); err != nil {
		t.Fatal(err)
	}
	if v := e.clusterView(id); v.Members[0].NetStatus != "" {
		t.Fatalf("null report = %+v", v.Members[0])
	}
	// 已结束：原样状态，每个 rank 列最后一个实例
	if _, err := e.pool.Exec(e.ctx, `UPDATE instances SET state = 'released' WHERE cluster_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(e.ctx, `UPDATE instance_clusters SET state = 'released', ended_at = $2 WHERE id = $1`, id, e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if v := e.clusterView(id); v.Status != rental.ClusterReleased || len(v.Members) != 2 {
		t.Fatalf("released view = %+v", v)
	}
	if _, err := e.svc.GetClusterView(e.ctx, "clu-0000000000"); codeOf(err) != "cluster_not_found" {
		t.Fatalf("missing = %v", err)
	}
}

func TestClusterForUserIsolation(t *testing.T) {
	e := newEnv(t, 0)
	f := e.fabric("A", "lan", "routed", false)
	e.lanMachines(f, "192.168.1", 6)
	pm := e.lanMachines(f, "192.168.1", 2)
	pool := e.toPool(pm...)
	mine := e.placeCluster(2)
	other := e.newUser("13600000002", startFunds)
	req := e.clusterReq(2)
	req.UserID = other
	theirs, err := e.svc.PlaceBatch(e.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	sys, err := e.svc.PlaceSystemCluster(e.ctx, rental.SystemClusterRequest{MachineIDs: pm, GroupID: pool, ImageID: e.imageID, Note: "n"})
	if err != nil {
		t.Fatal(err)
	}
	if v, err := e.svc.ClusterForUser(e.ctx, e.userID, mine.Cluster.ID); err != nil || v.ID != mine.Cluster.ID {
		t.Fatalf("own = %+v, %v", v, err)
	}
	for _, id := range []string{theirs.Cluster.ID, sys.Cluster.ID, "clu-0000000000"} {
		if _, err := e.svc.ClusterForUser(e.ctx, e.userID, id); codeOf(err) != "cluster_not_found" {
			t.Fatalf("ClusterForUser(%s) = %v", id, err)
		}
	}
	if _, err := e.svc.ClusterForUser(e.ctx, e.systemID, sys.Cluster.ID); codeOf(err) != "cluster_not_found" {
		t.Fatal("system clusters are not user clusters")
	}
	// 列表：active + 30 天内结束的，新的在前
	e.clock.Advance(time.Minute)
	second := e.placeCluster(2)
	list, err := e.svc.ListClustersForUser(e.ctx, e.userID)
	if err != nil || len(list) != 2 || list[0].ID != second.Cluster.ID || list[1].ID != mine.Cluster.ID {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if _, err := e.pool.Exec(e.ctx, `UPDATE instance_clusters SET state = 'failed', ended_at = $2 WHERE id = $1`, mine.Cluster.ID, e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(rental.ClusterListWindow - time.Hour)
	if list, _ := e.svc.ListClustersForUser(e.ctx, e.userID); len(list) != 2 || list[1].Status != rental.ClusterFailed {
		t.Fatalf("within window = %+v", list)
	}
	e.clock.Advance(2 * time.Hour)
	if list, _ := e.svc.ListClustersForUser(e.ctx, e.userID); len(list) != 1 || list[0].ID != second.Cluster.ID {
		t.Fatalf("after window = %+v", list)
	}
	// 后台列表：筛选与总数
	all, total, err := e.svc.AdminListClusters(e.ctx, rental.ClusterFilter{}, paging.Page{Number: 1, Size: 2})
	if err != nil || total != 4 || len(all) != 2 {
		t.Fatalf("admin all = %d %d %v", len(all), total, err)
	}
	yes := true
	if l, total, err := e.svc.AdminListClusters(e.ctx, rental.ClusterFilter{System: &yes}, paging.Page{}); err != nil || total != 1 || l[0].ID != sys.Cluster.ID {
		t.Fatalf("admin system = %+v %d %v", l, total, err)
	}
	if _, total, err := e.svc.AdminListClusters(e.ctx, rental.ClusterFilter{State: rental.ClusterFailed, UserID: &e.userID, FabricID: &f}, paging.Page{}); err != nil || total != 1 {
		t.Fatalf("admin failed = %d %v", total, err)
	}
	if _, _, err := e.svc.AdminListClusters(e.ctx, rental.ClusterFilter{State: "ready"}, paging.Page{}); codeOf(err) != "bad_request" {
		t.Fatalf("bad state = %v", err)
	}
}

func TestClusterAvailability(t *testing.T) {
	e := newEnv(t, 1) // 一台零散机器不计入
	f1 := e.fabric("L", "lan", "routed", false)
	f2 := e.fabric("D", "cx7", "direct", true)
	f3 := e.fabric("H", "lan", "routed", false)
	l := e.lanMachines(f1, "192.168.1", 5)
	e.cx7Machines(f2, 2, true)
	hidden := e.group("留池", false)
	for _, m := range e.lanMachines(f3, "192.168.3", 3) {
		e.setGroup(m, hidden)
	}
	av, err := e.svc.ClusterAvailability(e.ctx, e.productID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if av[2] != (rental.ClusterAvail{Available: 3, RDMA: true}) || av[4] != (rental.ClusterAvail{Available: 1, RDMA: false}) || len(av) != 2 {
		t.Fatalf("availability = %+v", av)
	}
	if av, err := e.svc.ClusterAvailability(e.ctx, e.productID, &hidden); err != nil ||
		av[2] != (rental.ClusterAvail{Available: 1}) || av[4] != (rental.ClusterAvail{}) {
		t.Fatalf("hidden group = %+v, %v", av, err)
	}
	// 占用 / 离线的机器不计入
	e.setMachine(l[0], "allocated", true)
	e.setMachine(l[1], "idle", false)
	if av, _ := e.svc.ClusterAvailability(e.ctx, e.productID, nil); av[2].Available != 2 || av[4].Available != 0 {
		t.Fatalf("after busy = %+v", av)
	}
}
