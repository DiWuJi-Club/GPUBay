package adminapi_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/adminapi"
)

// wfFixture 读 deploy/workflows/sdxl-txt2img 下的文件（真实的首批工作流，校验必过）。
func wfFixture(t *testing.T, name string) json.RawMessage {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "deploy", "workflows", "sdxl-txt2img", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func wfBody(slug string) map[string]any {
	return map[string]any{"slug": slug, "name": "SDXL 文生图", "summary": "简介", "description": "说明", "category": "txt2img",
		"tags": []string{"SDXL", "入门"}, "sort": 1}
}

func wfVersionBody(t *testing.T, ver string) map[string]any {
	return map[string]any{"version": ver, "graph": wfFixture(t, "graph.json"), "api_prompt": wfFixture(t, "api.json"),
		"manifest": wfFixture(t, "manifest.json"), "changelog": "首版"}
}

// createWorkflow 经接口建一个工作流，返回 id。
func createWorkflow(t *testing.T, c *client, slug string) int64 {
	t.Helper()
	code, out := c.do("POST", "/workflows", wfBody(slug))
	if code != 201 {
		t.Fatalf("create workflow = %d %v", code, out)
	}
	return int64(out["id"].(float64))
}

func TestAdminWorkflowCRUD(t *testing.T) {
	e := newEnv(t)
	root := e.loginAs("root", admin.RoleSuper)
	ops := e.loginAs("ops", admin.RoleOps)
	fin := e.loginAs("fin", admin.RoleFinance)

	if code, out := ops.do("POST", "/workflows", wfBody("sdxl")); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("ops create = %d %v", code, out)
	}
	code, out := root.do("POST", "/workflows", wfBody("sdxl"))
	if code != 201 || out["slug"] != "sdxl" || out["status"] != "draft" || out["has_cover"] != false || out["cover_url"] != nil ||
		len(out["tags"].([]any)) != 2 || out["cover_key"] != nil {
		t.Fatalf("create = %d %v", code, out)
	}
	id := int64(out["id"].(float64))
	if code, out := root.do("POST", "/workflows", wfBody("sdxl")); code != 409 {
		t.Fatalf("duplicate slug = %d %v", code, out)
	}
	if code, out := root.do("POST", "/workflows", map[string]any{"slug": "X", "name": "x", "category": "txt2img"}); code != 400 {
		t.Fatalf("bad slug = %d %v", code, out)
	}
	if code, out := root.do("POST", "/workflows", map[string]any{"slug": "abc", "name": "x", "category": "nope"}); code != 400 {
		t.Fatalf("bad category = %d %v", code, out)
	}

	// 改：slug 不可改；status 省略 = 保持
	upd := wfBody("sdxl")
	upd["name"], upd["status"] = "SDXL", "published"
	if code, out := root.do("PUT", fmt.Sprintf("/workflows/%d", id), upd); code != 200 || out["name"] != "SDXL" || out["status"] != "published" {
		t.Fatalf("update = %d %v", code, out)
	}
	upd = wfBody("sdxl")
	upd["summary"] = "新简介"
	if code, out := root.do("PUT", fmt.Sprintf("/workflows/%d", id), upd); code != 200 || out["status"] != "published" || out["summary"] != "新简介" {
		t.Fatalf("update without status = %d %v", code, out)
	}
	if code, out := root.do("PUT", fmt.Sprintf("/workflows/%d", id), wfBody("other")); code != 400 {
		t.Fatalf("change slug = %d %v", code, out)
	}
	if code, out := ops.do("PUT", fmt.Sprintf("/workflows/%d", id), upd); code != 403 {
		t.Fatalf("ops update = %d %v", code, out)
	}
	if code, out := root.do("PUT", "/workflows/999999", upd); code != 404 || codeOf(out) != "workflow_not_found" {
		t.Fatalf("update missing = %d %v", code, out)
	}

	// 读：全部角色
	code, out = fin.do("GET", "/workflows", nil)
	items, _ := out["items"].([]any)
	if code != 200 || len(items) != 1 {
		t.Fatalf("list = %d %v", code, out)
	}
	it := items[0].(map[string]any)
	if it["id"].(float64) != float64(id) || it["versions"].(float64) != 0 || it["installs_live"].(float64) != 0 ||
		it["runs_7d"].(float64) != 0 || it["cover_url"] != nil || it["description"] != "说明" {
		t.Fatalf("list item = %v", it)
	}
	if code, out := ops.do("GET", fmt.Sprintf("/workflows/%d", id), nil); code != 200 || out["slug"] != "sdxl" {
		t.Fatalf("get = %d %v", code, out)
	}
	if code, out := ops.do("GET", "/workflows/999999", nil); code != 404 || codeOf(out) != "workflow_not_found" {
		t.Fatalf("get missing = %d %v", code, out)
	}

	// 删：只有无版本的草稿
	if code, out := root.do("DELETE", fmt.Sprintf("/workflows/%d", id), nil); code != 409 || codeOf(out) != "invalid_state" {
		t.Fatalf("delete published = %d %v", code, out)
	}
	draft := createWorkflow(t, root, "draft-one")
	if code, out := root.do("POST", fmt.Sprintf("/workflows/%d/versions", draft), wfVersionBody(t, "1.0")); code != 201 {
		t.Fatalf("version = %d %v", code, out)
	}
	if code, out := root.do("DELETE", fmt.Sprintf("/workflows/%d", draft), nil); code != 409 || codeOf(out) != "invalid_state" {
		t.Fatalf("delete with versions = %d %v", code, out)
	}
	empty := createWorkflow(t, root, "empty-one")
	if code, out := fin.do("DELETE", fmt.Sprintf("/workflows/%d", empty), nil); code != 403 {
		t.Fatalf("finance delete = %d %v", code, out)
	}
	if code, out := root.do("DELETE", fmt.Sprintf("/workflows/%d", empty), nil); code != 200 || out["ok"] != true {
		t.Fatalf("delete = %d %v", code, out)
	}
	if code, _ := root.do("GET", fmt.Sprintf("/workflows/%d", empty), nil); code != 404 {
		t.Fatalf("deleted still readable = %d", code)
	}
	for _, action := range []string{"workflow.create", "workflow.update", "workflow.delete"} {
		if n := e.count(`SELECT count(*) FROM audit_logs WHERE action = $1 AND actor_type = 'admin' AND actor_id = $2`, action, root.admin.ID); n == 0 {
			t.Errorf("audit %s missing", action)
		}
	}
}

func TestAdminWorkflowVersions(t *testing.T) {
	e := newEnv(t)
	root := e.loginAs("root", admin.RoleSuper)
	ops := e.loginAs("ops", admin.RoleOps)
	fin := e.loginAs("fin", admin.RoleFinance)
	id := createWorkflow(t, root, "sdxl")
	base := fmt.Sprintf("/workflows/%d/versions", id)

	// 校验失败：manifest 里没有 graph 引用的模型（D16）→ workflow_version_invalid，消息以字段路径开头；不落库
	bad := wfVersionBody(t, "1.0")
	var m map[string]any
	_ = json.Unmarshal(wfFixture(t, "manifest.json"), &m)
	m["models"] = []any{}
	bad["manifest"] = m
	code, out := root.do("POST", base+"?validate=1", bad)
	if code != 400 || codeOf(out) != "workflow_version_invalid" || !strings.Contains(out["message"].(string), "sd_xl_base_1.0.safetensors") {
		t.Fatalf("validate bad = %d %v", code, out)
	}
	if code, out := root.do("POST", base, bad); code != 400 || codeOf(out) != "workflow_version_invalid" {
		t.Fatalf("create bad = %d %v", code, out)
	}
	if code, out := root.do("POST", "/workflows/999999/versions?validate=1", wfVersionBody(t, "1.0")); code != 404 || codeOf(out) != "workflow_not_found" {
		t.Fatalf("validate under missing workflow = %d %v", code, out)
	}
	if code, out := root.do("POST", base, map[string]any{"version": "1.0", "bogus": 1}); code != 400 || codeOf(out) != "bad_request" {
		t.Fatalf("unknown field = %d %v", code, out)
	}
	if code, out := root.do("POST", base, map[string]any{"version": "1.0", "status": "published"}); code != 400 {
		t.Fatalf("create published = %d %v", code, out)
	}

	good := wfVersionBody(t, "1.0")
	code, out = root.do("POST", base+"?validate=1", good)
	if code != 200 || out["ok"] != true || len(out["manifest_hash"].(string)) != 64 || out["download_gb"].(float64) != 6.94 || out["warnings"] == nil {
		t.Fatalf("validate good = %d %v", code, out)
	}
	hash := out["manifest_hash"].(string)
	if n := e.count(`SELECT count(*) FROM comfy_workflow_versions`); n != 0 {
		t.Fatal("validate=1 wrote a version")
	}
	if code, out := ops.do("POST", base, good); code != 403 {
		t.Fatalf("ops create version = %d %v", code, out)
	}
	code, out = root.do("POST", base, good)
	if code != 201 || out["status"] != "draft" || out["manifest_hash"] != hash || out["changelog"] != "首版" || out["graph"] == nil {
		t.Fatalf("create = %d %v", code, out)
	}
	vid := int64(out["id"].(float64))
	vpath := fmt.Sprintf("%s/%d", base, vid)
	if code, out := root.do("POST", base, good); code != 409 {
		t.Fatalf("duplicate version = %d %v", code, out)
	}

	// 列表不含本体；详情含 graph / api_prompt / manifest
	code, out = fin.do("GET", base, nil)
	items, _ := out["items"].([]any)
	if code != 200 || len(items) != 1 {
		t.Fatalf("versions = %d %v", code, out)
	}
	row := items[0].(map[string]any)
	if row["id"].(float64) != float64(vid) || row["models"].(float64) != 1 || row["quick_run"] != true || row["installs"].(float64) != 0 {
		t.Fatalf("version row = %v", row)
	}
	for _, k := range []string{"graph", "api_prompt", "manifest"} {
		if _, ok := row[k]; ok {
			t.Fatalf("version row has %s", k)
		}
	}
	code, out = fin.do("GET", vpath, nil)
	if code != 200 || out["graph"].(map[string]any)["nodes"] == nil || out["api_prompt"].(map[string]any)["4"] == nil ||
		len(out["manifest"].(map[string]any)["models"].([]any)) != 1 || out["download_gb"].(float64) != 6.94 {
		t.Fatalf("version detail = %d %v", code, out)
	}
	if code, out := fin.do("GET", fmt.Sprintf("%s/%d", base, vid+100), nil); code != 404 {
		t.Fatalf("missing version = %d %v", code, out)
	}
	other := createWorkflow(t, root, "other")
	if code, out := fin.do("GET", fmt.Sprintf("/workflows/%d/versions/%d", other, vid), nil); code != 404 {
		t.Fatalf("version under wrong workflow = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/workflows/999999/versions", nil); code != 404 || codeOf(out) != "workflow_not_found" {
		t.Fatalf("versions of missing workflow = %d %v", code, out)
	}

	// 草稿 PUT：只发 changelog（其余沿用库中值）；api_prompt 显式 null = 去掉快速出图
	if code, out := root.do("PUT", vpath+"?validate=1", map[string]any{"changelog": "改说明"}); code != 200 || out["manifest_hash"] != hash {
		t.Fatalf("validate put draft = %d %v", code, out)
	}
	if code, out := root.do("PUT", vpath, map[string]any{"changelog": "改说明"}); code != 200 || out["changelog"] != "改说明" ||
		out["manifest_hash"] != hash || out["graph"] == nil {
		t.Fatalf("put draft changelog = %d %v", code, out)
	}
	code, out = root.do("PUT", vpath, map[string]any{"api_prompt": nil, "manifest": map[string]any{"models": m0(t), "nodes": []any{},
		"inputs": []any{}, "outputs": []any{}}})
	if code != 200 || out["api_prompt"] != nil || out["manifest_hash"] == hash || out["changelog"] != "改说明" {
		t.Fatalf("put draft drop api = %d %v", code, out)
	}
	if code, out := root.do("PUT", vpath, good); code != 200 || out["manifest_hash"] != hash || out["changelog"] != "首版" {
		t.Fatalf("put draft back = %d %v", code, out)
	}
	if code, out := root.do("PUT", vpath, map[string]any{"status": "published"}); code != 409 || codeOf(out) != "invalid_state" {
		t.Fatalf("put draft→published = %d %v", code, out)
	}
	if code, out := root.do("PUT", vpath, map[string]any{"copy_from": vid}); code != 400 {
		t.Fatalf("put copy_from = %d %v", code, out)
	}

	// 发布：没有动态码 → totp_required；ops 无权；不存在的版本在动态码之前拒绝
	if code, out := root.do("POST", vpath+"/publish", map[string]any{}); code != 403 || codeOf(out) != "totp_required" {
		t.Fatalf("publish without totp = %d %v", code, out)
	}
	if code, out := ops.do("POST", vpath+"/publish", map[string]any{"totp": "000000"}); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("ops publish = %d %v", code, out)
	}
	if code, out := root.do("POST", base+"/999999/publish", map[string]any{"totp": "000000"}); code != 404 {
		t.Fatalf("publish missing = %d %v", code, out)
	}
	// 错误的动态码 → invalid_totp，窗口不打开（随后不带码 → totp_required）
	if code, out := root.do("POST", vpath+"/publish", map[string]any{"totp": "000000"}); code != 400 || codeOf(out) != "invalid_totp" {
		t.Fatalf("publish wrong totp = %d %v", code, out)
	}
	if code, out := root.do("POST", vpath+"/publish", map[string]any{}); code != 403 || codeOf(out) != "totp_required" {
		t.Fatalf("publish after wrong totp = %d %v", code, out)
	}
	// 库里内容通不过校验：动态码校验之前就拒绝（workflow_version_invalid），同一个动态码之后还能用
	var manifest string
	if err := e.pool.QueryRow(e.ctx, `SELECT manifest::text FROM comfy_workflow_versions WHERE id = $1`, vid).Scan(&manifest); err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE comfy_workflow_versions SET manifest = jsonb_set(manifest, '{models}', '[]') WHERE id = $1`, vid)
	totp := root.totp()
	if code, out := root.do("POST", vpath+"/publish", map[string]any{"totp": totp}); code != 400 || codeOf(out) != "workflow_version_invalid" {
		t.Fatalf("publish invalid stored content = %d %v", code, out)
	}
	if code, out := root.do("POST", vpath+"/publish", map[string]any{}); code != 400 || codeOf(out) != "workflow_version_invalid" {
		t.Fatalf("publish invalid without totp = %d %v", code, out)
	}
	e.exec(`UPDATE comfy_workflow_versions SET manifest = $2::jsonb WHERE id = $1`, vid, manifest)
	code, out = root.do("POST", vpath+"/publish", map[string]any{"totp": totp})
	if code != 200 || out["status"] != "published" || out["published_at"] == nil || out["manifest_hash"] != hash {
		t.Fatalf("publish = %d %v", code, out)
	}
	var diff []byte
	if err := e.pool.QueryRow(e.ctx, `SELECT diff FROM audit_logs WHERE action = 'workflow.version.publish' AND actor_id = $1 AND target_id = $2`,
		root.admin.ID, itoa(vid)).Scan(&diff); err != nil || !strings.Contains(string(diff), hash) {
		t.Fatalf("publish audit = %s %v", diff, err)
	}
	if code, out := root.do("POST", vpath+"/publish", map[string]any{}); code != 409 || codeOf(out) != "invalid_state" {
		t.Fatalf("republish = %d %v", code, out)
	}

	// 已发布：改 graph / manifest → 冻结（validate 同一规则）；只改 changelog 可以；只下线不清空 changelog
	frozen := map[string]any{"manifest": map[string]any{"models": m0(t), "nodes": []any{}, "inputs": []any{}, "outputs": []any{}}}
	if code, out := root.do("PUT", vpath, frozen); code != 409 || codeOf(out) != "workflow_version_frozen" {
		t.Fatalf("put published manifest = %d %v", code, out)
	}
	if code, out := root.do("PUT", vpath+"?validate=1", frozen); code != 409 || codeOf(out) != "workflow_version_frozen" {
		t.Fatalf("validate published manifest = %d %v", code, out)
	}
	if code, out := root.do("PUT", vpath+"?validate=1", map[string]any{"changelog": "仅改说明"}); code != 200 || out["manifest_hash"] != hash {
		t.Fatalf("validate published changelog = %d %v", code, out)
	}
	if code, out := root.do("PUT", vpath, good); code != 200 || out["status"] != "published" {
		t.Fatalf("put published unchanged = %d %v", code, out)
	}
	if code, out := root.do("PUT", vpath, map[string]any{"changelog": "修正说明"}); code != 200 || out["changelog"] != "修正说明" {
		t.Fatalf("put published changelog = %d %v", code, out)
	}

	// 复制为新版本（只给版本号）
	code, out = root.do("POST", base, map[string]any{"version": "1.1", "copy_from": vid})
	if code != 201 || out["status"] != "draft" || out["manifest_hash"] != hash || out["changelog"] != "" {
		t.Fatalf("copy = %d %v", code, out)
	}
	if code, out := root.do("POST", base, map[string]any{"version": "1.2", "copy_from": 999999}); code != 404 {
		t.Fatalf("copy missing = %d %v", code, out)
	}
	// 上次发布打开了二次验证窗口：窗口内再发布不用动态码
	if code, out := root.do("POST", fmt.Sprintf("%s/%d/publish", base, int64(out["id"].(float64))), map[string]any{}); code != 200 || out["status"] != "published" {
		t.Fatalf("publish within step-up window = %d %v", code, out)
	}

	if code, out := root.do("PUT", vpath, map[string]any{"status": "retired"}); code != 200 || out["status"] != "retired" || out["changelog"] != "修正说明" {
		t.Fatalf("retire = %d %v", code, out)
	}
	if code, out := root.do("PUT", vpath, map[string]any{"changelog": "x"}); code != 409 || codeOf(out) != "workflow_version_frozen" {
		t.Fatalf("put retired = %d %v", code, out)
	}
	if code, out := root.do("PUT", vpath+"?validate=1", map[string]any{"changelog": "x"}); code != 409 || codeOf(out) != "workflow_version_frozen" {
		t.Fatalf("validate retired = %d %v", code, out)
	}
	for _, action := range []string{"workflow.version.create", "workflow.version.update"} {
		if n := e.count(`SELECT count(*) FROM audit_logs WHERE action = $1 AND actor_type = 'admin'`, action); n == 0 {
			t.Errorf("audit %s missing", action)
		}
	}
}

// m0 是 sdxl-txt2img 清单里的 models（改 manifest 其它部分时保持 D16 通过）。
func m0(t *testing.T) any {
	var m map[string]any
	if err := json.Unmarshal(wfFixture(t, "manifest.json"), &m); err != nil {
		t.Fatal(err)
	}
	return m["models"]
}

// upload 以 multipart/form-data 上传 field 字段（带浏览器同源请求的 Sec-Fetch-Site: same-origin）。
func (c *client) upload(path, field, filename string, data []byte) (*http.Response, map[string]any) {
	c.e.t.Helper()
	return c.uploadWith(path, field, filename, data, map[string]string{"Sec-Fetch-Site": "same-origin"})
}

// uploadWith 同 upload，但只带 headers 里给的头（不自动加 Sec-Fetch-Site）。
func (c *client) uploadWith(path, field, filename string, data []byte, headers map[string]string) (*http.Response, map[string]any) {
	c.e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile(field, filename)
	_, _ = fw.Write(data)
	_ = mw.Close()
	req, _ := http.NewRequest("POST", c.base+path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp, out
}

func TestAdminWorkflowCover(t *testing.T) {
	e := newEnv(t)
	root := e.loginAs("root", admin.RoleSuper)
	ops := e.loginAs("ops", admin.RoleOps)
	id := createWorkflow(t, root, "sdxl")
	path := fmt.Sprintf("/workflows/%d/cover", id)
	png, err := os.ReadFile(filepath.Join("..", "..", "deploy", "workflows", "sdxl-txt2img", "cover.png"))
	if err != nil {
		t.Fatal(err)
	}

	if code, out := ops.do("GET", path, nil); code != 404 || codeOf(out) != "not_found" {
		t.Fatalf("no cover = %d %v", code, out)
	}
	if resp, out := ops.upload(path, "file", "c.png", png); resp.StatusCode != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("ops upload = %d %v", resp.StatusCode, out)
	}
	if code, out := root.do("POST", path, map[string]any{}); code != 400 {
		t.Fatalf("json upload = %d %v", code, out)
	}
	// 跨站防护：两个头都没有 / 跨站 Sec-Fetch-Site / 外站 Origin → 403
	for name, h := range map[string]map[string]string{
		"no origin headers": {},
		"cross-site":        {"Sec-Fetch-Site": "cross-site"},
		"same-site sibling": {"Sec-Fetch-Site": "same-site"},
		"foreign origin":    {"Origin": "https://evil.example"},
	} {
		if resp, out := root.uploadWith(path, "file", "c.png", png, h); resp.StatusCode != 403 || codeOf(out) != "forbidden" {
			t.Fatalf("%s upload = %d %v", name, resp.StatusCode, out)
		}
	}
	// multipart 只对封面接口放行：其它写接口仍要求 JSON
	if resp, out := root.upload(fmt.Sprintf("/workflows/%d/versions", id), "file", "c.png", png); resp.StatusCode != 400 || codeOf(out) != "bad_request" {
		t.Fatalf("multipart to versions = %d %v", resp.StatusCode, out)
	}
	if resp, out := root.upload(path, "other", "c.png", png); resp.StatusCode != 400 {
		t.Fatalf("missing field = %d %v", resp.StatusCode, out)
	}
	if resp, out := root.upload(path, "file", "c.txt", []byte("hello, not an image")); resp.StatusCode != 415 || codeOf(out) != "unsupported_file_type" {
		t.Fatalf("text upload = %d %v", resp.StatusCode, out)
	}
	big := append(append([]byte{}, png...), make([]byte, 2<<20)...)
	if resp, out := root.upload(path, "file", "big.png", big); resp.StatusCode != 413 || codeOf(out) != "file_too_large" {
		t.Fatalf("big upload = %d %v", resp.StatusCode, out)
	}
	if resp, out := root.upload("/workflows/999999/cover", "file", "c.png", png); resp.StatusCode != 404 || codeOf(out) != "workflow_not_found" {
		t.Fatalf("upload missing workflow = %d %v", resp.StatusCode, out)
	}
	if n := e.count(`SELECT count(*) FROM uploads`); n != 0 {
		t.Fatalf("rejected uploads stored: %d", n)
	}

	resp, out := root.upload(path, "file", "c.png", png)
	url, _ := out["cover_url"].(string)
	if resp.StatusCode != 200 || out["has_cover"] != true || !strings.HasPrefix(url, fmt.Sprintf("/admin/api/v1/workflows/%d/cover?v=", id)) ||
		len(url) != len(fmt.Sprintf("/admin/api/v1/workflows/%d/cover?v=", id))+8 {
		t.Fatalf("upload = %d %v", resp.StatusCode, out)
	}
	resp, _ = ops.send("GET", path, nil)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !bytes.Equal(body, png) || resp.Header.Get("Content-Type") != "image/png" ||
		resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("Cache-Control") != "private, max-age=86400" ||
		resp.Header.Get("Content-Security-Policy") != "default-src 'none'; sandbox" {
		t.Fatalf("get cover = %d %v %d bytes", resp.StatusCode, resp.Header, len(body))
	}
	// 列表带 cover_url；换封面后地址改变、旧文件删除
	_, list := ops.do("GET", "/workflows", nil)
	if list["items"].([]any)[0].(map[string]any)["cover_url"] != url {
		t.Fatalf("list cover_url = %v", list)
	}
	_, out = root.upload(path, "file", "c2.png", png)
	if out["cover_url"] == url || e.count(`SELECT count(*) FROM uploads WHERE purpose = 'workflow_cover'`) != 1 {
		t.Fatalf("replace cover = %v", out)
	}
	if n := e.count(`SELECT count(*) FROM audit_logs WHERE action = 'workflow.update' AND diff::text LIKE '%cover%' AND actor_id = $1`, root.admin.ID); n != 2 {
		t.Fatalf("cover audits = %d", n)
	}
}

func TestAdminWorkflowInstallsDeploymentDashboard(t *testing.T) {
	e := newEnv(t)
	e.machine("SN1")
	uid := e.renter("13600000001")
	inst := e.place(uid)
	e.ack()
	app, _ := e.publishedApp("comfyui", 8188)
	dep := e.deploy(uid, inst, app.ID, nil)

	root := e.loginAs("root", admin.RoleSuper)
	fin := e.loginAs("fin", admin.RoleFinance)
	id := createWorkflow(t, root, "sdxl")
	_, out := root.do("POST", fmt.Sprintf("/workflows/%d/versions", id), wfVersionBody(t, "1.0"))
	vid := int64(out["id"].(float64))
	if code, out := root.do("POST", fmt.Sprintf("/workflows/%d/versions/%d/publish", id, vid), map[string]any{"totp": root.totp()}); code != 200 {
		t.Fatalf("publish = %d %v", code, out)
	}
	now := e.clk.Now()
	e.exec(`INSERT INTO deployment_workflows (deployment_id, workflow_id, workflow_version_id, state, added_at) VALUES ($1, $2, $3, 'pending', $4)`,
		dep.ID, id, vid, now)

	// 安装列表（分页）
	code, out := fin.do("GET", fmt.Sprintf("/workflows/%d/installs?page=1&page_size=10", id), nil)
	items, _ := out["items"].([]any)
	if code != 200 || out["total"].(float64) != 1 || len(items) != 1 {
		t.Fatalf("installs = %d %v", code, out)
	}
	row := items[0].(map[string]any)
	if row["deployment_id"] != dep.ID || row["instance_id"] != inst.ID || row["user_id"].(float64) != float64(uid) || row["phone"] != "13600000001" ||
		row["version"] != "1.0" || row["version_id"].(float64) != float64(vid) || row["state"] != "pending" || row["installed_at"] != nil ||
		row["deployment_state"] != dep.State || row["added_at"] == nil {
		t.Fatalf("install row = %v", row)
	}
	if code, out := fin.do("GET", fmt.Sprintf("/workflows/%d/installs?page=0", id), nil); code != 400 {
		t.Fatalf("bad page = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/workflows/999999/installs", nil); code != 404 || codeOf(out) != "workflow_not_found" {
		t.Fatalf("installs of missing = %d %v", code, out)
	}

	// 部署详情：顶层 workflows（与 deployment.workflows 一致）
	code, out = fin.do("GET", "/deployments/"+dep.ID, nil)
	wfs, _ := out["workflows"].([]any)
	if code != 200 || len(wfs) != 1 {
		t.Fatalf("deployment = %d %v", code, out)
	}
	w0 := wfs[0].(map[string]any)
	if w0["workflow_id"].(float64) != float64(id) || w0["slug"] != "sdxl" || w0["version"] != "1.0" || w0["state"] != "pending" {
		t.Fatalf("deployment workflow = %v", w0)
	}
	if dw, _ := out["deployment"].(map[string]any)["workflows"].([]any); len(dw) != 1 {
		t.Fatalf("deployment.workflows = %v", out["deployment"])
	}
	// 非 ComfyUI 部署：workflows = []
	other, _ := e.publishedApp("vllm", 8000)
	d2 := e.deploy(uid, inst, other.ID, nil)
	if code, out := fin.do("GET", "/deployments/"+d2.ID, nil); code != 200 || out["workflows"] == nil || len(out["workflows"].([]any)) != 0 {
		t.Fatalf("non-comfy deployment = %d %v", code, out)
	}

	// 仪表盘：pending 不算在用；installed 且部署未终结才算；近 24 小时运行数
	_, out = fin.do("GET", "/dashboard", nil)
	if out["workflow_installs"].(float64) != 0 || out["workflow_runs_24h"].(float64) != 0 {
		t.Fatalf("dashboard before = %v", out)
	}
	e.exec(`UPDATE deployment_workflows SET state = 'installed', installed_at = $2 WHERE deployment_id = $1`, dep.ID, now)
	e.exec(`INSERT INTO comfy_workflow_runs (id, deployment_id, user_id, workflow_version_id, prompt_id, state, created_at)
		VALUES ('run-aaaaaaaaaaaa', $1, $2, $3, 'p1', 'done', $4), ('run-bbbbbbbbbbbb', $1, $2, $3, 'p2', 'done', $5)`,
		dep.ID, uid, vid, now, now.Add(-48*time.Hour))
	_, out = fin.do("GET", "/dashboard", nil)
	if out["workflow_installs"].(float64) != 1 || out["workflow_runs_24h"].(float64) != 1 {
		t.Fatalf("dashboard after = %v", out)
	}
	_, out = fin.do("GET", "/workflows", nil)
	if it := out["items"].([]any)[0].(map[string]any); it["installs_live"].(float64) != 1 || it["runs_7d"].(float64) != 2 || it["versions"].(float64) != 1 {
		t.Fatalf("list counts = %v", it)
	}
	e.exec(`UPDATE app_deployments SET state = 'removed' WHERE id = $1`, dep.ID)
	if _, out := fin.do("GET", "/dashboard", nil); out["workflow_installs"].(float64) != 0 {
		t.Fatalf("dashboard after removal = %v", out)
	}
}

// 没接工作流目录（Workflows 为 nil）：工作流路由不注册（404），仪表盘照常、工作流数字为 0。
func TestAdminWorkflowsNotConfigured(t *testing.T) {
	e := newEnvWith(t, func(s *adminapi.Server) { s.Workflows, s.Installer = nil, nil })
	fin := e.loginAs("fin", admin.RoleFinance)
	if code, out := fin.do("GET", "/workflows", nil); code != 404 || codeOf(out) != "not_found" {
		t.Fatalf("workflows = %d %v", code, out)
	}
	for _, r := range e.srv.Routes() {
		if strings.Contains(r.Pattern, "/workflows") {
			t.Fatalf("route registered: %s %s", r.Method, r.Pattern)
		}
	}
	code, out := fin.do("GET", "/dashboard", nil)
	if code != 200 || out["workflow_installs"].(float64) != 0 || out["workflow_runs_24h"].(float64) != 0 {
		t.Fatalf("dashboard = %d %v", code, out)
	}
}
