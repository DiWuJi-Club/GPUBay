package app_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/e2ehooks"
	"spark-cloud/internal/llm"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/rental"
)

// Token 商城多机节点组的集成测试（规格 §16.2 第 4 条）：池内同一 lan fabric 的 2 台 → 模型 nodes_per_replica = 2 上线 →
// Controller：PlaceSystemCluster → fake Agent 完成两条 Create → CreateCluster(vllm-node) → Runner 推进两条成员部署 →
// head 上的 FakeVLLM（/hooks/llm/upstream）探测通过 → ready → 网关请求计费。环境与 cluster_e2e_test 相同（cluEnv）。

// llmGroupModel 后台服务直接建模型 qwen3-test（每副本 n 台）并上线。
func (e *cluEnv) llmGroupModel(replicas, n int) llm.Model {
	e.t.Helper()
	m, err := e.a.LLM.CreateModel(e.ctx, audit.Actor{}, llm.ModelInput{Slug: llmSlug, Name: "Qwen3 多机测试", Kind: llm.KindChat,
		SourceID: "Qwen/Qwen3-32B", ImageID: dbtest.ImageID(e.t, e.pool), ContextLen: 32768, InputPrice: llmInputPrice, OutputPrice: llmOutputPrice,
		Replicas: replicas, NodesPerReplica: n})
	if err != nil {
		e.t.Fatal(err)
	}
	if m, err = e.a.LLM.SetModelStatus(e.ctx, audit.Actor{}, m.ID, llm.ModelOnline); err != nil {
		e.t.Fatal(err)
	}
	e.hook("/hooks/llm/upstream", nil) // head 的 8000 端口：FakeVLLM（served = 全部 online 模型）
	return m
}

func (e *cluEnv) poolID() int64 {
	e.t.Helper()
	id, err := e.a.Fleet.LLMPoolGroupID(e.ctx, e.pool)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

// liveGroupNodes 模型的非终态节点（按 ID 升序）。
func (e *cluEnv) liveGroupNodes(modelID int64) []llm.Node {
	e.t.Helper()
	rows, err := e.a.LLM.ListNodes(e.ctx, llm.NodeFilter{ModelID: modelID})
	if err != nil {
		e.t.Fatal(err)
	}
	var out []llm.Node
	for _, r := range rows {
		if r.State != llm.NodeRemoved && r.State != llm.NodeFailed {
			out = append(out, r.Node)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// sweepExcept 模拟机器 lost 失联：其余机器直接心跳，再跑一轮不经 fake Agent 心跳的清扫（/hooks/sweep {heartbeat: false}）。
func (e *cluEnv) sweepExcept(lost int64) {
	e.t.Helper()
	rows, err := e.pool.Query(e.ctx, `SELECT id FROM machines WHERE id <> $1 ORDER BY id`, lost)
	if err != nil {
		e.t.Fatal(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		e.t.Fatal(err)
	}
	for _, id := range ids {
		if _, err := e.a.Fleet.Heartbeat(e.ctx, id); err != nil {
			e.t.Fatal(err)
		}
	}
	e.hook("/hooks/sweep", map[string]any{"heartbeat": false})
}

// nodeEvents 节点的状态变化记录（失败时打印）。
func (e *cluEnv) nodeEvents(id string) string {
	e.t.Helper()
	rows, err := e.pool.Query(e.ctx, `SELECT at, from_state, to_state, detail FROM llm_node_events WHERE node_id = $1 ORDER BY id`, id)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var at time.Time
		var from, to, detail string
		if err := rows.Scan(&at, &from, &to, &detail); err != nil {
			e.t.Fatal(err)
		}
		fmt.Fprintf(&b, "\n  %s %s → %s %s", at.Format("15:04:05"), from, to, detail)
	}
	return b.String()
}

func (e *cluEnv) llmNode(id string) llm.Node {
	e.t.Helper()
	d, err := e.a.LLM.GetNode(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return d.Node.Node
}

// untilGroupsReady 逐轮推进（Controller + 探测 → fake Agent → 应用 Runner）直到模型恰有 want 个 ready 的组节点。
func (e *cluEnv) untilGroupsReady(modelID int64, want int) []llm.Node {
	e.t.Helper()
	var nodes []llm.Node
	for round := 0; round < 60; round++ {
		e.llmTick()
		e.agents()
		nodes = e.liveGroupNodes(modelID)
		ready := 0
		for _, n := range nodes {
			if n.State == llm.NodeReady {
				ready++
			}
		}
		if ready == want && len(nodes) == want {
			return nodes
		}
		e.appsTick(1)
	}
	e.t.Fatalf("group nodes not ready: %+v", nodes)
	return nil
}

// groupMembers 组节点的成员实例（按 rank）。
func (e *cluEnv) groupMembers(n llm.Node) []rental.Instance {
	e.t.Helper()
	rows, err := e.pool.Query(e.ctx, `SELECT instance_id FROM llm_node_members WHERE node_id = $1 ORDER BY rank`, n.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			e.t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	var out []rental.Instance
	for _, id := range ids {
		out = append(out, e.inst(id))
	}
	return out
}

// llmKey 一个余额充足的用户及其 API Key。
func (e *cluEnv) llmKey() (int64, string) {
	e.t.Helper()
	c, uid := e.user(1_000_000)
	out := c.do("POST", "/llm/keys", map[string]any{"name": "e2e"})
	return uid, out["key"].(string)
}

// v1Chat POST /v1/chat/completions（非流式）。
func (e *cluEnv) v1Chat(key string) (int, string) {
	e.t.Helper()
	b, _ := json.Marshal(map[string]any{"model": llmSlug, "messages": []map[string]string{{"role": "user", "content": "你好"}}})
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(rb)
}

// waitUsageRows 等用户 status = ok 的 llm_usage 行数达到 n（网关在写完响应之后记账）。
func (e *cluEnv) waitUsageRows(uid int64, n int) {
	e.t.Helper()
	var got int
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		e.scalar(&got, `SELECT count(*) FROM llm_usage WHERE user_id = $1 AND status = 'ok' AND cost_micro > 0`, uid)
		if got >= n {
			break
		}
	}
	if got != n {
		e.t.Fatalf("usage rows = %d, want %d", got, n)
	}
}

func TestLLMTwoNodeGroupE2E(t *testing.T) {
	e := newCluEnv(t)
	pool := e.poolID()
	fab := e.fabric("lan", nil)
	mids := e.machines(2, fab, e2ehooks.NetLAN, map[string]any{"group_id": pool})
	m := e.llmGroupModel(1, 2)
	if m.NodesPerReplica != 2 || m.TensorParallel != 2 || m.PipelineParallel != 1 {
		t.Fatalf("model = %+v", m)
	}

	nodes := e.untilGroupsReady(m.ID, 1)
	n := nodes[0]
	if n.Size != 2 || n.ClusterID == nil || n.AppClusterID == nil || n.InstanceID == nil || n.DeploymentID == nil {
		t.Fatalf("group node = %+v", n)
	}
	mem := e.groupMembers(n)
	if len(mem) != 2 || mem[0].ID != *n.InstanceID || !mem[0].System || !mem[1].System || *mem[0].ClusterID != *n.ClusterID || *mem[1].ClusterRank != 1 {
		t.Fatalf("group members = %+v", mem)
	}
	cd, err := e.a.Apps.GetClusterDeployment(e.ctx, *n.AppClusterID)
	if err != nil || cd.Status != appdeploy.ClusterStatusRunning || len(cd.Members) != 2 {
		t.Fatalf("cluster deployment = %+v %v", cd, err)
	}
	// 多机节点的保留变量：TP = 2、PP = 1（N = 1 的节点不填，见 TestPlatformNodeEnvSingleUnchanged）
	for _, cm := range cd.Members {
		if env := cm.Deployment.Env; env["TENSOR_PARALLEL_SIZE"] != "2" || env["PIPELINE_PARALLEL_SIZE"] != "1" || env["MODEL_ID"] != "Qwen/Qwen3-32B" {
			t.Fatalf("rank %d env = %v", cm.Rank, env)
		}
	}

	uid, key := e.llmKey()
	if code, body := e.v1Chat(key); code != 200 || !strings.Contains(body, "你好") {
		t.Fatalf("chat via group node = %d %s", code, body)
	}
	e.waitUsageRows(uid, 1)

	// rank1 的机器失联（它的 fake Agent 不再心跳 / 上报）→ 清扫判失联、成员暂停 → Router 不再收这组、组 unhealthy →
	// GroupSuspendFailAfter（10 分钟）后整组 failed、释放全部成员；池里凑不出同 fabric 的空闲 2 台 → llm_pool_short
	lost := mem[1].MachineID
	e.hook("/hooks/clock/advance", map[string]any{"seconds": int((rental.OfflineTimeout + time.Minute) / time.Second)})
	e.sweepExcept(lost) // 其余机器心跳；rank1 判失联 → 成员实例暂停
	if inst := e.inst(mem[1].ID); inst.SuspendedAt == nil || inst.State != rental.StateRunning {
		t.Fatalf("lost member = %+v", inst)
	}
	e.llmTick() // 探测：成员暂停 → 不健康；路由表刷新后排除成员暂停的组
	if code, body := e.v1Chat(key); code == 200 {
		t.Fatalf("router still uses the group with a suspended member: %s", body)
	}
	for i := 0; i < llm.ProbeFailThreshold && e.llmNode(n.ID).State != llm.NodeUnhealthy; i++ {
		e.llmTick()
	}
	if got := e.llmNode(n.ID); got.State != llm.NodeUnhealthy {
		t.Fatalf("group with suspended member = %+v%s", got, e.nodeEvents(n.ID))
	}
	e.hook("/hooks/clock/advance", map[string]any{"seconds": int((llm.GroupSuspendFailAfter + time.Minute) / time.Second)})
	e.sweepExcept(lost)
	e.llmTick()
	if got := e.llmNode(n.ID); got.State != llm.NodeFailed {
		t.Fatalf("group after member lost 10 min = %+v%s", got, e.nodeEvents(n.ID))
	}
	e.agentOn(mids[0]) // 只推进 rank0（失联的 rank1 不执行、不上报）
	if st := e.inst(mem[0].ID).State; st != rental.StateReleased {
		t.Fatalf("rank0 after group failed = %s", st)
	}
	if st := e.inst(mem[1].ID).State; st != rental.StateWiping {
		t.Fatalf("lost rank1 after group failed = %s", st)
	}
	e.llmTick()
	if live := e.liveGroupNodes(m.ID); len(live) != 0 {
		t.Fatalf("unexpected group nodes with one usable machine: %+v", live)
	}
	if e.alerts(llm.AlertPoolShort) == 0 {
		t.Fatal("no llm_pool_short alert")
	}

	// 补一台同 fabric 的池机器（失联的机器在下一轮 /hooks/agent 时恢复上报、洗机完成）→ 新组 ready
	e.machines(1, fab, e2ehooks.NetLAN, map[string]any{"group_id": pool})
	nodes = e.untilGroupsReady(m.ID, 1)
	if nodes[0].ID == n.ID || nodes[0].Size != 2 {
		t.Fatalf("expected a new group node, got %+v", nodes[0])
	}
	if st := e.inst(mem[1].ID).State; st != rental.StateReleased {
		t.Fatalf("lost rank1 after reconnect = %s", st)
	}
	if code, body := e.v1Chat(key); code != 200 {
		t.Fatalf("chat via new group = %d %s", code, body)
	}
	e.waitUsageRows(uid, 2)
}

// 成员进程崩溃（规格 §16.2 第 4 条后半）：杀掉 worker 的服务进程 → 成员部署 crashed、集群部署聚合 failed → 组 unhealthy →
// GroupRebuildAfter（2 分钟）后整组 RedeployCluster（rebuild_count = 1，同一节点、同一批机器）→ 重新 ready。
func TestLLMGroupCrashRebuildE2E(t *testing.T) {
	e := newCluEnv(t)
	pool := e.poolID()
	fab := e.fabric("lan", nil)
	e.machines(2, fab, e2ehooks.NetLAN, map[string]any{"group_id": pool})
	m := e.llmGroupModel(1, 2)
	n := e.untilGroupsReady(m.ID, 1)[0]
	mem := e.groupMembers(n)

	pidFile := filepath.Join(e.root, mem[1].ID, "root", "data", appdeploy.AppDirName, "vllm-node", "service.pid")
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill worker service %d: %v", pid, err)
	}

	advanced := false
	var got llm.Node
	for round := 0; round < 120; round++ {
		e.appsTick(1)
		e.llmTick()
		e.agents()
		got = e.llmNode(n.ID)
		if got.State == llm.NodeUnhealthy && !advanced {
			e.hook("/hooks/clock/advance", map[string]any{"seconds": int((llm.GroupRebuildAfter + 10*time.Second) / time.Second)})
			advanced = true
		}
		if got.RebuildCount == 1 && got.State == llm.NodeReady {
			break
		}
		if got.State == llm.NodeFailed {
			t.Fatalf("group failed instead of rebuilding: %+v%s", got, e.nodeEvents(n.ID))
		}
	}
	if !advanced || got.RebuildCount != 1 || got.State != llm.NodeReady {
		t.Fatalf("group after worker crash = %+v (went unhealthy: %v)", got, advanced)
	}
	after := e.groupMembers(got)
	if after[0].ID != mem[0].ID || after[1].ID != mem[1].ID {
		t.Fatalf("rebuild changed members: %v → %v", mem, after)
	}
	cd, err := e.a.Apps.GetClusterDeployment(e.ctx, *got.AppClusterID)
	if err != nil || cd.Status != appdeploy.ClusterStatusRunning {
		t.Fatalf("cluster deployment after rebuild = %+v %v", cd, err)
	}
	_, key := e.llmKey()
	if code, body := e.v1Chat(key); code != 200 {
		t.Fatalf("chat after rebuild = %d %s", code, body)
	}
}
