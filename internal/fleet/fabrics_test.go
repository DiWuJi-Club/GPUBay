package fleet_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/agent/features"
	"spark-cloud/internal/clusterspec"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/dbtest"
)

func intp(v int) *int       { return &v }
func i64p(v int64) *int64   { return &v }
func strp(v string) *string { return &v }

func lanInput(name string) fleet.FabricInput {
	return fleet.FabricInput{Name: name, Kind: fleet.FabricKindLAN, Isolation: fleet.IsolationRouted}
}

// testNet 一台 Spark 的典型网卡：管理口 enP7s7（lanIP/24）、一个 up 的 CX-7 口（RDMA 可用）。dbtest.CreateMachine 的 lan_ip 是 10.0.0.10。
func testNet(lanCIDR string) clusterspec.NetInfo {
	return clusterspec.NetInfo{Ifaces: []clusterspec.Iface{
		{Name: "enP7s7", Up: true, MTU: 1500, IPv4: []string{lanCIDR}, Driver: "r8127"},
		{Name: "enp1s0f0np0", Up: true, MTU: 9000, IPv4: []string{}, Driver: clusterspec.MLX5Driver, RDMADevice: "rocep1s0f0", RDMAActive: true, Uverbs: "uverbs0"},
	}}
}

// readyMachine 造一台新 Agent、已上报网卡的机器（尚未加入 fabric）。
func readyMachine(t *testing.T, svc *fleet.Service, pool *pgxpool.Pool, serial string) int64 {
	t.Helper()
	id := dbtest.CreateMachine(t, pool, serial)
	n := testNet("10.0.0.10/24")
	if err := svc.RecordHello(context.Background(), id, features.SharedSlots|features.Cluster, &n); err != nil {
		t.Fatal(err)
	}
	return id
}

// insertActiveCluster 直接插一个 active 集群（不依赖 rental）。
func insertActiveCluster(t *testing.T, pool *pgxpool.Pool, id string, fabricID int64, subnet int) {
	t.Helper()
	uid := dbtest.CreateUser(t, pool, "1390000"+id[len(id)-4:], true)
	if _, err := pool.Exec(context.Background(), `INSERT INTO instance_clusters (id, user_id, size, fabric_id, mode, rdma, subnet_index, state, created_at, updated_at)
		VALUES ($1, $2, 2, $3, 'routed', false, $4, 'active', $5, $5)`, id, uid, fabricID, subnet, dbtest.T0); err != nil {
		t.Fatal(err)
	}
}

func TestFabricCRUDValidation(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	bad := []struct {
		name string
		in   fleet.FabricInput
	}{
		{"空名称", fleet.FabricInput{Name: " ", Kind: "lan", Isolation: "routed"}},
		{"名称过长", fleet.FabricInput{Name: "一二三四五六七八九十一二三四五六七八九十一二三四五六七八九十一二三", Kind: "lan", Isolation: "routed"}},
		{"未知类型", fleet.FabricInput{Name: "x", Kind: "ib", Isolation: "routed"}},
		{"lan 只能 routed", fleet.FabricInput{Name: "x", Kind: "lan", Isolation: "direct"}},
		{"cx7 不能 routed", fleet.FabricInput{Name: "x", Kind: "cx7", Isolation: "routed"}},
		{"lan 不能 rdma", fleet.FabricInput{Name: "x", Kind: "lan", Isolation: "routed", RDMA: true}},
		{"mtu 越界", fleet.FabricInput{Name: "x", Kind: "lan", Isolation: "routed", MTU: 1000}},
		{"vlan 缺范围", fleet.FabricInput{Name: "x", Kind: "cx7", Isolation: "vlan"}},
		{"vlan 范围倒置", fleet.FabricInput{Name: "x", Kind: "cx7", Isolation: "vlan", VLANMin: intp(200), VLANMax: intp(100)}},
		{"vlan 越界", fleet.FabricInput{Name: "x", Kind: "cx7", Isolation: "vlan", VLANMin: intp(1), VLANMax: intp(100)}},
		{"非 vlan 带范围", fleet.FabricInput{Name: "x", Kind: "cx7", Isolation: "direct", VLANMin: intp(10), VLANMax: intp(20)}},
	}
	for _, tc := range bad {
		if _, err := svc.CreateFabric(ctx, opsActor, tc.in); codeOf(err) != "fabric_invalid" {
			t.Fatalf("%s: err = %v", tc.name, err)
		}
	}
	f, err := svc.CreateFabric(ctx, opsActor, fleet.FabricInput{Name: " 交换机 A ", Kind: "cx7", Isolation: "vlan", RDMA: true, MTU: 9000,
		VLANMin: intp(100), VLANMax: intp(199), Description: "机柜 A"})
	if err != nil || f.Name != "交换机 A" || !f.RDMA || f.MTU != 9000 || *f.VLANMin != 100 || *f.VLANMax != 199 {
		t.Fatalf("create = %+v, %v", f, err)
	}
	if _, err := svc.CreateFabric(ctx, opsActor, lanInput("交换机 A")); codeOf(err) != "fabric_invalid" {
		t.Fatalf("duplicate name = %v", err)
	}
	lan, err := svc.CreateFabric(ctx, opsActor, lanInput("办公网"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := svc.GetFabric(ctx, pool, f.ID); err != nil || got.Name != "交换机 A" {
		t.Fatalf("get = %+v, %v", got, err)
	}
	if _, err := svc.GetFabric(ctx, pool, 99999); !errors.Is(err, apperr.ErrFabricNotFound) {
		t.Fatalf("get missing = %v", err)
	}
	if _, err := svc.UpdateFabric(ctx, opsActor, lan.ID, lanInput("交换机 A")); codeOf(err) != "fabric_invalid" {
		t.Fatalf("rename to taken = %v", err)
	}
	if _, err := svc.UpdateFabric(ctx, opsActor, 99999, lanInput("y")); !errors.Is(err, apperr.ErrFabricNotFound) {
		t.Fatalf("update missing = %v", err)
	}

	// 占用：有机器 → in_use
	mid := readyMachine(t, svc, pool, "SN1")
	if _, err := svc.UpdateMachine(ctx, opsActor, mid, fleet.MachineUpdate{SetFabric: true, FabricID: &lan.ID}); err != nil {
		t.Fatal(err)
	}
	rows, err := svc.ListFabrics(ctx)
	if err != nil || len(rows) != 2 || rows[1].ID != lan.ID || rows[1].MachineCount != 1 || rows[1].ActiveClusters != 0 ||
		len(rows[1].ClusterNets) != 1 || rows[1].ClusterNets[0] != "10.0.0.0/24" || rows[0].ClusterNets == nil {
		t.Fatalf("list = %+v, %v", rows, err)
	}
	if err := svc.DeleteFabric(ctx, opsActor, lan.ID); !errors.Is(err, apperr.ErrFabricInUse) {
		t.Fatalf("delete with machine = %v", err)
	}
	// 有 active 集群 → in_use
	insertActiveCluster(t, pool, "clu-aaaaaa0001", f.ID, 1)
	if err := svc.DeleteFabric(ctx, opsActor, f.ID); !errors.Is(err, apperr.ErrFabricInUse) {
		t.Fatalf("delete with cluster = %v", err)
	}
	// 历史集群（已释放）仍引用 → in_use
	if _, err := pool.Exec(ctx, `UPDATE instance_clusters SET state = 'released', ended_at = $1`, dbtest.T0); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteFabric(ctx, opsActor, f.ID); !errors.Is(err, apperr.ErrFabricInUse) {
		t.Fatalf("delete with released cluster = %v", err)
	}
	empty, _ := svc.CreateFabric(ctx, opsActor, lanInput("空网"))
	if err := svc.DeleteFabric(ctx, opsActor, empty.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteFabric(ctx, opsActor, empty.ID); !errors.Is(err, apperr.ErrFabricNotFound) {
		t.Fatalf("delete twice = %v", err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action IN ('fabric.create', 'fabric.update', 'fabric.delete')`).Scan(&n)
	if n != 4 { // 三次 create（交换机 A、办公网、空网）+ 一次 delete
		t.Fatalf("fabric audits = %d", n)
	}
}

func TestDirectFabricMaxTwo(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	d, err := svc.CreateFabric(ctx, opsActor, fleet.FabricInput{Name: "直连", Kind: "cx7", Isolation: "direct", RDMA: true})
	if err != nil {
		t.Fatal(err)
	}
	ids := []int64{readyMachine(t, svc, pool, "SN1"), readyMachine(t, svc, pool, "SN2"), readyMachine(t, svc, pool, "SN3")}
	for _, id := range ids[:2] {
		m, err := svc.UpdateMachine(ctx, opsActor, id, fleet.MachineUpdate{SetFabric: true, FabricID: &d.ID})
		if err != nil || !m.ClusterReady || !m.ClusterRDMA || m.ClusterNet != "l2" || m.FabricName != "直连" || m.ClusterReason != "" {
			t.Fatalf("join direct = %+v, %v", m, err)
		}
	}
	if _, err := svc.UpdateMachine(ctx, opsActor, ids[2], fleet.MachineUpdate{SetFabric: true, FabricID: &d.ID}); !errors.Is(err, apperr.ErrFabricFull) {
		t.Fatalf("third machine = %v", err)
	}
	// 已在 fabric 里的机器重复设置同一 fabric 不算新增
	if _, err := svc.UpdateMachine(ctx, opsActor, ids[0], fleet.MachineUpdate{SetFabric: true, FabricID: &d.ID, ClusterIface: strp("enp1s0f0np0")}); err != nil {
		t.Fatalf("re-set same fabric = %v", err)
	}
	if _, err := svc.UpdateMachine(ctx, opsActor, ids[2], fleet.MachineUpdate{SetFabric: true, FabricID: i64p(99999)}); !errors.Is(err, apperr.ErrFabricNotFound) {
		t.Fatalf("missing fabric = %v", err)
	}
	// vlan fabric 挂 3 台后不能改成 direct
	v, _ := svc.CreateFabric(ctx, opsActor, fleet.FabricInput{Name: "交换机", Kind: "cx7", Isolation: "vlan", VLANMin: intp(10), VLANMax: intp(20)})
	for _, id := range ids {
		if _, err := svc.UpdateMachine(ctx, opsActor, id, fleet.MachineUpdate{SetFabric: true, FabricID: &v.ID}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.UpdateFabric(ctx, opsActor, v.ID, fleet.FabricInput{Name: "交换机", Kind: "cx7", Isolation: "direct"}); !errors.Is(err, apperr.ErrFabricFull) {
		t.Fatalf("vlan → direct with 3 machines = %v", err)
	}
}

func TestUpdateFabricRecomputesMachines(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	f, _ := svc.CreateFabric(ctx, opsActor, fleet.FabricInput{Name: "A", Kind: "cx7", Isolation: "direct"})
	id := readyMachine(t, svc, pool, "SN1")
	m, err := svc.UpdateMachine(ctx, opsActor, id, fleet.MachineUpdate{SetFabric: true, FabricID: &f.ID})
	if err != nil || !m.ClusterReady || m.ClusterRDMA || m.ClusterNet != "l2" {
		t.Fatalf("joined = %+v, %v", m, err)
	}
	// 打开 RDMA → 机器 cluster_rdma 随之变 true
	if _, err := svc.UpdateFabric(ctx, opsActor, f.ID, fleet.FabricInput{Name: "A", Kind: "cx7", Isolation: "direct", RDMA: true}); err != nil {
		t.Fatal(err)
	}
	if m, _ = svc.Get(ctx, pool, id); !m.ClusterReady || !m.ClusterRDMA {
		t.Fatalf("after rdma on = %+v", m)
	}
	// 改成 lan（无活跃集群可以改结构）→ 用管理网卡、网段 10.0.0.0/24、无 RDMA
	if _, err := svc.UpdateFabric(ctx, opsActor, f.ID, lanInput("A")); err != nil {
		t.Fatal(err)
	}
	if m, _ = svc.Get(ctx, pool, id); !m.ClusterReady || m.ClusterRDMA || m.ClusterNet != "10.0.0.0/24" {
		t.Fatalf("after kind → lan = %+v", m)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action = 'fabric.update' AND diff->'before'->>'kind' = 'cx7' AND diff->'after'->>'kind' = 'lan'`).Scan(&n)
	if n != 1 {
		t.Fatalf("update audit = %d", n)
	}
}

func TestUpdateFabricBlockedByActiveCluster(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	f, _ := svc.CreateFabric(ctx, opsActor, fleet.FabricInput{Name: "V", Kind: "cx7", Isolation: "vlan", VLANMin: intp(10), VLANMax: intp(20)})
	insertActiveCluster(t, pool, "clu-aaaaaa0002", f.ID, 2)
	for name, in := range map[string]fleet.FabricInput{
		"改隔离":       {Name: "V", Kind: "cx7", Isolation: "direct"},
		"改 VLAN 范围": {Name: "V", Kind: "cx7", Isolation: "vlan", VLANMin: intp(10), VLANMax: intp(30)},
		"改类型":       lanInput("V"),
	} {
		if _, err := svc.UpdateFabric(ctx, opsActor, f.ID, in); !errors.Is(err, apperr.ErrFabricInUse) {
			t.Fatalf("%s = %v", name, err)
		}
	}
	// 名称 / RDMA / MTU / 描述可以改
	got, err := svc.UpdateFabric(ctx, opsActor, f.ID, fleet.FabricInput{Name: "V2", Kind: "cx7", Isolation: "vlan", RDMA: true, MTU: 9000,
		VLANMin: intp(10), VLANMax: intp(20), Description: "改名"})
	if err != nil || got.Name != "V2" || !got.RDMA || got.MTU != 9000 || got.Description != "改名" {
		t.Fatalf("soft update = %+v, %v", got, err)
	}
}
