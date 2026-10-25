package comfywf

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/platform/apperr"
)

func TestWorkflowCRUD(t *testing.T) {
	e := newCatEnv(t)
	w, err := e.cat.CreateWorkflow(e.ctx, e.actor(), WorkflowInput{Slug: " sdxl-txt2img ", Name: " SDXL 文生图 ", Summary: "  简介 ",
		Category: "txt2img", Tags: []string{" SDXL", "", "SDXL", "文生图"}, Sort: 5})
	if err != nil {
		t.Fatal(err)
	}
	if w.Slug != "sdxl-txt2img" || w.Name != "SDXL 文生图" || w.Summary != "简介" || w.Status != StatusDraft || w.Sort != 5 ||
		strings.Join(w.Tags, ",") != "SDXL,文生图" || w.HasCover || w.CoverKey != nil || !w.CreatedAt.Equal(e.clk.Now()) {
		t.Fatalf("created = %+v", w)
	}
	if n := e.count(`SELECT count(*) FROM audit_logs WHERE action = 'workflow.create' AND target_id = $1`, target(w.ID)); n != 1 {
		t.Fatalf("workflow.create audits = %d", n)
	}

	long := func(n int) string { return strings.Repeat("字", n) }
	nineTags := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}
	for name, in := range map[string]WorkflowInput{
		"slug uppercase":   {Slug: "SDXL", Name: "x", Category: "txt2img"},
		"slug too short":   {Slug: "a", Name: "x", Category: "txt2img"},
		"slug too long":    {Slug: "a" + strings.Repeat("b", 48), Name: "x", Category: "txt2img"},
		"slug digit first": {Slug: "1abc", Name: "x", Category: "txt2img"},
		"no name":          {Slug: "ok-slug", Name: "  ", Category: "txt2img"},
		"name 49":          {Slug: "ok-slug", Name: long(49), Category: "txt2img"},
		"summary 121":      {Slug: "ok-slug", Name: "x", Summary: long(121), Category: "txt2img"},
		"desc 8001":        {Slug: "ok-slug", Name: "x", Description: long(8001), Category: "txt2img"},
		"bad category":     {Slug: "ok-slug", Name: "x", Category: "llm"},
		"nine tags":        {Slug: "ok-slug", Name: "x", Category: "txt2img", Tags: nineTags},
		"tag 17":           {Slug: "ok-slug", Name: "x", Category: "txt2img", Tags: []string{long(17)}},
		"bad status":       {Slug: "ok-slug", Name: "x", Category: "txt2img", Status: "hidden"},
		"sort range":       {Slug: "ok-slug", Name: "x", Category: "txt2img", Sort: 10001},
		"name control":     {Slug: "ok-slug", Name: "a\x07b", Category: "txt2img"},
		"name newline":     {Slug: "ok-slug", Name: "a\nb", Category: "txt2img"},
		"name NUL":         {Slug: "ok-slug", Name: "a\x00b", Category: "txt2img"},
		"name bad utf8":    {Slug: "ok-slug", Name: "a\xffb", Category: "txt2img"},
		"summary tab":      {Slug: "ok-slug", Name: "x", Summary: "a\tb", Category: "txt2img"},
		"tag control":      {Slug: "ok-slug", Name: "x", Category: "txt2img", Tags: []string{"a\x07b"}},
		"desc control":     {Slug: "ok-slug", Name: "x", Description: "a\x1bb", Category: "txt2img"},
	} {
		if _, err := e.cat.CreateWorkflow(e.ctx, e.actor(), in); errCode(err) != "bad_request" {
			t.Errorf("%s: err = %v, want bad_request", name, err)
		}
	}
	// 边界值可以
	if _, err := e.cat.CreateWorkflow(e.ctx, e.actor(), WorkflowInput{Slug: "a" + strings.Repeat("b", 47), Name: long(48), Summary: long(120),
		Description: long(8000), Category: "other", Tags: append(nineTags[:7:7], long(16)), Status: StatusRetired}); err != nil {
		t.Fatalf("boundary values: %v", err)
	}
	// 说明允许换行与制表符（\r\n 规范为 \n）
	md, err := e.cat.CreateWorkflow(e.ctx, e.actor(), WorkflowInput{Slug: "multi-line", Name: "x", Description: "第一行\r\n\t- 第二行", Category: "other"})
	if err != nil || md.Description != "第一行\n\t- 第二行" {
		t.Fatalf("multi-line description = %q, %v", md.Description, err)
	}
	if _, err := e.cat.CreateWorkflow(e.ctx, e.actor(), WorkflowInput{Slug: "sdxl-txt2img", Name: "dup", Category: "other"}); errCode(err) != "duplicate" {
		t.Fatalf("duplicate slug: %v", err)
	}

	// 更新：slug 不可改（被忽略），其余字段可改，审计 before / after
	e.clk.Advance(time.Minute)
	u, err := e.cat.UpdateWorkflow(e.ctx, e.actor(), w.ID, WorkflowInput{Slug: "sdxl-txt2img", Name: "新名字", Category: "img2img", Tags: nil, Status: StatusPublished})
	if err != nil {
		t.Fatal(err)
	}
	if u.Slug != "sdxl-txt2img" || u.Name != "新名字" || u.Category != "img2img" || len(u.Tags) != 0 || u.Tags == nil || u.Status != StatusPublished ||
		!u.UpdatedAt.Equal(e.clk.Now()) {
		t.Fatalf("updated = %+v", u)
	}
	if _, err := e.cat.UpdateWorkflow(e.ctx, e.actor(), w.ID, WorkflowInput{Slug: "renamed", Name: "x", Category: "other"}); errCode(err) != "bad_request" ||
		!strings.Contains(err.Error(), "slug 不可修改") {
		t.Fatalf("rename slug: %v", err)
	}
	if _, err := e.cat.UpdateWorkflow(e.ctx, e.actor(), 9999, WorkflowInput{Name: "x", Category: "other"}); errCode(err) != "workflow_not_found" {
		t.Fatalf("update missing: %v", err)
	}
	if _, err := e.cat.UpdateWorkflow(e.ctx, e.actor(), w.ID, WorkflowInput{Name: "x", Category: "nope"}); errCode(err) != "bad_request" {
		t.Fatalf("update invalid: %v", err)
	}
	if n := e.count(`SELECT count(*) FROM audit_logs WHERE action = 'workflow.update' AND diff->'before'->>'name' = 'SDXL 文生图' AND diff->'after'->>'name' = '新名字'`); n != 1 {
		t.Fatalf("workflow.update audits = %d", n)
	}
	got, err := e.cat.WorkflowBySlug(e.ctx, "sdxl-txt2img")
	if err != nil || got.ID != w.ID {
		t.Fatalf("by slug = %+v, %v", got, err)
	}
	if _, err := e.cat.GetWorkflow(e.ctx, 9999); errCode(err) != "workflow_not_found" {
		t.Fatalf("get missing: %v", err)
	}
	if _, err := e.cat.WorkflowBySlug(e.ctx, "nope"); errCode(err) != "workflow_not_found" {
		t.Fatalf("by slug missing: %v", err)
	}

	// 删除：只允许 draft 且无版本
	wantCode(t, e.cat.DeleteWorkflow(e.ctx, e.actor(), w.ID), "invalid_state") // published
	d := e.workflow("draft-with-version", "")
	e.version(d.ID, "1.0.0")
	wantCode(t, e.cat.DeleteWorkflow(e.ctx, e.actor(), d.ID), "invalid_state")
	empty := e.workflow("draft-empty", "")
	if err := e.cat.DeleteWorkflow(e.ctx, e.actor(), empty.ID); err != nil {
		t.Fatal(err)
	}
	wantCode(t, e.cat.DeleteWorkflow(e.ctx, e.actor(), empty.ID), "workflow_not_found")
	if n := e.count(`SELECT count(*) FROM audit_logs WHERE action = 'workflow.delete' AND diff->>'slug' = 'draft-empty'`); n != 1 {
		t.Fatalf("workflow.delete audits = %d", n)
	}
}

func TestDeleteWorkflowRemovesCover(t *testing.T) {
	e := newCatEnv(t)
	w := e.workflow("with-cover", "")
	w, err := e.cat.SetCover(e.ctx, e.actor(), w.ID, bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.cat.DeleteWorkflow(e.ctx, e.actor(), w.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.uploads.Open(e.ctx, *w.CoverKey); errCode(err) != "not_found" {
		t.Fatalf("cover after delete: %v", err)
	}
}

func TestVersionDraftValidation(t *testing.T) {
	e := newCatEnv(t)
	w := e.workflow("wf", "")
	bad := goodVersion(t, "1.0.0")
	bad.Manifest.Models = nil // graph 引用的模型不在清单里（D16）
	_, err := e.cat.CreateVersion(e.ctx, e.actor(), w.ID, bad)
	wantInvalid(t, err, "")
	for name, mut := range map[string]func(*VersionInput){
		"bad version":    func(in *VersionInput) { in.Version = "-1" },
		"empty graph":    func(in *VersionInput) { in.Graph = nil },
		"changelog 1001": func(in *VersionInput) { in.Changelog = ptr(strings.Repeat("字", 1001)) },
		"bad api":        func(in *VersionInput) { in.APIPrompt = json.RawMessage(`[1]`) },
		"changelog ctrl": func(in *VersionInput) { in.Changelog = ptr("a\x00b") },
		"graph NUL": func(in *VersionInput) {
			in.Graph = json.RawMessage(strings.Replace(string(in.Graph), `"last_node_id"`, `"x\u0000":1,"last_node_id"`, 1))
		},
		"graph bad utf8": func(in *VersionInput) {
			in.Graph = json.RawMessage(strings.Replace(string(in.Graph), `"last_node_id"`, "\"x\xff\":1,\"last_node_id\"", 1))
		},
		"api NUL": func(in *VersionInput) {
			in.APIPrompt = json.RawMessage(strings.Replace(string(in.APIPrompt), `"KSampler"}`, `"KSampler\u0000"}`, 1))
		},
		"manifest NUL": func(in *VersionInput) { in.Manifest.Inputs[0].Default = "a\x00" },
	} {
		in := goodVersion(t, "1.0.0")
		mut(&in)
		if _, err := e.cat.CreateVersion(e.ctx, e.actor(), w.ID, in); errCode(err) != "workflow_version_invalid" {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if n := e.count(`SELECT count(*) FROM comfy_workflow_versions`); n != 0 {
		t.Fatalf("invalid drafts saved: %d", n)
	}
	// NUL / 非法 UTF-8 的报错指向字段（不是 PostgreSQL 的原始错误）
	nul := goodVersion(t, "1.0.0")
	nul.Graph = json.RawMessage(strings.Replace(string(nul.Graph), `"last_node_id"`, `"x\u0000":1,"last_node_id"`, 1))
	_, err = e.cat.CreateVersion(e.ctx, e.actor(), w.ID, nul)
	wantInvalid(t, err, "graph")
	// 转义的反斜杠后跟 u0000 不是 NUL，照常保存
	esc := goodVersion(t, "0.0.9")
	esc.Graph = json.RawMessage(strings.Replace(string(esc.Graph), `"last_node_id"`, `"note":"C:\\u0000","last_node_id"`, 1))
	if _, err := e.cat.CreateVersion(e.ctx, e.actor(), w.ID, esc); err != nil {
		t.Fatalf("escaped backslash: %v", err)
	}
	e.exec(`DELETE FROM comfy_workflow_versions`)
	if _, err := e.cat.CreateVersion(e.ctx, e.actor(), 9999, goodVersion(t, "1.0.0")); errCode(err) != "workflow_not_found" {
		t.Fatalf("missing workflow: %v", err)
	}

	// 合法草稿：规范化后保存，manifest_hash / download_gb 已算好；输入的切片不被改动
	in := goodVersion(t, " 1.0.0 ")
	in.Manifest.Models[0].Dir = " Checkpoints "
	in.Manifest.Models[0].SizeGB = 6.938
	var pretty bytes.Buffer
	_ = json.Indent(&pretty, in.Graph, "", "    ")
	in.Graph = pretty.Bytes()
	v, err := e.cat.CreateVersion(e.ctx, e.actor(), w.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	if in.Manifest.Models[0].Dir != " Checkpoints " {
		t.Fatal("caller manifest mutated")
	}
	if v.Version != "1.0.0" || v.Status != StatusDraft || v.Manifest.Models[0].Dir != "checkpoints" || v.DownloadGB != "6.94" ||
		v.PublishedAt != nil || v.ManifestHash != ManifestHash(fixture(t, "graph_sdxl_ok.json"), fixture(t, "api_sdxl.json"), v.Manifest) ||
		bytes.Contains(v.Graph, []byte("\n")) || v.Changelog != "说明  1.0.0" {
		t.Fatalf("draft = %+v", v)
	}
	if _, err := e.cat.CreateVersion(e.ctx, e.actor(), w.ID, goodVersion(t, "1.0.0")); errCode(err) != "duplicate" {
		t.Fatalf("duplicate version: %v", err)
	}
	if n := e.count(`SELECT count(*) FROM audit_logs WHERE action = 'workflow.version.create' AND diff->>'manifest_hash' = $1`, v.ManifestHash); n != 1 {
		t.Fatalf("workflow.version.create audits = %d", n)
	}

	// 无 api_prompt（"null"）：存 NULL，不支持快速出图
	noAPI := VersionInput{Version: "1.0.1", Graph: fixture(t, "graph_sdxl_ok.json"), APIPrompt: json.RawMessage("null"),
		Manifest: Manifest{Models: okManifest().Models}}
	v2, err := e.cat.CreateVersion(e.ctx, e.actor(), w.ID, noAPI)
	if err != nil {
		t.Fatal(err)
	}
	if v2.APIPrompt != nil || e.count(`SELECT count(*) FROM comfy_workflow_versions WHERE id = $1 AND api_prompt IS NULL`, v2.ID) != 1 {
		t.Fatalf("api_prompt = %s", v2.APIPrompt)
	}

	// 草稿更新也做完整校验
	upd := goodVersion(t, "1.0.2")
	upd.Manifest.Models = nil
	_, err = e.cat.UpdateVersion(e.ctx, e.actor(), w.ID, v.ID, upd)
	wantInvalid(t, err, "")
	upd = goodVersion(t, "1.0.1") // 改成已有版本号
	if _, err := e.cat.UpdateVersion(e.ctx, e.actor(), w.ID, v.ID, upd); errCode(err) != "duplicate" {
		t.Fatalf("rename to existing: %v", err)
	}
	upd = goodVersion(t, "1.0.2")
	upd.Status = StatusPublished
	if _, err := e.cat.UpdateVersion(e.ctx, e.actor(), w.ID, v.ID, upd); errCode(err) != "invalid_state" {
		t.Fatalf("publish via update: %v", err)
	}
	upd.Status = ""
	upd.APIPrompt = nil
	upd.Manifest = Manifest{Models: okManifest().Models}
	e.clk.Advance(time.Minute)
	v3, err := e.cat.UpdateVersion(e.ctx, e.actor(), w.ID, v.ID, upd)
	if err != nil {
		t.Fatal(err)
	}
	if v3.Version != "1.0.2" || v3.APIPrompt != nil || len(v3.Manifest.Inputs) != 0 || v3.ManifestHash == v.ManifestHash || !v3.UpdatedAt.Equal(e.clk.Now()) {
		t.Fatalf("updated draft = %+v", v3)
	}
	if _, err := e.cat.UpdateVersion(e.ctx, e.actor(), w.ID+1, v.ID, upd); errCode(err) != "not_found" {
		t.Fatalf("version of another workflow: %v", err)
	}
}

func TestPublishFreezeRetire(t *testing.T) {
	e := newCatEnv(t)
	w := e.workflow("wf", "")
	d := e.version(w.ID, "1.0.0")
	// 草稿被改坏（模拟规则收紧）：发布时重新校验
	e.exec(`UPDATE comfy_workflow_versions SET manifest = '{"models":[]}' WHERE id = $1`, d.ID)
	_, err := e.cat.PublishVersion(e.ctx, e.actor(), w.ID, d.ID)
	wantInvalid(t, err, "")
	e.exec(`UPDATE comfy_workflow_versions SET manifest = $2, download_gb = 0, manifest_hash = 'stale' WHERE id = $1`, d.ID, mustJSON(t, okManifest()))

	e.clk.Advance(time.Hour)
	p, err := e.cat.PublishVersion(e.ctx, e.actor(), w.ID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	norm := okManifest()
	if err := ValidateManifest(&norm, fixture(t, "graph_sdxl_ok.json"), fixture(t, "api_sdxl.json")); err != nil {
		t.Fatal(err)
	}
	if p.Status != StatusPublished || p.PublishedAt == nil || !p.PublishedAt.Equal(e.clk.Now()) || p.DownloadGB != "6.94" ||
		p.ManifestHash != ManifestHash(fixture(t, "graph_sdxl_ok.json"), fixture(t, "api_sdxl.json"), norm) {
		t.Fatalf("published = %+v", p)
	}
	if n := e.count(`SELECT count(*) FROM audit_logs WHERE action = 'workflow.version.publish' AND diff->>'manifest_hash' = $1`, p.ManifestHash); n != 1 {
		t.Fatalf("publish audits = %d", n)
	}
	if _, err := e.cat.PublishVersion(e.ctx, e.actor(), w.ID, d.ID); errCode(err) != "invalid_state" {
		t.Fatalf("publish twice: %v", err)
	}
	if _, err := e.cat.PublishVersion(e.ctx, e.actor(), w.ID, 9999); errCode(err) != "not_found" {
		t.Fatalf("publish missing: %v", err)
	}

	// 已发布：改内容 → frozen；只改 changelog 可以；给出与当前相同的内容也可以
	for name, mut := range map[string]func(*VersionInput){
		"version":  func(in *VersionInput) { in.Version = "1.0.1" },
		"graph":    func(in *VersionInput) { in.Graph = json.RawMessage(`{"nodes":[],"links":[]}`) },
		"api":      func(in *VersionInput) { in.APIPrompt = json.RawMessage("null") },
		"manifest": func(in *VersionInput) { in.Manifest.Models[0].SizeGB = 7 },
		"status":   func(in *VersionInput) { in.Status = StatusDraft },
	} {
		in := goodVersion(t, "1.0.0")
		mut(&in)
		if _, err := e.cat.UpdateVersion(e.ctx, e.actor(), w.ID, d.ID, in); errCode(err) != "workflow_version_frozen" {
			t.Errorf("%s: err = %v", name, err)
		}
		if _, err := e.cat.ValidateVersionUpdate(e.ctx, w.ID, d.ID, in); errCode(err) != "workflow_version_frozen" {
			t.Errorf("validate %s: err = %v", name, err)
		}
	}
	same := goodVersion(t, "1.0.0")
	same.Manifest.Models[0].Dir = "CHECKPOINTS" // 规范化后相同
	same.Changelog = ptr("新说明")
	v, err := e.cat.UpdateVersion(e.ctx, e.actor(), w.ID, d.ID, same)
	if err != nil {
		t.Fatal(err)
	}
	if v.Changelog != "新说明" || v.Status != StatusPublished || v.ManifestHash != p.ManifestHash {
		t.Fatalf("changelog edit = %+v", v)
	}
	res, err := e.cat.ValidateVersionUpdate(e.ctx, w.ID, d.ID, VersionInput{Changelog: ptr("x")})
	if err != nil || res.ManifestHash != p.ManifestHash || res.DownloadGB != 6.94 {
		t.Fatalf("validate published = %+v, %v", res, err)
	}
	if _, err := e.cat.UpdateVersion(e.ctx, e.actor(), w.ID, d.ID, VersionInput{Changelog: ptr(strings.Repeat("字", 1001))}); errCode(err) != "workflow_version_invalid" {
		t.Fatalf("long changelog: %v", err)
	}
	r, err := e.cat.UpdateVersion(e.ctx, e.actor(), w.ID, d.ID, VersionInput{Status: StatusRetired})
	if err != nil || r.Status != StatusRetired || r.Changelog != "新说明" {
		t.Fatalf("retire = %+v, %v", r, err)
	}
	if n := e.count(`SELECT count(*) FROM audit_logs WHERE action = 'workflow.version.update' AND diff->'after'->>'status' = 'retired'`); n != 1 {
		t.Fatalf("retire audits = %d", n)
	}
	// 已下线：不能再改
	if _, err := e.cat.UpdateVersion(e.ctx, e.actor(), w.ID, d.ID, VersionInput{Changelog: ptr("x")}); errCode(err) != "workflow_version_frozen" {
		t.Fatalf("edit retired: %v", err)
	}
	if _, err := e.cat.ValidateVersionUpdate(e.ctx, w.ID, d.ID, VersionInput{}); errCode(err) != "workflow_version_frozen" {
		t.Fatalf("validate retired: %v", err)
	}
	if _, err := e.cat.LatestPublished(e.ctx, e.pool, w.ID); errCode(err) != "workflow_unavailable" {
		t.Fatalf("latest after retire: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCopyFromAndValidate(t *testing.T) {
	e := newCatEnv(t)
	w := e.workflow("wf", "")
	other := e.workflow("other", "")
	base := e.published(w.ID, "1.0.0")
	foreign := e.version(other.ID, "1.0.0")

	// 复制：graph / api / manifest 全取基底，changelog 不复制
	cp, err := e.cat.CreateVersion(e.ctx, e.actor(), w.ID, VersionInput{Version: "1.1.0", CopyFrom: &base.ID})
	if err != nil {
		t.Fatal(err)
	}
	if cp.Status != StatusDraft || cp.ManifestHash != base.ManifestHash || cp.Changelog != "" || len(cp.Manifest.Inputs) != len(base.Manifest.Inputs) {
		t.Fatalf("copy = %+v", cp)
	}
	// 复制但覆盖：api_prompt 显式 null + 不带 inputs 的清单
	cp2, err := e.cat.CreateVersion(e.ctx, e.actor(), w.ID, VersionInput{Version: "1.2.0", CopyFrom: &base.ID, APIPrompt: json.RawMessage("null"),
		Manifest: Manifest{Models: okManifest().Models}})
	if err != nil {
		t.Fatal(err)
	}
	if cp2.APIPrompt != nil || len(cp2.Manifest.Inputs) != 0 {
		t.Fatalf("copy override = %+v", cp2)
	}
	if _, err := e.cat.CreateVersion(e.ctx, e.actor(), w.ID, VersionInput{Version: "1.3.0", CopyFrom: &foreign.ID}); errCode(err) != "not_found" {
		t.Fatalf("copy from another workflow: %v", err)
	}
	if n := e.count(`SELECT count(*) FROM audit_logs WHERE action = 'workflow.version.create' AND (diff->>'copy_from')::bigint = $1`, base.ID); n != 2 {
		t.Fatalf("copy audits = %d", n)
	}

	// ValidateVersion 不落库，返回 hash / 下载量 / 提示
	before := e.count(`SELECT count(*) FROM comfy_workflow_versions`)
	res, err := e.cat.ValidateVersion(e.ctx, w.ID, VersionInput{Version: "9.9.9", CopyFrom: &base.ID})
	if err != nil {
		t.Fatal(err)
	}
	if res.ManifestHash != base.ManifestHash || res.DownloadGB != 6.94 || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "sha256") {
		t.Fatalf("validate = %+v", res)
	}
	if e.count(`SELECT count(*) FROM comfy_workflow_versions`) != before {
		t.Fatal("ValidateVersion saved a row")
	}
	in := goodVersion(t, "9.9.9")
	in.APIPrompt = nil
	in.Manifest = Manifest{Models: []ModelSpec{okManifest().Models[0], upscaleModel()}}
	in.Manifest.Models[1].Name = "unused.pth"
	res, err = e.cat.ValidateVersion(e.ctx, w.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	if res.DownloadGB != 7.01 || len(res.Warnings) != 4 || !strings.Contains(res.Warnings[0], "快速出图") {
		t.Fatalf("validate warnings = %+v", res)
	}
	in.Manifest.Models = nil
	_, err = e.cat.ValidateVersion(e.ctx, w.ID, in)
	wantInvalid(t, err, "")
	if _, err := e.cat.ValidateVersion(e.ctx, 9999, goodVersion(t, "1")); errCode(err) != "workflow_not_found" {
		t.Fatalf("validate missing workflow: %v", err)
	}
	b, _ := json.Marshal(res)
	if !strings.Contains(string(b), `"warnings":[`) {
		t.Fatalf("result json = %s", b)
	}
}

func TestLatestPublishedByPublishedAt(t *testing.T) {
	e := newCatEnv(t)
	w := e.workflow("wf", "")
	if _, err := e.cat.LatestPublished(e.ctx, e.pool, w.ID); errCode(err) != "workflow_unavailable" {
		t.Fatalf("no versions: %v", err)
	}
	v2 := e.version(w.ID, "2.0.0")
	v10 := e.version(w.ID, "1.10.0")
	e.version(w.ID, "3.0.0") // 草稿不算
	if _, err := e.cat.PublishVersion(e.ctx, e.actor(), w.ID, v10.ID); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(time.Hour)
	if _, err := e.cat.PublishVersion(e.ctx, e.actor(), w.ID, v2.ID); err != nil {
		t.Fatal(err)
	}
	got, err := e.cat.LatestPublished(e.ctx, e.pool, w.ID)
	if err != nil || got.ID != v2.ID {
		t.Fatalf("latest = %+v, %v", got, err)
	}
	// 再发布一个更早编号的：按 published_at 它最新
	e.clk.Advance(time.Hour)
	v1 := e.published(w.ID, "1.0.0")
	if got, _ := e.cat.LatestPublished(e.ctx, e.pool, w.ID); got.ID != v1.ID {
		t.Fatalf("latest = %s, want 1.0.0", got.Version)
	}
	vs, err := e.cat.ListVersions(e.ctx, w.ID)
	if err != nil || len(vs) != 4 || vs[0].Version != "1.0.0" || len(vs[0].Graph) == 0 {
		t.Fatalf("versions = %d, %v", len(vs), err)
	}
	g, err := e.cat.GetVersion(e.ctx, v2.ID)
	if err != nil || g.Version != "2.0.0" {
		t.Fatalf("get = %+v, %v", g, err)
	}
	if _, err := e.cat.WorkflowVersion(e.ctx, w.ID+1, v2.ID); errCode(err) != "not_found" {
		t.Fatalf("version of other workflow: %v", err)
	}
}

func TestListPublishedAndDetail(t *testing.T) {
	e := newCatEnv(t)
	newDep := e.deployFixture()
	a := e.workflow("aaa-txt", StatusPublished)
	b, _ := e.cat.CreateWorkflow(e.ctx, e.actor(), WorkflowInput{Slug: "bbb-video", Name: "视频", Category: CategoryVideo, Sort: -1, Status: StatusPublished, Tags: []string{"wan"}})
	noVer := e.workflow("no-version", StatusPublished) // 没有已发布版本：不上架
	draft := e.workflow("draft-wf", "")                // 草稿：不上架
	retired := e.workflow("retired-wf", StatusRetired)
	_ = noVer
	e.version(noVer.ID, "1.0.0")
	e.published(draft.ID, "1.0.0")
	e.published(retired.ID, "1.0.0")

	a1 := e.published(a.ID, "1.0.0")
	e.clk.Advance(time.Hour)
	a2 := e.published(a.ID, "1.1.0")
	bv := e.published(b.ID, "1.0.0")
	if _, err := e.cat.SetCover(e.ctx, e.actor(), b.ID, bytes.NewReader(pngBytes)); err != nil {
		t.Fatal(err)
	}

	// 安装计数：state = installed 且部署未终结；pending / failed、removed / lost 的部署不算；按工作流计（不分版本）
	e.attach(newDep("running"), a1, WFStateInstalled)
	e.attach(newDep("stopped"), a2, WFStateInstalled)
	e.attach(newDep("failed"), a2, WFStateInstalled)
	e.attach(newDep("running"), a2, WFStatePending)
	e.attach(newDep("queued"), a2, WFStateFailed)
	e.attach(newDep("removed"), a2, WFStateInstalled)
	e.attach(newDep("lost"), a2, WFStateInstalled)
	e.attach(newDep("running"), bv, WFStateInstalled)

	items, err := e.cat.ListPublished(e.ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Slug != "bbb-video" || items[1].Slug != "aaa-txt" {
		t.Fatalf("items = %+v", items)
	}
	it := items[1]
	if it.Installs != 3 || it.Version.ID != a2.ID || it.Version.Version != "1.1.0" || it.Version.DownloadGB != 6.94 || it.Version.Models != 1 ||
		it.Version.Nodes != 0 || !it.Version.QuickRun || it.Version.ComfyUIMin != "0.3.60" || it.CoverURL != nil || len(it.Tags) != 0 {
		t.Fatalf("aaa = %+v", it)
	}
	if items[0].Installs != 1 || items[0].CoverURL == nil || !strings.HasPrefix(*items[0].CoverURL, "/api/v1/workflows/bbb-video/cover?v=") {
		t.Fatalf("bbb = %+v", items[0])
	}
	// JSON：download_gb 是数字，不出现 cover_key
	raw, _ := json.Marshal(items)
	for _, want := range []string{`"download_gb":6.94`, `"installs":3`, `"cover_url":null`, `"quick_run":true`, `"tags":[]`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("json missing %s: %s", want, raw)
		}
	}
	if strings.Contains(string(raw), "cover_key") || strings.Contains(string(raw), "api_prompt") || strings.Contains(string(raw), "description") {
		t.Fatalf("json leaks internal fields: %s", raw)
	}

	videos, err := e.cat.ListPublished(e.ctx, CategoryVideo)
	if err != nil || len(videos) != 1 || videos[0].Slug != "bbb-video" {
		t.Fatalf("video = %+v, %v", videos, err)
	}
	if _, err := e.cat.ListPublished(e.ctx, "llm"); errCode(err) != "bad_request" {
		t.Fatalf("bad category: %v", err)
	}

	// 详情：当前版本 + 清单 + 已发布版本列表（published_at 倒序）
	d, err := e.cat.DetailBySlug(e.ctx, "aaa-txt")
	if err != nil {
		t.Fatal(err)
	}
	if d.Installs != 3 || d.Version.ID != a2.ID || d.Version.Changelog != "说明 1.1.0" || d.Version.PublishedAt == nil ||
		len(d.Version.Manifest.Inputs) != len(okManifest().Inputs) || len(d.Versions) != 2 || d.Versions[0].ID != a2.ID || d.Versions[1].ID != a1.ID {
		t.Fatalf("detail = %+v", d)
	}
	raw, _ = json.Marshal(d)
	for _, want := range []string{`"description":""`, `"manifest":{`, `"versions":[{`, `"download_gb":6.94`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("detail json missing %s: %s", want, raw)
		}
	}
	if strings.Contains(string(raw), "api_prompt") || strings.Contains(string(raw), "graph") {
		t.Fatalf("detail json leaks graph / api_prompt: %s", raw)
	}
	for _, slug := range []string{"no-version", "draft-wf", "retired-wf", "nope"} {
		if _, err := e.cat.DetailBySlug(e.ctx, slug); errCode(err) != "workflow_not_found" {
			t.Errorf("detail %s: %v", slug, err)
		}
	}

	// 后台列表：全部工作流，带版本数 / 在用安装数 / 7 日运行数
	uid := int64(0)
	_ = e.pool.QueryRow(e.ctx, `SELECT id FROM users LIMIT 1`).Scan(&uid)
	runAt := func(id string, at time.Time) {
		e.exec(`INSERT INTO comfy_workflow_runs (id, deployment_id, user_id, workflow_version_id, prompt_id, created_at)
			VALUES ($1, 'dep-000000000001', $2, $3, 'p', $4)`, id, uid, a1.ID, at)
	}
	runAt("run-000000000001", e.clk.Now().Add(-time.Hour))
	runAt("run-000000000002", e.clk.Now().Add(-6*24*time.Hour))
	runAt("run-000000000003", e.clk.Now().Add(-8*24*time.Hour))
	rows, err := e.cat.ListAdmin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 || rows[0].Slug != "bbb-video" {
		t.Fatalf("admin rows = %+v", rows)
	}
	var ar WorkflowRow
	for _, r := range rows {
		if r.ID == a.ID {
			ar = r
		}
	}
	if ar.Versions != 2 || ar.InstallsLive != 3 || ar.Runs7d != 2 || rows[0].HasCover != true {
		t.Fatalf("admin row = %+v", ar)
	}
	raw, _ = json.Marshal(rows[0])
	if !strings.Contains(string(raw), `"has_cover":true`) || !strings.Contains(string(raw), `"installs_live":1`) || strings.Contains(string(raw), "cover_key") {
		t.Fatalf("admin row json = %s", raw)
	}

	// 后台版本表
	vr, err := e.cat.ListVersionRows(e.ctx, a.ID)
	if err != nil || len(vr) != 2 || vr[0].ID != a2.ID || vr[0].Installs != 2 || vr[1].Installs != 1 || vr[0].DownloadGB != 6.94 ||
		vr[0].Models != 1 || !vr[0].QuickRun || vr[0].ComfyUIMin != "0.3.60" || vr[0].PublishedAt == nil {
		t.Fatalf("version rows = %+v, %v", vr, err)
	}
}

// 内部 Version 序列化（后台版本详情直接用）：download_gb 是数字，graph / api_prompt 原样。
func TestVersionJSON(t *testing.T) {
	v := Version{ID: 1, Version: "1.0.0", DownloadGB: "6.90", Graph: json.RawMessage(`{"nodes":[],"links":[]}`)}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"download_gb":6.9`, `"api_prompt":null`, `"graph":{"nodes":[],"links":[]}`, `"manifest":{"models":[]`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("json missing %s: %s", want, b)
		}
	}
	if strings.Count(string(b), "download_gb") != 1 {
		t.Fatalf("download_gb twice: %s", b)
	}
	w := Workflow{Slug: "x", CoverKey: new(string), HasCover: true}
	b, _ = json.Marshal(w)
	if strings.Contains(string(b), "cover_key") || !strings.Contains(string(b), `"has_cover":true`) {
		t.Fatalf("workflow json = %s", b)
	}
}

var (
	pngBytes  = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01")
	webpBytes = []byte("RIFF\x24\x00\x00\x00WEBPVP8 \x00\x00")
)

func TestCover(t *testing.T) {
	e := newCatEnv(t)
	w := e.workflow("wf", "")
	if _, _, err := e.cat.OpenCover(e.ctx, "wf"); errCode(err) != "workflow_not_found" { // 草稿
		t.Fatalf("draft cover: %v", err)
	}
	if _, _, err := e.cat.OpenCoverByID(e.ctx, w.ID); errCode(err) != "not_found" { // 没有封面
		t.Fatalf("no cover: %v", err)
	}
	// 类型 / 大小
	if _, err := e.cat.SetCover(e.ctx, e.actor(), w.ID, strings.NewReader("%PDF-1.7\n")); errCode(err) != "unsupported_file_type" {
		t.Fatalf("pdf cover: %v", err)
	}
	big := io.MultiReader(bytes.NewReader(pngBytes), bytes.NewReader(make([]byte, MaxCoverBytes)))
	_, err := e.cat.SetCover(e.ctx, e.actor(), w.ID, big)
	if ae := apperr.As(err); ae == nil || ae.Code != "file_too_large" || ae.Status != 413 || !strings.Contains(ae.Message, "2MB") {
		t.Fatalf("big cover: %v", err)
	}
	if _, err := e.cat.SetCover(e.ctx, e.actor(), 9999, bytes.NewReader(pngBytes)); errCode(err) != "workflow_not_found" {
		t.Fatalf("missing workflow: %v", err)
	}
	if n := e.count(`SELECT count(*) FROM uploads`); n != 0 {
		t.Fatalf("uploads after failures = %d", n)
	}

	w1, err := e.cat.SetCover(e.ctx, e.actor(), w.ID, bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatal(err)
	}
	if !w1.HasCover || w1.CoverKey == nil || !strings.HasSuffix(*w1.CoverKey, ".png") {
		t.Fatalf("after set = %+v", w1)
	}
	up, rc, err := e.cat.OpenCoverByID(e.ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(rc)
	rc.Close()
	if up.ContentType != "image/png" || !bytes.Equal(body, pngBytes) {
		t.Fatalf("cover = %+v %q", up, body)
	}

	// 替换：旧文件与登记被删除，URL 随之变化
	w2, err := e.cat.SetCover(e.ctx, e.actor(), w.ID, bytes.NewReader(webpBytes))
	if err != nil {
		t.Fatal(err)
	}
	if *w2.CoverKey == *w1.CoverKey || !strings.HasSuffix(*w2.CoverKey, ".webp") || *CoverURL(w2) == *CoverURL(w1) {
		t.Fatalf("replace = %v / %v", *w1.CoverKey, *w2.CoverKey)
	}
	if _, _, err := e.uploads.Open(e.ctx, *w1.CoverKey); errCode(err) != "not_found" {
		t.Fatalf("old cover still there: %v", err)
	}
	if n := e.count(`SELECT count(*) FROM uploads`); n != 1 {
		t.Fatalf("uploads after replace = %d", n)
	}
	if n := e.count(`SELECT count(*) FROM audit_logs WHERE action = 'workflow.update' AND diff->'cover'->>'replaced' = 'true'`); n != 1 {
		t.Fatalf("cover audits = %d", n)
	}

	// 上架后控制台可取；下线的也可取（已装的用户仍看得到）
	if _, err := e.cat.UpdateWorkflow(e.ctx, e.actor(), w.ID, WorkflowInput{Name: "wf", Category: "other", Status: StatusRetired}); err != nil {
		t.Fatal(err)
	}
	up, rc, err = e.cat.OpenCover(e.ctx, "wf")
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if up.ContentType != "image/webp" {
		t.Fatalf("console cover = %+v", up)
	}
	if _, _, err := e.cat.OpenCover(e.ctx, "nope"); errCode(err) != "workflow_not_found" {
		t.Fatalf("missing slug: %v", err)
	}
	if got := CoverURL(Workflow{Slug: "x"}); got != nil {
		t.Fatalf("CoverURL without cover = %v", *got)
	}
}
