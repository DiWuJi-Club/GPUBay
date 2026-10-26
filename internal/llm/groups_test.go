package llm_test

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/llm"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/rental"
)

// ---- 模型 ----

func TestModelParallelValidation(t *testing.T) {
	e := newEnv(t)
	v4 := e.clusterVersion("1.1.0", 4, 360)
	v2 := e.clusterVersion("1.1.1", 2, 360)
	plain := e.plainVersion("1.0.9")
	pin := func(id int64) func(*llm.ModelInput) { return func(in *llm.ModelInput) { in.AppVersionID = &id } }
	bad := []struct {
		name, field string
		mod         func(*llm.ModelInput)
	}{
		{"n3", "nodes_per_replica", func(in *llm.ModelInput) { in.NodesPerReplica = 3; in.AppVersionID = &v4 }},
		{"n8", "nodes_per_replica", func(in *llm.ModelInput) { in.NodesPerReplica = 8; in.AppVersionID = &v4 }},
		{"tp-pp-mismatch", "tensor_parallel", func(in *llm.ModelInput) {
			in.NodesPerReplica, in.TensorParallel, in.PipelineParallel, in.AppVersionID = 2, 2, 2, &v4
		}},
		{"pp-not-divisor", "tensor_parallel", func(in *llm.ModelInput) { in.NodesPerReplica, in.PipelineParallel, in.AppVersionID = 4, 3, &v4 }},
		{"tp-on-single", "tensor_parallel", func(in *llm.ModelInput) { in.TensorParallel = 2 }},
		{"embedding-n2", "nodes_per_replica", func(in *llm.ModelInput) { in.Kind, in.NodesPerReplica, in.AppVersionID = llm.KindEmbedding, 2, &v4 }},
		{"version-no-cluster", "nodes_per_replica", func(in *llm.ModelInput) { in.NodesPerReplica, in.AppVersionID = 2, &plain }},
		{"version-max-2", "nodes_per_replica", func(in *llm.ModelInput) { in.NodesPerReplica, in.AppVersionID = 4, &v2 }},
		{"pp-unsupported", "pipeline_parallel", func(in *llm.ModelInput) { in.NodesPerReplica, in.PipelineParallel, in.AppVersionID = 4, 2, &v4 }},
		{"reserved-tp", "node_env", func(in *llm.ModelInput) {
			in.NodesPerReplica, in.AppVersionID, in.NodeEnv = 2, &v4, map[string]string{"TENSOR_PARALLEL_SIZE": "2"}
		}},
	}
	for _, c := range bad {
		in := e.modelInput("bad-" + c.name)
		c.mod(&in)
		_, err := e.svc.CreateModel(e.ctx, e.admin(), in)
		if errCode(err) != "llm_model_invalid" || !strings.Contains(err.Error(), c.field) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	// 零值补齐：TP = N / PP、PP = 1；N = 0 → 1
	m2 := e.createModel("g2", func(in *llm.ModelInput) { in.NodesPerReplica = 2 }, pin(v4))
	if m2.NodesPerReplica != 2 || m2.TensorParallel != 2 || m2.PipelineParallel != 1 {
		t.Fatalf("defaults %+v", m2)
	}
	m4 := e.createModel("g4", func(in *llm.ModelInput) { in.NodesPerReplica = 4 }, pin(v4))
	if m4.TensorParallel != 4 || m4.PipelineParallel != 1 {
		t.Fatalf("n 4 defaults %+v", m4)
	}
	if one := e.createModel("single"); one.NodesPerReplica != 1 || one.TensorParallel != 1 || one.PipelineParallel != 1 {
		t.Fatalf("single defaults %+v", one)
	}
	// 改 N：有非终态节点 → 拒绝；无节点 → 允许；TP / PP 随时可改
	m := e.createModel("chg", pin(v4))
	mach := e.poolMachine("CHG1")
	nodeID := e.rawNode("node-chg000000001", m.ID, mach, llm.NodeReady)
	in := e.modelInput("chg")
	in.AppVersionID, in.NodesPerReplica = &v4, 2
	if _, err := e.svc.UpdateModel(e.ctx, e.admin(), m.ID, in); errCode(err) != "llm_model_invalid" ||
		!strings.Contains(err.Error(), "请先把副本数调为 0 并等节点全部释放，再修改每副本机器数") {
		t.Fatalf("change N with live node: %v", err)
	}
	e.exec(`UPDATE llm_nodes SET state = 'removed' WHERE id = $1`, nodeID)
	got, err := e.svc.UpdateModel(e.ctx, e.admin(), m.ID, in)
	if err != nil || got.NodesPerReplica != 2 || got.TensorParallel != 2 {
		t.Fatalf("change N without nodes: %+v %v", got, err)
	}
	e.rawNode("node-chg000000002", m.ID, mach, llm.NodeReady)
	in.Name = "renamed"
	if got, err := e.svc.UpdateModel(e.ctx, e.admin(), m.ID, in); err != nil || got.Name != "renamed" || got.TensorParallel != 2 {
		t.Fatalf("update with nodes: %+v %v", got, err)
	}
	in.TensorParallel, in.PipelineParallel = 1, 2
	if _, err := e.svc.UpdateModel(e.ctx, e.admin(), m.ID, in); errCode(err) != "llm_model_invalid" || !strings.Contains(err.Error(), "cupy") {
		t.Fatalf("PP 2: %v", err)
	}
}

// N = 1 的平台变量与计划 9 逐项相同（硬编码期望）。
func TestPlatformNodeEnvSingleUnchanged(t *testing.T) {
	key := strings.Repeat("k", 43)
	for _, m := range []llm.Model{
		{Slug: "qwen3-4b", Kind: llm.KindChat, Source: "modelscope", SourceID: "Qwen/Qwen3-4B", ContextLen: 32768},
		{Slug: "qwen3-4b", Kind: llm.KindChat, Source: "modelscope", SourceID: "Qwen/Qwen3-4B", ContextLen: 32768,
			NodesPerReplica: 1, TensorParallel: 1, PipelineParallel: 1},
	} {
		want := map[string]string{"MODEL_ID": "Qwen/Qwen3-4B", "MODEL_SOURCE": "modelscope", "SERVED_MODEL_NAME": "qwen3-4b",
			"MAX_MODEL_LEN": "32768", "API_KEY": key, "TASK": "generate"}
		if got := llm.PlatformNodeEnv(m, key); !maps.Equal(got, want) {
			t.Fatalf("PlatformNodeEnv(N=%d) = %v", m.NodesPerReplica, got)
		}
		if got := llm.NodeEnv(m, key); !maps.Equal(got, want) {
			t.Fatalf("NodeEnv(N=%d) = %v", m.NodesPerReplica, got)
		}
	}
}

func TestPlatformNodeEnvGroup(t *testing.T) {
	m := llm.Model{Slug: "qwen3-235b", Kind: llm.KindChat, Source: "modelscope", SourceID: "Qwen/Qwen3-235B", ContextLen: 32768,
		NodesPerReplica: 4, TensorParallel: 2, PipelineParallel: 2, NodeEnv: map[string]string{"MAX_NUM_SEQS": "16"}}
	got := llm.NodeEnv(m, "k")
	if got["TENSOR_PARALLEL_SIZE"] != "2" || got["PIPELINE_PARALLEL_SIZE"] != "2" || got["MAX_NUM_SEQS"] != "16" || len(got) != 9 {
		t.Fatalf("group NodeEnv = %v", got)
	}
}

// ---- 建组 ----

// 两个 fabric（3 台 / 2 台），N = 2 → 选最贴合的 2 台那个；成员按机器 ID 给 rank，节点指向 rank0。
func TestProvisionGroupPicksFabricWithN(t *testing.T) {
	ne := newNodeEnv(t)
	fa, fb := ne.fabric("lan", "routed"), ne.fabric("cx7", "vlan")
	for _, s := range []string{"A1", "A2", "A3"} {
		ne.fabricMachine(s, fa, "192.168.1.0/24")
	}
	b1, b2 := ne.fabricMachine("B1", fb, "l2"), ne.fabricMachine("B2", fb, "l2")
	m := ne.groupModel("qwen3-32b", 2, 1, ne.clusterVersion("1.1.0", 4, 360))
	rep := ne.tick()
	if rep.Provisioned != 1 {
		t.Fatalf("report %+v", rep)
	}
	calls := ne.rental.clusterPlaceCalls()
	if len(calls) != 1 || !slices.Equal(calls[0].MachineIDs, []int64{b1, b2}) || calls[0].GroupID != ne.poolGroupID || calls[0].ImageID != m.ImageID {
		t.Fatalf("PlaceSystemCluster calls %+v", calls)
	}
	nodes := ne.nodesOf(m.ID)
	if len(nodes) != 1 {
		t.Fatalf("nodes %+v", nodes)
	}
	n := nodes[0]
	ms := ne.members(n.ID)
	if n.Size != 2 || n.ClusterID == nil || n.State != llm.NodeProvisioning || len(ms) != 2 || ms[0].MachineID != b1 || ms[1].MachineID != b2 ||
		n.MachineID != b1 || *n.InstanceID != ms[0].InstanceID || !ms[0].Live || !ms[1].Live {
		t.Fatalf("node %+v members %+v", n.Node, ms)
	}
	if len(n.Members) != 2 || n.Members[1].MachineID != b2 || n.Members[1].InstanceState != rental.StateCreating {
		t.Fatalf("row members %+v", n.Members)
	}
	if len(ne.rental.placed) != 0 {
		t.Fatal("PlaceSystem used for a group model")
	}
	if err := llm.CheckInvariants(ne.ctx, ne.pool); err != nil {
		t.Fatal(err)
	}
}

// N 大的模型先补：唯一的 2 台 fabric 给 N = 2 的模型，N = 1 的模型（排在前面）缺额告警。
func TestProvisionLargerNFirst(t *testing.T) {
	ne := newNodeEnv(t)
	f := ne.fabric("lan", "routed")
	ne.fabricMachine("F1", f, "10.1.0.0/24")
	ne.fabricMachine("F2", f, "10.1.0.0/24")
	single := ne.onlineModel("qwen3-4b", 1)
	group := ne.groupModel("qwen3-32b", 2, 1, ne.clusterVersion("1.1.0", 4, 360))
	ne.tick()
	if got := ne.nodesOf(group.ID); len(got) != 1 || got[0].Size != 2 {
		t.Fatalf("group nodes %+v", got)
	}
	if got := ne.nodesOf(single.ID); len(got) != 0 {
		t.Fatalf("single nodes %+v", got)
	}
	if ne.alertCount(llm.AlertPoolShort) != 1 {
		t.Fatal("pool short alert missing")
	}
}

// N = 1 优先用不属于 fabric 的池机器（即使 ID 更大）；只剩 fabric 机器时照常用。
func TestSingleNodePrefersNonFabric(t *testing.T) {
	ne := newNodeEnv(t)
	f := ne.fabric("lan", "routed")
	fm := ne.fabricMachine("F1", f, "10.1.0.0/24")
	plain := ne.poolMachine("P1")
	m := ne.onlineModel("qwen3-4b", 1)
	ne.tick()
	if got := ne.nodesOf(m.ID); len(got) != 1 || got[0].MachineID != plain {
		t.Fatalf("nodes %+v (fabric machine %d, plain %d)", got, fm, plain)
	}
	if _, err := ne.svc.UpdateModel(ne.ctx, ne.admin(), m.ID, func() llm.ModelInput {
		in := ne.modelInput("qwen3-4b")
		in.Replicas = 2
		return in
	}()); err != nil {
		t.Fatal(err)
	}
	ne.clk.Advance(time.Second)
	ne.tick()
	if got := ne.nodesOf(m.ID); len(got) != 2 || got[1].MachineID != fm {
		t.Fatalf("second node %+v", got)
	}
}

// 不够 N 台的同 fabric 就绪机器 → 不拼组、告警（一次）；direct 只组 2 台；未就绪 / 旧 Agent 机器不算。
func TestProvisionGroupShortAlert(t *testing.T) {
	ne := newNodeEnv(t)
	fa, fd := ne.fabric("lan", "routed"), ne.fabric("cx7", "direct")
	for _, s := range []string{"A1", "A2", "A3"} {
		ne.fabricMachine(s, fa, "10.1.0.0/24")
	}
	notReady := ne.fabricMachine("A4", fa, "10.1.0.0/24")
	ne.exec(`UPDATE machines SET cluster_ready = false WHERE id = $1`, notReady)
	oldAgent := ne.fabricMachine("A5", fa, "10.1.0.0/24")
	ne.exec(`UPDATE machines SET agent_features = 1 WHERE id = $1`, oldAgent)
	otherNet := ne.fabricMachine("A6", fa, "10.2.0.0/24")
	_ = otherNet
	for _, s := range []string{"D1", "D2", "D3", "D4"} { // direct：即使有 4 台也只组 2 台
		ne.fabricMachine(s, fd, "l2")
	}
	ne.poolMachine("P1")
	ne.poolMachine("P2")
	m := ne.groupModel("qwen3-235b", 4, 1, ne.clusterVersion("1.1.0", 4, 360))
	ne.tick()
	ne.tick()
	if got := ne.nodesOf(m.ID); len(got) != 0 {
		t.Fatalf("nodes %+v", got)
	}
	if len(ne.rental.clusterPlaceCalls()) != 0 {
		t.Fatal("PlaceSystemCluster called without a fitting fabric")
	}
	if ne.alertCount(llm.AlertPoolShort) != 1 {
		t.Fatalf("pool short alerts %d", ne.alertCount(llm.AlertPoolShort))
	}
}

// 插节点行失败（这里：PlaceSystemCluster 返回后、插行前 head 机器被别的节点占了）→ ReleaseSystemCluster，换下一个 fabric。
func TestCreateGroupNodeInsertFailureReleases(t *testing.T) {
	ne := newNodeEnv(t)
	fa, fb := ne.fabric("lan", "routed"), ne.fabric("lan", "routed")
	a1 := ne.fabricMachine("A1", fa, "10.1.0.0/24")
	ne.fabricMachine("A2", fa, "10.1.0.0/24")
	ne.fabricMachine("B1", fb, "10.2.0.0/24")
	ne.fabricMachine("B2", fb, "10.2.0.0/24")
	ne.fabricMachine("B3", fb, "10.2.0.0/24")
	m := ne.groupModel("qwen3-32b", 2, 1, ne.clusterVersion("1.1.0", 4, 360))
	other := ne.createModel("other")
	var first string
	ne.rental.afterPlaceCluster = func(cid string) {
		if first == "" {
			first = cid
			ne.rawNode("node-race00000001", other.ID, a1, llm.NodeReady)
		}
	}
	rep := ne.tick()
	if rep.Provisioned != 1 || first == "" {
		t.Fatalf("report %+v", rep)
	}
	if rel := ne.rental.releasedClusters(); !slices.Equal(rel, []string{first}) {
		t.Fatalf("released clusters %v (first %s)", rel, first)
	}
	if n := ne.queryInt(`SELECT count(*) FROM instances WHERE cluster_id = $1 AND state <> 'wiping'`, first); n != 0 {
		t.Fatalf("%d instances of the failed cluster not released", n)
	}
	nodes := ne.nodesOf(m.ID)
	if len(nodes) != 1 || *nodes[0].ClusterID == first {
		t.Fatalf("nodes %+v", nodes)
	}
	if n := ne.queryInt(`SELECT count(*) FROM llm_node_members`); n != 2 {
		t.Fatalf("member rows %d", n)
	}
}

// ---- 推进 provisioning ----

func TestAdvanceGroupWaitsAllRunning(t *testing.T) {
	ne := newNodeEnv(t)
	f := ne.fabric("lan", "routed")
	ne.fabricMachine("A1", f, "10.1.0.0/24")
	ne.fabricMachine("A2", f, "10.1.0.0/24")
	m := ne.groupModel("qwen3-32b", 2, 1, ne.clusterVersion("1.1.0", 4, 360))
	ne.tick()
	n := ne.nodesOf(m.ID)[0]
	ne.setInstance(n.Members[0].InstanceID, rental.StateRunning)
	ne.tick()
	if len(ne.apps.clusterCreateCalls()) != 0 || len(ne.apps.createCalls()) != 0 {
		t.Fatal("deployment created before all members running")
	}
	if got := ne.node(n.ID); got.State != llm.NodeProvisioning || got.AppClusterID != nil {
		t.Fatalf("node %+v", got)
	}
}

func TestAdvanceGroupCreatesClusterDeployment(t *testing.T) {
	ne := newNodeEnv(t)
	f := ne.fabric("lan", "routed")
	ne.fabricMachine("A1", f, "10.1.0.0/24")
	ne.fabricMachine("A2", f, "10.1.0.0/24")
	ver := ne.clusterVersion("1.1.0", 4, 360)
	m := ne.groupModel("qwen3-32b", 2, 1, ver, func(in *llm.ModelInput) { in.NodeEnv = map[string]string{"MAX_NUM_SEQS": "16"} })
	ne.tick()
	n := ne.nodesOf(m.ID)[0]
	for _, mb := range n.Members {
		ne.setInstance(mb.InstanceID, rental.StateRunning)
	}
	ne.tick()
	calls := ne.apps.clusterCreateCalls()
	if len(calls) != 1 {
		t.Fatalf("CreateCluster calls %+v", calls)
	}
	c := calls[0]
	if c.ClusterID != *n.ClusterID || c.UserID != ne.systemUserID || c.AppID != ne.appID || c.VersionID == nil || *c.VersionID != ver ||
		c.Env["TENSOR_PARALLEL_SIZE"] != "2" || c.Env["PIPELINE_PARALLEL_SIZE"] != "1" || c.Env["MAX_NUM_SEQS"] != "16" ||
		c.Env["SERVED_MODEL_NAME"] != "qwen3-32b" || len(c.Env["API_KEY"]) < 32 {
		t.Fatalf("CreateCluster request %+v", c)
	}
	got := ne.node(n.ID)
	if got.AppClusterID == nil || got.DeploymentID == nil || *got.DeploymentID != ne.memberDeployment(*got.AppClusterID, n.Members[0].InstanceID) {
		t.Fatalf("node %+v", got)
	}
	if len(ne.apps.createCalls()) != 0 {
		t.Fatal("single-instance Create used for a group")
	}
	ne.tick()
	if len(ne.apps.clusterCreateCalls()) != 1 {
		t.Fatal("CreateCluster called again")
	}
}

// 上一轮 CreateCluster 成功但回写失败：认领集群上现有的集群部署。
func TestAdvanceGroupClaimsExisting(t *testing.T) {
	ne := newNodeEnv(t)
	f := ne.fabric("lan", "routed")
	ne.fabricMachine("A1", f, "10.1.0.0/24")
	ne.fabricMachine("A2", f, "10.1.0.0/24")
	m := ne.groupModel("qwen3-32b", 2, 1, ne.clusterVersion("1.1.0", 4, 360))
	ne.tick()
	n := ne.nodesOf(m.ID)[0]
	for _, mb := range n.Members {
		ne.setInstance(mb.InstanceID, rental.StateRunning)
	}
	existing, err := ne.apps.CreateCluster(ne.ctx, appdeploy.ClusterCreateRequest{UserID: ne.systemUserID, ClusterID: *n.ClusterID, AppID: ne.appID})
	if err != nil {
		t.Fatal(err)
	}
	ne.tick()
	if got := ne.node(n.ID); got.AppClusterID == nil || *got.AppClusterID != existing.ID || got.State != llm.NodeProvisioning {
		t.Fatalf("node %+v (existing %s)", got, existing.ID)
	}
}

// 集群部署 running/ok → 探测 head（rank0 实例）→ ready。
func TestAdvanceGroupPromotesViaHeadProbe(t *testing.T) {
	ne := newNodeEnv(t)
	_, n := ne.readyGroup(2)
	ms := ne.members(n.ID)
	if ne.probe.count() == 0 || !slices.Contains(ne.probe.targets, ms[0].InstanceID) || slices.Contains(ne.probe.targets, ms[1].InstanceID) {
		t.Fatalf("probe targets %v (head %s)", ne.probe.targets, ms[0].InstanceID)
	}
	if n.LastReadyAt == nil || n.Error != "" {
		t.Fatalf("node %+v", n)
	}
	if ev := ne.eventStates(n.ID); !slices.Equal(ev, []string{">provisioning", "provisioning>ready"}) {
		t.Fatalf("events %v", ev)
	}
	if err := llm.CheckInvariants(ne.ctx, ne.pool); err != nil {
		t.Fatal(err)
	}
}

// 开通中成员实例没了 → 整组 failed（不可重建）+ 释放全部成员。
func TestAdvanceGroupMemberGoneFails(t *testing.T) {
	ne := newNodeEnv(t)
	f := ne.fabric("lan", "routed")
	for _, s := range []string{"A1", "A2", "A3", "A4"} {
		ne.fabricMachine(s, f, "10.1.0.0/24")
	}
	m := ne.groupModel("qwen3-235b", 4, 1, ne.clusterVersion("1.1.0", 4, 360))
	ne.tick()
	n := ne.nodesOf(m.ID)[0]
	ne.setInstance(n.Members[2].InstanceID, rental.StateFailed)
	rep := ne.tick()
	got := ne.node(n.ID)
	if got.State != llm.NodeFailed || !strings.Contains(got.Error, "rank 2") || rep.Failed != 1 {
		t.Fatalf("node %+v report %+v", got, rep)
	}
	if rel := ne.rental.releasedClusters(); !slices.Equal(rel, []string{*n.ClusterID}) {
		t.Fatalf("released clusters %v", rel)
	}
	for _, mb := range ne.members(n.ID) {
		if mb.Live {
			t.Fatalf("member still live %+v", mb)
		}
	}
}

// 组的建节点超时 = 版本 timeout_minutes + 30 分钟。
func TestAdvanceGroupTimeout(t *testing.T) {
	if got := llm.ProvisionTimeoutFor(appdeploy.Version{Spec: appdeploy.Spec{TimeoutMinutes: 360}}); got != 390*time.Minute {
		t.Fatal(got)
	}
	if got := llm.ProvisionTimeoutFor(appdeploy.Version{}); got != time.Duration(appdeploy.DefaultTimeoutMinutes+30)*time.Minute {
		t.Fatal(got)
	}
	ne := newNodeEnv(t)
	f := ne.fabric("lan", "routed")
	ne.fabricMachine("A1", f, "10.1.0.0/24")
	ne.fabricMachine("A2", f, "10.1.0.0/24")
	m := ne.groupModel("qwen3-32b", 2, 1, ne.clusterVersion("1.1.0", 4, 360))
	ne.tick()
	n := ne.nodesOf(m.ID)[0]
	for _, mb := range n.Members {
		ne.setInstance(mb.InstanceID, rental.StateRunning)
	}
	ne.tick() // 集群部署 queued
	ne.clk.Advance(389 * time.Minute)
	ne.tick()
	if got := ne.node(n.ID); got.State != llm.NodeProvisioning {
		t.Fatalf("failed before timeout: %+v", got)
	}
	ne.clk.Advance(2 * time.Minute)
	ne.tick()
	got := ne.node(n.ID)
	if got.State != llm.NodeFailed || got.Error != "建节点超时（超过 390 分钟）" {
		t.Fatalf("node %+v", got)
	}
	if !slices.Contains(ne.rental.releasedClusters(), *n.ClusterID) {
		t.Fatal("timed-out group not released")
	}
}

// ---- unhealthy ----

// 成员实例没了 → Prober 立即 unhealthy → Controller 整组 failed、ReleaseSystemCluster 一次、成员 live = false。
func TestGroupMemberGoneFailsWholeGroup(t *testing.T) {
	ne := newNodeEnv(t)
	_, n := ne.readyGroup(2)
	ms := ne.members(n.ID)
	ne.setInstance(ms[1].InstanceID, rental.StateReleased)
	ne.probeTick()
	if got := ne.node(n.ID); got.State != llm.NodeUnhealthy {
		t.Fatalf("after probe %+v", got)
	}
	ne.tick()
	got := ne.node(n.ID)
	if got.State != llm.NodeFailed || !strings.Contains(got.Error, "rank 1") {
		t.Fatalf("node %+v", got)
	}
	ne.tick()
	if rel := ne.rental.releasedClusters(); !slices.Equal(rel, []string{*n.ClusterID}) {
		t.Fatalf("released clusters %v", rel)
	}
	for _, mb := range ne.members(n.ID) {
		if mb.Live {
			t.Fatalf("member still live %+v", mb)
		}
	}
	if err := llm.CheckInvariants(ne.ctx, ne.pool); err != nil {
		t.Fatal(err)
	}
}

func TestGroupMachineFaultyFails(t *testing.T) {
	ne := newNodeEnv(t)
	_, n := ne.readyGroup(2)
	ms := ne.members(n.ID)
	ne.exec(`UPDATE llm_nodes SET state = 'unhealthy', unhealthy_since = $2 WHERE id = $1`, n.ID, ne.clk.Now())
	ne.exec(`UPDATE machines SET state = 'faulty' WHERE id = $1`, ms[1].MachineID)
	ne.tick()
	if got := ne.node(n.ID); got.State != llm.NodeFailed || !strings.Contains(got.Error, "机器故障") {
		t.Fatalf("node %+v", got)
	}
	if !slices.Contains(ne.rental.releasedClusters(), *n.ClusterID) {
		t.Fatal("group not released")
	}
}

// 成员失联（实例暂停）：Prober 计失败 → unhealthy；9 分钟仍 unhealthy（不重新部署），10 分钟 failed。
func TestGroupSuspendedTenMinutesFails(t *testing.T) {
	ne := newNodeEnv(t)
	_, n := ne.readyGroup(2)
	ms := ne.members(n.ID)
	ne.suspend(ms[1].InstanceID)
	for i := 0; i < llm.ProbeFailThreshold; i++ {
		ne.probeTick()
	}
	if got := ne.node(n.ID); got.State != llm.NodeUnhealthy {
		t.Fatalf("after %d probes %+v", llm.ProbeFailThreshold, got)
	}
	ne.clk.Advance(9 * time.Minute)
	ne.tick()
	if got := ne.node(n.ID); got.State != llm.NodeUnhealthy || len(ne.apps.clusterRedeployCalls()) != 0 {
		t.Fatalf("at 9 minutes %+v redeploys %d", got, len(ne.apps.clusterRedeployCalls()))
	}
	ne.clk.Advance(time.Minute)
	ne.tick()
	if got := ne.node(n.ID); got.State != llm.NodeFailed || got.Error != "成员机器失联" {
		t.Fatalf("at 10 minutes %+v", got)
	}
	if !slices.Contains(ne.rental.releasedClusters(), *n.ClusterID) {
		t.Fatal("group not released")
	}
}

// 成员进程崩溃（成员部署 failed）→ 立即 unhealthy；2 分钟后整组 RedeployCluster（rebuild + 1）；3 次后 failed + 严重告警。
func TestGroupCrashRedeploysAll(t *testing.T) {
	ne := newNodeEnv(t)
	_, n := ne.readyGroup(2)
	ms := ne.members(n.ID)
	worker := ne.memberDeployment(*n.AppClusterID, ms[1].InstanceID)
	ne.setDeployment(worker, appdeploy.StateFailed, appdeploy.HealthUnknown, "")
	ne.probeTick()
	if got := ne.node(n.ID); got.State != llm.NodeUnhealthy {
		t.Fatalf("after crash %+v", got)
	}
	ne.clk.Advance(GroupRebuildAfterMinus())
	ne.tick()
	if len(ne.apps.clusterRedeployCalls()) != 0 {
		t.Fatal("redeployed before GroupRebuildAfter")
	}
	for k := 1; k <= llm.MaxRebuilds; k++ {
		ne.clk.Advance(time.Minute + time.Second)
		ne.tick()
		got := ne.node(n.ID)
		if got.State != llm.NodeProvisioning || got.RebuildCount != k || len(ne.apps.clusterRedeployCalls()) != k {
			t.Fatalf("rebuild %d: %+v redeploys %d", k, got, len(ne.apps.clusterRedeployCalls()))
		}
		c := ne.apps.clusterRedeployCalls()[k-1]
		if c.ClusterDeploymentID != *n.AppClusterID || c.UserID != ne.systemUserID || c.EnvSet {
			t.Fatalf("redeploy request %+v", c)
		}
		ne.setDeployment(worker, appdeploy.StateFailed, appdeploy.HealthUnknown, "")
		ne.tick() // 重建后仍失败 → unhealthy（rebuild_count > 0）
		if got := ne.node(n.ID); got.State != llm.NodeUnhealthy {
			t.Fatalf("rebuild %d failed again: %+v", k, got)
		}
		ne.clk.Advance(time.Minute)
	}
	ne.clk.Advance(time.Minute + time.Second)
	ne.tick()
	got := ne.node(n.ID)
	if got.State != llm.NodeFailed || got.RebuildCount != llm.MaxRebuilds {
		t.Fatalf("after %d rebuilds %+v", llm.MaxRebuilds, got)
	}
	if c := ne.queryInt(`SELECT count(*) FROM alerts WHERE kind = $1 AND level = 'critical'`, llm.AlertNodeFailed); c != 1 {
		t.Fatalf("critical alerts %d", c)
	}
	if !slices.Contains(ne.rental.releasedClusters(), *n.ClusterID) {
		t.Fatal("group not released")
	}
}

// GroupRebuildAfterMinus 比 GroupRebuildAfter 少一分钟。
func GroupRebuildAfterMinus() time.Duration { return llm.GroupRebuildAfter - time.Minute }

// ---- 排空 / 移除 / 孤儿 ----

func TestDrainGroupReleasesAll(t *testing.T) {
	ne := newNodeEnv(t)
	m, n := ne.readyGroup(2)
	if _, err := ne.svc.SetModelStatus(ne.ctx, ne.admin(), m.ID, llm.ModelOffline); err != nil {
		t.Fatal(err)
	}
	ne.tick()
	if got := ne.node(n.ID); got.State != llm.NodeDraining {
		t.Fatalf("node %+v", got)
	}
	ne.clk.Advance(llm.DrainTimeout)
	ne.tick()
	if got := ne.node(n.ID); got.State != llm.NodeRemoving {
		t.Fatalf("node %+v", got)
	}
	if rel := ne.rental.releasedClusters(); !slices.Equal(rel, []string{*n.ClusterID}) {
		t.Fatalf("released clusters %v", rel)
	}
	if len(ne.rental.releasedIDs()) != 0 {
		t.Fatal("ReleaseSystem used for a group")
	}
	for _, mb := range ne.members(n.ID) {
		if i, _ := ne.rental.Get(ne.ctx, mb.InstanceID); i.State != rental.StateWiping {
			t.Fatalf("member %d instance %s", mb.Rank, i.State)
		}
	}
}

func TestRemovingGroupDoneWhenAllEnded(t *testing.T) {
	ne := newNodeEnv(t)
	_, n := ne.readyGroup(2)
	if _, err := ne.svc.AdminRemoveNode(ne.ctx, ne.admin(), n.ID, "测试"); err != nil {
		t.Fatal(err)
	}
	ms := ne.members(n.ID)
	ne.setInstance(ms[0].InstanceID, rental.StateReleased) // rank1 仍在洗机
	ne.tick()
	if got := ne.node(n.ID); got.State != llm.NodeRemoving {
		t.Fatalf("one member still wiping: %+v", got)
	}
	ne.setInstance(ms[1].InstanceID, rental.StateReleased)
	rep := ne.tick()
	if got := ne.node(n.ID); got.State != llm.NodeRemoved || rep.Removed != 1 {
		t.Fatalf("node %+v report %+v", got, rep)
	}
	for _, mb := range ne.members(n.ID) {
		if mb.Live {
			t.Fatalf("member still live %+v", mb)
		}
	}
	// 移除中成员实例被别处复活（仍在位）→ 重试整组释放
	_, n2 := ne.readyGroup2()
	if _, err := ne.svc.AdminRemoveNode(ne.ctx, ne.admin(), n2.ID, "测试"); err != nil {
		t.Fatal(err)
	}
	before := len(ne.rental.releasedClusters())
	ne.setInstance(ne.members(n2.ID)[1].InstanceID, rental.StateRunning)
	ne.tick()
	if len(ne.rental.releasedClusters()) != before+1 {
		t.Fatal("removing group with a live member did not retry ReleaseSystemCluster")
	}
}

// readyGroup2 在同一环境里再建一组（新 fabric 4 台、新模型；第一组的模型若在补副本，可能先占走其中 2 台）。
func (ne *nodeEnv) readyGroup2() (llm.Model, llm.Node) {
	ne.t.Helper()
	f := ne.fabric("lan", "routed")
	for _, s := range []string{"H1", "H2", "H3", "H4"} {
		ne.fabricMachine(s, f, "10.9.0.0/24")
	}
	m := ne.groupModel("second", 2, 1, ne.clusterVersion("1.2.0", 4, 360))
	ne.tick()
	var id string
	for _, r := range ne.nodesOf(m.ID) {
		if r.State == llm.NodeProvisioning {
			id = r.ID
		}
	}
	if id == "" {
		ne.t.Fatal("second group not provisioned")
	}
	return m, ne.bringGroupReady(id)
}

// 组成员的系统实例不是孤儿；节点终态（成员 live = false）后仍未释放的成员实例按孤儿释放。
func TestOrphansIgnoreGroupMembers(t *testing.T) {
	ne := newNodeEnv(t)
	f := ne.fabric("lan", "routed")
	ne.fabricMachine("A1", f, "10.1.0.0/24")
	ne.fabricMachine("A2", f, "10.1.0.0/24")
	m := ne.groupModel("qwen3-32b", 2, 1, ne.clusterVersion("1.1.0", 4, 360))
	ne.tick()
	n := ne.nodesOf(m.ID)[0]
	ne.clk.Advance(5 * time.Minute)
	if rep := ne.tick(); rep.Orphans != 0 || len(ne.rental.releasedIDs()) != 0 {
		t.Fatalf("group members treated as orphans: %+v %v", rep, ne.rental.releasedIDs())
	}
	ne.exec(`UPDATE llm_nodes SET state = 'failed' WHERE id = $1`, n.ID)
	ne.exec(`UPDATE llm_node_members SET live = false WHERE node_id = $1`, n.ID)
	ne.exec(`UPDATE llm_models SET replicas = 0, status = 'offline' WHERE id = $1`, m.ID)
	rep := ne.tick()
	if rep.Orphans != 2 {
		t.Fatalf("report %+v", rep)
	}
}

// ---- Prober ----

func TestProberGroupMemberSuspendedCountsFailure(t *testing.T) {
	ne := newNodeEnv(t)
	_, n := ne.readyGroup(2)
	calls := ne.probe.count()
	ne.suspend(ne.members(n.ID)[1].InstanceID)
	ne.probeTick()
	got := ne.node(n.ID)
	if got.State != llm.NodeReady || got.ProbeFailCount != 1 || !strings.Contains(got.Error, "rank 1") {
		t.Fatalf("after one probe %+v", got)
	}
	for i := 1; i < llm.ProbeFailThreshold; i++ {
		ne.probeTick()
	}
	if got := ne.node(n.ID); got.State != llm.NodeUnhealthy {
		t.Fatalf("after threshold %+v", got)
	}
	if ne.probe.count() != calls {
		t.Fatal("head probed although a member is suspended")
	}
	if ne.alertCount(llm.AlertNodeUnhealthy) != 1 {
		t.Fatal("unhealthy alert missing")
	}
}

func TestProberGroupDeploymentFailedImmediate(t *testing.T) {
	ne := newNodeEnv(t)
	_, n := ne.readyGroup(4)
	ms := ne.members(n.ID)
	ne.setDeployment(ne.memberDeployment(*n.AppClusterID, ms[3].InstanceID), appdeploy.StateFailed, appdeploy.HealthUnknown, appdeploy.ErrCodeDisk)
	ne.probeTick()
	got := ne.node(n.ID)
	if got.State != llm.NodeUnhealthy || !strings.Contains(got.Error, "rank 3") {
		t.Fatalf("node %+v", got)
	}
	// 成员部署不健康（进程在、健康检查失败）→ 计一次失败
	ne.setDeployment(ne.memberDeployment(*n.AppClusterID, ms[3].InstanceID), appdeploy.StateRunning, appdeploy.HealthOK, "")
	ne.probeTick()
	if got := ne.node(n.ID); got.State != llm.NodeReady {
		t.Fatalf("recovered %+v", got)
	}
	ne.setDeployment(ne.memberDeployment(*n.AppClusterID, ms[2].InstanceID), appdeploy.StateRunning, appdeploy.HealthUnhealthy, "")
	ne.probeTick()
	if got := ne.node(n.ID); got.State != llm.NodeReady || got.ProbeFailCount != 1 {
		t.Fatalf("unhealthy member %+v", got)
	}
}

// ---- Router ----

func TestRouterExcludesGroupWithSuspendedMember(t *testing.T) {
	ne := newNodeEnv(t)
	m, n := ne.readyGroup(2)
	ms := ne.members(n.ID)
	r := ne.svc.Router()
	pick := func() (llm.RouteNode, error) {
		t.Helper()
		if err := r.Refresh(ne.ctx); err != nil {
			t.Fatal(err)
		}
		return r.Pick(m.ID, 1, nil)
	}
	if rn, err := pick(); err != nil || rn.ID != n.ID || rn.Target.InstanceID != ms[0].InstanceID {
		t.Fatalf("pick %+v %v", rn, err)
	}
	ne.suspend(ms[1].InstanceID)
	if _, err := pick(); !errors.Is(err, llm.ErrNoNode) {
		t.Fatalf("suspended worker: %v", err)
	}
	ne.exec(`UPDATE instances SET suspended_at = NULL WHERE id = $1`, ms[1].InstanceID)
	ne.setInstance(ms[1].InstanceID, rental.StateStopped)
	if _, err := pick(); !errors.Is(err, llm.ErrNoNode) {
		t.Fatalf("stopped worker: %v", err)
	}
	ne.setInstance(ms[1].InstanceID, rental.StateRunning)
	if rn, err := pick(); err != nil || rn.ID != n.ID {
		t.Fatalf("recovered pick %+v %v", rn, err)
	}
}

// ---- 后台 ----

func TestAdminRebuildGroup(t *testing.T) {
	ne := newNodeEnv(t)
	_, n := ne.readyGroup(2)
	got, err := ne.svc.AdminRebuildNode(ne.ctx, ne.admin(), n.ID)
	if err != nil || got.State != llm.NodeProvisioning || got.RebuildCount != 0 {
		t.Fatalf("rebuild %+v %v", got, err)
	}
	if calls := ne.apps.clusterRedeployCalls(); len(calls) != 1 || calls[0].ClusterDeploymentID != *n.AppClusterID {
		t.Fatalf("redeploy calls %+v", calls)
	}
	if len(ne.apps.redeployCalls()) != 0 {
		t.Fatal("single Redeploy used for a group")
	}
	if ne.auditCount("llm.node.rebuild", n.ID) != 1 {
		t.Fatal("audit missing")
	}
	ne.exec(`UPDATE llm_nodes SET state = 'failed' WHERE id = $1`, n.ID)
	if _, err := ne.svc.AdminRebuildNode(ne.ctx, ne.admin(), n.ID); !errors.Is(err, apperr.ErrLLMNodeInvalidState) {
		t.Fatalf("rebuild failed group: %v", err)
	}
	if len(ne.rental.clusterPlaceCalls()) != 1 {
		t.Fatal("failed group recreated by admin rebuild")
	}
}

func TestAdminRemoveGroup(t *testing.T) {
	ne := newNodeEnv(t)
	_, n := ne.readyGroup(2)
	got, err := ne.svc.AdminRemoveNode(ne.ctx, ne.admin(), n.ID, "下线")
	if err != nil || got.State != llm.NodeRemoving {
		t.Fatalf("remove %+v %v", got, err)
	}
	if rel := ne.rental.releasedClusters(); !slices.Equal(rel, []string{*n.ClusterID}) {
		t.Fatalf("released clusters %v", rel)
	}
	// 排空同样整组（Controller 释放）
	_, n2 := ne.readyGroup2()
	if _, err := ne.svc.AdminDrainNode(ne.ctx, ne.admin(), n2.ID); err != nil {
		t.Fatal(err)
	}
	ne.clk.Advance(llm.DrainTimeout)
	ne.tick()
	if !slices.Contains(ne.rental.releasedClusters(), *n2.ClusterID) {
		t.Fatal("drained group not released")
	}
}

func TestPoolViewFabrics(t *testing.T) {
	ne := newNodeEnv(t)
	fa, fd := ne.fabric("lan", "routed"), ne.fabric("cx7", "direct")
	ne.exec(`UPDATE cluster_fabrics SET rdma = true WHERE id = $1`, fd)
	var as []int64
	for _, s := range []string{"A1", "A2", "A3", "A4", "A5", "A6"} {
		as = append(as, ne.fabricMachine(s, fa, "10.1.0.0/24"))
	}
	ne.fabricMachine("D1", fd, "l2")
	ne.fabricMachine("D2", fd, "l2")
	plain := ne.poolMachine("P1")
	m := ne.groupModel("qwen3-32b", 2, 1, ne.clusterVersion("1.1.0", 4, 360))
	ne.tick() // 组落在最贴合的 direct fabric（2 台）
	n := ne.nodesOf(m.ID)[0]
	pv, err := ne.svc.PoolView(ne.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pv.Total != 9 || pv.Free != 7 || len(pv.Fabrics) != 2 {
		t.Fatalf("pool %+v", pv)
	}
	a, d := pv.Fabrics[0], pv.Fabrics[1]
	if a.FabricID != fa || a.Kind != "lan" || a.RDMA || a.Total != 6 || a.Free != 6 || a.Groups["2"] != 3 || a.Groups["4"] != 1 {
		t.Fatalf("fabric a %+v", a)
	}
	if d.FabricID != fd || d.Kind != "cx7" || !d.RDMA || d.Total != 2 || d.Free != 0 || d.Groups["2"] != 0 || d.Groups["4"] != 0 {
		t.Fatalf("fabric d %+v", d)
	}
	byID := map[int64]llm.PoolMachine{}
	for _, pm := range pv.Machines {
		byID[pm.Machine.ID] = pm
	}
	for rank, mb := range ne.members(n.ID) {
		pm := byID[mb.MachineID]
		if pm.Node == nil || pm.Node.ID != n.ID || pm.Node.Rank != rank || pm.Node.Size != 2 || pm.FabricID == nil || *pm.FabricID != fd {
			t.Fatalf("member machine %+v node %+v", pm, pm.Node)
		}
	}
	if pm := byID[plain]; pm.FabricID != nil || pm.FabricName != "" || pm.Node != nil {
		t.Fatalf("plain machine %+v", pm)
	}
	if pm := byID[as[0]]; pm.FabricName == "" || *pm.FabricID != fa {
		t.Fatalf("fabric machine %+v", pm)
	}
}

func TestMachineHasLiveNodeMember(t *testing.T) {
	ne := newNodeEnv(t)
	_, n := ne.readyGroup(2)
	worker := ne.members(n.ID)[1]
	if busy, err := ne.svc.MachineHasLiveNode(ne.ctx, worker.MachineID); err != nil || !busy {
		t.Fatalf("worker machine busy=%v %v", busy, err)
	}
	if _, err := ne.svc.AdminRemoveNode(ne.ctx, ne.admin(), n.ID, "下线"); err != nil {
		t.Fatal(err)
	}
	for _, mb := range ne.members(n.ID) {
		ne.setInstance(mb.InstanceID, rental.StateReleased)
	}
	ne.tick()
	if busy, err := ne.svc.MachineHasLiveNode(ne.ctx, worker.MachineID); err != nil || busy {
		t.Fatalf("after removal busy=%v %v", busy, err)
	}
}

// 节点列表 / 详情：组节点带成员与集群部署聚合。
func TestNodeRowsGroupMembers(t *testing.T) {
	ne := newNodeEnv(t)
	_, n := ne.readyGroup(2)
	d, err := ne.svc.GetNode(ne.ctx, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	row := d.Node
	if row.Size != 2 || len(row.Members) != 2 || row.AppCluster == nil || row.AppCluster.Status != appdeploy.ClusterStatusRunning ||
		row.AppCluster.Health != appdeploy.HealthOK || row.Members[1].Deployment == nil || row.Members[1].Deployment.State != appdeploy.StateRunning ||
		row.Members[1].InstanceState != rental.StateRunning || row.Members[0].MachineName == "" {
		t.Fatalf("row %+v members %+v app cluster %+v", row.Node, row.Members, row.AppCluster)
	}
}

// ---- 不变量 ----

func TestInvariantsGroup(t *testing.T) {
	ne := newNodeEnv(t)
	_, n := ne.readyGroup(2)
	check := func(want string) {
		t.Helper()
		err := llm.CheckInvariants(ne.ctx, ne.pool)
		if want == "" {
			if err != nil {
				t.Fatalf("unexpected: %v", err)
			}
			return
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("want %q, got %v", want, err)
		}
	}
	check("")
	ms := ne.members(n.ID)
	// ⑧ 成员缺一行
	ne.exec(`UPDATE llm_node_members SET live = false WHERE node_id = $1 AND rank = 1`, n.ID)
	check("invariant 8:")
	ne.exec(`UPDATE llm_node_members SET live = true WHERE node_id = $1 AND rank = 1`, n.ID)
	check("")
	// ⑧ rank 不连续
	ne.exec(`UPDATE llm_node_members SET rank = 2 WHERE node_id = $1 AND rank = 1`, n.ID)
	check("invariant 8:")
	ne.exec(`UPDATE llm_node_members SET rank = 1 WHERE node_id = $1 AND rank = 2`, n.ID)
	check("")
	// ⑧ 成员实例不属于节点的集群
	ne.exec(`UPDATE instances SET cluster_id = NULL, cluster_rank = NULL WHERE id = $1`, ms[1].InstanceID)
	check("invariant 8:")
	ne.exec(`UPDATE instances SET cluster_id = $2, cluster_rank = 1 WHERE id = $1`, ms[1].InstanceID, *n.ClusterID)
	check("")
	// ⑩ ready 的组：成员部署不在 running
	dep := ne.memberDeployment(*n.AppClusterID, ms[1].InstanceID)
	ne.setDeployment(dep, appdeploy.StateInstalling, appdeploy.HealthUnknown, "")
	check("invariant 10:")
	ne.setDeployment(dep, appdeploy.StateRunning, appdeploy.HealthOK, "")
	check("")
	// ⑨ 终态仍 live
	ne.exec(`UPDATE llm_nodes SET state = 'failed' WHERE id = $1`, n.ID)
	check("invariant 9:")
}

// RedeployCluster 返回 app_invalid_state（有成员部署正在安装 / 卸载）是暂时性的：保持 unhealthy、不计重建、不告警，下一轮再试。
func TestGroupRedeployInvalidStateRetries(t *testing.T) {
	ne := newNodeEnv(t)
	_, n := ne.readyGroup(2)
	ne.setDeployment(ne.memberDeployment(*n.AppClusterID, ne.members(n.ID)[1].InstanceID), appdeploy.StateFailed, appdeploy.HealthUnknown, "")
	ne.probeTick()
	ne.apps.clusterRedeployErr = apperr.ErrAppInvalidState
	ne.clk.Advance(llm.GroupRebuildAfter)
	ne.tick()
	if got := ne.node(n.ID); got.State != llm.NodeUnhealthy || got.RebuildCount != 0 || ne.alertCount(llm.AlertNodeFailed) != 0 {
		t.Fatalf("after invalid_state %+v alerts %d", got, ne.alertCount(llm.AlertNodeFailed))
	}
	ne.apps.mu.Lock()
	ne.apps.clusterRedeployErr = nil
	ne.apps.mu.Unlock()
	ne.tick()
	if got := ne.node(n.ID); got.State != llm.NodeProvisioning || got.RebuildCount != 1 {
		t.Fatalf("retry %+v", got)
	}
}
