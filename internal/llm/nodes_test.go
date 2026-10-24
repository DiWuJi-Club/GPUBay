package llm_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/llm"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/rental"
)

func TestAdminNodeOps(t *testing.T) {
	ne := newNodeEnv(t)
	ne.poolMachine("N1")
	ne.poolMachine("N2")
	ne.poolMachine("N3")
	m := ne.onlineModel("qwen3-4b", 2)
	ne.tick()
	nodes := ne.bringReady(m.ID)
	a, b := nodes[0], nodes[1]
	actor := ne.admin()

	// 不存在
	if _, err := ne.svc.AdminDrainNode(ne.ctx, actor, "node-nonexistent0"); !errors.Is(err, apperr.ErrLLMNodeNotFound) {
		t.Fatalf("drain unknown: %v", err)
	}
	// 重建 ready → Redeploy → provisioning，rebuild_count 不变
	got, err := ne.svc.AdminRebuildNode(ne.ctx, actor, a.ID)
	if err != nil || got.State != llm.NodeProvisioning || got.RebuildCount != 0 {
		t.Fatalf("rebuild ready: %+v %v", got, err)
	}
	calls := ne.apps.redeployCalls()
	if len(calls) != 1 || calls[0].DeploymentID != *a.DeploymentID || calls[0].EnvSet || calls[0].UserID != ne.systemUserID {
		t.Fatalf("redeploy calls %+v", calls)
	}
	if ne.auditCount("llm.node.rebuild", a.ID) != 1 {
		t.Fatal("rebuild audit missing")
	}
	// provisioning 不能重建
	if _, err := ne.svc.AdminRebuildNode(ne.ctx, actor, a.ID); errCode(err) != "llm_node_invalid_state" {
		t.Fatalf("rebuild provisioning: %v", err)
	}
	// 排空：provisioning → draining；再排空 → invalid_state
	got, err = ne.svc.AdminDrainNode(ne.ctx, actor, a.ID)
	if err != nil || got.State != llm.NodeDraining || got.DrainSince == nil {
		t.Fatalf("drain: %+v %v", got, err)
	}
	if _, err := ne.svc.AdminDrainNode(ne.ctx, actor, a.ID); !errors.Is(err, apperr.ErrLLMNodeInvalidState) {
		t.Fatalf("drain draining: %v", err)
	}
	if ne.auditCount("llm.node.drain", a.ID) != 1 {
		t.Fatal("drain audit missing")
	}
	if diff := ne.lastAuditDiff("llm.node.drain", a.ID); diff["from"] != "provisioning" || diff["to"] != "draining" {
		t.Fatalf("drain diff %v", diff)
	}

	// 移除：reason 必填；ready → removing + 立即 ReleaseSystem（不等在途）
	if _, err := ne.svc.AdminRemoveNode(ne.ctx, actor, b.ID, "  "); errCode(err) != "bad_request" {
		t.Fatalf("remove without reason: %v", err)
	}
	defer ne.svc.Router().Acquire(b.ID)()
	got, err = ne.svc.AdminRemoveNode(ne.ctx, actor, b.ID, "显卡报错")
	if err != nil || got.State != llm.NodeRemoving || !slices.Contains(ne.rental.releasedIDs(), *b.InstanceID) {
		t.Fatalf("remove: %+v %v released %v", got, err, ne.rental.releasedIDs())
	}
	if diff := ne.lastAuditDiff("llm.node.remove", b.ID); diff["reason"] != "显卡报错" || diff["from"] != "ready" {
		t.Fatalf("remove diff %v", diff)
	}
	if _, err := ne.svc.AdminRemoveNode(ne.ctx, actor, b.ID, "again"); !errors.Is(err, apperr.ErrLLMNodeInvalidState) {
		t.Fatalf("remove removing: %v", err)
	}
	if _, err := ne.svc.AdminDrainNode(ne.ctx, actor, b.ID); !errors.Is(err, apperr.ErrLLMNodeInvalidState) {
		t.Fatalf("drain removing: %v", err)
	}

	// failed 节点：机器还没释放完 → 重建 invalid_state；机器空出来后 → 在同一台机器上新建节点行
	ne.exec(`UPDATE llm_nodes SET state = 'failed' WHERE id = $1`, a.ID)
	if _, err := ne.svc.AdminRebuildNode(ne.ctx, actor, a.ID); !errors.Is(err, apperr.ErrLLMNodeInvalidState) {
		t.Fatalf("rebuild failed on busy machine: %v", err)
	}
	ne.setInstance(*a.InstanceID, rental.StateReleased)
	fresh, err := ne.svc.AdminRebuildNode(ne.ctx, actor, a.ID)
	if err != nil || fresh.ID == a.ID || fresh.MachineID != a.MachineID || fresh.State != llm.NodeProvisioning || fresh.InstanceID == nil {
		t.Fatalf("rebuild failed: %+v %v", fresh, err)
	}
	if diff := ne.lastAuditDiff("llm.node.rebuild", fresh.ID); diff["replaces"] != a.ID {
		t.Fatalf("rebuild-failed diff %v", diff)
	}
	if ne.node(a.ID).State != llm.NodeFailed {
		t.Fatal("old failed node must stay failed (not revived)")
	}
	// failed → remove → removed
	got, err = ne.svc.AdminRemoveNode(ne.ctx, actor, a.ID, "清理")
	if err != nil || got.State != llm.NodeRemoved {
		t.Fatalf("remove failed: %+v %v", got, err)
	}
	if _, err := ne.svc.AdminRemoveNode(ne.ctx, actor, a.ID, "清理"); !errors.Is(err, apperr.ErrLLMNodeInvalidState) {
		t.Fatalf("remove removed: %v", err)
	}
	if ne.auditCount("llm.node.remove", a.ID) != 1 {
		t.Fatal("remove audit count")
	}
}

func TestMachineHasLiveNode(t *testing.T) {
	ne := newNodeEnv(t)
	mach := ne.poolMachine("M1")
	other := ne.poolMachine("M2")
	m := ne.onlineModel("qwen3-4b", 1)
	has := func(id int64) bool {
		t.Helper()
		b, err := ne.svc.MachineHasLiveNode(ne.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if has(mach) || has(other) {
		t.Fatal("no nodes yet")
	}
	ne.tick()
	n := ne.nodesOf(m.ID)[0]
	if n.MachineID != mach || !has(mach) || has(other) {
		t.Fatal("provisioning node should count")
	}
	// 节点 failed 但系统实例还在（洗机中也算）→ 仍 true
	ne.exec(`UPDATE llm_nodes SET state = 'failed' WHERE id = $1`, n.ID)
	ne.exec(`UPDATE instances SET state = 'wiping' WHERE id = $1`, *n.InstanceID)
	if !has(mach) {
		t.Fatal("live system instance should count")
	}
	ne.setInstance(*n.InstanceID, rental.StateReleased)
	if has(mach) {
		t.Fatal("failed node with released instance should not count")
	}
	// removed 节点不算
	ne.rawNode("node-removed00001", m.ID, other, llm.NodeRemoved)
	if has(other) {
		t.Fatal("removed node should not count")
	}
}

func TestPoolViewAndNodeDetail(t *testing.T) {
	ne := newNodeEnv(t)
	busy := ne.poolMachine("V1")
	leased := ne.poolMachine("V2")
	free := ne.poolMachine("V3")
	ne.exec(`UPDATE machines SET display_name = 'Spark 01' WHERE id = $1`, busy)
	now := ne.clk.Now()
	until := now.Add(72 * time.Hour)
	ne.rentalLease(leased, "running", "active", now.Add(-time.Hour), until)
	m := ne.onlineModel("qwen3-4b", 1)
	ne.tick()
	n := ne.bringReady(m.ID)[0]
	if n.MachineID != busy {
		t.Fatalf("node on %d", n.MachineID)
	}

	pv, err := ne.svc.PoolView(ne.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pv.GroupID != ne.poolGroupID || pv.Total != 3 || pv.Free != 1 || len(pv.Machines) != 3 {
		t.Fatalf("pool %+v", pv)
	}
	byID := map[int64]llm.PoolMachine{}
	for _, pm := range pv.Machines {
		byID[pm.Machine.ID] = pm
	}
	if pm := byID[busy]; pm.Node == nil || pm.Node.ID != n.ID || pm.Node.ModelSlug != "qwen3-4b" || pm.Node.State != llm.NodeReady ||
		pm.LeaseUntil != nil || pm.Machine.DisplayName != "Spark 01" {
		t.Fatalf("busy %+v", pm)
	}
	if pm := byID[leased]; pm.Node != nil || pm.LeaseUntil == nil || !pm.LeaseUntil.Equal(until) {
		t.Fatalf("leased %+v", pm)
	}
	if pm := byID[free]; pm.Node != nil || pm.LeaseUntil != nil || !pm.Machine.AgentOnline || pm.Machine.Serial != "V3" {
		t.Fatalf("free %+v", pm)
	}
	b, _ := json.Marshal(byID[free])
	if string(b) != `{"machine":{"id":`+itoa(free)+`,"serial":"V3","display_name":"","state":"idle","agent_online":true}}` {
		t.Fatalf("pool machine json %s", b)
	}

	// 节点详情：部署摘要、在途、事件、部署事件
	ne.exec(`UPDATE app_deployments SET phase = 'health', progress = 100 WHERE id = $1`, *n.DeploymentID)
	rel := ne.svc.Router().Acquire(n.ID)
	defer rel()
	d, err := ne.svc.GetNode(ne.ctx, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Node.Inflight != 1 || d.Node.MachineName != "Spark 01" || d.Node.Deployment == nil ||
		*d.Node.Deployment != (llm.NodeDeployment{State: appdeploy.StateRunning, Health: appdeploy.HealthOK, Phase: "health", Progress: 100}) {
		t.Fatalf("detail %+v deployment %+v", d.Node, d.Node.Deployment)
	}
	if len(d.Events) != 2 || d.Events[0].ToState != llm.NodeReady || len(d.DeploymentEvents) != 1 {
		t.Fatalf("events %+v / %+v", d.Events, d.DeploymentEvents)
	}
	var raw struct {
		Node map[string]any `json:"node"`
	}
	b, _ = json.Marshal(d)
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"id", "model_id", "model_slug", "machine_id", "machine_name", "instance_id", "deployment_id", "state",
		"deployment", "inflight", "probe_fail_count", "rebuild_count", "unhealthy_since", "drain_since", "last_probe_at", "last_ready_at",
		"error", "created_at", "updated_at"} {
		if _, ok := raw.Node[k]; !ok {
			t.Fatalf("node json lacks %q: %s", k, b)
		}
	}
	if _, ok := raw.Node["api_key_enc"]; ok {
		t.Fatal("node key leaked into json")
	}
	if _, err := ne.svc.GetNode(ne.ctx, "node-nonexistent0"); !errors.Is(err, apperr.ErrLLMNodeNotFound) {
		t.Fatalf("unknown node: %v", err)
	}
	// 筛选
	if rows, _ := ne.svc.ListNodes(ne.ctx, llm.NodeFilter{State: llm.NodeReady}); len(rows) != 1 {
		t.Fatalf("ready rows %d", len(rows))
	}
	if rows, _ := ne.svc.ListNodes(ne.ctx, llm.NodeFilter{State: llm.NodeFailed}); len(rows) != 0 {
		t.Fatalf("failed rows %d", len(rows))
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
