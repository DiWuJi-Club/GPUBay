package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/comfywf"
	"spark-cloud/internal/comfywf/comfytest"
	"spark-cloud/internal/e2ehooks"
)

// ComfyUI 工作流的集成测试（计划 11 规格 §16.2）：在应用部署集成环境（newAppsEnv：真实 app.Build + 嵌入式 PG + 假时钟 +
// sshtest shell 模式的假容器）之上，fake comfyui（1.1.0 语义）的 app.sh 按部署包里的 workflows/manifest.json 在
// <实例根>/root/data/comfyui 下放工作流文件、touch 模型、建节点目录；假容器的 8188 转到进程内 fake ComfyUI（comfytest）。

type wfEnv struct {
	*appsEnv
	comfy    *comfytest.Server
	comfyImg int64
	wfs      map[string]e2ehooks.SeededWorkflow
}

func newWfEnv(t *testing.T, machines int) *wfEnv {
	t.Helper()
	e := &wfEnv{appsEnv: newAppsEnv(t, machines)}
	t.Cleanup(func() { assertWorkflowsConsistent(t, e.pool) })
	e.comfy = comfytest.New()
	ts := httptest.NewServer(e.comfy)
	t.Cleanup(ts.Close)
	e.up.SetTarget(comfywf.ComfyPort, strings.TrimPrefix(ts.URL, "http://")) // SeedApps 登记的是通用 fake 服务：改指 fake ComfyUI
	v, err := e.a.AppCatalog.GetVersion(e.ctx, e.apps["comfyui"].VersionID)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Spec.SupportsWorkflows || len(v.ImageIDs) != 1 {
		t.Fatalf("fake comfyui version = %+v", v)
	}
	e.comfyImg = v.ImageIDs[0]
	seeded, err := e2ehooks.SeedWorkflows(e.ctx, e.a)
	if err != nil {
		t.Fatal(err)
	}
	e.wfs = map[string]e2ehooks.SeededWorkflow{}
	for _, s := range seeded {
		e.wfs[s.Slug] = s
	}
	return e
}

// orderComfy 下单预装 comfyui 与工作流 slugs，返回实例 id 与部署 id。
func (e *wfEnv) orderComfy(slugs ...string) (string, string) {
	e.t.Helper()
	ids := []int64{}
	for _, s := range slugs {
		ids = append(ids, e.wfs[s].ID)
	}
	inst := e.order(map[string]any{"image_id": e.comfyImg, "app_id": e.apps["comfyui"].ID, "app_workflow_ids": ids})
	sum := inst["apps_summary"].([]any)
	if len(sum) != 1 {
		e.t.Fatalf("apps_summary = %v", sum)
	}
	return inst["id"].(string), sum[0].(map[string]any)["id"].(string)
}

func (e *wfEnv) set(dep string) map[string]comfywf.Installed {
	e.t.Helper()
	ws, err := e.a.WorkflowInstaller.ListInstalled(e.ctx, dep)
	if err != nil {
		e.t.Fatal(err)
	}
	out := map[string]comfywf.Installed{}
	for _, w := range ws {
		out[w.Slug] = w
	}
	return out
}

// wantSet 断言部署的集合恰为 slug → state。
func (e *wfEnv) wantSet(dep string, want map[string]string) map[string]comfywf.Installed {
	e.t.Helper()
	got := e.set(dep)
	states := map[string]string{}
	for s, w := range got {
		states[s] = w.State
	}
	if fmt.Sprint(states) != fmt.Sprint(want) {
		e.t.Fatalf("deployment %s workflows = %v, want %v", dep, states, want)
	}
	return got
}

// comfyData 是实例假容器里的 /root/data/comfyui。
func (e *wfEnv) comfyData(instID string) string {
	return filepath.Join(e.appRootFor(instID), comfywf.DataDirName)
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func (e *wfEnv) wfFile(instID, slug string) string {
	return filepath.Join(e.comfyData(instID), "user", "default", "workflows", "spark-"+slug+".json")
}

func (e *wfEnv) modelFile(instID, name string) string {
	return filepath.Join(e.comfyData(instID), "models", "checkpoints", name)
}

func (e *wfEnv) nodeDir(instID string) string {
	return filepath.Join(e.comfyData(instID), "custom_nodes", e2ehooks.FakeNodeName)
}

// bundleManifest 读假容器里部署包目录下的 workflows/manifest.json（平台生成的容器内清单）。
func (e *wfEnv) bundleManifest(instID string) (slugs []string, versions map[string]string) {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.appDir(instID, "comfyui"), "workflows", "manifest.json"))
	if err != nil {
		e.t.Fatal(err)
	}
	var m struct {
		Workflows []struct {
			Slug, Version, File string
		} `json:"workflows"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		e.t.Fatalf("manifest.json %q: %v", b, err)
	}
	versions = map[string]string{}
	for _, w := range m.Workflows {
		slugs = append(slugs, w.Slug)
		versions[w.Slug] = w.Version
		if w.File != w.Slug+".json" || !exists(filepath.Join(e.appDir(instID, "comfyui"), "workflows", w.File)) {
			e.t.Fatalf("manifest entry %+v: graph file missing in bundle", w)
		}
	}
	return slugs, versions
}

func (e *wfEnv) wfPath(instID, dep string, wid int64) string {
	return fmt.Sprintf("/instances/%s/apps/%s/workflows/%d", instID, dep, wid)
}

// 全链路（规格 §16.2）：下单预装 A → running、A installed、容器里有 spark-e2e-wf-a.json 与模型 → 添加 B（有节点）→ 部署回 queued、
// 包里两个条目 → running、B installed、节点目录 → 更新 A 到新版本 → version 变、重装（模型跳过）→ 移除 B → 文件消失、节点 / 模型保留 →
// 到期停机 → 续费恢复 → skip install、集合不变 → 换机 → 新部署集合复制为 pending → running 后 installed → 释放 → removed、集合行保留。
func TestWorkflowsFullLifecycle(t *testing.T) {
	e := newWfEnv(t, 2)
	a, b := e.wfs[e2ehooks.WorkflowA], e.wfs[e2ehooks.WorkflowB]
	id, dep := e.orderComfy(e2ehooks.WorkflowA)
	e.wantSet(dep, map[string]string{a.Slug: "pending"})
	e.agent()
	d := e.untilState(dep, appdeploy.StateRunning)
	e.wantSet(dep, map[string]string{a.Slug: "installed"})
	if !exists(e.wfFile(id, a.Slug)) || !exists(e.modelFile(id, e2ehooks.FakeModelA)) {
		t.Fatalf("container after preinstall: wf %v model %v\nlog:\n%s", exists(e.wfFile(id, a.Slug)), exists(e.modelFile(id, e2ehooks.FakeModelA)), d.LogTail)
	}
	if strings.Contains(d.LogTail, "downloading") { // MODEL_ID 默认 none：不下预置模型
		t.Fatalf("MODEL_ID none still downloaded: %s", d.LogTail)
	}
	if slugs, _ := e.bundleManifest(id); !slices.Equal(slugs, []string{a.Slug}) {
		t.Fatalf("bundle manifest = %v", slugs)
	}
	if g, err := os.ReadFile(e.wfFile(id, a.Slug)); err != nil || !json.Valid(g) || !strings.Contains(string(g), e2ehooks.FakeModelA) {
		t.Fatalf("installed workflow file = %q %v", g, err)
	}
	// 用户自己保存的工作流文件（不带 spark- 前缀）一直不动
	user := filepath.Join(e.comfyData(id), "user", "default", "workflows", "mine.json")
	if err := os.WriteFile(user, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// 控制台视图：部署带 workflows
	if v := e.view(id, dep); len(v["workflows"].([]any)) != 1 || v["workflows"].([]any)[0].(map[string]any)["state"] != "installed" {
		t.Fatalf("view workflows = %v", v["workflows"])
	}

	// 添加 B（有自定义节点）
	added := e.c.do("POST", "/instances/"+id+"/apps/"+dep+"/workflows", map[string]any{"workflow_id": b.ID})
	if added["deployment"].(map[string]any)["state"] != "queued" {
		t.Fatalf("add B = %v", added)
	}
	e.wantSet(dep, map[string]string{a.Slug: "installed", b.Slug: "pending"})
	if code, out := e.call("POST", "/instances/"+id+"/apps/"+dep+"/workflows", map[string]any{"workflow_id": b.ID}); code != 409 || out["code"] != "workflow_exists" {
		t.Fatalf("add B twice = %d %v", code, out)
	}
	d = e.untilState(dep, appdeploy.StateRunning)
	e.wantSet(dep, map[string]string{a.Slug: "installed", b.Slug: "installed"})
	if slugs, _ := e.bundleManifest(id); !slices.Equal(slugs, []string{a.Slug, b.Slug}) {
		t.Fatalf("bundle manifest after add = %v", slugs)
	}
	if !exists(e.wfFile(id, b.Slug)) || !exists(e.modelFile(id, e2ehooks.FakeModelB)) {
		t.Fatalf("B not installed in container\nlog:\n%s", d.LogTail)
	}
	if c, err := os.ReadFile(filepath.Join(e.nodeDir(id), ".spark-commit")); err != nil || strings.TrimSpace(string(c)) != e2ehooks.FakeNodeCommit {
		t.Fatalf(".spark-commit = %q %v", c, err)
	}
	if !strings.Contains(d.LogTail, "skip model checkpoints/"+e2ehooks.FakeModelA) {
		t.Fatalf("A's model re-installed:\n%s", d.LogTail)
	}

	// 有新版本 → 更新 A
	nv, err := e2ehooks.PublishFakeVersion(e.ctx, e.a, a.Slug, "1.0.1", "")
	if err != nil {
		t.Fatal(err)
	}
	if w := e.set(dep)[a.Slug]; !w.UpdateAvailable || w.LatestVersion != "1.0.1" {
		t.Fatalf("A before update = %+v", w)
	}
	e.c.do("PUT", e.wfPath(id, dep, a.ID), map[string]any{})
	if w := e.set(dep)[a.Slug]; w.VersionID != nv.VersionID || w.State != "pending" {
		t.Fatalf("A after update = %+v", w)
	}
	e.untilState(dep, appdeploy.StateRunning)
	if w := e.wantSet(dep, map[string]string{a.Slug: "installed", b.Slug: "installed"})[a.Slug]; w.Version != "1.0.1" || w.UpdateAvailable {
		t.Fatalf("A after reinstall = %+v", w)
	}
	if _, vers := e.bundleManifest(id); vers[a.Slug] != "1.0.1" {
		t.Fatalf("bundle versions = %v", vers)
	}

	// 移除 B：容器里的 spark-e2e-wf-b.json 被删，节点目录与模型保留（D8），包里不再有 B 的 graph
	e.c.do("DELETE", e.wfPath(id, dep, b.ID), nil)
	e.wantSet(dep, map[string]string{a.Slug: "installed"})
	d = e.untilState(dep, appdeploy.StateRunning)
	if exists(e.wfFile(id, b.Slug)) || !exists(e.wfFile(id, a.Slug)) || !exists(e.nodeDir(id)) || !exists(e.modelFile(id, e2ehooks.FakeModelB)) || !exists(user) {
		t.Fatalf("after remove B: wf-b %v wf-a %v node %v model-b %v user file %v\nlog:\n%s", exists(e.wfFile(id, b.Slug)), exists(e.wfFile(id, a.Slug)),
			exists(e.nodeDir(id)), exists(e.modelFile(id, e2ehooks.FakeModelB)), exists(user), d.LogTail)
	}
	if slugs, _ := e.bundleManifest(id); !slices.Equal(slugs, []string{a.Slug}) || exists(filepath.Join(e.appDir(id, "comfyui"), "workflows", b.Slug+".json")) {
		t.Fatalf("bundle after remove = %v", slugs)
	}

	// 到期停机 → 续费 → 恢复：集合不变走 skip install
	e.clk.Advance(25 * time.Hour)
	e.agent()
	if err := e.a.Rental.Sweep(e.ctx); err != nil {
		t.Fatal(err)
	}
	e.agent()
	if st := e.c.do("GET", "/instances/"+id, nil)["state"]; st != "stopped" {
		t.Fatalf("expired instance = %v", st)
	}
	e.c.do("POST", "/instances/"+id+"/renew", map[string]string{"duration": "1d"})
	e.agent()
	if d := e.dep(dep); d.State != appdeploy.StateQueued {
		t.Fatalf("after renew = %s", d.State)
	}
	d = e.untilState(dep, appdeploy.StateRunning)
	if !strings.Contains(d.LogTail, "skip install") {
		t.Fatalf("resume did not skip install:\n%s", d.LogTail)
	}
	e.wantSet(dep, map[string]string{a.Slug: "installed"})

	// 换机：新部署集合复制为 pending → 新机器上重装 → installed
	moved, err := e.a.Rental.AdminMigrate(e.ctx, audit.Actor{AdminID: 1}, id, "bad gpu")
	if err != nil {
		t.Fatal(err)
	}
	if d := e.dep(dep); d.State != appdeploy.StateLost {
		t.Fatalf("old deployment = %s", d.State)
	}
	live, err := e.a.Apps.ListLive(e.ctx, moved.ID)
	if err != nil || len(live) != 1 {
		t.Fatalf("migrated deployments = %+v %v", live, err)
	}
	newDep := live[0].ID
	if w := e.wantSet(newDep, map[string]string{a.Slug: "pending"})[a.Slug]; w.VersionID != nv.VersionID {
		t.Fatalf("migrated set = %+v", w)
	}
	e.agent()
	e.untilState(newDep, appdeploy.StateRunning)
	e.wantSet(newDep, map[string]string{a.Slug: "installed"})
	if !exists(e.wfFile(moved.ID, a.Slug)) || !exists(e.modelFile(moved.ID, e2ehooks.FakeModelA)) {
		t.Fatal("workflow not installed on the new instance")
	}

	// 释放 → removed(instance_gone)，集合行保留
	e.c.do("POST", "/instances/"+moved.ID+"/release", map[string]any{})
	if d := e.dep(newDep); d.State != appdeploy.StateRemoved {
		t.Fatalf("after release = %s", d.State)
	}
	if len(e.set(newDep)) != 1 {
		t.Fatalf("set rows dropped on release: %v", e.set(newDep))
	}
}

// 失败链路：清单里有名字含 fail-disk 的模型 → fake install 汇报 "disk: …" 退出 2 → 部署 failed(disk)、该工作流 failed（已装的仍 installed）
// → 移除它 → 重新部署成功 → running，集合里没有 pending / failed。
func TestWorkflowsInstallFailure(t *testing.T) {
	e := newWfEnv(t, 1)
	a := e.wfs[e2ehooks.WorkflowA]
	bad, err := e2ehooks.CreateFakeWorkflow(e.ctx, e.a, e2ehooks.FakeWorkflow{Slug: "e2e-wf-disk", Name: "E2E 磁盘不足", Category: comfywf.CategoryTxt2Img,
		Model: e2ehooks.FailDiskModel, SizeGB: 150})
	if err != nil {
		t.Fatal(err)
	}
	id, dep := e.orderComfy(e2ehooks.WorkflowA)
	e.agent()
	e.untilState(dep, appdeploy.StateRunning)
	e.c.do("POST", "/instances/"+id+"/apps/"+dep+"/workflows", map[string]any{"workflow_id": bad.ID})
	d := e.untilState(dep, appdeploy.StateFailed)
	if d.ErrorCode != appdeploy.ErrCodeDisk || !strings.HasPrefix(d.Message, "disk:") {
		t.Fatalf("failed = %s %q\nlog:\n%s", d.ErrorCode, d.Message, d.LogTail)
	}
	e.wantSet(dep, map[string]string{a.Slug: "installed", bad.Slug: "failed"})
	if v := e.view(id, dep); v["error_code"] != "disk" {
		t.Fatalf("view = %v", v)
	}
	e.c.do("DELETE", e.wfPath(id, dep, bad.ID), nil)
	if d := e.dep(dep); d.State != appdeploy.StateQueued {
		t.Fatalf("after removing the failing workflow = %s", d.State)
	}
	d = e.untilState(dep, appdeploy.StateRunning)
	if d.ErrorCode != "" {
		t.Fatalf("recovered with error %q", d.ErrorCode)
	}
	e.wantSet(dep, map[string]string{a.Slug: "installed"})
	if !exists(e.wfFile(id, a.Slug)) || exists(e.modelFile(id, e2ehooks.FailDiskModel)) {
		t.Fatal("container files after recovery")
	}
}

// 快速出图：经平台代理（Pool.Dial → 假容器 8188 → fake ComfyUI）提交 → prompt 里写入 inputs、种子随机 → 第 3 个在飞的运行 429
// workflow_busy → fake 置 done → 查询得到文件、文件字节透传 → 在飞数下降后可再提交；ComfyUI 拒绝 → 422。
func TestWorkflowsQuickRun(t *testing.T) {
	e := newWfEnv(t, 1)
	a := e.wfs[e2ehooks.WorkflowA]
	id, dep := e.orderComfy(e2ehooks.WorkflowA)
	e.agent()
	e.untilState(dep, appdeploy.StateRunning)
	runs := e.wfPath(id, dep, a.ID) + "/runs"
	r1 := e.c.do("POST", runs, map[string]any{"inputs": map[string]any{"prompt": "a red fox", "steps": 7}})
	r2 := e.c.do("POST", runs, map[string]any{"inputs": map[string]any{}})
	if r1["state"] != "queued" || r1["seed"] == nil || r2["seed"] == nil || r1["seed"] == r2["seed"] {
		t.Fatalf("runs = %v %v", r1, r2)
	}
	if code, out := e.call("POST", runs, map[string]any{"inputs": map[string]any{}}); code != 429 || out["code"] != "workflow_busy" {
		t.Fatalf("third run = %d %v, want 429 workflow_busy", code, out)
	}
	prompts := e.comfy.Prompts()
	if len(prompts) != 2 || prompts[0].ClientID != r1["id"] || prompts[0].ID != r1["prompt_id"] {
		t.Fatalf("prompts = %+v", prompts)
	}
	var p map[string]struct {
		Inputs map[string]any `json:"inputs"`
	}
	if err := json.Unmarshal(prompts[0].Prompt, &p); err != nil {
		t.Fatal(err)
	}
	if p["6"].Inputs["text"] != "a red fox" || p["3"].Inputs["steps"] != 7.0 || p["3"].Inputs["seed"] != r1["seed"] {
		t.Fatalf("submitted prompt = %+v (seed %v)", p, r1["seed"])
	}
	if err := json.Unmarshal(prompts[1].Prompt, &p); err != nil || p["6"].Inputs["text"] != "a cat" || p["3"].Inputs["steps"] != 20.0 {
		t.Fatalf("defaults not applied: %+v %v", p, err)
	}

	get := func(run map[string]any) map[string]any { return e.c.do("GET", runs+"/"+run["id"].(string), nil) }
	if v := get(r1); v["state"] != "queued" {
		t.Fatalf("r1 = %v", v)
	}
	e.comfy.SetHistory(r1["prompt_id"].(string), comfytest.DoneN(2))
	v := get(r1)
	files, _ := v["files"].([]any)
	if v["state"] != "done" || len(files) != 2 {
		t.Fatalf("r1 done = %v", v)
	}
	f0 := files[0].(map[string]any)
	if f0["kind"] != "image" || !strings.HasSuffix(f0["url"].(string), "/runs/"+r1["id"].(string)+"/files/0") {
		t.Fatalf("file = %v", f0)
	}
	resp, err := e.c.http.Get(e.ts.URL + f0["url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" || string(body) != string(comfytest.OnePixelPNG) {
		t.Fatalf("file = %d %s %d bytes", resp.StatusCode, resp.Header.Get("Content-Type"), len(body))
	}
	if code, _ := e.call("GET", runs+"/"+r1["id"].(string)+"/files/5", nil); code != 404 {
		t.Fatalf("file out of range = %d", code)
	}
	// r1 完成：在飞 1 个，可以再提交
	r3 := e.c.do("POST", runs, map[string]any{"inputs": map[string]any{"seed": 42}})
	if r3["seed"] != 42.0 {
		t.Fatalf("r3 = %v", r3)
	}
	e.comfy.SetHistory(r2["prompt_id"].(string), comfytest.ErrorState("CUDA out of memory"))
	if v := get(r2); v["state"] != "error" || !strings.Contains(fmt.Sprint(v["message"]), "CUDA out of memory") {
		t.Fatalf("r2 = %v", v)
	}
	e.comfy.RejectPrompts("value not in list")
	if code, out := e.call("POST", runs, map[string]any{"inputs": map[string]any{}}); code != 422 || out["code"] != "workflow_prompt_rejected" {
		t.Fatalf("rejected = %d %v", code, out)
	}
	// 表单校验：越界 / 未知参数 → 400，不提交
	before := len(e.comfy.Prompts())
	for _, in := range []map[string]any{{"steps": 500}, {"nope": 1}} {
		if code, out := e.call("POST", runs, map[string]any{"inputs": in}); code != 400 || out["code"] != "workflow_input_invalid" {
			t.Fatalf("inputs %v = %d %v", in, code, out)
		}
	}
	if len(e.comfy.Prompts()) != before {
		t.Fatal("invalid inputs reached ComfyUI")
	}
	// 部署不在 running → app_invalid_state
	e.c.do("POST", "/instances/"+id+"/apps/"+dep+"/stop", map[string]any{})
	e.untilState(dep, appdeploy.StateStopped)
	if code, out := e.call("POST", runs, map[string]any{"inputs": map[string]any{}}); code != 409 || out["code"] != "app_invalid_state" {
		t.Fatalf("run on stopped = %d %v", code, out)
	}
}

// assertWorkflowsConsistent 是规格 §14 的工作流一致性检查（按实际状态机收紧 / 放宽后的版本）：running 的部署上没有 pending / failed 行
// （Runner 回写 running 时 pending、failed 都变 installed）；installed 行都有 installed_at；每个部署 ≤ 8 行；集合只挂在 comfyui 部署上。
func assertWorkflowsConsistent(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	checks := []struct{ what, sql string }{
		{"pending / failed workflows on running deployments", `SELECT dw.deployment_id || ' wf ' || dw.workflow_id || ' ' || dw.state
			FROM deployment_workflows dw JOIN app_deployments d ON d.id = dw.deployment_id WHERE d.state = 'running' AND dw.state IN ('pending', 'failed')`},
		{"installed rows without installed_at", `SELECT deployment_id || ' wf ' || workflow_id FROM deployment_workflows WHERE state = 'installed' AND installed_at IS NULL`},
		{"deployments with more than 8 workflows", `SELECT deployment_id || ' ' || count(*) FROM deployment_workflows GROUP BY deployment_id HAVING count(*) > 8`},
		{"workflows on non-comfyui deployments", `SELECT dw.deployment_id || ' ' || a.slug FROM deployment_workflows dw
			JOIN app_deployments d ON d.id = dw.deployment_id JOIN apps a ON a.id = d.app_id WHERE a.slug <> 'comfyui'`},
		{"workflow version not belonging to its workflow", `SELECT dw.deployment_id || ' wf ' || dw.workflow_id FROM deployment_workflows dw
			JOIN comfy_workflow_versions v ON v.id = dw.workflow_version_id WHERE v.workflow_id <> dw.workflow_id`},
	}
	for _, c := range checks {
		rows, err := pool.Query(ctx, c.sql)
		if err != nil {
			t.Errorf("consistency %s: %v", c.what, err)
			continue
		}
		var bad []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Errorf("consistency %s: %v", c.what, err)
			}
			bad = append(bad, s)
		}
		rows.Close()
		if len(bad) > 0 {
			t.Errorf("consistency (§14): %s: %v", c.what, bad)
		}
	}
}
