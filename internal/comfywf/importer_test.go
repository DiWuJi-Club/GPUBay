package comfywf

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// copyDir 把 testdata/import/<name> 复制成 root/<as>（只有普通文件）。
func copyDir(t *testing.T, name, root, as string) string {
	t.Helper()
	src := filepath.Join("testdata", "import", name)
	dst := filepath.Join(root, as)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, ent := range ents {
		b, err := os.ReadFile(filepath.Join(src, ent.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, ent.Name()), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

func setVersion(t *testing.T, dir, from, to string) {
	t.Helper()
	p := filepath.Join(dir, "workflow.json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Replace(string(b), `"version": "`+from+`"`, `"version": "`+to+`"`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestImportDir(t *testing.T) {
	e := newCatEnv(t)
	root := t.TempDir()
	demo := copyDir(t, "sdxl-demo", root, "sdxl-demo")
	noAPI := copyDir(t, "sdxl-demo", root, "zz-no-api")
	if err := os.Remove(filepath.Join(noAPI, "api.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(noAPI, "cover.png")); err != nil {
		t.Fatal(err)
	}
	// 没有 api.json 时清单里不能有 inputs / outputs
	if err := os.WriteFile(filepath.Join(noAPI, "manifest.json"), []byte(`{"models":[{"name":"sd_xl_base_1.0.safetensors","dir":"checkpoints","size_gb":6.94,
		"hf":{"repo":"stabilityai/stable-diffusion-xl-base-1.0","file":"sd_xl_base_1.0.safetensors"}}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("ignored"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".hidden"), 0o755); err != nil {
		t.Fatal(err)
	}

	res, err := e.cat.ImportDir(e.ctx, e.actor(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 || res[0] != (ImportResult{Slug: "sdxl-demo", Version: "1.0.0", Created: true}) ||
		res[1] != (ImportResult{Slug: "zz-no-api", Version: "1.0.0", Created: true}) {
		t.Fatalf("first import = %+v", res)
	}
	w, err := e.cat.WorkflowBySlug(e.ctx, "sdxl-demo")
	if err != nil {
		t.Fatal(err)
	}
	if w.Status != StatusDraft || w.Name != "SDXL 演示" || w.Category != CategoryTxt2Img || w.Sort != 10 || strings.Join(w.Tags, ",") != "SDXL,文生图" ||
		!w.HasCover || !strings.Contains(w.Description, "测试用") {
		t.Fatalf("imported workflow = %+v", w)
	}
	up, rc, err := e.cat.OpenCoverByID(e.ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(rc)
	rc.Close()
	if up.ContentType != "image/png" || len(body) == 0 {
		t.Fatalf("cover = %+v", up)
	}
	vs, err := e.cat.ListVersions(e.ctx, w.ID)
	if err != nil || len(vs) != 1 {
		t.Fatalf("versions = %+v, %v", vs, err)
	}
	v := vs[0]
	if v.Status != StatusDraft || v.Changelog != "首个版本" || v.Manifest.ComfyUIMin != "0.3.60" || len(v.Manifest.Inputs) != 6 || v.APIPrompt == nil ||
		v.DownloadGB != "6.94" {
		t.Fatalf("imported version = %+v", v)
	}
	// 种子默认值等大整数原样保留（json.Number），可直接发布
	if _, err := e.cat.PublishVersion(e.ctx, e.actor(), w.ID, v.ID); err != nil {
		t.Fatalf("publish imported: %v", err)
	}
	nw, _ := e.cat.WorkflowBySlug(e.ctx, "zz-no-api")
	if nvs, _ := e.cat.ListVersions(e.ctx, nw.ID); len(nvs) != 1 || nvs[0].APIPrompt != nil || nw.HasCover {
		t.Fatalf("no-api import = %+v / %+v", nw, nvs)
	}

	// 第二次：sdxl-demo 加版本 1.1.0；zz-no-api 版本号重复 → 该项报错但不影响其它；符号链接跳过；坏目录报错
	setVersion(t, demo, "1.0.0", "1.1.0")
	if err := os.WriteFile(filepath.Join(demo, "workflow.json"), []byte(strings.Replace(mustRead(t, filepath.Join(demo, "workflow.json")),
		`"name": "SDXL 演示"`, `"name": "改过的名字"`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(demo, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	fileLink := copyDir(t, "sdxl-demo", root, "file-link")
	if err := os.Remove(filepath.Join(fileLink, "graph.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(demo, "graph.json"), filepath.Join(fileLink, "graph.json")); err != nil {
		t.Fatal(err)
	}
	bad := copyDir(t, "sdxl-demo", root, "bad-field")
	if err := os.WriteFile(filepath.Join(bad, "workflow.json"), []byte(`{"name":"x","category":"txt2img","version":"1.0.0","typo":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	invalidVer := copyDir(t, "sdxl-demo", root, "bad-manifest")
	if err := os.WriteFile(filepath.Join(invalidVer, "manifest.json"), []byte(`{"models":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	twoCovers := copyDir(t, "sdxl-demo", root, "two-covers")
	if err := os.WriteFile(filepath.Join(twoCovers, "cover.webp"), pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	res, err = e.cat.ImportDir(e.ctx, e.actor(), root)
	if err == nil || !strings.Contains(err.Error(), "4 个") {
		t.Fatalf("second import err = %v", err)
	}
	got := map[string]ImportResult{}
	for _, r := range res {
		got[r.Slug] = r
	}
	if len(res) != 7 {
		t.Fatalf("results = %+v", res)
	}
	if r := got["sdxl-demo"]; r.Err != nil || r.Created || r.Version != "1.1.0" {
		t.Fatalf("sdxl-demo = %+v", r)
	}
	if r := got["zz-no-api"]; errCode(r.Err) != "duplicate" || r.Version != "1.0.0" {
		t.Fatalf("zz-no-api = %+v", r)
	}
	if r := got["link"]; r.Skipped == "" || r.Err != nil {
		t.Fatalf("link = %+v", r)
	}
	if r := got["file-link"]; !strings.Contains(r.Skipped, "graph.json 是符号链接") {
		t.Fatalf("file-link = %+v", r)
	}
	if r := got["bad-field"]; r.Err == nil || !strings.Contains(r.Err.Error(), "typo") {
		t.Fatalf("bad-field = %+v", r)
	}
	if r := got["bad-manifest"]; errCode(r.Err) != "workflow_version_invalid" {
		t.Fatalf("bad-manifest = %+v", r)
	}
	if r := got["two-covers"]; r.Err == nil || !strings.Contains(r.Err.Error(), "封面只能有一个") {
		t.Fatalf("two-covers = %+v", r)
	}
	// 已有工作流只加版本：展示信息不动；失败的目录没有留下工作流 / 封面
	w2, _ := e.cat.WorkflowBySlug(e.ctx, "sdxl-demo")
	if w2.Name != "SDXL 演示" || *w2.CoverKey != *w.CoverKey {
		t.Fatalf("existing workflow changed: %+v", w2)
	}
	if n := e.count(`SELECT count(*) FROM comfy_workflows`); n != 2 {
		t.Fatalf("workflows = %d", n)
	}
	if n := e.count(`SELECT count(*) FROM comfy_workflow_versions`); n != 3 {
		t.Fatalf("versions = %d", n)
	}
	if n := e.count(`SELECT count(*) FROM uploads`); n != 1 {
		t.Fatalf("uploads = %d", n)
	}
	if _, err := e.cat.ImportDir(e.ctx, e.actor(), filepath.Join(root, "missing")); err == nil {
		t.Fatal("missing dir must fail")
	}
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// 导入的版本号重复时不建工作流、不存封面（新 slug 里不会发生；这里验证校验在建工作流之前）。
func TestImportInvalidLeavesNothing(t *testing.T) {
	e := newCatEnv(t)
	root := t.TempDir()
	d := copyDir(t, "sdxl-demo", root, "Bad_Slug")
	_ = d
	res, err := e.cat.ImportDir(e.ctx, e.actor(), root)
	if err == nil || len(res) != 1 || errCode(res[0].Err) != "bad_request" {
		t.Fatalf("bad slug = %+v, %v", res, err)
	}
	if n := e.count(`SELECT count(*) FROM uploads`) + e.count(`SELECT count(*) FROM comfy_workflows`); n != 0 {
		t.Fatalf("rows left = %d", n)
	}
}
