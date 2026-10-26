package rental_test

import (
	"strings"
	"testing"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/rental"
)

// ---- 不变量 ----

func TestCheckClusterInvariants(t *testing.T) {
	e := newEnv(t, 1)
	f := e.fabric("A", "lan", "routed", false)
	sysM := e.lanMachines(f, "192.168.1", 2)
	pool := e.toPool(sysM...)
	sys, err := e.svc.PlaceSystemCluster(e.ctx, rental.SystemClusterRequest{MachineIDs: sysM, GroupID: pool, ImageID: e.imageID, Note: "n"})
	if err != nil {
		t.Fatal(err)
	}
	e.runMembers(sys.Instances...)
	e.lanMachines(f, "192.168.1", 3)
	user, r := e.readyCluster(2)
	moved, err := e.svc.AdminMigrate(e.ctx, audit.Actor{AdminID: 1}, r[1].ID, "swap")
	if err != nil {
		t.Fatal(err)
	}
	e.runMembers(moved)
	e.lanMachines(f, "192.168.1", 2)
	ab := e.placeCluster(2)
	ar := byRank(t, ab.Instances)
	e.finish(ar[1], fleet.CmdCreateInstance, false, "boom")
	e.sweepClusters()
	if e.clusterView(ab.Cluster.ID).State != rental.ClusterFailed {
		t.Fatal("aborted precondition")
	}
	_, single := e.place(catalog.D1d)
	e.checkInvariants()
	// ③ 经 instance_transfers 成立：旧成员即使不再指向集群，它的新购订单经换机链连到新成员
	if _, err := e.pool.Exec(e.ctx, `UPDATE instances SET cluster_id = NULL, cluster_rank = NULL WHERE id = $1`, r[1].ID); err != nil {
		t.Fatal(err)
	}
	e.checkInvariants()

	expect := func(n string, breakSQL, fixSQL string, args ...any) {
		t.Helper()
		if _, err := e.pool.Exec(e.ctx, breakSQL, args...); err != nil {
			t.Fatalf("break %s: %v", n, err)
		}
		err := rental.CheckClusterInvariants(e.ctx, e.pool)
		if err == nil || !strings.HasPrefix(err.Error(), "cluster invariant "+n+":") {
			t.Fatalf("invariant %s: got %v", n, err)
		}
		if _, err := e.pool.Exec(e.ctx, fixSQL, args...); err != nil {
			t.Fatalf("fix %s: %v", n, err)
		}
		e.checkInvariants()
	}
	// ① 同 rank 两个在位成员 / 成员不在 fabric 上
	expect("1", `UPDATE instances SET cluster_rank = 0 WHERE id = $1`, `UPDATE instances SET cluster_rank = 1 WHERE id = $1`, moved.ID)
	expect("1", `UPDATE machines SET fabric_id = NULL WHERE id = $1 AND $2::bigint IS NOT NULL`, `UPDATE machines SET fabric_id = $2 WHERE id = $1`, r[0].MachineID, f)
	// ② 子网重复（索引本会拒绝：去掉索引后复核查询仍能发现）
	if _, err := e.pool.Exec(e.ctx, `DROP INDEX instance_clusters_subnet_live_uniq`); err != nil {
		t.Fatal(err)
	}
	expect("2", `UPDATE instance_clusters SET subnet_index = (SELECT subnet_index FROM instance_clusters WHERE id = $2) WHERE id = $1`,
		`UPDATE instance_clusters SET subnet_index = 200 WHERE id = $1 AND $2::text IS NOT NULL`, user.Cluster.ID, sys.Cluster.ID)
	// ③ 新购订单数不等于 size / 订单连不到成员（换机后的新成员经 instance_transfers 成立已在基线里）
	expect("3", `UPDATE orders SET kind = 'renew' WHERE batch_id = $1 AND instance_id = $2`, `UPDATE orders SET kind = 'new' WHERE batch_id = $1 AND instance_id = $2`,
		*user.Cluster.BatchID, r[0].ID)
	expect("3", `UPDATE orders SET instance_id = $3 WHERE batch_id = $1 AND instance_id = $2`, `UPDATE orders SET instance_id = $2 WHERE batch_id = $1 AND instance_id = $3`,
		*user.Cluster.BatchID, r[0].ID, single.ID)
	// ④ 系统集群成员有订单
	expect("4", `UPDATE orders SET instance_id = $2 WHERE instance_id = $1`, `UPDATE orders SET instance_id = $1 WHERE instance_id = $2`,
		single.ID, sys.Instances[0].ID)
	// ⑤ failed 用户集群的订单未全额退款
	expect("5", `UPDATE orders SET refunded_amount = 0, status = 'paid' WHERE instance_id = $1`,
		`UPDATE orders SET refunded_amount = amount, status = 'refunded' WHERE instance_id = $1`, ar[0].ID)
}
