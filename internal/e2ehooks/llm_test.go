package e2ehooks_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/llm"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/rental"
)

// /hooks/llm/upstream 挂 FakeVLLM（served 缺省 = online 模型）；/hooks/llm/node 在池内机器上造 ready 节点（系统实例 running、无部署）；
// 网关经 sshtest 真把请求转到 FakeVLLM 并记账；/hooks/llm/tick {probe:false} 只跑 Controller，缺省还跑探测（无部署的钩子节点计一次失败）；
// /hooks/sweep 结算；排空 → tick → Agent 洗机 → tick → removed、机器回到 idle（Playwright 每个用例结束的清理路径）。
func TestLLMHooks(t *testing.T) {
	e := newEnvWith(t, true)
	ctx := context.Background()
	apps := e.must("POST", "/hooks/apps/seed", map[string]any{})["apps"].([]any)
	if len(apps) != 5 {
		t.Fatalf("seed = %v", apps)
	}
	var img int64
	if err := e.pool.QueryRow(ctx, `SELECT id FROM images WHERE enabled ORDER BY sort, id LIMIT 1`).Scan(&img); err != nil {
		t.Fatal(err)
	}
	m, err := e.a.LLM.CreateModel(ctx, audit.Actor{}, llm.ModelInput{Slug: "qwen3-e2e", Name: "Qwen3 e2e", Kind: llm.KindChat,
		SourceID: "Qwen/Qwen3-8B", ImageID: img, ContextLen: 32768, InputPrice: 50, OutputPrice: 150, Replicas: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.a.LLM.SetModelStatus(ctx, audit.Actor{}, m.ID, llm.ModelOnline); err != nil {
		t.Fatal(err)
	}
	poolID, err := e.a.Fleet.LLMPoolGroupID(ctx, e.pool)
	if err != nil {
		t.Fatal(err)
	}
	inPool := int64(e.must("POST", "/hooks/machines", map[string]any{"count": 1, "group_id": poolID})["ids"].([]any)[0].(float64))
	outside := int64(e.must("POST", "/hooks/machines", map[string]any{"count": 1})["ids"].([]any)[0].(float64))

	if up := e.must("POST", "/hooks/llm/upstream", map[string]any{}); up["port"] != float64(8000) || len(up["served"].([]any)) != 1 ||
		up["served"].([]any)[0] != "qwen3-e2e" {
		t.Fatalf("upstream = %v", up)
	}
	if code, out := e.call("POST", "/hooks/llm/node", map[string]any{"model_id": m.ID, "machine_id": outside}); code != 400 ||
		!strings.Contains(out["error"].(string), "Token 服务池") {
		t.Fatalf("node on non-pool machine = %d %v", code, out)
	}
	if code, _ := e.call("POST", "/hooks/llm/node", map[string]any{"model_id": 999999, "machine_id": inPool}); code != 400 {
		t.Fatalf("node with unknown model = %d", code)
	}
	created := e.must("POST", "/hooks/llm/node", map[string]any{"model_id": m.ID, "machine_id": inPool})
	nodeID, instID := created["node_id"].(string), created["instance_id"].(string)
	inst, err := e.a.Rental.Get(ctx, instID)
	if err != nil || inst.State != rental.StateRunning || !inst.System || inst.MachineID != inPool {
		t.Fatalf("instance = %+v, %v", inst, err)
	}
	node := func() llm.Node {
		t.Helper()
		d, err := e.a.LLM.GetNode(ctx, nodeID)
		if err != nil {
			t.Fatal(err)
		}
		return d.Node.Node
	}
	if n := node(); n.State != llm.NodeReady || n.DeploymentID != nil || n.InstanceID == nil || *n.InstanceID != instID {
		t.Fatalf("node = %+v", n)
	}
	if code, _ := e.call("POST", "/hooks/llm/node", map[string]any{"model_id": m.ID, "machine_id": inPool}); code != 400 {
		t.Fatalf("second node on the same machine = %d", code)
	}

	// 用户调用：经网关 → 上游连接池 → sshtest → FakeVLLM
	uid := int64(e.must("POST", "/hooks/users", map[string]any{"verified": true, "balance": 1000})["id"].(float64))
	key, _, err := e.a.LLM.CreateKey(ctx, uid, llm.KeyInput{Name: "e2e"}, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	pub := httptest.NewServer(e.a.Handler)
	defer pub.Close()
	req, _ := http.NewRequest("POST", pub.URL+"/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"qwen3-e2e","messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "你好，世界") {
		t.Fatalf("chat = %d %s", resp.StatusCode, b)
	}
	var cost int64
	deadline := time.Now().Add(5 * time.Second)
	for cost == 0 && time.Now().Before(deadline) {
		_ = e.pool.QueryRow(ctx, `SELECT COALESCE(sum(cost_micro), 0) FROM llm_usage WHERE user_id = $1 AND node_id = $2`, uid, nodeID).Scan(&cost)
		time.Sleep(10 * time.Millisecond)
	}
	if cost != 100*50+50*150 {
		t.Fatalf("usage cost_micro = %d", cost)
	}

	// tick：probe=false 只跑 Controller（副本数已满足，无动作）；缺省带探测 → 无部署的钩子节点计一次失败（不是钩子错误）
	if rep := e.must("POST", "/hooks/llm/tick", map[string]any{"probe": false}); rep["provisioned"] != float64(0) || rep["error"] != "" {
		t.Fatalf("tick (no probe) = %v", rep)
	}
	if n := node(); n.ProbeFailCount != 0 || n.LastProbeAt != nil {
		t.Fatalf("node after tick without probe = %+v", n)
	}
	if rep := e.must("POST", "/hooks/llm/tick", map[string]any{}); rep["error"] != "" {
		t.Fatalf("tick = %v", rep)
	}
	if n := node(); n.State != llm.NodeReady || n.ProbeFailCount != 0 || n.LastProbeAt == nil || n.Error != "" {
		t.Fatalf("node after probe = %+v", n)
	}
	// 健康跟随 FakeVLLM：served 里没有该模型 → 探测失败计数；后台重建对无部署的钩子节点不可用
	e.must("POST", "/hooks/llm/upstream", map[string]any{"served": []string{"other-model"}})
	e.must("POST", "/hooks/llm/tick", map[string]any{})
	if n := node(); n.State != llm.NodeReady || n.ProbeFailCount != 1 || !strings.Contains(n.Error, "not served") {
		t.Fatalf("node after failed probe = %+v", n)
	}
	e.must("POST", "/hooks/llm/upstream", map[string]any{})
	e.must("POST", "/hooks/llm/tick", map[string]any{})
	if n := node(); n.ProbeFailCount != 0 {
		t.Fatalf("node after recovered probe = %+v", n)
	}
	if _, err := e.a.LLM.AdminRebuildNode(ctx, audit.Actor{}, nodeID); !errors.Is(err, apperr.ErrLLMNodeInvalidState) {
		t.Fatalf("rebuild hook node: err = %v, want llm_node_invalid_state", err)
	}

	// 结算：推进 10 分钟 + /hooks/sweep
	e.must("POST", "/hooks/clock/advance", map[string]any{"seconds": 601})
	e.must("POST", "/hooks/sweep", map[string]any{})
	var settlements int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM llm_settlements WHERE user_id = $1`, uid).Scan(&settlements); err != nil || settlements != 1 {
		t.Fatalf("settlements = %d, %v", settlements, err)
	}

	// 清理路径：排空 → 推进 5 秒（> 2 × 路由刷新）→ tick → removing → Agent 洗机 → tick → removed、机器 idle
	if _, err := e.a.LLM.AdminDrainNode(ctx, audit.Actor{}, nodeID); err != nil {
		t.Fatal(err)
	}
	e.must("POST", "/hooks/clock/advance", map[string]any{"seconds": 5})
	e.must("POST", "/hooks/llm/tick", map[string]any{"probe": false})
	if n := node(); n.State != llm.NodeRemoving {
		t.Fatalf("node after drain tick = %+v", n)
	}
	e.must("POST", "/hooks/agent", map[string]any{"machine_id": inPool})
	if rep := e.must("POST", "/hooks/llm/tick", map[string]any{"probe": false}); rep["removed"] != float64(1) || rep["error"] != "" {
		t.Fatalf("tick after wipe = %v", rep)
	}
	if mach, err := e.a.Fleet.Get(ctx, e.pool, inPool); err != nil || mach.State != "idle" {
		t.Fatalf("machine after removal = %+v, %v", mach, err)
	}
	if err := llm.CheckInvariants(ctx, e.pool); err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(node()); !strings.Contains(string(b), `"state":"removed"`) {
		t.Fatalf("node = %s", b)
	}
}
