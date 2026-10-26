package rental_test

import (
	"testing"

	"spark-cloud/internal/clusterspec"
	"spark-cloud/internal/platform/apperr"
)

func TestClusterSpecForMacvlanVLAN(t *testing.T) {
	e := newEnv(t, 0)
	f := e.fabric("V", "cx7", "vlan", true, 200, 210)
	if _, err := e.pool.Exec(e.ctx, `UPDATE cluster_fabrics SET mtu = 9000 WHERE id = $1`, f); err != nil {
		t.Fatal(err)
	}
	e.cx7Machines(f, 2, true)
	res := e.placeCluster(2)
	c := res.Cluster
	for r := 0; r < 2; r++ {
		s, err := e.svc.ClusterSpecFor(e.ctx, e.pool, c.ID, r)
		if err != nil {
			t.Fatal(err)
		}
		// iface 自动选 up 的 mlx5 口；macvlan 不带 host_ip；有效 RDMA
		if s.Mode != clusterspec.ModeMacvlan || s.Iface != "enp1s0f1np1" || s.VLAN != 200 || s.MTU != 9000 || !s.RDMA || s.Rank != r ||
			s.Size != 2 || s.Version != 1 || s.Subnet != "10.210.0.0/24" || len(s.Members) != 2 {
			t.Fatalf("spec %d = %+v", r, s)
		}
		for _, m := range s.Members {
			if m.HostIP != "" {
				t.Fatalf("macvlan member with host_ip: %+v", m)
			}
		}
	}
	// 成员 RDMA 不可用 → 有效 RDMA 为假
	e.cx7Machines(f, 1, true)
	e.cx7Machines(f, 1, false)
	res2 := e.placeCluster(2)
	if s, err := e.svc.ClusterSpecFor(e.ctx, e.pool, res2.Cluster.ID, 0); err != nil || s.RDMA || s.VLAN != 201 {
		t.Fatalf("degraded spec = %+v, %v", s, err)
	}
	// 后台指定的集群网卡未连接：该 rank 构造失败（不下发坏 Spec）；macvlan 的其它 rank 不受对端网卡影响
	rank1 := byRank(t, res.Instances)[1]
	if _, err := e.pool.Exec(e.ctx, `UPDATE machines SET cluster_iface = 'enp1s0f0np0' WHERE id = $1`, rank1.MachineID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ClusterSpecFor(e.ctx, e.pool, c.ID, 1); err == nil {
		t.Fatal("down iface must fail")
	}
	if _, err := e.svc.ClusterSpecFor(e.ctx, e.pool, c.ID, 0); err != nil {
		t.Fatalf("rank 0 = %v", err)
	}
}

func TestClusterSpecForMissingRank(t *testing.T) {
	e := newEnv(t, 0)
	f := e.fabric("A", "lan", "routed", false)
	e.lanMachines(f, "192.168.1", 4)
	res := e.placeCluster(4)
	ranked := byRank(t, res.Instances)
	if _, err := e.pool.Exec(e.ctx, `UPDATE instances SET state = 'released' WHERE id = $1`, ranked[2].ID); err != nil {
		t.Fatal(err)
	}
	s, err := e.svc.ClusterSpecFor(e.ctx, e.pool, res.Cluster.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.Size != 4 || len(s.Members) != 3 || s.Members[0].Rank != 0 || s.Members[1].Rank != 1 || s.Members[2].Rank != 3 {
		t.Fatalf("spec = %+v", s)
	}
	for _, m := range s.Members {
		if m.HostIP != e.lanIP(ranked[m.Rank].MachineID) {
			t.Fatalf("routed member host_ip = %+v", m)
		}
	}
	if _, err := e.svc.ClusterSpecFor(e.ctx, e.pool, res.Cluster.ID, 2); err == nil {
		t.Fatal("missing rank must fail validation")
	}
	if _, err := e.svc.ClusterSpecFor(e.ctx, e.pool, "clu-0000000000", 0); err != apperr.ErrClusterNotFound {
		t.Fatalf("unknown cluster = %v", err)
	}
	// wiping 的成员也不在位
	if _, err := e.pool.Exec(e.ctx, `UPDATE instances SET state = 'wiping' WHERE id = $1`, ranked[3].ID); err != nil {
		t.Fatal(err)
	}
	if s, err := e.svc.ClusterSpecFor(e.ctx, e.pool, res.Cluster.ID, 1); err != nil || len(s.Members) != 2 {
		t.Fatalf("spec = %+v, %v", s, err)
	}
}
