package e2ehooks_test

import (
	"context"
	"strings"
	"testing"

	"spark-cloud/internal/agent/features"
	"spark-cloud/internal/e2ehooks"
	"spark-cloud/internal/fleet"
)

// 计划 13：/hooks/fabrics 建 fabric；/hooks/machines 的 fabric_id / net / old_agent 让 fake Agent 带对应能力位与网卡，
// 接入后加入 fabric，返回时 cluster_ready 已按 fleet 规则算好（lan：127.0.0.0/24；cx7：l2；cx7-rdma + rdma fabric：cluster_rdma）。
func TestFabricAndClusterMachineHooks(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	pool := e.a.Fleet.Pool()

	lan := e.must("POST", "/hooks/fabrics", map[string]any{"kind": "lan"})
	cx7 := e.must("POST", "/hooks/fabrics", map[string]any{"kind": "cx7", "isolation": "vlan", "rdma": true})
	direct := e.must("POST", "/hooks/fabrics", map[string]any{"name": "e2e-direct", "kind": "cx7"})
	lanID, cx7ID, directID := int64(lan["id"].(float64)), int64(cx7["id"].(float64)), int64(direct["id"].(float64))
	if f, err := e.a.Fleet.GetFabric(ctx, pool, lanID); err != nil || f.Isolation != fleet.IsolationRouted || f.Name != lan["name"] {
		t.Fatalf("lan fabric = %+v %v", f, err)
	}
	if f, err := e.a.Fleet.GetFabric(ctx, pool, cx7ID); err != nil || f.Isolation != fleet.IsolationVLAN || !f.RDMA || f.VLANMin == nil || *f.VLANMin != 100 || *f.VLANMax != 199 {
		t.Fatalf("cx7 fabric = %+v %v", f, err)
	}
	if f, err := e.a.Fleet.GetFabric(ctx, pool, directID); err != nil || f.Isolation != fleet.IsolationDirect || f.Name != "e2e-direct" {
		t.Fatalf("direct fabric = %+v %v", f, err)
	}
	for _, bad := range []map[string]any{{"kind": "ib"}, {"kind": "lan", "rdma": true}, {"name": "e2e-direct", "kind": "cx7"}} {
		if st, out := e.call("POST", "/hooks/fabrics", bad); st != 400 {
			t.Fatalf("fabric %v = %d %v", bad, st, out)
		}
	}

	machines := func(body map[string]any) ([]int64, []bool) {
		t.Helper()
		out := e.must("POST", "/hooks/machines", body)
		var ids []int64
		var ready []bool
		for i, id := range out["ids"].([]any) {
			ids = append(ids, int64(id.(float64)))
			ready = append(ready, out["cluster_ready"].([]any)[i].(bool))
		}
		return ids, ready
	}
	get := func(id int64) fleet.Machine {
		t.Helper()
		m, err := e.a.Fleet.Get(ctx, pool, id)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}

	ids, ready := machines(map[string]any{"count": 2, "fabric_id": lanID, "net": "lan"})
	for i, id := range ids {
		m := get(id)
		if !ready[i] || !m.ClusterReady || m.ClusterRDMA || m.ClusterNet != "127.0.0.0/24" || m.FabricID == nil || *m.FabricID != lanID ||
			m.AgentFeatures&features.Cluster == 0 || m.NetInfo == nil || len(m.NetInfo.Ifaces) != 1 {
			t.Fatalf("lan machine = %+v (ready %v)", m, ready[i])
		}
	}
	ids, ready = machines(map[string]any{"count": 2, "fabric_id": cx7ID, "net": "cx7-rdma"})
	for i, id := range ids {
		m := get(id)
		if !ready[i] || !m.ClusterReady || !m.ClusterRDMA || m.ClusterNet != "l2" || len(m.NetInfo.Ifaces) != 2 || m.NetInfo.Ifaces[1].Uverbs != "uverbs0" {
			t.Fatalf("cx7-rdma machine = %+v", m)
		}
	}
	ids, _ = machines(map[string]any{"count": 1, "fabric_id": cx7ID, "net": "cx7"})
	if m := get(ids[0]); !m.ClusterReady || m.ClusterRDMA || m.ClusterNet != "l2" {
		t.Fatalf("cx7 (no rdma) machine = %+v", m)
	}
	ids, ready = machines(map[string]any{"count": 1, "fabric_id": lanID, "old_agent": true})
	if m := get(ids[0]); ready[0] || m.ClusterReady || m.AgentFeatures != features.SharedSlots || !strings.Contains(m.ClusterReason, "Agent 不支持集群") {
		t.Fatalf("old agent machine = %+v", m)
	}
	// 不带 fabric：照旧（不在集群网络、默认网卡）
	ids, ready = machines(map[string]any{"count": 1})
	if m := get(ids[0]); ready[0] || m.FabricID != nil || m.NetInfo == nil || m.AgentFeatures&features.Cluster == 0 {
		t.Fatalf("plain machine = %+v", m)
	}
	// direct fabric 最多 2 台
	machines(map[string]any{"count": 2, "fabric_id": directID, "net": "cx7"})
	if st, out := e.call("POST", "/hooks/machines", map[string]any{"count": 1, "fabric_id": directID, "net": "cx7"}); st != 400 ||
		!strings.Contains(out["error"].(string), "fabric_full") {
		t.Fatalf("third direct machine = %d %v", st, out)
	}
	for _, bad := range []map[string]any{{"count": 1, "net": "wifi"}, {"count": 1, "fabric_id": 99999}} {
		if st, out := e.call("POST", "/hooks/machines", bad); st != 400 {
			t.Fatalf("machines %v = %d %v", bad, st, out)
		}
	}
	if _, err := e2ehooks.NetPreset("cx7", "10.0.0.5"); err != nil {
		t.Fatal(err)
	}
}
