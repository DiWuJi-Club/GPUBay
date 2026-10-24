package adminapi_test

import (
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/adminapi"
	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/llm"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/randx"
	"spark-cloud/internal/rental"
)

// ---- Token 商城（计划 9）测试夹具 ----

func (e *env) qint(sql string, args ...any) int64 {
	e.t.Helper()
	var n int64
	if err := e.pool.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// llmFixture 库里预置的已发布内部应用 vllm-node（spec 取仓库 deploy/apps/vllm-node/spec.json）与它的兼容镜像「LLM Serving」。
type llmFixture struct {
	imageID   int64
	appID     int64
	versionID int64
	poolID    int64
}

func (e *env) seedLLM() llmFixture {
	e.t.Helper()
	raw, err := os.ReadFile("../../deploy/apps/vllm-node/spec.json")
	if err != nil {
		e.t.Fatal(err)
	}
	var spec appdeploy.Spec
	if err := json.Unmarshal(raw, &spec); err != nil {
		e.t.Fatal(err)
	}
	if err := appdeploy.ValidateSpec(&spec); err != nil {
		e.t.Fatal(err)
	}
	b, _ := json.Marshal(spec)
	now := e.clk.Now()
	var f llmFixture
	f.imageID = e.qint(`INSERT INTO images (name, ref, enabled, sort, created_at, updated_at)
		VALUES ('LLM Serving', 'spark/llm-serving:25.09', true, 100, $1, $1) RETURNING id`, now)
	f.appID = e.qint(`INSERT INTO apps (slug, name, category, icon, status, internal, created_at, updated_at)
		VALUES ($1, 'vLLM 节点（平台内部）', 'llm', 'llm', 'published', true, $2, $2) RETURNING id`, llm.NodeAppSlug, now)
	f.versionID = e.qint(`INSERT INTO app_versions (app_id, version, status, image_ids, spec, script, spec_hash, published_at, created_at, updated_at)
		VALUES ($1, '1.0.0', 'published', $2, $3, 'true', $4, $5, $5, $5) RETURNING id`,
		f.appID, []int64{f.imageID}, b, appdeploy.SpecHash(spec, "true"), now)
	f.poolID = e.qint(`SELECT id FROM machine_groups WHERE purpose = 'llm'`)
	return f
}

func modelBody(slug string, imageID int64) map[string]any {
	return map[string]any{"slug": slug, "name": "Qwen3 4B", "description": "测试模型", "kind": "chat", "source": "modelscope",
		"source_id": "Qwen/Qwen3-4B", "image_id": imageID, "app_version_id": nil, "context_len": 32768, "max_output_tokens": 8192,
		"default_max_tokens": 4096, "input_price": 100, "output_price": 400, "node_env": map[string]string{}, "replicas": 1,
		"max_concurrency": 32, "features": []string{"tools"}, "sort": 0}
}

// onlineModel 经 llm.Service 建一个 1 副本、已上线的模型。
func (e *env) onlineModel(f llmFixture, slug string) llm.Model {
	e.t.Helper()
	a := audit.Actor{AdminID: 1}
	m, err := e.llm.CreateModel(e.ctx, a, llm.ModelInput{Slug: slug, Name: slug, Kind: llm.KindChat, Source: "modelscope",
		SourceID: "Qwen/Qwen3-4B", ImageID: f.imageID, ContextLen: 32768, InputPrice: 100, OutputPrice: 400, Replicas: 1})
	if err != nil {
		e.t.Fatal(err)
	}
	if m, err = e.llm.SetModelStatus(e.ctx, a, m.ID, llm.ModelOnline); err != nil {
		e.t.Fatal(err)
	}
	return m
}

// poolMachine 插一台空闲在线机器并放进 Token 服务池。
func (e *env) poolMachine(f llmFixture, serial string) int64 {
	e.t.Helper()
	id := e.machine(serial)
	e.exec(`UPDATE machines SET group_id = $2 WHERE id = $1`, id, f.poolID)
	return id
}

// readyNode 在池机器上用真实 rental.PlaceSystem 开系统实例（fake Agent 完成 Create → running），
// 再直接插一条 queued 部署与 ready 节点行（不走 Controller）。返回节点 ID 与实例。
func (e *env) readyNode(f llmFixture, m llm.Model, machineID int64) (string, rental.Instance) {
	e.t.Helper()
	inst, err := e.rental.PlaceSystem(e.ctx, rental.SystemPlaceRequest{MachineID: machineID, GroupID: f.poolID, ImageID: f.imageID, Note: "test"})
	if err != nil {
		e.t.Fatalf("PlaceSystem: %v", err)
	}
	e.ack()
	now := e.clk.Now()
	dep := randx.ID("dep-", 12)
	e.exec(`INSERT INTO app_deployments (id, instance_id, user_id, app_id, app_version_id, state, env, port, spec_hash,
		next_poll_at, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, 'queued', '{}', 8000, 'x', $6, $6, $6)`,
		dep, inst.ID, e.systemUserID, f.appID, f.versionID, now)
	id := randx.ID("node-", 12)
	e.exec(`INSERT INTO llm_nodes (id, model_id, machine_id, instance_id, deployment_id, state, api_key_enc, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'ready', '\x00', $6, $6)`, id, m.ID, machineID, inst.ID, dep, now)
	return id, inst
}

func (e *env) adminAudits(action, target string) int {
	e.t.Helper()
	return e.count(`SELECT count(*) FROM audit_logs WHERE action = $1 AND target_id = $2 AND actor_type = 'admin'`, action, target)
}

// ---- 用例 ----

func TestAdminLLMModelsCRUD(t *testing.T) {
	e := newEnv(t)
	f := e.seedLLM()
	root := e.loginAs("root", admin.RoleSuper)
	ops := e.loginAs("ops", admin.RoleOps)
	fin := e.loginAs("fin", admin.RoleFinance)

	if code, out := ops.do("POST", "/llm/models", modelBody("qwen3-4b", f.imageID)); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("ops create = %d %v", code, out)
	}
	code, out := root.do("POST", "/llm/models", modelBody("qwen3-4b", f.imageID))
	if code != 201 || out["slug"] != "qwen3-4b" || out["status"] != "draft" || out["input_price"].(float64) != 100 {
		t.Fatalf("create = %d %v", code, out)
	}
	id := itoa(int64(out["id"].(float64)))
	if code, out := root.do("POST", "/llm/models", map[string]any{"slug": "x", "bogus": 1}); code != 400 {
		t.Fatalf("unknown field = %d %v", code, out)
	}

	// 修改（PUT 不带 slug）：价格变化进审计 diff
	body := modelBody("", f.imageID)
	delete(body, "slug")
	body["name"], body["output_price"] = "Qwen3 4B Instruct", 500
	code, out = root.do("PUT", "/llm/models/"+id, body)
	if code != 200 || out["name"] != "Qwen3 4B Instruct" || out["output_price"].(float64) != 500 || out["slug"] != "qwen3-4b" {
		t.Fatalf("update = %d %v", code, out)
	}
	if code, out := ops.do("PUT", "/llm/models/"+id, body); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("ops update = %d %v", code, out)
	}

	// 上下线要动态码
	if code, out := root.do("POST", "/llm/models/"+id+"/status", map[string]any{"status": "online"}); code != 403 || codeOf(out) != "totp_required" {
		t.Fatalf("status without totp = %d %v", code, out)
	}
	if code, out := ops.do("POST", "/llm/models/"+id+"/status", map[string]any{"status": "online", "totp": ops.totp()}); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("ops status = %d %v", code, out)
	}
	code, out = root.do("POST", "/llm/models/"+id+"/status", map[string]any{"status": "online", "totp": root.totp()})
	if code != 200 || out["status"] != "online" {
		t.Fatalf("status online = %d %v", code, out)
	}

	// 列表全员可看，带节点计数
	m, err := e.llm.GetModel(e.ctx, int64(out["id"].(float64)))
	if err != nil {
		t.Fatal(err)
	}
	e.readyNode(f, m, e.poolMachine(f, "SN-P1"))
	code, out = fin.do("GET", "/llm/models", nil)
	items, _ := out["items"].([]any)
	if code != 200 || len(items) != 1 {
		t.Fatalf("list = %d %v", code, out)
	}
	row := items[0].(map[string]any)
	nodes := row["nodes"].(map[string]any)
	if row["slug"] != "qwen3-4b" || nodes["ready"].(float64) != 1 || nodes["total"].(float64) != 1 || row["node_env"] == nil {
		t.Fatalf("list row = %v", row)
	}

	// 删除：只允许 draft 且无引用
	if code, out := root.do("DELETE", "/llm/models/"+id, map[string]any{}); code != 409 || codeOf(out) != "llm_model_in_use" {
		t.Fatalf("delete online = %d %v", code, out)
	}
	code, out = root.do("POST", "/llm/models", modelBody("bge-m3", f.imageID))
	if code != 201 {
		t.Fatalf("create 2 = %d %v", code, out)
	}
	id2 := itoa(int64(out["id"].(float64)))
	if code, out := ops.do("DELETE", "/llm/models/"+id2, map[string]any{}); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("ops delete = %d %v", code, out)
	}
	if code, out := root.do("DELETE", "/llm/models/"+id2, map[string]any{}); code != 200 {
		t.Fatalf("delete draft = %d %v", code, out)
	}
	for action, target := range map[string]string{"llm.model.create": id, "llm.model.update": id, "llm.model.status": id, "llm.model.delete": id2} {
		if n := e.adminAudits(action, target); n != 1 {
			t.Errorf("audit %s %s = %d", action, target, n)
		}
	}
}

func TestAdminLLMPoolAndNodes(t *testing.T) {
	e := newEnv(t)
	f := e.seedLLM()
	m := e.onlineModel(f, "qwen3-4b")
	m1 := e.poolMachine(f, "SN-P1")
	m2 := e.poolMachine(f, "SN-P2")
	nodeID, inst := e.readyNode(f, m, m1)
	ops := e.loginAs("ops", admin.RoleOps)
	fin := e.loginAs("fin", admin.RoleFinance)

	// 机器池视图
	code, out := ops.do("GET", "/llm/pool", nil)
	if code != 200 || out["group_id"].(float64) != float64(f.poolID) || out["total"].(float64) != 2 || out["free"].(float64) != 1 {
		t.Fatalf("pool = %d %v", code, out)
	}
	var withNode map[string]any
	for _, x := range out["machines"].([]any) {
		pm := x.(map[string]any)
		if pm["machine"].(map[string]any)["id"].(float64) == float64(m1) {
			withNode = pm
		}
	}
	if withNode == nil || withNode["node"].(map[string]any)["id"] != nodeID || withNode["node"].(map[string]any)["model_slug"] != "qwen3-4b" {
		t.Fatalf("pool machine with node = %v", withNode)
	}
	if code, out := fin.do("GET", "/llm/pool", nil); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("finance pool = %d %v", code, out)
	}

	// 有节点的机器不能移出池：DELETE /groups/{池}/machines、PUT /machines/{id} 改组、POST /groups/{别的组}/machines
	pool := itoa(f.poolID)
	if code, out := ops.do("DELETE", "/groups/"+pool+"/machines", map[string]any{"machine_ids": []int64{m1}}); code != 409 || codeOf(out) != "llm_pool_machine_busy" {
		t.Fatalf("unassign busy = %d %v", code, out)
	}
	if code, out := ops.do("PUT", "/machines/"+itoa(m1), map[string]any{"group_id": nil}); code != 409 || codeOf(out) != "llm_pool_machine_busy" {
		t.Fatalf("machine group change = %d %v", code, out)
	}
	code, out = ops.do("POST", "/groups", map[string]any{"name": "华东一区", "region": "上海"})
	if code != 201 {
		t.Fatalf("create group = %d %v", code, out)
	}
	other := itoa(int64(out["id"].(float64)))
	if code, out := ops.do("POST", "/groups/"+other+"/machines", map[string]any{"machine_ids": []int64{m1}}); code != 409 || codeOf(out) != "llm_pool_machine_busy" {
		t.Fatalf("assign busy elsewhere = %d %v", code, out)
	}
	if code, out := ops.do("PUT", "/machines/"+itoa(m1), map[string]any{"display_name": "池一号"}); code != 200 {
		t.Fatalf("rename pool machine = %d %v", code, out)
	}
	if code, out := ops.do("PUT", "/machines/"+itoa(m1), map[string]any{"group_id": f.poolID}); code != 200 {
		t.Fatalf("keep in pool = %d %v", code, out)
	}
	if g, _ := e.fleet.Get(e.ctx, e.pool, m1); g.GroupID == nil || *g.GroupID != f.poolID {
		t.Fatalf("busy machine left the pool: %+v", g.GroupID)
	}
	// 空闲机器照常移出
	if code, out := ops.do("DELETE", "/groups/"+pool+"/machines", map[string]any{"machine_ids": []int64{m2}}); code != 200 {
		t.Fatalf("unassign free = %d %v", code, out)
	}

	// 节点列表 / 详情
	code, out = ops.do("GET", "/llm/nodes?model_id="+itoa(m.ID), nil)
	items, _ := out["items"].([]any)
	if code != 200 || len(items) != 1 || items[0].(map[string]any)["id"] != nodeID || items[0].(map[string]any)["model_slug"] != "qwen3-4b" ||
		items[0].(map[string]any)["machine_name"] != "池一号" || items[0].(map[string]any)["deployment"] == nil {
		t.Fatalf("nodes = %d %v", code, out)
	}
	if code, out := ops.do("GET", "/llm/nodes?state=draining", nil); code != 200 || len(out["items"].([]any)) != 0 {
		t.Fatalf("nodes draining = %d %v", code, out)
	}
	if code, out := ops.do("GET", "/llm/nodes/node-missing", nil); code != 404 || codeOf(out) != "llm_node_not_found" {
		t.Fatalf("missing node = %d %v", code, out)
	}

	// 重建（ready → provisioning，Redeploy）→ 排空 → 移除（动态码）
	if code, out := fin.do("POST", "/llm/nodes/"+nodeID+"/rebuild", map[string]any{}); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("finance rebuild = %d %v", code, out)
	}
	if code, out := ops.do("POST", "/llm/nodes/"+nodeID+"/rebuild", map[string]any{}); code != 200 || out["state"] != llm.NodeProvisioning {
		t.Fatalf("rebuild = %d %v", code, out)
	}
	if code, out := ops.do("POST", "/llm/nodes/"+nodeID+"/drain", map[string]any{}); code != 200 || out["state"] != llm.NodeDraining {
		t.Fatalf("drain = %d %v", code, out)
	}
	if code, out := ops.do("POST", "/llm/nodes/"+nodeID+"/drain", map[string]any{}); code != 409 || codeOf(out) != "llm_node_invalid_state" {
		t.Fatalf("drain twice = %d %v", code, out)
	}
	if code, out := ops.do("POST", "/llm/nodes/"+nodeID+"/remove", map[string]any{"reason": "换机"}); code != 403 || codeOf(out) != "totp_required" {
		t.Fatalf("remove without totp = %d %v", code, out)
	}
	if code, out := ops.do("POST", "/llm/nodes/"+nodeID+"/remove", map[string]any{"reason": " ", "totp": ops.totp()}); code != 400 {
		t.Fatalf("remove without reason = %d %v", code, out)
	}
	code, out = ops.do("POST", "/llm/nodes/"+nodeID+"/remove", map[string]any{"reason": "换机", "totp": ops.totp()})
	if code != 200 || out["state"] != llm.NodeRemoving {
		t.Fatalf("remove = %d %v", code, out)
	}
	if got, _ := e.rental.Get(e.ctx, inst.ID); got.State != rental.StateWiping {
		t.Fatalf("system instance after remove = %s", got.State)
	}
	code, out = ops.do("GET", "/llm/nodes/"+nodeID, nil)
	if code != 200 || out["node"].(map[string]any)["state"] != llm.NodeRemoving || len(out["events"].([]any)) != 3 {
		t.Fatalf("node detail = %d %v", code, out)
	}
	// removing 仍是非终态：机器还不能移出池
	if code, out := ops.do("DELETE", "/groups/"+pool+"/machines", map[string]any{"machine_ids": []int64{m1}}); code != 409 || codeOf(out) != "llm_pool_machine_busy" {
		t.Fatalf("unassign removing = %d %v", code, out)
	}
	for _, action := range []string{"llm.node.rebuild", "llm.node.drain", "llm.node.remove"} {
		if n := e.adminAudits(action, nodeID); n != 1 {
			t.Errorf("audit %s = %d", action, n)
		}
	}
}

func TestAdminLLMAccountsUsageExport(t *testing.T) {
	e := newEnv(t)
	f := e.seedLLM()
	m := e.onlineModel(f, "qwen3-4b")
	uid := e.renter("13600000001")
	other := e.renter("13600000002")
	_, key, err := e.llm.CreateKey(e.ctx, uid, llm.KeyInput{Name: "生产"}, "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.llm.CreateKey(e.ctx, other, llm.KeyInput{Name: "测试"}, "1.2.3.4"); err != nil {
		t.Fatal(err)
	}
	record := func(req string, cost int64) {
		t.Helper()
		if err := e.llm.RecordUsage(e.ctx, llm.Usage{RequestID: req, UserID: uid, KeyID: key.ID, ModelID: m.ID, Endpoint: llm.EndpointChat,
			PromptTokens: 1000, CompletionTokens: 200, InputPrice: 100, OutputPrice: 400, CostMicro: cost, Status: llm.UsageOK,
			HTTPStatus: 200, LatencyMS: 321}); err != nil {
			t.Fatal(err)
		}
	}
	record("req-aaaaaaaaaaaaaaaa", 123_456_789) // ¥1.234568，≥ ¥1 立即结算（扣 123 分）
	if err := e.llm.Settle(e.ctx); err != nil {
		t.Fatal(err)
	}
	record("req-bbbbbbbbbbbbbbbb", 2_500_000) // 未结算 2.5 分
	ops := e.loginAs("ops", admin.RoleOps)
	fin := e.loginAs("fin", admin.RoleFinance)

	// 账户列表（ops / finance 都可看）
	code, out := fin.do("GET", "/llm/accounts?user_id="+itoa(uid), nil)
	items, _ := out["items"].([]any)
	if code != 200 || out["total"].(float64) != 1 || len(items) != 1 {
		t.Fatalf("accounts = %d %v", code, out)
	}
	acc := items[0].(map[string]any)
	if acc["user_id"].(float64) != float64(uid) || acc["phone"] != "13600000001" || acc["key_count"].(float64) != 1 ||
		acc["unsettled"].(float64) != 3 || acc["month_cost"].(float64) != 125 || acc["limits"].(map[string]any)["rpm"].(float64) != llm.DefaultRPM {
		t.Fatalf("account row = %v", acc)
	}
	if code, out := ops.do("GET", "/llm/accounts", nil); code != 200 || out["total"].(float64) != 2 {
		t.Fatalf("ops accounts = %d %v", code, out)
	}
	if code, out := ops.do("GET", "/llm/accounts?blocked=maybe", nil); code != 400 {
		t.Fatalf("bad blocked = %d %v", code, out)
	}

	// 改账户（ops）
	if code, out := fin.do("PUT", "/llm/accounts/"+itoa(uid), map[string]any{"rpm_limit": 100}); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("finance update account = %d %v", code, out)
	}
	code, out = ops.do("PUT", "/llm/accounts/"+itoa(uid), map[string]any{"rpm_limit": 100, "tpm_limit": nil})
	if code != 200 || out["rpm_limit"].(float64) != 100 || out["tpm_limit"] != nil {
		t.Fatalf("update account = %d %v", code, out)
	}
	if n := e.adminAudits("llm.account.update", itoa(uid)); n != 1 {
		t.Fatalf("account audit = %d", n)
	}

	// Key（只有 hint）与本月消费；停用（ops）
	if code, out := fin.do("GET", "/llm/keys", nil); code != 400 {
		t.Fatalf("keys without user_id = %d %v", code, out)
	}
	code, out = fin.do("GET", "/llm/keys?user_id="+itoa(uid), nil)
	items, _ = out["items"].([]any)
	if code != 200 || len(items) != 1 || items[0].(map[string]any)["month_cost"].(float64) != 125 || items[0].(map[string]any)["key_hint"] == "" ||
		items[0].(map[string]any)["key_hash"] != nil {
		t.Fatalf("keys = %d %v", code, out)
	}
	if code, out := fin.do("POST", "/llm/keys/"+itoa(key.ID)+"/disable", map[string]any{"reason": "滥用"}); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("finance disable key = %d %v", code, out)
	}
	if code, out := ops.do("POST", "/llm/keys/"+itoa(key.ID)+"/disable", map[string]any{"reason": "滥用"}); code != 200 {
		t.Fatalf("disable key = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/llm/keys?user_id="+itoa(uid), nil); code != 200 || out["items"].([]any)[0].(map[string]any)["status"] != llm.KeyDisabled {
		t.Fatalf("keys after disable = %d %v", code, out)
	}

	// 用量明细（finance）
	if code, out := ops.do("GET", "/llm/usage", nil); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("ops usage = %d %v", code, out)
	}
	code, out = fin.do("GET", "/llm/usage?user_id="+itoa(uid)+"&model_id="+itoa(m.ID), nil)
	items, _ = out["items"].([]any)
	if code != 200 || out["total"].(float64) != 2 || items[0].(map[string]any)["request_id"] != "req-bbbbbbbbbbbbbbbb" ||
		items[0].(map[string]any)["user_phone"] != "13600000001" || items[0].(map[string]any)["model"] != "qwen3-4b" {
		t.Fatalf("usage = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/llm/usage?status=ok&key_id="+itoa(key.ID)+"&page_size=1", nil); code != 200 || out["total"].(float64) != 2 || len(out["items"].([]any)) != 1 {
		t.Fatalf("usage page = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/llm/usage?from=2025-01-01&to=2025-12-31", nil); code != 400 {
		t.Fatalf("usage > 90 days = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/llm/usage?status=weird", nil); code != 400 {
		t.Fatalf("usage bad status = %d %v", code, out)
	}

	// CSV：首行列名，费用 6 位小数（元）
	resp, _ := fin.send("GET", "/llm/usage/export?user_id="+itoa(uid), nil)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/csv") {
		t.Fatalf("export = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	recs, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(readAll(t, resp), "\ufeff"))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"请求ID", "时间", "用户", "模型", "Key", "端点", "流式", "输入token", "输出token", "估算",
		"输入单价(元/百万token)", "输出单价(元/百万token)", "费用(元)", "状态", "延迟(ms)", "节点"}
	if len(recs) != 3 || !slices.Equal(recs[0], want) {
		t.Fatalf("csv = %v", recs)
	}
	first := recs[1]
	if first[0] != "req-aaaaaaaaaaaaaaaa" || first[2] != "13600000001" || first[3] != "qwen3-4b" || first[10] != "1.00" || first[11] != "4.00" ||
		first[12] != "1.234568" || first[13] != "成功" || first[14] != "321" || !strings.Contains(first[4], "生产") {
		t.Fatalf("csv row = %v", first)
	}
	if recs[2][12] != "0.025000" {
		t.Fatalf("csv cost 2 = %v", recs[2])
	}
	if resp, _ := ops.send("GET", "/llm/usage/export", nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("ops export = %d", resp.StatusCode)
	}

	// 按天汇总：已结算（llm_usage_daily）+ 未结算合并
	today := dbtest.T0.In(cstZone).Format("2006-01-02")
	code, out = fin.do("GET", "/llm/usage/daily?group_by=user&from="+today+"&to="+today, nil)
	items, _ = out["items"].([]any)
	if code != 200 || len(items) != 1 {
		t.Fatalf("daily by user = %d %v", code, out)
	}
	d := items[0].(map[string]any)
	if d["day"] != today || d["user_id"].(float64) != float64(uid) || d["phone"] != "13600000001" || d["requests"].(float64) != 2 ||
		d["cost_micro"].(float64) != 125_956_789 || d["model"] != nil {
		t.Fatalf("daily row = %v", d)
	}
	code, out = fin.do("GET", "/llm/usage/daily", nil)
	items, _ = out["items"].([]any)
	if code != 200 || len(items) != 1 || items[0].(map[string]any)["model"] != "qwen3-4b" || items[0].(map[string]any)["user_id"] != nil ||
		items[0].(map[string]any)["prompt_tokens"].(float64) != 2000 {
		t.Fatalf("daily by model = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/llm/usage/daily?group_by=key", nil); code != 400 {
		t.Fatalf("daily bad group = %d %v", code, out)
	}

	// 结算记录
	code, out = fin.do("GET", "/llm/settlements?user_id="+itoa(uid), nil)
	items, _ = out["items"].([]any)
	if code != 200 || out["total"].(float64) != 1 || items[0].(map[string]any)["charged"].(float64) != 123 || items[0].(map[string]any)["user_id"].(float64) != float64(uid) {
		t.Fatalf("settlements = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/llm/settlements?user_id="+itoa(other), nil); code != 200 || out["total"].(float64) != 0 {
		t.Fatalf("settlements other = %d %v", code, out)
	}

	// 仪表盘卡片（全员）
	code, out = ops.do("GET", "/llm/summary", nil)
	if code != 200 {
		t.Fatalf("summary = %d %v", code, out)
	}
	td := out["today"].(map[string]any)
	if td["requests"].(float64) != 2 || td["prompt_tokens"].(float64) != 2000 || td["completion_tokens"].(float64) != 400 ||
		td["settled_amount"].(float64) != 123 || td["unsettled_estimate"].(float64) != 3 || out["debt_total"].(float64) != 0 || out["debt_users"].(float64) != 0 {
		t.Fatalf("summary = %v", out)
	}
	// 欠费
	e.exec(`UPDATE llm_accounts SET debt = 7 WHERE user_id = $1`, other)
	if code, out := fin.do("GET", "/llm/summary", nil); code != 200 || out["debt_total"].(float64) != 7 || out["debt_users"].(float64) != 1 {
		t.Fatalf("summary debt = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/llm/accounts?debt=true", nil); code != 200 || out["total"].(float64) != 1 ||
		out["items"].([]any)[0].(map[string]any)["user_id"].(float64) != float64(other) {
		t.Fatalf("accounts debt = %d %v", code, out)
	}
}

var cstZone = time.FixedZone("CST", 8*3600)

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAdminUsersHideSystem(t *testing.T) {
	e := newEnv(t)
	uid := e.renter("13600000001")
	ops := e.loginAs("ops", admin.RoleOps)
	fin := e.loginAs("fin", admin.RoleFinance)
	hasSystem := func(out map[string]any) bool {
		for _, x := range out["items"].([]any) {
			if x.(map[string]any)["phone"] == llm.SystemUserPhone {
				return true
			}
		}
		return false
	}
	code, out := ops.do("GET", "/users", nil)
	if code != 200 || out["total"].(float64) != 1 || hasSystem(out) {
		t.Fatalf("users = %d %v", code, out)
	}
	code, out = ops.do("GET", "/users?include_system=true", nil)
	if code != 200 || out["total"].(float64) != 2 || !hasSystem(out) {
		t.Fatalf("users include_system = %d %v", code, out)
	}
	if code, out := ops.do("GET", "/users?include_system=false", nil); code != 200 || out["total"].(float64) != 1 {
		t.Fatalf("users include_system=false = %d %v", code, out)
	}
	if code, out := ops.do("GET", "/users?include_system=yes", nil); code != 400 {
		t.Fatalf("users include_system=yes = %d %v", code, out)
	}
	sys := itoa(e.systemUserID)
	if code, out := ops.do("POST", "/users/"+sys+"/ban", map[string]any{"reason": "测试"}); code != 409 || codeOf(out) != "invalid_state" {
		t.Fatalf("ban system = %d %v", code, out)
	}
	if code, out := fin.do("POST", "/users/"+sys+"/adjust", map[string]any{"amount": 100, "remark": "x", "totp": fin.totp()}); code != 409 || codeOf(out) != "invalid_state" {
		t.Fatalf("adjust system = %d %v", code, out)
	}
	if code, out := fin.do("POST", "/users/"+sys+"/manual-recharge", map[string]any{"amount": 100, "remark": "x", "totp": fin.totp()}); code != 409 || codeOf(out) != "invalid_state" {
		t.Fatalf("manual recharge system = %d %v", code, out)
	}
	if n := e.count(`SELECT count(*) FROM wallet_ledger WHERE user_id = $1`, e.systemUserID); n != 0 {
		t.Fatalf("system ledger rows = %d", n)
	}
	// 普通用户照常；用户详情带 Token 商城概况
	if _, _, err := e.llm.CreateKey(e.ctx, uid, llm.KeyInput{Name: "k"}, "1.2.3.4"); err != nil {
		t.Fatal(err)
	}
	if code, out := ops.do("POST", "/users/"+itoa(uid)+"/ban", map[string]any{"reason": "测试"}); code != 200 {
		t.Fatalf("ban user = %d %v", code, out)
	}
	e.exec(`UPDATE llm_accounts SET debt = 5, blocked = true, blocked_reason = 'x' WHERE user_id = $1`, uid)
	code, out = ops.do("GET", "/users/"+itoa(uid), nil)
	l, _ := out["llm"].(map[string]any)
	if code != 200 || l == nil || l["keys"].(float64) != 1 || l["debt"].(float64) != 5 || l["blocked"] != true || l["month_cost"].(float64) != 0 || l["unsettled"].(float64) != 0 {
		t.Fatalf("user detail llm = %d %v", code, out["llm"])
	}
	// 系统账号详情可看
	if code, out := ops.do("GET", "/users/"+sys, nil); code != 200 || out["llm"] == nil {
		t.Fatalf("system user detail = %d %v", code, out)
	}
}

func TestAdminInstanceViewSystem(t *testing.T) {
	e := newEnv(t)
	f := e.seedLLM()
	m := e.onlineModel(f, "qwen3-4b")
	_, sysInst := e.readyNode(f, m, e.poolMachine(f, "SN-P1"))
	e.machine("SN1")
	uid := e.renter("13600000001")
	userInst := e.place(uid)
	e.ack()
	fin := e.loginAs("fin", admin.RoleFinance)
	for id, want := range map[string]bool{sysInst.ID: true, userInst.ID: false} {
		code, out := fin.do("GET", "/instances/"+id, nil)
		if code != 200 || out["instance"].(map[string]any)["system"] != want {
			t.Fatalf("instance %s system = %d %v", id, code, out["instance"])
		}
	}
	code, out := fin.do("GET", "/instances?user_id="+itoa(e.systemUserID), nil)
	items, _ := out["items"].([]any)
	if code != 200 || len(items) != 1 || items[0].(map[string]any)["system"] != true || items[0].(map[string]any)["expires_at"] != nil {
		t.Fatalf("system instances = %d %v", code, out)
	}
}

func TestDashboardLLM(t *testing.T) {
	e := newEnv(t)
	f := e.seedLLM()
	m := e.onlineModel(f, "qwen3-4b")
	e.readyNode(f, m, e.poolMachine(f, "SN-P1"))
	e.exec(`INSERT INTO llm_nodes (id, model_id, machine_id, state, api_key_enc, created_at, updated_at)
		VALUES ('node-unhealthy01', $1, $2, 'unhealthy', '\x00', $3, $3)`, m.ID, e.poolMachine(f, "SN-P2"), e.clk.Now())
	e.exec(`INSERT INTO llm_nodes (id, model_id, machine_id, state, api_key_enc, created_at, updated_at)
		VALUES ('node-failed00001', $1, $2, 'failed', '\x00', $3, $3)`, m.ID, e.poolMachine(f, "SN-P3"), e.clk.Now())
	uid := e.renter("13600000001")
	_, key, err := e.llm.CreateKey(e.ctx, uid, llm.KeyInput{Name: "k"}, "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.llm.RecordUsage(e.ctx, llm.Usage{RequestID: "req-dddddddddddddddd", UserID: uid, KeyID: key.ID, ModelID: m.ID,
		Endpoint: llm.EndpointChat, PromptTokens: 10, CompletionTokens: 5, InputPrice: 100, OutputPrice: 400, CostMicro: 200_000_000,
		Status: llm.UsageOK, HTTPStatus: 200}); err != nil {
		t.Fatal(err)
	}
	if err := e.llm.Settle(e.ctx); err != nil {
		t.Fatal(err)
	}
	fin := e.loginAs("fin", admin.RoleFinance)
	code, out := fin.do("GET", "/dashboard", nil)
	l, _ := out["llm"].(map[string]any)
	if code != 200 || l == nil || l["nodes_ready"].(float64) != 1 || l["nodes_total"].(float64) != 2 || l["today_requests"].(float64) != 1 ||
		l["today_settled_amount"].(float64) != 200 {
		t.Fatalf("dashboard llm = %d %v", code, out["llm"])
	}
	code, out = fin.do("GET", "/llm/summary", nil)
	if n := out["nodes"].(map[string]any); code != 200 || n["ready"].(float64) != 1 || n["total"].(float64) != 2 || n["unhealthy"].(float64) != 1 {
		t.Fatalf("summary nodes = %d %v", code, out)
	}
}

// 未装配 LLM 时：不注册 /llm/*，仪表盘 / 用户详情不带 llm 字段。
func TestAdminWithoutLLM(t *testing.T) {
	e := newEnvWith(t, func(s *adminapi.Server) { s.LLM = nil })
	for _, r := range e.srv.Routes() {
		if strings.HasPrefix(r.Pattern, "/admin/api/v1/llm/") {
			t.Fatalf("route %s registered without LLM", r.Pattern)
		}
	}
	ops := e.loginAs("ops", admin.RoleOps)
	if code, out := ops.do("GET", "/dashboard", nil); code != 200 || out["llm"] != nil {
		t.Fatalf("dashboard = %d %v", code, out)
	}
	if code, _ := ops.do("GET", "/llm/summary", nil); code != 404 {
		t.Fatalf("llm summary without LLM = %d", code)
	}
	uid := e.renter("13600000001")
	if code, out := ops.do("GET", "/users/"+itoa(uid), nil); code != 200 || out["llm"] != nil {
		t.Fatalf("user detail = %d %v", code, out)
	}
}
