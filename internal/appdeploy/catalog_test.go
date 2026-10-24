package appdeploy_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/paging"
)

func codeOf(err error) string {
	if e := apperr.As(err); e != nil {
		return e.Code
	}
	if err != nil {
		return "raw: " + err.Error()
	}
	return ""
}

func (e *env) auditLogs(action, targetID string) []audit.Log {
	e.t.Helper()
	logs, _, err := audit.List(e.ctx, e.pool, audit.Filter{Action: action, TargetID: targetID}, paging.Page{Number: 1, Size: 100})
	if err != nil {
		e.t.Fatal(err)
	}
	return logs
}

func id(v int64) string { return strconv.FormatInt(v, 10) }

func TestAppCRUDAndSlugImmutable(t *testing.T) {
	e := newEnv(t, 0)
	for name, in := range map[string]appdeploy.AppInput{
		"bad slug":     {Slug: "Bad_Slug", Name: "x", Category: "llm"},
		"no name":      {Slug: "noname", Category: "llm"},
		"long name":    {Slug: "longname", Name: strings.Repeat("名", 33), Category: "llm"},
		"long summary": {Slug: "longsum", Name: "x", Summary: strings.Repeat("x", 81), Category: "llm"},
		"category":     {Slug: "cat", Name: "x", Category: "game"},
		"icon":         {Slug: "icon", Name: "x", Category: "llm", Icon: "rocket"},
		"sort":         {Slug: "sort", Name: "x", Category: "llm", Sort: 10001},
		"status":       {Slug: "status", Name: "x", Category: "llm", Status: "live"},
	} {
		if _, err := e.cat.CreateApp(e.ctx, e.actor(), in); codeOf(err) != "bad_request" {
			t.Errorf("%s: want bad_request, got %v", name, err)
		}
	}

	a, err := e.cat.CreateApp(e.ctx, e.actor(), appdeploy.AppInput{Slug: "vllm", Name: "vLLM", Category: "llm"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != "draft" || a.Icon != "app" {
		t.Fatalf("defaults not applied: %+v", a)
	}
	_, err = e.cat.CreateApp(e.ctx, e.actor(), appdeploy.AppInput{Slug: "vllm", Name: "again", Category: "llm"})
	if codeOf(err) != "duplicate" || !strings.Contains(err.Error(), "slug 已存在") {
		t.Fatalf("duplicate slug: %v", err)
	}

	u, err := e.cat.UpdateApp(e.ctx, e.actor(), a.ID, appdeploy.AppInput{Slug: "renamed", Name: "vLLM 推理", Category: "llm", Icon: "chat", Sort: 5})
	if err != nil {
		t.Fatal(err)
	}
	if u.Slug != "vllm" || u.Name != "vLLM 推理" || u.Icon != "chat" || u.Sort != 5 {
		t.Fatalf("update: %+v", u)
	}
	if got, err := e.cat.AppBySlug(e.ctx, "vllm"); err != nil || got.ID != a.ID || got.Name != "vLLM 推理" {
		t.Fatalf("AppBySlug: %+v %v", got, err)
	}
	if _, err := e.cat.AppBySlug(e.ctx, "renamed"); codeOf(err) != "not_found" {
		t.Fatalf("slug changed by update: %v", err)
	}
	if _, err := e.cat.UpdateApp(e.ctx, e.actor(), 99999, appdeploy.AppInput{Name: "x", Category: "llm"}); codeOf(err) != "not_found" {
		t.Fatalf("update missing: %v", err)
	}

	tmp := e.app("tmp", "")
	if err := e.cat.DeleteApp(e.ctx, e.actor(), tmp.ID); err != nil {
		t.Fatalf("delete draft: %v", err)
	}
	if _, err := e.cat.GetApp(e.ctx, tmp.ID); codeOf(err) != "not_found" {
		t.Fatalf("deleted app still there: %v", err)
	}
	pub := e.app("pub", "published")
	if err := e.cat.DeleteApp(e.ctx, e.actor(), pub.ID); codeOf(err) != "invalid_state" {
		t.Fatalf("delete published app: %v", err)
	}
	e.version(a.ID, "1.0.0")
	if err := e.cat.DeleteApp(e.ctx, e.actor(), a.ID); codeOf(err) != "invalid_state" {
		t.Fatalf("delete app with versions: %v", err)
	}

	rows, err := e.cat.ListAppsAdmin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Slug != "pub" || rows[1].Slug != "vllm" || rows[1].Versions != 1 || rows[0].Versions != 0 || rows[1].DeploymentsLive != 0 {
		t.Fatalf("ListAppsAdmin: %+v", rows)
	}

	if n := len(e.auditLogs("app.create", id(a.ID))); n != 1 {
		t.Errorf("app.create audits for vllm = %d", n)
	}
	if n := len(e.auditLogs("app.update", id(a.ID))); n != 1 {
		t.Errorf("app.update audits = %d", n)
	}
	if n := len(e.auditLogs("app.delete", id(tmp.ID))); n != 1 {
		t.Errorf("app.delete audits = %d", n)
	}
	if n := len(e.auditLogs("app.delete", "")); n != 1 {
		t.Errorf("failed deletes must not audit: %d app.delete entries", n)
	}
}

func TestVersionDraftValidation(t *testing.T) {
	e := newEnv(t, 0)
	a := e.app("vllm", "")
	invalid := func(name string, mut func(in *appdeploy.VersionInput), want string) {
		t.Helper()
		in := e.goodVersionInput()
		mut(&in)
		_, err := e.cat.CreateVersion(e.ctx, e.actor(), a.ID, in)
		if codeOf(err) != "app_version_invalid" || !strings.Contains(strings.ToLower(err.Error()), want) {
			t.Errorf("%s: want app_version_invalid containing %q, got %v", name, want, err)
		}
		if _, err := e.cat.ValidateVersion(e.ctx, a.ID, in); codeOf(err) != "app_version_invalid" {
			t.Errorf("%s: ValidateVersion: %v", name, err)
		}
	}
	invalid("spec", func(in *appdeploy.VersionInput) { in.Spec.Ports[0].Port = 22 }, "ports[0].port")
	invalid("syntax", func(in *appdeploy.VersionInput) { in.Script = "#!/bin/sh\nif true; then\n  echo x\n" }, "第 2 行")
	invalid("bashism", func(in *appdeploy.VersionInput) { in.Script = "#!/bin/sh\narr=(1 2)\n" }, "第 2 行")
	invalid("too big", func(in *appdeploy.VersionInput) { in.Script = "#" + strings.Repeat("x", appdeploy.MaxScriptBytes) }, "script")
	invalid("empty script", func(in *appdeploy.VersionInput) { in.Script = "" }, "script")
	invalid("version", func(in *appdeploy.VersionInput) { in.Version = "-1" }, "version")
	invalid("no images", func(in *appdeploy.VersionInput) { in.ImageIDs = nil }, "image_ids")
	invalid("missing image", func(in *appdeploy.VersionInput) { in.ImageIDs = append(in.ImageIDs, 99999) }, "image_ids")
	var disabled int64
	if err := e.pool.QueryRow(e.ctx, `UPDATE images SET enabled = false WHERE id = (SELECT max(id) FROM images) RETURNING id`).Scan(&disabled); err != nil {
		t.Fatal(err)
	}
	invalid("disabled image", func(in *appdeploy.VersionInput) { in.ImageIDs = []int64{disabled} }, "image_ids")
	invalid("changelog", func(in *appdeploy.VersionInput) { in.Changelog = strings.Repeat("x", 1001) }, "changelog")
	if n := e.count(`SELECT count(*) FROM app_versions`); n != 0 {
		t.Fatalf("invalid versions stored: %d", n)
	}

	in := e.goodVersionInput()
	in.ImageIDs = []int64{e.imageID, e.imageID} // 去重
	hash, err := e.cat.ValidateVersion(e.ctx, a.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	v, err := e.cat.CreateVersion(e.ctx, e.actor(), a.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	norm := goodSpec()
	if err := appdeploy.ValidateSpec(&norm); err != nil {
		t.Fatal(err)
	}
	want := appdeploy.SpecHash(norm, goodScript)
	if v.Status != "draft" || v.SpecHash != want || hash != want || v.PublishedAt != nil || !slices.Equal(v.ImageIDs, []int64{e.imageID}) {
		t.Fatalf("draft: %+v (hash %s, want %s)", v, hash, want)
	}
	got, err := e.cat.GetVersion(e.ctx, v.ID)
	if err != nil || got.Script != goodScript || got.Spec.TimeoutMinutes != 60 || got.SpecHash != want {
		t.Fatalf("GetVersion: %+v %v", got, err)
	}
	if _, err := e.cat.CreateVersion(e.ctx, e.actor(), a.ID, e.goodVersionInput()); codeOf(err) != "duplicate" {
		t.Fatalf("duplicate version: %v", err)
	}
	if _, err := e.cat.CreateVersion(e.ctx, e.actor(), 99999, func() appdeploy.VersionInput { i := e.goodVersionInput(); i.Version = "2"; return i }()); codeOf(err) != "not_found" {
		t.Fatalf("version for missing app: %v", err)
	}
	if n := len(e.auditLogs("app.version.create", id(v.ID))); n != 1 {
		t.Fatalf("app.version.create audits = %d", n)
	}

	// draft 可以整体修改
	in = e.goodVersionInput()
	in.Version = "1.0.1"
	in.Script = goodScript + "# v2\n"
	up, err := e.cat.UpdateVersion(e.ctx, e.actor(), a.ID, v.ID, in)
	if err != nil || up.Version != "1.0.1" || up.SpecHash != appdeploy.SpecHash(norm, in.Script) {
		t.Fatalf("update draft: %+v %v", up, err)
	}
	in.Script = "if then"
	if _, err := e.cat.UpdateVersion(e.ctx, e.actor(), a.ID, v.ID, in); codeOf(err) != "app_version_invalid" {
		t.Fatalf("update draft with bad script: %v", err)
	}
	if n := len(e.auditLogs("app.version.update", id(v.ID))); n != 1 {
		t.Fatalf("app.version.update audits = %d", n)
	}
}

func TestPublishFreezesAndBroadcasts(t *testing.T) {
	e := newEnv(t, 3)
	e.exec(`UPDATE machines SET agent_online = false WHERE id = $1`, e.machines[2])
	a := e.app("vllm", "published")
	v := e.version(a.ID, "1.0.0")
	e.clk.Advance(time.Minute)
	e.exec(`UPDATE images SET enabled = false WHERE id = $1`, e.imageID)
	if _, _, err := e.cat.PublishVersion(e.ctx, e.actor(), a.ID, v.ID); codeOf(err) != "app_version_invalid" || !strings.Contains(err.Error(), "image_ids") {
		t.Fatalf("publish with disabled image: %v", err)
	}
	e.exec(`UPDATE images SET enabled = true WHERE id = $1`, e.imageID)

	pv, n, err := e.cat.PublishVersion(e.ctx, e.actor(), a.ID, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pv.Status != "published" || pv.PublishedAt == nil || !pv.PublishedAt.Equal(e.clk.Now()) || n != 2 {
		t.Fatalf("publish: %+v sent=%d", pv, n)
	}
	ref := e.imageRef(e.imageID)
	for i, mid := range e.machines {
		want := 1
		if i == 2 {
			want = 0
		}
		if got := e.count(`SELECT count(*) FROM agent_commands WHERE machine_id = $1 AND type = 'pull_image' AND payload->>'image_ref' = $2`, mid, ref); got != want {
			t.Errorf("machine %d pull_image commands = %d, want %d", i, got, want)
		}
	}
	if _, _, err := e.cat.PublishVersion(e.ctx, e.actor(), a.ID, v.ID); codeOf(err) != "invalid_state" {
		t.Fatalf("publish twice: %v", err)
	}
	logs := e.auditLogs("app.version.publish", id(v.ID))
	if len(logs) != 1 || !strings.Contains(string(logs[0].Diff), `"spec_hash"`) || !strings.Contains(string(logs[0].Diff), pv.SpecHash) {
		t.Fatalf("publish audit: %+v", logs)
	}

	sent, err := e.cat.Prepull(e.ctx, e.actor(), a.ID, v.ID)
	if err != nil || sent != 2 {
		t.Fatalf("prepull: %d %v", sent, err)
	}
	if got := e.count(`SELECT count(*) FROM agent_commands WHERE type = 'pull_image'`); got != 4 {
		t.Fatalf("pull_image after prepull = %d", got)
	}
	if n := len(e.auditLogs("app.version.prepull", id(v.ID))); n != 1 {
		t.Fatalf("prepull audits = %d", n)
	}

	// 已发布：结构字段冻结（未规范化的同一 spec 视为未改）
	same := func() appdeploy.VersionInput {
		return appdeploy.VersionInput{Version: "1.0.0", ImageIDs: []int64{e.imageID}, Spec: goodSpec(), Script: goodScript}
	}
	for name, mut := range map[string]func(in *appdeploy.VersionInput){
		"spec":    func(in *appdeploy.VersionInput) { in.Spec.Ports[0].Port = 9000 },
		"script":  func(in *appdeploy.VersionInput) { in.Script += "\n" },
		"version": func(in *appdeploy.VersionInput) { in.Version = "1.0.1" },
		"images":  func(in *appdeploy.VersionInput) { in.ImageIDs = nil },
		"status":  func(in *appdeploy.VersionInput) { in.Status = "draft" },
	} {
		in := same()
		mut(&in)
		if _, err := e.cat.UpdateVersion(e.ctx, e.actor(), a.ID, v.ID, in); codeOf(err) != "app_version_frozen" {
			t.Errorf("%s change on published: %v", name, err)
		}
	}
	in := same()
	in.Changelog = "修复启动参数"
	up, err := e.cat.UpdateVersion(e.ctx, e.actor(), a.ID, v.ID, in)
	if err != nil || up.Changelog != "修复启动参数" || up.Status != "published" || up.SpecHash != pv.SpecHash {
		t.Fatalf("changelog update: %+v %v", up, err)
	}
	if lp, err := e.cat.LatestPublished(e.ctx, e.pool, a.ID); err != nil || lp.ID != v.ID {
		t.Fatalf("LatestPublished: %+v %v", lp, err)
	}
	in.Status = "retired"
	up, err = e.cat.UpdateVersion(e.ctx, e.actor(), a.ID, v.ID, in)
	if err != nil || up.Status != "retired" {
		t.Fatalf("retire: %+v %v", up, err)
	}
	if _, err := e.cat.LatestPublished(e.ctx, e.pool, a.ID); codeOf(err) != "app_unavailable" {
		t.Fatalf("LatestPublished after retire: %v", err)
	}
	in.Status = ""
	in.Changelog = "again"
	if _, err := e.cat.UpdateVersion(e.ctx, e.actor(), a.ID, v.ID, in); codeOf(err) != "app_version_frozen" {
		t.Fatalf("update retired: %v", err)
	}
}

func TestCopyFromAndLatest(t *testing.T) {
	e := newEnv(t, 1)
	a := e.app("vllm", "published")
	in := e.goodVersionInput()
	in.Version = "9.0.0"
	in.Script = goodScript + "# custom\n"
	in.Changelog = "first"
	v1, err := e.cat.CreateVersion(e.ctx, e.actor(), a.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.cat.PublishVersion(e.ctx, e.actor(), a.ID, v1.ID); err != nil {
		t.Fatal(err)
	}

	v2, err := e.cat.CreateVersion(e.ctx, e.actor(), a.ID, appdeploy.VersionInput{Version: "10.0.0", CopyFrom: &v1.ID})
	if err != nil {
		t.Fatal(err)
	}
	full, err := e.cat.GetVersion(e.ctx, v2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if full.Version != "10.0.0" || full.Status != "draft" || full.Script != in.Script || !slices.Equal(full.ImageIDs, v1.ImageIDs) ||
		full.SpecHash != v1.SpecHash || len(full.Spec.Env) != len(v1.Spec.Env) || full.Changelog != "" {
		t.Fatalf("copy: %+v", full)
	}
	other := e.app("other", "")
	if _, err := e.cat.CreateVersion(e.ctx, e.actor(), other.ID, appdeploy.VersionInput{Version: "1", CopyFrom: &v1.ID}); codeOf(err) != "not_found" {
		t.Fatalf("copy across apps: %v", err)
	}

	e.clk.Advance(time.Minute)
	if _, _, err := e.cat.PublishVersion(e.ctx, e.actor(), a.ID, v2.ID); err != nil {
		t.Fatal(err)
	}
	lp, err := e.cat.LatestPublished(e.ctx, e.pool, a.ID)
	if err != nil || lp.ID != v2.ID {
		t.Fatalf("LatestPublished = %+v %v, want %d (newest published_at, not max string)", lp, err, v2.ID)
	}
	vs, err := e.cat.ListVersions(e.ctx, a.ID)
	if err != nil || len(vs) != 2 {
		t.Fatalf("ListVersions: %+v %v", vs, err)
	}
}

func TestListPublishedFiltersDrafts(t *testing.T) {
	e := newEnv(t, 0)
	var img2 int64
	if err := e.pool.QueryRow(e.ctx, `SELECT id FROM images WHERE enabled AND id <> $1 ORDER BY id LIMIT 1`, e.imageID).Scan(&img2); err != nil {
		t.Fatal(err)
	}
	publish := func(appID int64, ver string, images []int64) appdeploy.Version {
		in := e.goodVersionInput()
		in.Version = ver
		in.ImageIDs = images
		v, err := e.cat.CreateVersion(e.ctx, e.actor(), appID, in)
		if err != nil {
			t.Fatal(err)
		}
		pv, _, err := e.cat.PublishVersion(e.ctx, e.actor(), appID, v.ID)
		if err != nil {
			t.Fatal(err)
		}
		return pv
	}
	draftApp := e.app("draft-app", "")
	publish(draftApp.ID, "1", []int64{e.imageID})
	noVer := e.app("no-version", "published")
	e.version(noVer.ID, "1") // 只有草稿版本
	shown := e.app("shown", "published")
	publish(shown.ID, "1", []int64{e.imageID})
	e.clk.Advance(time.Minute)
	latest := publish(shown.ID, "2", []int64{img2, e.imageID})
	first, err := e.cat.CreateApp(e.ctx, e.actor(), appdeploy.AppInput{Slug: "first", Name: "first", Category: "tool", Sort: -1, Status: "published"})
	if err != nil {
		t.Fatal(err)
	}
	publish(first.ID, "1", []int64{e.imageID})

	items, err := e.cat.ListPublished(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var slugs []string
	for _, it := range items {
		slugs = append(slugs, it.Slug)
	}
	if !slices.Equal(slugs, []string{"first", "shown"}) {
		t.Fatalf("ListPublished slugs = %v", slugs)
	}
	it := items[1]
	if it.Version.ID != latest.ID || len(it.Images) != 2 || it.Images[0].ID != img2 || it.Images[1].ID != e.imageID || it.Images[0].Name == "" {
		t.Fatalf("item: %+v", it)
	}
}

func TestCoverageCountsUnknownAndStale(t *testing.T) {
	e := newEnv(t, 5)
	ref := e.imageRef(e.imageID)
	now := e.clk.Now()
	e.exec(`UPDATE machines SET cached_images = ARRAY[$2, 'other:1'], cached_images_at = $3 WHERE id = $1`, e.machines[0], ref, now)
	e.exec(`UPDATE machines SET cached_images = ARRAY[$2], cached_images_at = $3 WHERE id = $1`, e.machines[1], ref, now.Add(-6*time.Minute))
	e.exec(`UPDATE machines SET agent_online = false, cached_images = ARRAY[$2], cached_images_at = $3 WHERE id = $1`, e.machines[3], ref, now)
	e.exec(`UPDATE machines SET cached_images = ARRAY['other:1'], cached_images_at = $2 WHERE id = $1`, e.machines[4], now.Add(-time.Minute)) // 新鲜但没有该镜像
	a := e.app("vllm", "")
	v := e.version(a.ID, "1")

	cov, err := e.cat.VersionCoverage(e.ctx, v)
	if err != nil {
		t.Fatal(err)
	}
	var name string
	if err := e.pool.QueryRow(e.ctx, `SELECT name FROM images WHERE id = $1`, e.imageID).Scan(&name); err != nil {
		t.Fatal(err)
	}
	want := []appdeploy.Coverage{{ImageID: e.imageID, ImageName: name, Cached: 1, Online: 4, Unknown: 2}}
	if !slices.Equal(cov, want) {
		t.Fatalf("coverage = %+v, want %+v", cov, want)
	}
}

func TestCheckShellSyntax(t *testing.T) {
	for _, ok := range []string{"echo ok\n", goodScript, "f() { local a=1; echo \"${a:-x}\" $((a + 1)); }\nf\n"} {
		if err := appdeploy.CheckShellSyntax(t.Context(), ok); err != nil {
			t.Fatalf("valid script %q: %v", ok, err)
		}
	}
	for script, want := range map[string][]string{
		"if true; then\n  echo x\n":    {"第 1 行", "fi"},
		"echo a\necho b\nfi\n":         {"第 3 行"},
		"echo a\narr=(1 2)\n":          {"第 2 行", "posix"},
		"echo a\necho 'unterminated\n": {"第 2 行", "quote"},
	} {
		err := appdeploy.CheckShellSyntax(t.Context(), script)
		if codeOf(err) != "app_version_invalid" {
			t.Fatalf("%q: want app_version_invalid, got %v", script, err)
		}
		for _, w := range want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%q: message %q missing %q", script, err.Error(), w)
			}
		}
	}
}

func writeApp(t *testing.T, root, slug string, meta map[string]any) {
	t.Helper()
	dir := filepath.Join(root, slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mj, _ := json.Marshal(meta)
	sj, _ := json.Marshal(goodSpec())
	for name, body := range map[string][]byte{"app.json": mj, "spec.json": sj, "app.sh": []byte(goodScript)} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestImportDir(t *testing.T) {
	e := newEnv(t, 0)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# apps\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	meta := func(version string, images ...string) map[string]any {
		return map[string]any{"name": "测试", "summary": "s", "category": "llm", "icon": "chat", "sort": 10, "version": version, "images": images, "changelog": "c"}
	}
	writeApp(t, root, "beta", meta("1.0.0", "vLLM", "PyTorch"))
	writeApp(t, root, "alpha", meta("1.0.0", "PyTorch"))

	res, err := e.cat.ImportDir(e.ctx, e.actor(), root)
	if err != nil {
		t.Fatal(err)
	}
	want := []appdeploy.ImportResult{{Slug: "alpha", Version: "1.0.0", Created: true}, {Slug: "beta", Version: "1.0.0", Created: true}}
	if !slices.Equal(res, want) {
		t.Fatalf("first import = %+v", res)
	}
	beta, err := e.cat.AppBySlug(e.ctx, "beta")
	if err != nil || beta.Status != "draft" || beta.Icon != "chat" || beta.Sort != 10 {
		t.Fatalf("beta: %+v %v", beta, err)
	}
	vs, err := e.cat.ListVersions(e.ctx, beta.ID)
	if err != nil || len(vs) != 1 || vs[0].Status != "draft" || len(vs[0].ImageIDs) != 2 || vs[0].Changelog != "c" {
		t.Fatalf("beta versions: %+v %v", vs, err)
	}
	var vllmID int64
	if err := e.pool.QueryRow(e.ctx, `SELECT id FROM images WHERE name = 'vLLM'`).Scan(&vllmID); err != nil {
		t.Fatal(err)
	}
	if vs[0].ImageIDs[0] != vllmID {
		t.Fatalf("image order: %v (vLLM = %d)", vs[0].ImageIDs, vllmID)
	}

	writeApp(t, root, "alpha", meta("1.0.1", "PyTorch"))
	writeApp(t, root, "beta", meta("1.0.1", "vLLM"))
	res, err = e.cat.ImportDir(e.ctx, e.actor(), root)
	if err != nil {
		t.Fatal(err)
	}
	want = []appdeploy.ImportResult{{Slug: "alpha", Version: "1.0.1"}, {Slug: "beta", Version: "1.0.1"}}
	if !slices.Equal(res, want) {
		t.Fatalf("second import = %+v", res)
	}
	if n := e.count(`SELECT count(*) FROM apps`); n != 2 {
		t.Fatalf("apps = %d", n)
	}

	writeApp(t, root, "alpha", meta("1.0.2", "PyTorch"))
	writeApp(t, root, "beta", meta("1.0.2", "Nope Image"))
	res, err = e.cat.ImportDir(e.ctx, e.actor(), root)
	if err == nil || !strings.Contains(err.Error(), "Nope Image") || !strings.Contains(err.Error(), "beta") {
		t.Fatalf("unknown image: %v", err)
	}
	if len(res) != 1 || res[0].Slug != "alpha" || res[0].Version != "1.0.2" {
		t.Fatalf("partial import results = %+v", res)
	}
	if n := e.count(`SELECT count(*) FROM app_versions`); n != 5 {
		t.Fatalf("versions after partial import = %d, want 5", n)
	}
	if _, err := e.cat.ImportDir(e.ctx, e.actor(), filepath.Join(root, "missing")); err == nil {
		t.Fatal("missing dir must fail")
	}

	// 符号链接（目录或文件）跳过并给出原因；超大文件在读之前拒绝
	root2 := t.TempDir()
	if err := os.Symlink(filepath.Join(root, "alpha"), filepath.Join(root2, "a-link")); err != nil {
		t.Fatal(err)
	}
	writeApp(t, root2, "b-file", meta("9.0.0", "PyTorch"))
	if err := os.Remove(filepath.Join(root2, "b-file", "app.sh")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(root2, "b-file", "app.sh")); err != nil {
		t.Fatal(err)
	}
	writeApp(t, root2, "c-big", meta("9.0.0", "PyTorch"))
	if err := os.WriteFile(filepath.Join(root2, "c-big", "app.sh"), []byte("#"+strings.Repeat("x", appdeploy.MaxScriptBytes)), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = e.cat.ImportDir(e.ctx, e.actor(), root2)
	if err == nil || !strings.Contains(err.Error(), "c-big") || !strings.Contains(err.Error(), "app.sh 超过") {
		t.Fatalf("oversized app.sh: %v", err)
	}
	want = []appdeploy.ImportResult{{Slug: "a-link", Skipped: "a-link 是符号链接，已跳过"}, {Slug: "b-file", Skipped: "app.sh 是符号链接，已跳过"}}
	if !slices.Equal(res, want) {
		t.Fatalf("symlink results = %+v", res)
	}
	if n := e.count(`SELECT count(*) FROM apps`); n != 2 {
		t.Fatalf("apps after skipped imports = %d", n)
	}
}

// 镜像登记成 Docker Hub 全名（或省略 tag）时，与 docker image ls 的短名（Agent 上报的形态）对得上。
func TestCoverageMatchesFamiliarRef(t *testing.T) {
	e := newEnv(t, 2)
	e.exec(`UPDATE images SET ref = 'docker.io/library/ubuntu' WHERE id = $1`, e.imageID)
	now := e.clk.Now()
	e.exec(`UPDATE machines SET cached_images = ARRAY['ubuntu:latest'], cached_images_at = $2 WHERE id = $1`, e.machines[0], now)
	e.exec(`UPDATE machines SET cached_images = ARRAY['other:1'], cached_images_at = $2 WHERE id = $1`, e.machines[1], now)
	v := e.version(e.app("vllm", "").ID, "1")
	cov, err := e.cat.VersionCoverage(e.ctx, v)
	if err != nil || len(cov) != 1 || cov[0].Cached != 1 || cov[0].Online != 2 {
		t.Fatalf("coverage = %+v %v", cov, err)
	}
}

// 登记的镜像地址补 :latest 后超过 256 字节：覆盖率按与 cached_images 相同的规则截断后比对。
func TestCoverageMatchesTruncatedRef(t *testing.T) {
	e := newEnv(t, 1)
	long := "r/" + strings.Repeat("x", 250)
	e.exec(`UPDATE images SET ref = $2 WHERE id = $1`, e.imageID, long)
	e.exec(`UPDATE machines SET cached_images = ARRAY[$2], cached_images_at = $3 WHERE id = $1`, e.machines[0], (long + ":latest")[:256], e.clk.Now())
	v := e.version(e.app("vllm", "").ID, "1")
	cov, err := e.cat.VersionCoverage(e.ctx, v)
	if err != nil || len(cov) != 1 || cov[0].Cached != 1 {
		t.Fatalf("coverage = %+v %v", cov, err)
	}
}

// 计划 9：内部应用（apps.internal）不在用户目录里出现，后台列表照常可见并带 internal。
func TestInternalAppHiddenFromPublished(t *testing.T) {
	e := newEnv(t, 0)
	pub := func(a appdeploy.App) {
		v := e.version(a.ID, "1.0.0")
		if _, _, err := e.cat.PublishVersion(e.ctx, e.actor(), a.ID, v.ID); err != nil {
			t.Fatal(err)
		}
	}
	shown := e.app("shown", appdeploy.StatusPublished)
	pub(shown)
	node, err := e.cat.CreateApp(e.ctx, e.actor(), appdeploy.AppInput{Slug: "vllm-node", Name: "节点", Category: "llm", Status: appdeploy.StatusPublished, Internal: true})
	if err != nil || !node.Internal {
		t.Fatalf("create internal = %+v, %v", node, err)
	}
	pub(node)
	items, err := e.cat.ListPublished(e.ctx)
	if err != nil || len(items) != 1 || items[0].Slug != "shown" || items[0].Internal {
		t.Fatalf("ListPublished = %+v, %v", items, err)
	}
	views, err := e.svc.CatalogViews(e.ctx, false)
	if err != nil || len(views) != 1 || views[0].Slug != "shown" {
		t.Fatalf("CatalogViews = %+v, %v", views, err)
	}
	if _, err := e.svc.CatalogViewBySlug(e.ctx, "vllm-node"); codeOf(err) != "app_not_found" {
		t.Fatalf("CatalogViewBySlug internal = %v", err)
	}
	// 后台：列表带 internal；UpdateApp 不改 internal（与 slug 一样创建后不变）
	rows, err := e.cat.ListAppsAdmin(e.ctx)
	if err != nil || len(rows) != 2 || rows[0].Internal || !rows[1].Internal {
		t.Fatalf("ListAppsAdmin = %+v, %v", rows, err)
	}
	b, _ := json.Marshal(rows[1])
	if !strings.Contains(string(b), `"internal":true`) {
		t.Fatalf("admin row json = %s", b)
	}
	up, err := e.cat.UpdateApp(e.ctx, e.actor(), node.ID, appdeploy.AppInput{Name: "节点 2", Category: "llm", Status: appdeploy.StatusPublished})
	if err != nil || !up.Internal || up.Name != "节点 2" {
		t.Fatalf("UpdateApp = %+v, %v", up, err)
	}
	if got, err := e.cat.AppBySlug(e.ctx, "vllm-node"); err != nil || !got.Internal {
		t.Fatalf("AppBySlug = %+v, %v", got, err)
	}
}

func TestImportReadsInternal(t *testing.T) {
	e := newEnv(t, 0)
	root := t.TempDir()
	writeApp(t, root, "plain", map[string]any{"name": "普通", "category": "llm", "version": "1.0.0", "images": []string{"PyTorch"}})
	writeApp(t, root, "vllm-node", map[string]any{"name": "节点", "category": "llm", "internal": true, "version": "1.0.0", "images": []string{"PyTorch"}})
	res, err := e.cat.ImportDir(e.ctx, e.actor(), root)
	if err != nil || len(res) != 2 || !res[0].Created || !res[1].Created {
		t.Fatalf("import = %+v, %v", res, err)
	}
	plain, err := e.cat.AppBySlug(e.ctx, "plain")
	if err != nil || plain.Internal {
		t.Fatalf("plain = %+v, %v", plain, err)
	}
	node, err := e.cat.AppBySlug(e.ctx, "vllm-node")
	if err != nil || !node.Internal || node.Status != appdeploy.StatusDraft {
		t.Fatalf("vllm-node = %+v, %v", node, err)
	}
}
