package e2ehooks_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/comfywf"
	"spark-cloud/internal/e2ehooks"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/rental"
)

// /hooks/workflows/seed 幂等地建 A（有封面、无节点）与 B（FakeNode）；/hooks/workflows/version 发布新版本；
// fake comfyui（1.1.0：supports_workflows、MODEL_ID 默认 none）预装 A、B → tick 到 running → 假容器里有工作流文件 / 模型 / 节点；
// 快速出图经假容器的 8188 到 fake ComfyUI，/hooks/comfy/history 把运行推到 done。
func TestWorkflowHooks(t *testing.T) {
	e := newEnvWith(t, true)
	ctx := context.Background()
	pool := e.a.Fleet.Pool()

	apps := map[string]map[string]any{}
	for _, it := range e.must("POST", "/hooks/apps/seed", map[string]any{})["apps"].([]any) {
		m := it.(map[string]any)
		apps[m["slug"].(string)] = m
	}
	comfyVer, err := e.a.AppCatalog.GetVersion(ctx, int64(apps["comfyui"]["version_id"].(float64)))
	if err != nil {
		t.Fatal(err)
	}
	if comfyVer.Version != "1.1.0" || !comfyVer.Spec.SupportsWorkflows || comfyVer.Spec.Env[0].Default != "none" || comfyVer.Spec.Env[0].Options[0] != "none" {
		t.Fatalf("fake comfyui version = %s spec %+v", comfyVer.Version, comfyVer.Spec)
	}

	// 自愈：A 已存在（草稿、无版本、无封面，例如上次 seed 中途失败）→ seed 补版本、发布、上架、补封面
	if _, err := e.a.Workflows.CreateWorkflow(ctx, audit.Actor{}, comfywf.WorkflowInput{Slug: e2ehooks.WorkflowA, Name: "half-seeded",
		Category: comfywf.CategoryTxt2Img}); err != nil {
		t.Fatal(err)
	}
	seed := func() []any {
		t.Helper()
		return e.must("POST", "/hooks/workflows/seed", map[string]any{})["workflows"].([]any)
	}
	first := seed()
	if len(first) != 2 {
		t.Fatalf("seed = %v", first)
	}
	wf := func(items []any, i int) (id, vid int64, slug, version string) {
		m := items[i].(map[string]any)
		return int64(m["id"].(float64)), int64(m["version_id"].(float64)), m["slug"].(string), m["version"].(string)
	}
	aID, aVer, aSlug, aV := wf(first, 0)
	bID, bVer, bSlug, _ := wf(first, 1)
	if aSlug != e2ehooks.WorkflowA || bSlug != e2ehooks.WorkflowB || aV != "1.0.0" || aID == bID {
		t.Fatalf("seed = %v", first)
	}
	again := seed()
	if a2, av2, _, _ := wf(again, 0); a2 != aID || av2 != aVer {
		t.Fatalf("seed not idempotent: %v then %v", first, again)
	}
	wa, err := e.a.Workflows.GetWorkflow(ctx, aID)
	if err != nil || !wa.HasCover || wa.Status != comfywf.StatusPublished {
		t.Fatalf("A = %+v %v", wa, err)
	}
	vb, err := e.a.Workflows.GetVersion(ctx, bVer)
	if err != nil || vb.Status != comfywf.StatusPublished || len(vb.Manifest.Nodes) != 1 || vb.Manifest.Nodes[0].Commit != strings.Repeat("a", 40) ||
		vb.Manifest.Nodes[0].NodeTypes[0] != "FakeNodeType" || len(vb.APIPrompt) == 0 || len(vb.Manifest.Inputs) != 3 {
		t.Fatalf("B version = %+v %v", vb, err)
	}

	// 新版本
	nv := e.must("POST", "/hooks/workflows/version", map[string]any{})
	if nv["slug"] != e2ehooks.WorkflowA || nv["version"] != "1.0.1" || int64(nv["version_id"].(float64)) == aVer {
		t.Fatalf("version = %v", nv)
	}
	if _, av3, _, _ := wf(seed(), 0); av3 != int64(nv["version_id"].(float64)) {
		t.Fatalf("seed after new version returns version %d, want %v", av3, nv["version_id"])
	}
	// 默认版本号 = 最大 patch + 1
	if nv := e.must("POST", "/hooks/workflows/version", map[string]any{"version": "1.0.5"}); nv["version"] != "1.0.5" {
		t.Fatalf("explicit version = %v", nv)
	}
	if nv := e.must("POST", "/hooks/workflows/version", map[string]any{"slug": e2ehooks.WorkflowA}); nv["version"] != "1.0.6" {
		t.Fatalf("default version after 1.0.5 = %v", nv)
	}
	if code, _ := e.call("POST", "/hooks/workflows/version", map[string]any{"slug": "no-such"}); code != 400 {
		t.Fatalf("version of unknown slug = %d, want 400", code)
	}

	// 预装 A、B 到 fake comfyui
	e.must("POST", "/hooks/machines", map[string]any{"count": 1})
	u := e.must("POST", "/hooks/users", map[string]any{"verified": true, "balance": 100000})
	uid := int64(u["id"].(float64))
	comfyID := int64(apps["comfyui"]["id"].(float64))
	_, inst, err := e.a.Rental.Place(ctx, rental.PlaceRequest{UserID: uid, ProductID: dbtest.ProductID(t, pool),
		ImageID: comfyVer.ImageIDs[0], Duration: catalog.D1d, AppID: &comfyID, AppWorkflowIDs: []int64{aID, bID}})
	if err != nil {
		t.Fatal(err)
	}
	e.must("POST", "/hooks/agent", map[string]any{})
	live, err := e.a.Apps.ListLive(ctx, inst.ID)
	if err != nil || len(live) != 1 {
		t.Fatalf("deployments = %v %v", live, err)
	}
	dep := live[0].ID
	var d appdeploy.Deployment
	for i := 0; i < 60 && d.State != appdeploy.StateRunning; i++ {
		e.must("POST", "/hooks/apps/tick", map[string]any{})
		if d, err = e.a.Apps.Get(ctx, dep); err != nil {
			t.Fatal(err)
		}
		if d.State == appdeploy.StateFailed {
			t.Fatalf("deployment failed: %s %q\n%s", d.ErrorCode, d.Message, d.LogTail)
		}
	}
	if d.State != appdeploy.StateRunning {
		t.Fatalf("deployment = %s, want running\n%s", d.State, d.LogTail)
	}
	installed, err := e.a.WorkflowInstaller.ListInstalled(ctx, dep)
	if err != nil || len(installed) != 2 || installed[0].State != "installed" || installed[1].State != "installed" {
		t.Fatalf("installed = %+v %v", installed, err)
	}
	data := filepath.Join(e.up.Root(), inst.ID, "root", "data", "comfyui")
	for _, f := range []string{"user/default/workflows/spark-e2e-wf-a.json", "user/default/workflows/spark-e2e-wf-b.json",
		"models/checkpoints/e2e-a.safetensors", "models/checkpoints/e2e-b.safetensors"} {
		if _, err := os.Stat(filepath.Join(data, f)); err != nil {
			t.Errorf("container file %s: %v", f, err)
		}
	}
	if b, err := os.ReadFile(filepath.Join(data, "custom_nodes", "FakeNode", ".spark-commit")); err != nil || strings.TrimSpace(string(b)) != strings.Repeat("a", 40) {
		t.Errorf(".spark-commit = %q %v", b, err)
	}

	// 快速出图：经假容器 8188 → fake ComfyUI
	run, err := e.a.WorkflowRuns.Start(ctx, comfywf.StartRequest{UserID: uid, InstanceID: inst.ID, DeploymentID: dep, WorkflowID: aID,
		Inputs: map[string]any{"prompt": "a dog", "steps": 5}})
	if err != nil {
		t.Fatal(err)
	}
	prompts := e.must("GET", "/hooks/comfy/prompts", nil)["prompts"].([]any)
	if len(prompts) != 1 || prompts[0].(map[string]any)["client_id"] != run.ID ||
		!strings.Contains(string(mustJSON(t, prompts[0].(map[string]any)["prompt"])), `"a dog"`) {
		t.Fatalf("prompts = %v", prompts)
	}
	get := func() comfywf.RunView {
		t.Helper()
		v, err := e.a.WorkflowRuns.Get(ctx, comfywf.GetRequest{UserID: uid, InstanceID: inst.ID, DeploymentID: dep, WorkflowID: aID, RunID: run.ID})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if v := get(); v.State != comfywf.RunQueued {
		t.Fatalf("run = %+v, want queued", v)
	}
	if out := e.must("POST", "/hooks/comfy/history", map[string]any{"run_id": run.ID, "state": "running"}); out["prompt_id"] != run.PromptID {
		t.Fatalf("history = %v", out)
	}
	if v := get(); v.State != comfywf.RunRunning {
		t.Fatalf("run = %+v, want running", v)
	}
	e.must("POST", "/hooks/comfy/history", map[string]any{"run_id": run.ID, "state": "done", "files": 2})
	if v := get(); v.State != comfywf.RunDone || len(v.Files) != 2 {
		t.Fatalf("run = %+v, want done with 2 files", v)
	}
	e.must("POST", "/hooks/comfy/history", map[string]any{"prompt_id": run.PromptID, "state": "error", "message": "boom"})
	for _, body := range []map[string]any{{"state": "done"}, {"run_id": "run-nope", "state": "done"}, {"run_id": run.ID, "state": "bogus"},
		{"run_id": run.ID, "state": "done", "files": 9}} {
		if code, _ := e.call("POST", "/hooks/comfy/history", body); code != 400 {
			t.Errorf("history %v = %d, want 400", body, code)
		}
	}
	e.must("POST", "/hooks/comfy/reject", map[string]any{"message": "bad value"})
	if _, err := e.a.WorkflowRuns.Start(ctx, comfywf.StartRequest{UserID: uid, InstanceID: inst.ID, DeploymentID: dep, WorkflowID: aID}); err == nil ||
		!strings.Contains(err.Error(), "bad value") {
		t.Fatalf("start after reject = %v", err)
	}
	e.must("POST", "/hooks/comfy/reset", map[string]any{})
	if n := len(e.must("GET", "/hooks/comfy/prompts", nil)["prompts"].([]any)); n != 0 {
		t.Fatalf("prompts after reset = %d", n)
	}
	// reset 之后仍可按 run_id 设置（从运行记录找 prompt_id）
	e.must("POST", "/hooks/comfy/history", map[string]any{"run_id": run.ID, "state": "done"})
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// /hooks/workflows/seed-many {n} 幂等地建 n 个额外的 fake 工作流 e2e-wf-c、e2e-wf-d…（上架、已发布 1.0.0、无封面无节点），
// 只返回这些额外项；n 超出 1～7 → 400。
func TestWorkflowSeedMany(t *testing.T) {
	e := newEnvWith(t, false)
	ctx := context.Background()
	slugs := func(out map[string]any) []string {
		var s []string
		for _, it := range out["workflows"].([]any) {
			s = append(s, it.(map[string]any)["slug"].(string))
		}
		return s
	}
	first := e.must("POST", "/hooks/workflows/seed-many", map[string]any{"n": 2})
	if got := strings.Join(slugs(first), ","); got != "e2e-wf-c,e2e-wf-d" {
		t.Fatalf("seed-many 2 = %v", got)
	}
	more := e.must("POST", "/hooks/workflows/seed-many", map[string]any{"n": 3})
	if got := strings.Join(slugs(more), ","); got != "e2e-wf-c,e2e-wf-d,e2e-wf-e" {
		t.Fatalf("seed-many 3 = %v", got)
	}
	c0 := first["workflows"].([]any)[0].(map[string]any)
	c1 := more["workflows"].([]any)[0].(map[string]any)
	if c0["id"] != c1["id"] || c0["version_id"] != c1["version_id"] || c1["version"] != "1.0.0" {
		t.Fatalf("not idempotent: %v then %v", c0, c1)
	}
	w, err := e.a.Workflows.GetWorkflow(ctx, int64(c1["id"].(float64)))
	if err != nil || w.Status != comfywf.StatusPublished || w.HasCover || w.Category != comfywf.CategoryOther || w.Name != "E2E 工作流 C" ||
		w.Sort != 100 || w.Summary != "fake 额外工作流（上限用例）" || w.Slug != "e2e-wf-c" {
		t.Fatalf("C = %+v %v", w, err)
	}
	v, err := e.a.Workflows.GetVersion(ctx, int64(c1["version_id"].(float64)))
	if err != nil || len(v.Manifest.Nodes) != 0 || v.Manifest.Models[0].Name != "e2e-c.safetensors" || len(v.Manifest.Inputs) != 3 {
		t.Fatalf("C version = %+v %v", v, err)
	}
	if e := e2ehooks.ExtraWorkflow(6); e.Slug != "e2e-wf-i" || e.Sort != 106 || e.Model != "e2e-i.safetensors" {
		t.Fatalf("ExtraWorkflow(6) = %+v", e)
	}
	for _, i := range []int{-1, e2ehooks.MaxExtraWorkflows} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("ExtraWorkflow(%d) did not panic", i)
				}
			}()
			e2ehooks.ExtraWorkflow(i)
		}()
	}
	for _, n := range []int{0, 8} {
		if code, _ := e.call("POST", "/hooks/workflows/seed-many", map[string]any{"n": n}); code != 400 {
			t.Errorf("seed-many %d = %d, want 400", n, code)
		}
	}
}
