package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"mime/multipart"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/api"
	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/comfywf"
	"spark-cloud/internal/comfywf/comfytest"
	"spark-cloud/internal/platform/dbtest"
)

// raw 发一个请求并返回原始响应（响应体已读完）。
func (c *client) raw(method, path string, body io.Reader, headers map[string]string) (*http.Response, []byte) {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.base+path, body)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

// upload 以 multipart 的 file 字段上传 data。
func (c *client) uploadFile(path string, data []byte) (int, map[string]any) {
	c.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "我的图.png")
	fw.Write(data)
	mw.Close()
	req, _ := http.NewRequest("POST", c.base+path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

type seededWorkflows struct {
	comfy    appdeploy.App
	comfyVer appdeploy.Version
	ids      map[string]int64 // slug → 工作流 id
	vers     map[string]int64 // slug → 已发布版本 id
}

// seedWorkflows 建 comfyui 应用（支持工作流，主端口 8188）并导入 deploy/workflows 的六个工作流、发布它们的版本；
// qwen-image-txt2img 留作草稿工作流（市场里看不到）。
func (e *testEnv) seedWorkflows() seededWorkflows {
	e.t.Helper()
	ctx := context.Background()
	actor := audit.Actor{AdminID: 1}
	a, err := e.appCat.CreateApp(ctx, actor, appdeploy.AppInput{Slug: "comfyui", Name: "ComfyUI", Summary: "出图", Description: "ComfyUI",
		Category: "image", Icon: "image", Status: appdeploy.StatusPublished})
	if err != nil {
		e.t.Fatal(err)
	}
	spec := appdeploy.Spec{Ports: []appdeploy.PortSpec{{Port: 8188, Label: "ComfyUI"}}, SupportsWorkflows: true}
	v, err := e.appCat.CreateVersion(ctx, actor, a.ID, appdeploy.VersionInput{Version: "1.1.0", ImageIDs: []int64{dbtest.ImageID(e.t, e.pool)},
		Spec: spec, Script: appScript})
	if err != nil {
		e.t.Fatal(err)
	}
	if v, _, err = e.appCat.PublishVersion(ctx, actor, a.ID, v.ID); err != nil {
		e.t.Fatal(err)
	}
	res, err := e.wfCat.ImportDir(ctx, actor, "../../deploy/workflows")
	if err != nil {
		e.t.Fatalf("ImportDir: %v %v", err, res)
	}
	s := seededWorkflows{comfy: a, comfyVer: v, ids: map[string]int64{}, vers: map[string]int64{}}
	for _, r := range res {
		w, err := e.wfCat.WorkflowBySlug(ctx, r.Slug)
		if err != nil {
			e.t.Fatal(err)
		}
		vs, err := e.wfCat.ListVersions(ctx, w.ID)
		if err != nil || len(vs) != 1 {
			e.t.Fatalf("versions of %s: %v %v", r.Slug, vs, err)
		}
		pv, err := e.wfCat.PublishVersion(ctx, actor, w.ID, vs[0].ID)
		if err != nil {
			e.t.Fatalf("publish %s: %v", r.Slug, err)
		}
		s.ids[r.Slug], s.vers[r.Slug] = w.ID, pv.ID
	}
	if len(s.ids) != 6 {
		e.t.Fatalf("imported %v", s.ids)
	}
	e.exec(`UPDATE comfy_workflows SET status = 'published' WHERE slug <> 'qwen-image-txt2img'`)
	return s
}

// comfyDeployment 在一台 running 实例上建 comfyui 部署（queued），返回实例与部署 id。
func (c *client) comfyDeployment(t *testing.T, e *testEnv, s seededWorkflows, phone string) (string, string) {
	t.Helper()
	id := c.runningInstance(t, e, phone)
	code, dep := c.do("POST", "/instances/"+id+"/apps", map[string]any{"app_id": s.comfy.ID})
	if code != 201 {
		t.Fatalf("create comfyui = %d %v", code, dep)
	}
	return id, dep["id"].(string)
}

func TestWorkflowMarketAnonymous(t *testing.T) {
	e := newWorkflowsTestEnv(t, nil)
	s := e.seedWorkflows()
	anon := e.client()
	code, out := anon.do("GET", "/workflows", nil)
	if code != 200 {
		t.Fatalf("GET /workflows = %d %v", code, out)
	}
	items := out["items"].([]any)
	if len(items) != 5 {
		t.Fatalf("items = %d (draft workflow must be hidden): %v", len(items), out)
	}
	var img2img map[string]any
	for _, it := range items {
		m := it.(map[string]any)
		if m["slug"] == "qwen-image-txt2img" {
			t.Fatal("draft workflow listed")
		}
		if m["slug"] == "sdxl-img2img" {
			img2img = m
		}
		for _, k := range []string{"description", "cover_key", "status"} {
			if _, has := m[k]; has {
				t.Fatalf("list item exposes %q: %v", k, m)
			}
		}
	}
	if img2img == nil || img2img["id"].(float64) != float64(s.ids["sdxl-img2img"]) || img2img["category"] != "img2img" ||
		img2img["installs"].(float64) != 0 || len(img2img["tags"].([]any)) == 0 || img2img["name"] != "SDXL 图生图" {
		t.Fatalf("sdxl-img2img item = %v", img2img)
	}
	ver := img2img["version"].(map[string]any)
	if ver["id"].(float64) != float64(s.vers["sdxl-img2img"]) || ver["version"] != "1.0.0" || ver["models"].(float64) != 1 || ver["nodes"].(float64) != 0 ||
		ver["quick_run"] != true || ver["download_gb"].(float64) <= 6 || ver["comfyui_min"] != "0.3.60" {
		t.Fatalf("version summary = %v", ver)
	}
	coverURL, _ := img2img["cover_url"].(string)
	if !strings.HasPrefix(coverURL, "/api/v1/workflows/sdxl-img2img/cover") {
		t.Fatalf("cover_url = %v", img2img["cover_url"])
	}
	raw, _ := anon.raw("GET", "/workflows", nil, nil)
	if raw.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("list Cache-Control = %q", raw.Header.Get("Cache-Control"))
	}
	if strings.Contains(rawJSON(out), "class_type") {
		t.Fatal("list leaks api_prompt / graph")
	}

	// 分类过滤
	code, out = anon.do("GET", "/workflows?category=img2img", nil)
	if code != 200 || len(out["items"].([]any)) != 1 || out["items"].([]any)[0].(map[string]any)["slug"] != "sdxl-img2img" {
		t.Fatalf("category filter = %d %v", code, out)
	}
	if code, out := anon.do("GET", "/workflows?category=nope", nil); code != 400 || out["code"] != "bad_request" {
		t.Fatalf("bad category = %d %v", code, out)
	}

	// 详情：带 description / manifest / changelog / versions，不带 api_prompt 本体
	resp, body := anon.raw("GET", "/workflows/sdxl-img2img", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("detail = %d %s", resp.StatusCode, body)
	}
	for _, leak := range []string{"class_type", "api_prompt", `"graph"`, "cover_key"} {
		if strings.Contains(string(body), leak) {
			t.Fatalf("detail leaks %q: %s", leak, body)
		}
	}
	var d map[string]any
	if err := json.Unmarshal(body, &d); err != nil {
		t.Fatal(err)
	}
	dv := d["version"].(map[string]any)
	m := dv["manifest"].(map[string]any)
	if d["description"] == "" || dv["changelog"] == "" || dv["published_at"] == nil || len(m["inputs"].([]any)) == 0 || len(m["models"].([]any)) != 1 ||
		dv["quick_run"] != true || len(d["versions"].([]any)) != 1 || d["cover_url"] != coverURL {
		t.Fatalf("detail = %v", d)
	}
	for _, slug := range []string{"nope", "qwen-image-txt2img"} {
		if code, out := anon.do("GET", "/workflows/"+slug, nil); code != 404 || out["code"] != "workflow_not_found" {
			t.Fatalf("GET /workflows/%s = %d %v", slug, code, out)
		}
	}

	// 封面：公共长缓存 + ETag，If-None-Match → 304
	resp, img := anon.raw("GET", "/workflows/sdxl-img2img/cover", nil, nil)
	etag := resp.Header.Get("ETag")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" || resp.Header.Get("Cache-Control") != "public, max-age=86400" ||
		resp.Header.Get("Content-Security-Policy") != "default-src 'none'; sandbox" ||
		resp.Header.Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(etag, `"`) || len(img) == 0 || !bytes.HasPrefix(img, []byte("\x89PNG")) {
		t.Fatalf("cover = %d %v (%d bytes)", resp.StatusCode, resp.Header, len(img))
	}
	var key string
	if err := e.pool.QueryRow(context.Background(), `SELECT cover_key FROM comfy_workflows WHERE slug = 'sdxl-img2img'`).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(etag, key) || strings.Contains(etag, strings.TrimSuffix(key[strings.LastIndexByte(key, '/')+1:], ".png")) {
		t.Fatalf("etag %s leaks the cover key %s", etag, key)
	}
	resp, img = anon.raw("GET", "/workflows/sdxl-img2img/cover", nil, map[string]string{"If-None-Match": `"other", ` + etag})
	if resp.StatusCode != 304 || len(img) != 0 || resp.Header.Get("ETag") != etag || resp.Header.Get("Cache-Control") != "public, max-age=86400" {
		t.Fatalf("conditional cover = %d %v", resp.StatusCode, resp.Header)
	}
	if resp, _ := anon.raw("GET", "/workflows/sdxl-img2img/cover", nil, map[string]string{"If-None-Match": `"other"`}); resp.StatusCode != 200 {
		t.Fatalf("mismatched If-None-Match = %d", resp.StatusCode)
	}
	e.exec(`UPDATE comfy_workflows SET cover_key = NULL WHERE slug = 'sdxl-txt2img'`)
	if code, out := anon.do("GET", "/workflows/sdxl-txt2img/cover", nil); code != 404 || out["code"] != "not_found" {
		t.Fatalf("no cover = %d %v", code, out)
	}
	if code, out := anon.do("GET", "/workflows/qwen-image-txt2img/cover", nil); code != 404 || out["code"] != "workflow_not_found" {
		t.Fatalf("draft cover = %d %v", code, out)
	}
}

func TestWorkflowMarketIPRateLimit(t *testing.T) {
	e := newWorkflowsTestEnv(t, func(s *api.Server) { s.CalendarLimit = 3 })
	e.seedWorkflows()
	anon := e.client()
	for i, p := range []string{"/workflows", "/workflows/sdxl-img2img", "/workflows"} {
		if resp, _ := anon.raw("GET", p, nil, nil); resp.StatusCode != 200 {
			t.Fatalf("request %d %s = %d", i, p, resp.StatusCode)
		}
	}
	if code, out := anon.do("GET", "/workflows", nil); code != http.StatusTooManyRequests || out["code"] != "rate_limited" {
		t.Fatalf("4th = %d %v", code, out)
	}
	// 封面不占列表 / 详情的额度（一页市场有十几张封面），有自己的宽松限流
	for i := 0; i < 20; i++ {
		if resp, _ := anon.raw("GET", "/workflows/sdxl-img2img/cover", nil, nil); resp.StatusCode != 200 {
			t.Fatalf("cover %d after list limit = %d", i, resp.StatusCode)
		}
	}
	if code, out := anon.do("GET", "/apps", nil); code != http.StatusTooManyRequests {
		t.Fatalf("shared limiter with /apps = %d %v", code, out)
	}
	e.clk.Advance(time.Minute)
	if code, _ := anon.do("GET", "/workflows", nil); code != 200 {
		t.Fatalf("after a minute = %d", code)
	}
}

func TestOrderAndBookingWithWorkflows(t *testing.T) {
	e := newWorkflowsTestEnv(t, nil)
	s := e.seedWorkflows()
	dbtest.CreateMachine(t, e.pool, "SN001")
	dbtest.CreateMachine(t, e.pool, "SN002")
	dbtest.CreateMachine(t, e.pool, "SN003")
	c, _ := e.fundedClient("13800000201")
	img := dbtest.ImageID(t, e.pool)
	pid := dbtest.ProductID(t, e.pool)
	wids := []int64{s.ids["sdxl-txt2img"], s.ids["sdxl-img2img"]}

	// 没有 app_id 的预装工作流 → 400，不下单
	if code, out := c.do("POST", "/orders", map[string]any{"product_id": pid, "duration": "1d", "image_id": img, "app_workflow_ids": wids}); code != 400 ||
		out["code"] != "bad_request" {
		t.Fatalf("order without app_id = %d %v", code, out)
	}
	booking := map[string]any{"product_id": pid, "image_id": img, "count": 1, "duration": "1d", "quantity": 1,
		"start_at": dbtest.T0.Add(3 * time.Hour).Format(time.RFC3339), "app_workflow_ids": wids}
	if code, out := c.do("POST", "/bookings", booking); code != 400 || out["code"] != "bad_request" {
		t.Fatalf("booking without app_id = %d %v", code, out)
	}
	if n := e.countRows(`SELECT count(*) FROM instances`); n != 0 {
		t.Fatalf("instances after rejected = %d", n)
	}
	// 草稿工作流 → workflow_not_found（经 PrepareInstall → ResolveWorkflows），不下单
	if code, out := c.do("POST", "/orders", map[string]any{"product_id": pid, "duration": "1d", "image_id": img, "app_id": s.comfy.ID,
		"app_workflow_ids": []int64{s.ids["qwen-image-txt2img"]}}); code != 404 || out["code"] != "workflow_not_found" {
		t.Fatalf("order with draft workflow = %d %v", code, out)
	}

	// 超过上限：去重后 > 8 → workflow_limit（ResolveWorkflows）；列表本身 > 64 → 在进事务之前就拒绝
	nine := []int64{}
	for i := int64(1); i <= 9; i++ {
		nine = append(nine, 100000+i)
	}
	huge := slices.Repeat(wids, 33) // 66 个（去重后只有 2 个）也拒绝：只看列表长度
	for _, ids := range [][]int64{nine, huge} {
		if code, out := c.do("POST", "/orders", map[string]any{"product_id": pid, "duration": "1d", "image_id": img, "app_id": s.comfy.ID,
			"app_workflow_ids": ids}); code != 409 || out["code"] != "workflow_limit" {
			t.Fatalf("order with %d workflows = %d %v", len(ids), code, out)
		}
		b := maps.Clone(booking)
		b["app_id"], b["app_workflow_ids"] = s.comfy.ID, ids
		if code, out := c.do("POST", "/bookings", b); code != 409 || out["code"] != "workflow_limit" {
			t.Fatalf("booking with %d workflows = %d %v", len(ids), code, out)
		}
	}
	if code, out := c.do("POST", "/orders", map[string]any{"product_id": pid, "duration": "1d", "image_id": img, "app_id": s.comfy.ID,
		"app_workflow_ids": slices.Repeat(wids, 32)}); code != 201 { // 64 个（去重后 2 个）可以
		t.Fatalf("order with 64 ids = %d %v", code, out)
	}

	code, out := c.do("POST", "/orders", map[string]any{"product_id": pid, "duration": "1d", "image_id": img, "app_id": s.comfy.ID, "app_workflow_ids": wids})
	if code != 201 {
		t.Fatalf("order with workflows = %d %v", code, out)
	}
	inst := out["instance"].(map[string]any)["id"].(string)
	if n := e.countRows(`SELECT count(*) FROM deployment_workflows dw JOIN app_deployments d ON d.id = dw.deployment_id
		WHERE d.instance_id = $1 AND dw.state = 'pending'`, inst); n != 2 {
		t.Fatalf("order deployment workflows = %d", n)
	}

	booking["app_id"] = s.comfy.ID
	code, out = c.do("POST", "/bookings", booking)
	if code != 201 {
		t.Fatalf("booking with workflows = %d %v", code, out)
	}
	bi := out["instances"].([]any)[0].(map[string]any)["id"].(string)
	if n := e.countRows(`SELECT count(*) FROM deployment_workflows dw JOIN app_deployments d ON d.id = dw.deployment_id WHERE d.instance_id = $1`, bi); n != 2 {
		t.Fatalf("booking deployment workflows = %d", n)
	}
}

func TestInstanceWorkflowsInstall(t *testing.T) {
	e := newWorkflowsTestEnv(t, nil)
	s := e.seedWorkflows()
	apps := e.seedApps()
	c := e.client()
	id, depID := c.comfyDeployment(t, e, s, "13800000211")
	base := "/instances/" + id + "/apps/" + depID + "/workflows"

	if code, out := c.do("GET", base, nil); code != 200 || len(out["items"].([]any)) != 0 {
		t.Fatalf("empty list = %d %v", code, out)
	}
	wid := s.ids["sdxl-img2img"]
	code, out := c.do("POST", base, map[string]any{"workflow_id": wid})
	if code != 201 {
		t.Fatalf("add = %d %v", code, out)
	}
	dep := out["deployment"].(map[string]any)
	ws := out["workflows"].([]any)
	if dep["id"] != depID || dep["app_slug"] != "comfyui" || dep["state"] != "queued" || len(ws) != 1 || len(dep["workflows"].([]any)) != 1 {
		t.Fatalf("add result = %v", out)
	}
	w := ws[0].(map[string]any)
	if w["workflow_id"].(float64) != float64(wid) || w["slug"] != "sdxl-img2img" || w["state"] != "pending" || w["version"] != "1.0.0" ||
		w["version_id"].(float64) != float64(s.vers["sdxl-img2img"]) || w["quick_run"] != true || w["update_available"] != false {
		t.Fatalf("installed = %v", w)
	}
	if code, out := c.do("POST", base, map[string]any{"workflow_id": wid}); code != 409 || out["code"] != "workflow_exists" {
		t.Fatalf("duplicate = %d %v", code, out)
	}
	if code, out := c.do("POST", base, map[string]any{"workflow_id": s.ids["qwen-image-txt2img"]}); code != 404 || out["code"] != "workflow_not_found" {
		t.Fatalf("draft = %d %v", code, out)
	}
	if code, out := c.do("POST", base, map[string]any{"workflow_id": wid, "bogus": 1}); code != 400 {
		t.Fatalf("unknown field = %d %v", code, out)
	}
	// 部署列表与单个部署视图都带 workflows[]
	_, list := c.do("GET", "/instances/"+id+"/apps", nil)
	if lw := list["items"].([]any)[0].(map[string]any)["workflows"].([]any); len(lw) != 1 || lw[0].(map[string]any)["slug"] != "sdxl-img2img" {
		t.Fatalf("deployment view workflows = %v", lw)
	}
	if code, out := c.do("GET", base, nil); code != 200 || len(out["items"].([]any)) != 1 {
		t.Fatalf("list = %d %v", code, out)
	}

	// 已装工作流详情：按已装的版本给表单（快速出图抽屉用），即使有了新版本
	ctx := context.Background()
	base0, err := e.wfCat.GetVersion(ctx, s.vers["sdxl-img2img"])
	if err != nil {
		t.Fatal(err)
	}
	m2 := base0.Manifest
	m2.Inputs = m2.Inputs[:len(m2.Inputs)-1]
	e.clk.Advance(time.Minute)
	v2, err := e.wfCat.CreateVersion(ctx, audit.Actor{AdminID: 1}, wid, comfywf.VersionInput{Version: "1.1.0", CopyFrom: &base0.ID, Manifest: m2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.wfCat.PublishVersion(ctx, audit.Actor{AdminID: 1}, wid, v2.ID); err != nil {
		t.Fatal(err)
	}
	resp, body := c.raw("GET", fmt.Sprintf("%s/%d", base, wid), nil, nil)
	if resp.StatusCode != 200 || strings.Contains(string(body), "class_type") || strings.Contains(string(body), "api_prompt") {
		t.Fatalf("installed detail = %d %s", resp.StatusCode, body)
	}
	var det map[string]any
	if err := json.Unmarshal(body, &det); err != nil {
		t.Fatal(err)
	}
	dw := det["workflow"].(map[string]any)
	if dw["slug"] != "sdxl-img2img" || dw["version"] != "1.0.0" || dw["update_available"] != true || dw["latest_version"] != "1.1.0" ||
		det["quick_run"] != true || len(det["inputs"].([]any)) != len(base0.Manifest.Inputs) || len(det["outputs"].([]any)) != len(base0.Manifest.Outputs) {
		t.Fatalf("installed detail = %v", det)
	}
	if in0 := det["inputs"].([]any)[0].(map[string]any); in0["key"] != base0.Manifest.Inputs[0].Key || in0["type"] == nil || in0["label"] == nil {
		t.Fatalf("input = %v", in0)
	}
	if code, out := c.do("GET", fmt.Sprintf("%s/%d", base, s.ids["sdxl-txt2img"]), nil); code != 404 || out["code"] != "workflow_not_found" {
		t.Fatalf("not installed detail = %d %v", code, out)
	}
	if code, out := c.do("GET", base+"/abc", nil); code != 404 || out["code"] != "workflow_not_found" {
		t.Fatalf("bad wid detail = %d %v", code, out)
	}

	// 更新：不带请求体 / 带 version_id 都可以；目标就是当前版本 → 无操作，同样形状
	resp, body = c.raw("PUT", fmt.Sprintf("%s/%d", base, wid), nil, map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"deployment"`) || !strings.Contains(string(body), `"workflows"`) {
		t.Fatalf("update empty body = %d %s", resp.StatusCode, body)
	}
	if code, out := c.do("PUT", fmt.Sprintf("%s/%d", base, wid), map[string]any{"version_id": s.vers["sdxl-img2img"]}); code != 200 ||
		out["deployment"].(map[string]any)["id"] != depID || len(out["workflows"].([]any)) != 1 {
		t.Fatalf("update = %d %v", code, out)
	}
	if code, out := c.do("PUT", base+"/abc", map[string]any{}); code != 404 || out["code"] != "workflow_not_found" {
		t.Fatalf("bad wid = %d %v", code, out)
	}

	// 别人：四个接口一律 404，集合不变
	other := e.client()
	oid := other.runningInstance(t, e, "13800000212")
	for _, p := range []struct {
		method, path string
		body         any
	}{
		{"GET", base, nil}, {"GET", fmt.Sprintf("%s/%d", base, wid), nil}, {"POST", base, map[string]any{"workflow_id": s.ids["sdxl-txt2img"]}},
		{"PUT", fmt.Sprintf("%s/%d", base, wid), map[string]any{}}, {"DELETE", fmt.Sprintf("%s/%d", base, wid), nil},
		{"GET", "/instances/" + oid + "/apps/" + depID + "/workflows", nil},
		{"POST", "/instances/" + oid + "/apps/" + depID + "/workflows", map[string]any{"workflow_id": s.ids["sdxl-txt2img"]}},
	} {
		if code, out := other.do(p.method, p.path, p.body); code != 404 || out["code"] != "not_found" {
			t.Fatalf("stranger %s %s = %d %v", p.method, p.path, code, out)
		}
	}
	if n := e.countRows(`SELECT count(*) FROM deployment_workflows WHERE deployment_id = $1`, depID); n != 1 {
		t.Fatalf("set after stranger = %d", n)
	}

	// 非 comfyui 部署 → workflow_app_unsupported
	_, vdep := c.do("POST", "/instances/"+id+"/apps", map[string]any{"app_id": apps.vllm.ID})
	if code, out := c.do("POST", "/instances/"+id+"/apps/"+vdep["id"].(string)+"/workflows", map[string]any{"workflow_id": wid}); code != 409 ||
		out["code"] != "workflow_app_unsupported" {
		t.Fatalf("vllm = %d %v", code, out)
	}

	// 移除
	code, out = c.do("DELETE", fmt.Sprintf("%s/%d", base, wid), nil)
	if code != 200 || out["deployment"].(map[string]any)["id"] != depID || len(out["workflows"].([]any)) != 0 ||
		len(out["deployment"].(map[string]any)["workflows"].([]any)) != 0 {
		t.Fatalf("remove = %d %v", code, out)
	}
	if code, out := c.do("DELETE", fmt.Sprintf("%s/%d", base, wid), nil); code != 404 || out["code"] != "workflow_not_found" {
		t.Fatalf("remove again = %d %v", code, out)
	}
}

func TestWorkflowWriteRateLimit(t *testing.T) {
	e := newWorkflowsTestEnv(t, nil) // 默认 AppsLimit = 60，与应用部署写操作共用
	s := e.seedWorkflows()
	c := e.client()
	id, depID := c.comfyDeployment(t, e, s, "13800000221") // 第 1 次写
	base := "/instances/" + id + "/apps/" + depID + "/workflows"
	for i := 2; i <= 60; i++ { // 失败的写也计数
		if code, out := c.do("DELETE", fmt.Sprintf("%s/%d", base, s.ids["sdxl-txt2img"]), nil); code != 404 {
			t.Fatalf("write %d = %d %v", i, code, out)
		}
	}
	if code, out := c.do("POST", base, map[string]any{"workflow_id": s.ids["sdxl-txt2img"]}); code != http.StatusTooManyRequests || out["code"] != "rate_limited" {
		t.Fatalf("61st write = %d %v", code, out)
	}
	if code, out := c.do("POST", "/instances/"+id+"/apps/"+depID+"/stop", map[string]any{}); code != http.StatusTooManyRequests {
		t.Fatalf("app write after limit = %d %v", code, out)
	}
	if code, out := c.do("GET", base, nil); code != 200 {
		t.Fatalf("read after limit = %d %v", code, out)
	}
	e.clk.Advance(time.Hour)
	if code, out := c.do("POST", base, map[string]any{"workflow_id": s.ids["sdxl-txt2img"]}); code != 201 {
		t.Fatalf("write after an hour = %d %v", code, out)
	}
}

func TestQuickRunFlow(t *testing.T) {
	e := newWorkflowsTestEnv(t, nil)
	s := e.seedWorkflows()
	c := e.client()
	id, depID := c.comfyDeployment(t, e, s, "13800000231")
	wid := s.ids["sdxl-img2img"]
	base := fmt.Sprintf("/instances/%s/apps/%s/workflows/%d", id, depID, wid)
	if code, out := c.do("POST", "/instances/"+id+"/apps/"+depID+"/workflows", map[string]any{"workflow_id": wid}); code != 201 {
		t.Fatalf("add = %d %v", code, out)
	}
	// 部署 queued、工作流待装：ComfyUI 未运行
	if code, out := c.uploadFile(base+"/uploads", comfytest.OnePixelPNG); code != 409 || out["code"] != "app_invalid_state" {
		t.Fatalf("upload before running = %d %v", code, out)
	}
	e.exec(`UPDATE app_deployments SET state = 'running' WHERE id = $1`, depID)
	e.exec(`UPDATE deployment_workflows SET state = 'installed', installed_at = now() WHERE deployment_id = $1`, depID)

	// 上传：必须 multipart；平台重命名
	if code, out := c.do("POST", base+"/uploads", map[string]any{}); code != 400 || out["code"] != "bad_request" {
		t.Fatalf("json upload = %d %v", code, out)
	}
	code, up := c.uploadFile(base+"/uploads", comfytest.OnePixelPNG)
	name, _ := up["name"].(string)
	if code != 201 || !strings.HasPrefix(name, "spark-quickrun/") || !strings.HasSuffix(name, ".png") || strings.Contains(name, "我的图") {
		t.Fatalf("upload = %d %v", code, up)
	}
	if code, out := c.uploadFile(base+"/uploads", []byte("not an image at all")); code != 415 || out["code"] != "unsupported_file_type" {
		t.Fatalf("upload text = %d %v", code, out)
	}
	if code, out := c.uploadFile(base+"/uploads", append(slices.Clone(comfytest.OnePixelPNG), make([]byte, comfywf.MaxUploadBytes)...)); code != 413 || out["code"] != "file_too_large" {
		t.Fatalf("upload too large = %d %v", code, out)
	}

	// 运行
	code, run := c.do("POST", base+"/runs", map[string]any{"inputs": map[string]any{"image": name, "steps": 4, "seed": 9007199254740991}})
	if code != 201 || run["state"] != "queued" || run["seed"].(float64) != 9007199254740991 || len(run["files"].([]any)) != 0 || run["prompt_id"] == "" {
		t.Fatalf("start = %d %v", code, run)
	}
	runID := run["id"].(string)
	prompts := e.comfy.Prompts()
	if len(prompts) != 1 || !strings.Contains(string(prompts[0].Prompt), "9007199254740991") || !strings.Contains(string(prompts[0].Prompt), name) {
		t.Fatalf("prompts = %+v", prompts)
	}
	if code, out := c.do("POST", base+"/runs", map[string]any{"inputs": map[string]any{"image": name, "steps": "many"}}); code != 400 ||
		out["code"] != "workflow_input_invalid" {
		t.Fatalf("bad input = %d %v", code, out)
	}
	if code, out := c.do("GET", base+"/runs/"+runID, nil); code != 200 || out["state"] != "queued" || out["id"] != runID {
		t.Fatalf("get queued = %d %v", code, out)
	}
	e.comfy.SetHistory(run["prompt_id"].(string), comfytest.DoneN(2))
	code, got := c.do("GET", base+"/runs/"+runID, nil)
	files, _ := got["files"].([]any)
	if code != 200 || got["state"] != "done" || len(files) != 2 {
		t.Fatalf("get done = %d %v", code, got)
	}
	for i, f := range files {
		fm := f.(map[string]any)
		want := fmt.Sprintf("/api/v1%s/runs/%s/files/%d", base, runID, i)
		if fm["url"] != want || fm["kind"] != "image" || fm["index"].(float64) != float64(i) {
			t.Fatalf("file %d = %v (want url %s)", i, fm, want)
		}
	}
	resp, body := c.raw("GET", base+"/runs/"+runID+"/files/1", nil, nil)
	if resp.StatusCode != 200 || !bytes.Equal(body, comfytest.OnePixelPNG) || resp.Header.Get("Content-Type") != "image/png" ||
		resp.Header.Get("Cache-Control") != "private, max-age=3600" || resp.Header.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.HasPrefix(resp.Header.Get("Content-Disposition"), "inline") {
		t.Fatalf("file = %d %v %d bytes", resp.StatusCode, resp.Header, len(body))
	}
	// 单段 Range 透传 206（视频拖动进度条）；平台自己的 Content-Type / nosniff / CSP 不变
	resp, body = c.raw("GET", base+"/runs/"+runID+"/files/1", nil, map[string]string{"Range": "bytes=1-3"})
	if resp.StatusCode != 206 || !bytes.Equal(body, comfytest.OnePixelPNG[1:4]) ||
		resp.Header.Get("Content-Range") != fmt.Sprintf("bytes 1-3/%d", len(comfytest.OnePixelPNG)) || resp.Header.Get("Accept-Ranges") != "bytes" ||
		resp.Header.Get("Content-Length") != "3" || resp.Header.Get("Content-Type") != "image/png" || resp.Header.Get("X-Content-Type-Options") != "nosniff" ||
		resp.Header.Get("Content-Security-Policy") != "default-src 'none'; sandbox" {
		t.Fatalf("range file = %d %v %q", resp.StatusCode, resp.Header, body)
	}
	// 多段 / 不合法 → 完整 200
	for _, rng := range []string{"bytes=0-1,3-4", "bytes=-3", "bytes=x"} {
		resp, body = c.raw("GET", base+"/runs/"+runID+"/files/1", nil, map[string]string{"Range": rng})
		if resp.StatusCode != 200 || !bytes.Equal(body, comfytest.OnePixelPNG) || resp.Header.Get("Content-Range") != "" {
			t.Fatalf("range %q = %d %v %d bytes", rng, resp.StatusCode, resp.Header, len(body))
		}
	}
	for _, p := range []string{"/files/2", "/files/-1", "/files/x"} {
		if code, out := c.do("GET", base+"/runs/"+runID+p, nil); code != 404 {
			t.Fatalf("file %s = %d %v", p, code, out)
		}
	}
	if code, out := c.do("GET", base+"/runs/run-nope", nil); code != 404 {
		t.Fatalf("unknown run = %d %v", code, out)
	}

	// ComfyUI 拒绝 → 422
	e.comfy.RejectPrompts("Value not in list")
	if code, out := c.do("POST", base+"/runs", map[string]any{"inputs": map[string]any{"image": name}}); code != 422 || out["code"] != "workflow_prompt_rejected" ||
		!strings.Contains(out["message"].(string), "Value not in list") {
		t.Fatalf("rejected = %d %v", code, out)
	}
	e.comfy.RejectPrompts("")

	// 别人：四个接口 404
	other := e.client()
	other.runningInstance(t, e, "13800000232")
	for _, p := range []struct {
		method, path string
		body         any
	}{{"POST", base + "/runs", map[string]any{"inputs": map[string]any{"image": name}}}, {"GET", base + "/runs/" + runID, nil},
		{"GET", base + "/runs/" + runID + "/files/0", nil}} {
		if code, out := other.do(p.method, p.path, p.body); code != 404 || out["code"] != "not_found" {
			t.Fatalf("stranger %s %s = %d %v", p.method, p.path, code, out)
		}
	}
	if code, out := other.uploadFile(base+"/uploads", comfytest.OnePixelPNG); code != 404 || out["code"] != "not_found" {
		t.Fatalf("stranger upload = %d %v", code, out)
	}
	// 大文件被拒时服务端先读完请求体再回错误：客户端拿到 JSON 404，而不是发送中途连接被重置
	big := append(slices.Clone(comfytest.OnePixelPNG), make([]byte, 12<<20)...)
	for i := 0; i < 3; i++ {
		if code, out := other.uploadFile(base+"/uploads", big); code != 404 || out["code"] != "not_found" {
			t.Fatalf("stranger big upload %d = %d %v", i, code, out)
		}
	}

	// 每用户每小时 120 次（按库计数，含已结束的）：第 121 次 → 429
	var uid int64
	if err := e.pool.QueryRow(context.Background(), `SELECT user_id FROM app_deployments WHERE id = $1`, depID).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	e.exec(`INSERT INTO comfy_workflow_runs (id, deployment_id, user_id, workflow_version_id, prompt_id, seed, state, created_at, finished_at)
		SELECT 'run-fill' || lpad(g::text, 8, '0'), $1, $2, $3, 'p' || g, 1, 'done', $4, $4 FROM generate_series(1, 119) g`,
		depID, uid, s.vers["sdxl-img2img"], e.clk.Now())
	if n := e.countRows(`SELECT count(*) FROM comfy_workflow_runs WHERE user_id = $1`, uid); n != 120 {
		t.Fatalf("runs = %d", n)
	}
	if code, out := c.do("POST", base+"/runs", map[string]any{"inputs": map[string]any{"image": name}}); code != http.StatusTooManyRequests ||
		out["code"] != "rate_limited" {
		t.Fatalf("121st run = %d %v", code, out)
	}
}

// POST …/runs 的进程内尝试限流（每用户每小时 240 次，在解析请求体之前计数；按库的 120 次仍是权威限制）。
func TestQuickRunAttemptLimit(t *testing.T) {
	e := newWorkflowsTestEnv(t, nil)
	s := e.seedWorkflows()
	c := e.client()
	id, depID := c.comfyDeployment(t, e, s, "13800000241")
	base := fmt.Sprintf("/instances/%s/apps/%s/workflows", id, depID)
	for i := 1; i <= 240; i++ { // 失败的尝试（不存在的工作流）也计数
		if code, out := c.do("POST", base+"/abc/runs", map[string]any{}); code != 404 {
			t.Fatalf("attempt %d = %d %v", i, code, out)
		}
	}
	if code, out := c.do("POST", fmt.Sprintf("%s/%d/runs", base, s.ids["sdxl-img2img"]), map[string]any{"inputs": map[string]any{}}); code != http.StatusTooManyRequests ||
		out["code"] != "rate_limited" {
		t.Fatalf("241st attempt = %d %v", code, out)
	}
	if code, _ := c.do("POST", base+"/abc/runs", "not json"); code != http.StatusTooManyRequests {
		t.Fatalf("limited before decoding = %d", code)
	}
	other := e.client()
	_, odep := other.comfyDeployment(t, e, s, "13800000242")
	if code, _ := other.do("POST", "/instances/x/apps/"+odep+"/workflows/abc/runs", map[string]any{}); code != 404 {
		t.Fatalf("other user = %d", code)
	}
	e.clk.Advance(time.Hour)
	if code, _ := c.do("POST", base+"/abc/runs", map[string]any{}); code != 404 {
		t.Fatalf("after an hour = %d", code)
	}
}
