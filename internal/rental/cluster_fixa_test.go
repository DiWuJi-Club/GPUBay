package rental_test

import (
	"context"
	"testing"
	"time"


	"spark-cloud/internal/audit"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/rental"
)

// 计划 13 终审 A 组修复的回归测试。

// 中止的集群还有成员在洗机：其子网 / VLAN 仍占用（离线成员重连后 Recover 会按旧 Spec 建 spc0），新集群不能拿到；
// 成员全部结束后可以复用（规格 §5.5 第 4 步）。
func TestClusterAbortHoldsSubnetAndVLANUntilMembersEnd(t *testing.T) {
	e := newEnv(t, 0)
	f := e.fabric("V", "cx7", "vlan", false, 100, 101)
	e.cx7Machines(f, 8, false)
	a := e.placeCluster(2)
	if a.Cluster.SubnetIndex != 0 || a.Cluster.VLAN == nil || *a.Cluster.VLAN != 100 {
		t.Fatalf("first cluster = %+v", a.Cluster)
	}
	ra := byRank(t, a.Instances)
	e.runMembers(ra[0])
	e.finish(ra[1], fleet.CmdCreateInstance, false, "cluster-net: vlan add failed")
	if e.sweepClusters() != 1 || e.clusterView(a.Cluster.ID).State != rental.ClusterFailed {
		t.Fatal("cluster A should abort")
	}
	if e.get(ra[0].ID).State != rental.StateWiping {
		t.Fatal("rank0 of A should be wiping")
	}
	b := e.placeCluster(2)
	if b.Cluster.SubnetIndex == a.Cluster.SubnetIndex || b.Cluster.VLAN == nil || *b.Cluster.VLAN == *a.Cluster.VLAN {
		t.Fatalf("cluster B reused A's subnet / vlan while A still wiping: %+v", b.Cluster)
	}
	// VLAN 范围只有 100～101：A 的 100 仍占用、B 拿了 101 → 第三个集群组不成
	if _, err := e.svc.PlaceBatch(e.ctx, e.clusterReq(2)); codeOf(err) != "no_capacity" {
		t.Fatalf("vlan held by aborted cluster = %v", err)
	}
	e.checkInvariants()
	// A 的成员洗完（失败成员的机器洗完也放回库存）→ 子网与 VLAN 可以复用
	e.finish(ra[0], fleet.CmdWipeInstance, true, "ok")
	e.finish(ra[1], fleet.CmdWipeInstance, true, "ok")
	c := e.placeCluster(2)
	if c.Cluster.SubnetIndex != a.Cluster.SubnetIndex || c.Cluster.VLAN == nil || *c.Cluster.VLAN != *a.Cluster.VLAN {
		t.Fatalf("after A ended, want subnet %d vlan %d; got %+v", a.Cluster.SubnetIndex, *a.Cluster.VLAN, c.Cluster)
	}
	e.checkInvariants()
}

// 组建中（成员都 running 但集群尚未就绪）不能整组释放：否则下一轮清扫以「没有成员」中止并全额退款（违背「已付不退」）。
func TestReleaseClusterFormingRejected(t *testing.T) {
	e := newEnv(t, 0)
	f := e.fabric("A", "lan", "routed", false)
	e.lanMachines(f, "192.168.1", 2)
	res := e.placeCluster(2)
	r := byRank(t, res.Instances)
	e.runMembers(r...)
	if err := e.svc.ReleaseCluster(e.ctx, e.userID, res.Cluster.ID, ""); codeOf(err) != "invalid_state" {
		t.Fatalf("release forming cluster = %v", err)
	}
	for _, inst := range r {
		if e.get(inst.ID).State != rental.StateRunning {
			t.Fatal("members must stay running")
		}
	}
	if e.sweepClusters() != 1 || e.clusterView(res.Cluster.ID).ReadyAt == nil {
		t.Fatal("cluster should become ready")
	}
	bal := e.balance(e.userID)
	if err := e.svc.ReleaseCluster(e.ctx, e.userID, res.Cluster.ID, ""); err != nil {
		t.Fatal(err)
	}
	e.sweepClusters()
	if e.balance(e.userID) != bal {
		t.Fatal("release of a ready cluster must not refund")
	}
}

// 就绪后换机：新成员开通完成时到期对齐到原成员（换机不改变到期），集群视图不再提示到期不一致。
func TestMigrateReadyClusterMemberKeepsExpiry(t *testing.T) {
	e := newEnv(t, 0)
	f := e.fabric("A", "lan", "routed", false)
	e.lanMachines(f, "192.168.1", 3)
	res, r := e.readyCluster(2)
	want := *r[1].ExpiresAt
	e.clock.Advance(time.Hour)
	moved, err := e.svc.AdminMigrate(e.ctx, audit.Actor{AdminID: 1}, r[1].ID, "nic broken")
	if err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(12 * time.Minute) // 开通耗时：不应让新成员的到期比其余成员晚
	e.runMembers(moved)
	got := e.get(moved.ID)
	if got.State != rental.StateRunning || got.ExpiresAt == nil {
		t.Fatalf("moved = %+v", got)
	}
	if d := got.ExpiresAt.Sub(want); d < -time.Second || d > time.Second {
		t.Fatalf("moved expires %v, want %v (diff %v)", got.ExpiresAt, want, d)
	}
	if d := got.ExpiresAt.Sub(*e.get(r[0].ID).ExpiresAt); d < -time.Second || d > time.Second {
		t.Fatalf("moved expires differs from rank0 by %v", d)
	}
	if v := e.clusterView(res.Cluster.ID); len(v.Members) != 2 || v.Members[1].InstanceID != moved.ID {
		t.Fatalf("view members = %+v", v.Members)
	}
	e.checkInvariants()
}

// 集群网络导致开通失败的机器：洗完回 idle 但暂不参与集群分配（cluster_ready = false，后台可见原因），
// Agent 重新上报网卡（Hello）后恢复；洗机也失败 → 机器 faulty + wipe_failed 告警（不再永远 allocated）。
func TestClusterNetFailureSuspendsMachineReadiness(t *testing.T) {
	e := newEnv(t, 0)
	f := e.fabric("A", "lan", "routed", false)
	e.lanMachines(f, "192.168.1", 4)
	res := e.placeCluster(2)
	r := byRank(t, res.Instances)
	e.runMembers(r[0])
	e.finish(r[1], fleet.CmdCreateInstance, false, "cluster-net: veth add failed")
	bad := r[1].MachineID
	m, err := e.fleet.Get(e.ctx, e.pool, bad)
	if err != nil {
		t.Fatal(err)
	}
	if m.ClusterReady || m.ClusterReason == "" {
		t.Fatalf("failed machine must be excluded from clusters with a visible reason: ready=%v reason=%q", m.ClusterReady, m.ClusterReason)
	}
	e.sweepClusters()
	e.finish(r[1], fleet.CmdWipeInstance, true, "ok")
	e.finish(r[0], fleet.CmdWipeInstance, true, "ok")
	if e.machineState(bad) != fleet.StateIdle {
		t.Fatalf("machine %s", e.machineState(bad))
	}
	// 4 台里只剩 3 台就绪：组 2 台的集群只能有 1 个，且不含失败机器
	c := e.placeCluster(2)
	for _, inst := range c.Instances {
		if inst.MachineID == bad {
			t.Fatal("failed machine must not be picked again")
		}
	}
	// Agent 重连上报网卡 → 重算就绪
	m, _ = e.fleet.Get(e.ctx, e.pool, bad)
	if err := e.fleet.RecordHello(e.ctx, bad, 3, m.NetInfo); err != nil {
		t.Fatal(err)
	}
	if m, _ = e.fleet.Get(e.ctx, e.pool, bad); !m.ClusterReady || m.ClusterReason != "" {
		t.Fatalf("after hello ready=%v reason=%q", m.ClusterReady, m.ClusterReason)
	}

	// 洗机失败：机器 faulty、critical 告警
	d := e.placeCluster(2)
	rd := byRank(t, d.Instances)
	e.finish(rd[0], fleet.CmdCreateInstance, false, "cluster-net: veth add failed")
	e.finish(rd[0], fleet.CmdWipeInstance, false, "disk busy")
	if e.machineState(rd[0].MachineID) != fleet.StateFaulty || e.alerts("wipe_failed") != 1 {
		t.Fatalf("wipe failure: machine %s, wipe_failed alerts %d", e.machineState(rd[0].MachineID), e.alerts("wipe_failed"))
	}
}

// 开通集群与改 fabric 串行化：fabric 行被（UpdateFabric / UpdateMachine 的）FOR NO KEY UPDATE 锁住时，
// 用户集群与系统集群的开通都要等它提交（锁序 fabric → machine）。
func TestClusterPlacementWaitsForFabricLock(t *testing.T) {
	for _, system := range []bool{false, true} {
		name := "user"
		if system {
			name = "system"
		}
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, 0)
			f := e.fabric("A", "lan", "routed", false)
			ms := e.lanMachines(f, "192.168.1", 2)
			var pool int64
			if system {
				pool = e.toPool(ms...)
			}
			tx, err := e.pool.Begin(e.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			if _, err := tx.Exec(e.ctx, `SELECT id FROM cluster_fabrics WHERE id = $1 FOR NO KEY UPDATE`, f); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				if system {
					_, err := e.svc.PlaceSystemCluster(e.ctx, rental.SystemClusterRequest{MachineIDs: ms, GroupID: pool, ImageID: e.imageID, Note: "x"})
					done <- err
					return
				}
				_, err := e.svc.PlaceBatch(e.ctx, e.clusterReq(2))
				done <- err
			}()
			select {
			case err := <-done:
				t.Fatalf("placement finished while fabric locked: %v", err)
			case <-time.After(300 * time.Millisecond):
			}
			if err := tx.Commit(e.ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("placement did not finish after fabric lock released")
			}
		})
	}
}
