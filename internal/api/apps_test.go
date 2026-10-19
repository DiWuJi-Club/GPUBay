package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/api"
	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/dbtest"
)

const appScript = "#!/bin/sh\ncase \"$1\" in install) ;; start) sleep 1 & echo $! > \"$SPARK_APP_DIR/service.pid\" ;; esac\n"

func vllmSpec() appdeploy.Spec {
	return appdeploy.Spec{
		Ports: []appdeploy.PortSpec{{Port: 8000, Label: "API", Path: "/v1", HealthPath: "/health"}},
		Env: []appdeploy.EnvSpec{
			{Name: "MODEL_ID", Label: "模型", Type: "enum", Required: true, Options: []string{"Qwen/Qwen3-8B", "Qwen/Qwen3-4B"}, Default: "Qwen/Qwen3-8B"},
			{Name: "API_KEY", Label: "接口密钥", Type: "secret"},
		},
		Links: []appdeploy.LinkSpec{{Label: "文档", Path: "/docs"}},
	}
}

type seededApps struct {
	vllm, jupyter, other appdeploy.App
	vllmVer              appdeploy.Version
}

// seedApps 建 vllm（已上架，带说明）、jupyterlab（已上架，必填 secret）、webui（已上架，只兼容另一个镜像）、drafty（草稿应用，有已发布版本）。
func (e *testEnv) seedApps() seededApps {
	e.t.Helper()
	ctx := context.Background()
	actor := audit.Actor{AdminID: 1}
	mk := func(slug, status string, spec appdeploy.Spec, imageIDs []int64) (appdeploy.App, appdeploy.Version) {
		a, err := e.appCat.CreateApp(ctx, actor, appdeploy.AppInput{Slug: slug, Name: slug + " 应用", Summary: "简介 " + slug,
			Description: "说明 " + slug, Category: "llm", Icon: "llm", Status: status})
		if err != nil {
			e.t.Fatalf("CreateApp %s: %v", slug, err)
		}
		v, err := e.appCat.CreateVersion(ctx, actor, a.ID, appdeploy.VersionInput{Version: "1.0.0", ImageIDs: imageIDs, Spec: spec, Script: appScript})
		if err != nil {
			e.t.Fatalf("CreateVersion %s: %v", slug, err)
		}
		if v, _, err = e.appCat.PublishVersion(ctx, actor, a.ID, v.ID); err != nil {
			e.t.Fatalf("PublishVersion %s: %v", slug, err)
		}
		return a, v
	}
	img := dbtest.ImageID(e.t, e.pool)
	var otherImg int64
	if err := e.pool.QueryRow(ctx, `SELECT id FROM images WHERE enabled AND id <> $1 ORDER BY sort, id LIMIT 1`, img).Scan(&otherImg); err != nil {
		e.t.Fatal(err)
	}
	var s seededApps
	s.vllm, s.vllmVer = mk("vllm", appdeploy.StatusPublished, vllmSpec(), []int64{img})
	s.jupyter, _ = mk("jupyterlab", appdeploy.StatusPublished, appdeploy.Spec{
		Ports: []appdeploy.PortSpec{{Port: 8888}},
		Env:   []appdeploy.EnvSpec{{Name: "JUPYTER_TOKEN", Label: "访问令牌", Type: "secret", Required: true}},
	}, []int64{img})
	s.other, _ = mk("webui", appdeploy.StatusPublished, appdeploy.Spec{Ports: []appdeploy.PortSpec{{Port: 3000}}}, []int64{otherImg})
	mk("drafty", appdeploy.StatusDraft, appdeploy.Spec{Ports: []appdeploy.PortSpec{{Port: 9000}}}, []int64{img})
	return s
}

func (e *testEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("exec %q: %v", sql, err)
	}
}

func (e *testEnv) countRows(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

// rawJSON 把解码后的响应重新编码，用于断言整个响应里不出现某个字符串。
func rawJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestAppsCatalogAnonymous(t *testing.T) {
	e := newAppsTestEnv(t, nil)
	e.seedApps()
	anon := e.client()
	code, out := anon.do("GET", "/apps", nil)
	if code != 200 {
		t.Fatalf("GET /apps = %d %v", code, out)
	}
	items := out["items"].([]any)
	slugs := []string{}
	for _, it := range items {
		m := it.(map[string]any)
		slugs = append(slugs, m["slug"].(string))
		ver := m["version"].(map[string]any)
		if _, ok := ver["env"].([]any); !ok || m["images"] == nil || len(m["images"].([]any)) != 1 {
			t.Fatalf("item %v: version.env / images missing", m)
		}
		if _, has := m["description"]; has {
			t.Fatalf("list item carries description: %v", m)
		}
		for _, k := range []string{"script", "spec_hash", "status", "image_ids"} {
			if _, has := ver[k]; has {
				t.Fatalf("version exposes %q: %v", k, ver)
			}
		}
	}
	if strings.Join(slugs, ",") != "vllm,jupyterlab,webui" {
		t.Fatalf("slugs = %v (draft app must be hidden)", slugs)
	}
	v := items[0].(map[string]any)["version"].(map[string]any)
	if v["version"] != "1.0.0" || v["timeout_minutes"].(float64) != 60 || len(v["ports"].([]any)) != 1 || len(v["links"].([]any)) != 1 ||
		len(v["env"].([]any)) != 2 {
		t.Fatalf("vllm version = %v", v)
	}
	if strings.Contains(rawJSON(out), "SPARK_APP_DIR") {
		t.Fatal("catalog leaks the script")
	}
	code, out = anon.do("GET", "/apps/vllm", nil)
	if code != 200 || out["slug"] != "vllm" || out["description"] != "说明 vllm" || out["version"].(map[string]any)["id"] == nil {
		t.Fatalf("GET /apps/vllm = %d %v", code, out)
	}
	if strings.Contains(rawJSON(out), "SPARK_APP_DIR") {
		t.Fatal("catalog item leaks the script")
	}
	for _, slug := range []string{"nope", "drafty"} {
		if code, out := anon.do("GET", "/apps/"+slug, nil); code != 404 || out["code"] != "app_not_found" {
			t.Fatalf("GET /apps/%s = %d %v", slug, code, out)
		}
	}
}

func TestInstanceAppsFlow(t *testing.T) {
	e := newAppsTestEnv(t, nil)
	apps := e.seedApps()
	c := e.client()
	id := c.runningInstance(t, e, "13800000101")

	code, dep := c.do("POST", "/instances/"+id+"/apps", map[string]any{"app_id": apps.vllm.ID, "env": map[string]string{"API_KEY": "abcdefgh"}})
	if code != 201 {
		t.Fatalf("create = %d %v", code, dep)
	}
	depID := dep["id"].(string)
	if dep["state"] != "queued" || dep["desired"] != "running" || dep["instance_id"] != id || dep["app_slug"] != "vllm" || dep["app_name"] != "vllm 应用" ||
		dep["app_icon"] != "llm" || dep["version"] != "1.0.0" || dep["port"].(float64) != 8000 || dep["url"] != nil ||
		dep["update_available"] != false || dep["latest_version"] != "1.0.0" || dep["latest_version_id"].(float64) != float64(apps.vllmVer.ID) {
		t.Fatalf("created deployment = %v", dep)
	}
	if se := dep["secret_env"].(map[string]any); len(se) != 1 || se["API_KEY"] != true {
		t.Fatalf("secret_env = %v", dep["secret_env"])
	}
	env := dep["env"].(map[string]any)
	if _, has := env["API_KEY"]; has || env["MODEL_ID"] != "Qwen/Qwen3-8B" {
		t.Fatalf("env = %v", env)
	}
	if links, ok := dep["links"].([]any); !ok || len(links) != 0 {
		t.Fatalf("links without url = %v (want [])", dep["links"])
	}
	raw := rawJSON(dep)
	for _, leak := range []string{"abcdefgh", "secret_env_enc", "SecretEnvEnc", "log_tail", "user_id", "row_version"} {
		if strings.Contains(raw, leak) {
			t.Fatalf("deployment view leaks %q: %s", leak, raw)
		}
	}

	code, out := c.do("GET", "/instances/"+id+"/apps", nil)
	if code != 200 || len(out["items"].([]any)) != 1 || out["items"].([]any)[0].(map[string]any)["id"] != depID {
		t.Fatalf("list = %d %v", code, out)
	}
	code, inst := c.do("GET", "/instances/"+id, nil)
	sum := inst["apps_summary"].([]any)
	if code != 200 || len(sum) != 1 {
		t.Fatalf("apps_summary = %d %v", code, inst["apps_summary"])
	}
	if s := sum[0].(map[string]any); len(s) != 5 || s["id"] != depID || s["name"] != "vllm 应用" || s["state"] != "queued" || s["progress"].(float64) != 0 || s["url"] != nil {
		t.Fatalf("summary = %v", s)
	}
	// 实例列表也带摘要；没有部署的实例是空数组
	c.do("GET", "/instances", nil)

	// Runner 登记端口后 url / links 出现
	e.exec(`INSERT INTO instance_ports (instance_id, port, note, deployment_id, created_at, updated_at) VALUES ($1, 8000, 'vllm', $2, now(), now())`, id, depID)
	wantURL := e.ingress.PortURL(id, 8000)
	code, one := c.do("GET", "/instances/"+id+"/apps/"+depID, nil)
	if code != 200 || one["url"] != wantURL || !strings.HasPrefix(wantURL, "http") || !strings.Contains(wantURL, id+"-8000.spark.test") {
		t.Fatalf("get with port = %d %v (want url %s)", code, one, wantURL)
	}
	links := one["links"].([]any)
	if len(links) != 1 || links[0].(map[string]any)["label"] != "文档" || links[0].(map[string]any)["url"] != strings.TrimSuffix(wantURL, "/")+"/docs" {
		t.Fatalf("links = %v", links)
	}
	if _, inst := c.do("GET", "/instances/"+id, nil); inst["apps_summary"].([]any)[0].(map[string]any)["url"] != wantURL {
		t.Fatalf("summary url = %v", inst["apps_summary"])
	}
	_, list := c.do("GET", "/instances", nil)
	if s := list["items"].([]any)[0].(map[string]any)["apps_summary"].([]any); len(s) != 1 || s[0].(map[string]any)["url"] != wantURL {
		t.Fatalf("instances list summary = %v", s)
	}

	// 新版本发布后 update_available
	e.clk.Advance(time.Minute)
	v2, err := e.appCat.CreateVersion(context.Background(), audit.Actor{AdminID: 1}, apps.vllm.ID,
		appdeploy.VersionInput{Version: "1.1.0", ImageIDs: apps.vllmVer.ImageIDs, Spec: vllmSpec(), Script: appScript})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.appCat.PublishVersion(context.Background(), audit.Actor{AdminID: 1}, apps.vllm.ID, v2.ID); err != nil {
		t.Fatal(err)
	}
	if _, one := c.do("GET", "/instances/"+id+"/apps/"+depID, nil); one["update_available"] != true || one["latest_version"] != "1.1.0" ||
		one["latest_version_id"].(float64) != float64(v2.ID) || one["version"] != "1.0.0" {
		t.Fatalf("after new version = %v", one)
	}

	// 别的用户：与不存在同样 404
	other := e.client()
	otherID := other.runningInstance(t, e, "13800000102")
	for _, p := range []string{"/instances/" + id + "/apps", "/instances/" + id + "/apps/" + depID, "/instances/" + otherID + "/apps/" + depID,
		"/instances/" + id + "/apps/" + depID + "/log", "/instances/" + id + "/apps/" + depID + "/events"} {
		if code, out := other.do("GET", p, nil); code != 404 || out["code"] != "not_found" {
			t.Fatalf("stranger GET %s = %d %v", p, code, out)
		}
	}
	if code, out := other.do("POST", "/instances/"+id+"/apps", map[string]any{"app_id": apps.jupyter.ID, "env": map[string]string{"JUPYTER_TOKEN": "abcdefgh12"}}); code != 404 {
		t.Fatalf("stranger create = %d %v", code, out)
	}
	if _, out := other.do("GET", "/instances/"+otherID, nil); len(out["apps_summary"].([]any)) != 0 {
		t.Fatalf("empty summary = %v", out["apps_summary"])
	}
	// 错误码透传
	if code, out := c.do("POST", "/instances/"+id+"/apps", map[string]any{"app_id": apps.vllm.ID}); code != 409 || out["code"] != "app_exists" {
		t.Fatalf("duplicate = %d %v", code, out)
	}
	if code, out := c.do("POST", "/instances/"+id+"/apps", map[string]any{"app_id": apps.other.ID}); code != 409 || out["code"] != "app_image_mismatch" {
		t.Fatalf("mismatch = %d %v", code, out)
	}
	if code, out := c.do("POST", "/instances/"+id+"/apps", map[string]any{"app_id": apps.jupyter.ID}); code != 400 || out["code"] != "app_env_required" {
		t.Fatalf("env required = %d %v", code, out)
	}
	if code, out := c.do("POST", "/instances/"+id+"/apps", map[string]any{"app_id": apps.jupyter.ID, "bogus": 1}); code != 400 || out["code"] != "bad_request" {
		t.Fatalf("unknown field = %d %v", code, out)
	}
}

func TestInstanceAppsStateOps(t *testing.T) {
	e := newAppsTestEnv(t, nil)
	apps := e.seedApps()
	c := e.client()
	id := c.runningInstance(t, e, "13800000111")
	_, dep := c.do("POST", "/instances/"+id+"/apps", map[string]any{"app_id": apps.vllm.ID, "env": map[string]string{"API_KEY": "abcdefgh"}})
	depID := dep["id"].(string)
	base := "/instances/" + id + "/apps/" + depID
	setState := func(state string) { e.exec(`UPDATE app_deployments SET state = $2 WHERE id = $1`, depID, state) }

	setState("running")
	if code, out := c.do("POST", base+"/stop", map[string]any{}); code != 200 || out["desired"] != "stopped" || out["id"] != depID {
		t.Fatalf("stop = %d %v", code, out)
	}
	setState("stopped")
	if code, out := c.do("POST", base+"/start", map[string]any{}); code != 200 || out["state"] != "queued" || out["desired"] != "running" {
		t.Fatalf("start = %d %v", code, out)
	}
	setState("failed")
	code, out := c.do("POST", base+"/redeploy", map[string]any{"env": map[string]string{"MODEL_ID": "Qwen/Qwen3-4B"}})
	if code != 200 || out["state"] != "queued" || out["env"].(map[string]any)["MODEL_ID"] != "Qwen/Qwen3-4B" || out["secret_env"].(map[string]any)["API_KEY"] != true {
		t.Fatalf("redeploy with env = %d %v", code, out)
	}
	// 不带 env 字段 → 变量全部沿用（带 "env": {} 会把 MODEL_ID 重置为默认值）
	setState("failed")
	code, out = c.do("POST", base+"/redeploy", map[string]any{})
	if code != 200 || out["state"] != "queued" || out["env"].(map[string]any)["MODEL_ID"] != "Qwen/Qwen3-4B" || out["secret_env"].(map[string]any)["API_KEY"] != true {
		t.Fatalf("redeploy without env = %d %v", code, out)
	}
	setState("failed")
	code, out = c.do("POST", base+"/redeploy", map[string]any{"env": nil})
	if code != 200 || out["env"].(map[string]any)["MODEL_ID"] != "Qwen/Qwen3-4B" {
		t.Fatalf("redeploy env null = %d %v", code, out)
	}
	setState("failed")
	code, out = c.do("POST", base+"/redeploy", map[string]any{"env": map[string]string{}})
	if code != 200 || out["env"].(map[string]any)["MODEL_ID"] != "Qwen/Qwen3-8B" || out["secret_env"].(map[string]any)["API_KEY"] != true {
		t.Fatalf("redeploy env {} = %d %v", code, out)
	}
	if code, out := c.do("POST", base+"/redeploy", map[string]any{"env": "x"}); code != 400 || out["code"] != "bad_request" {
		t.Fatalf("redeploy bad env = %d %v", code, out)
	}
	setState("installing")
	if code, out := c.do("POST", base+"/redeploy", map[string]any{}); code != 409 || out["code"] != "app_invalid_state" {
		t.Fatalf("redeploy installing = %d %v", code, out)
	}
	if code, out := c.do("POST", base+"/start", map[string]any{}); code != 409 || out["code"] != "app_invalid_state" {
		t.Fatalf("start installing = %d %v", code, out)
	}

	// 别人的实例：写操作一律 404，部署不变
	other := e.client()
	other.runningInstance(t, e, "13800000112")
	for _, p := range []struct{ method, path string }{{"POST", base + "/stop"}, {"POST", base + "/start"}, {"POST", base + "/redeploy"}, {"DELETE", base}} {
		if code, out := other.do(p.method, p.path, map[string]any{}); code != 404 || out["code"] != "not_found" {
			t.Fatalf("stranger %s %s = %d %v", p.method, p.path, code, out)
		}
	}
	if code, out := c.do("GET", base, nil); code != 200 || out["state"] != "installing" || out["desired"] != "running" {
		t.Fatalf("after stranger ops = %d %v", code, out)
	}
	if code, out := c.do("DELETE", base, nil); code != 200 || out["state"] != "removing" || out["desired"] != "removed" {
		t.Fatalf("remove = %d %v", code, out)
	}
	if code, out := c.do("GET", "/instances/"+id+"/apps/dep-nope", nil); code != 404 {
		t.Fatalf("unknown deployment = %d %v", code, out)
	}
}

func TestInstanceAppsLogAndEvents(t *testing.T) {
	e := newAppsTestEnv(t, nil)
	apps := e.seedApps()
	c := e.client()
	id := c.runningInstance(t, e, "13800000121")
	_, dep := c.do("POST", "/instances/"+id+"/apps", map[string]any{"app_id": apps.vllm.ID})
	depID := dep["id"].(string)
	base := "/instances/" + id + "/apps/" + depID
	e.exec(`UPDATE app_deployments SET log_tail = 'downloading model' WHERE id = $1`, depID)
	code, out := c.do("GET", base+"/log", nil)
	if code != 200 || out["log"] != "downloading model" || out["live"] != false || out["at"] == nil {
		t.Fatalf("log = %d %v", code, out)
	}
	for i := 0; i < 60; i++ {
		e.exec(`INSERT INTO app_deploy_events (deployment_id, at, from_state, to_state, detail) VALUES ($1, now(), 'queued', 'queued', $2)`, depID, fmt.Sprintf("e%02d", i))
	}
	code, out = c.do("GET", base+"/events", nil)
	items := out["items"].([]any)
	if code != 200 || len(items) != 50 {
		t.Fatalf("events = %d (%d items)", code, len(items))
	}
	prev := 0.0
	for _, it := range items {
		m := it.(map[string]any)
		if m["id"].(float64) <= prev {
			t.Fatalf("events not ascending: %v", items)
		}
		prev = m["id"].(float64)
	}
	if items[0].(map[string]any)["detail"] != "e10" || items[49].(map[string]any)["detail"] != "e59" {
		t.Fatalf("events window = %v … %v", items[0], items[49])
	}
}

// 密文解不开（密钥轮换 / 数据损坏）时列表不能 500：secret_env 按版本声明的 secret 显示为已设置，只记 Warn（带部署 ID、不带值）；
// 摘要（实例列表 / 详情）根本不解密。
func TestInstanceAppsUnreadableSecrets(t *testing.T) {
	e := newAppsTestEnv(t, nil)
	apps := e.seedApps()
	c := e.client()
	id := c.runningInstance(t, e, "13800000151")
	_, vdep := c.do("POST", "/instances/"+id+"/apps", map[string]any{"app_id": apps.vllm.ID}) // API_KEY 未设置
	_, jdep := c.do("POST", "/instances/"+id+"/apps", map[string]any{"app_id": apps.jupyter.ID, "env": map[string]string{"JUPYTER_TOKEN": "abcdefgh12"}})
	if vdep["secret_env"] == nil || len(vdep["secret_env"].(map[string]any)) != 0 {
		t.Fatalf("unset secret = %v", vdep["secret_env"])
	}
	// 没有 links 的 spec：links 省略（AppSpec.links?），不会是 null
	if spec := jdep["spec"].(map[string]any); len(spec["ports"].([]any)) != 1 {
		t.Fatalf("spec = %v", spec)
	} else if v, has := spec["links"]; has && v == nil {
		t.Fatalf("spec.links = null: %v", spec)
	}
	e.exec(`UPDATE app_deployments SET secret_env_enc = $2 WHERE instance_id = $1`, id, []byte("not a secretbox ciphertext"))

	for _, p := range []string{"/instances", "/instances/" + id} {
		code, out := c.do("GET", p, nil)
		if code != 200 {
			t.Fatalf("GET %s with unreadable secrets = %d %v", p, code, out)
		}
	}
	if strings.Contains(e.appLog.String(), "unreadable") {
		t.Fatalf("summaries decrypted secrets: %s", e.appLog.String())
	}
	code, out := c.do("GET", "/instances/"+id+"/apps", nil)
	items := out["items"].([]any)
	if code != 200 || len(items) != 2 {
		t.Fatalf("list with unreadable secrets = %d %v", code, out)
	}
	byID := map[string]map[string]any{}
	for _, it := range items {
		byID[it.(map[string]any)["id"].(string)] = it.(map[string]any)
	}
	if se := byID[vdep["id"].(string)]["secret_env"].(map[string]any); len(se) != 1 || se["API_KEY"] != true {
		t.Fatalf("vllm fallback secret_env = %v", se)
	}
	if se := byID[jdep["id"].(string)]["secret_env"].(map[string]any); len(se) != 1 || se["JUPYTER_TOKEN"] != true {
		t.Fatalf("jupyter fallback secret_env = %v", se)
	}
	logs := e.appLog.String()
	if !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, vdep["id"].(string)) || !strings.Contains(logs, jdep["id"].(string)) ||
		strings.Contains(logs, "abcdefgh12") {
		t.Fatalf("warn log = %s", logs)
	}
	if code, out := c.do("GET", "/instances/"+id+"/apps/"+vdep["id"].(string), nil); code != 200 || out["state"] != "queued" {
		t.Fatalf("get with unreadable secrets = %d %v", code, out)
	}
}

func TestOrderWithApp(t *testing.T) {
	e := newAppsTestEnv(t, nil)
	apps := e.seedApps()
	dbtest.CreateMachine(t, e.pool, "SN001")
	dbtest.CreateMachine(t, e.pool, "SN002")
	c, uid := e.fundedClient("13800000131")
	ctx := context.Background()
	img := dbtest.ImageID(t, e.pool)
	body := map[string]any{"product_id": dbtest.ProductID(t, e.pool), "duration": "1d", "image_id": img,
		"app_id": apps.vllm.ID, "app_env": map[string]string{"API_KEY": "abcdefgh"}}
	code, out := c.do("POST", "/orders", body)
	if code != 201 {
		t.Fatalf("order with app = %d %v", code, out)
	}
	inst := out["instance"].(map[string]any)
	sum := inst["apps_summary"].([]any)
	if len(sum) != 1 || sum[0].(map[string]any)["state"] != "queued" || sum[0].(map[string]any)["name"] != "vllm 应用" {
		t.Fatalf("order instance apps_summary = %v", inst["apps_summary"])
	}
	instances := e.countRows(`SELECT count(*) FROM instances`)
	balance, err := e.wallet.Balance(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	// 校验失败：整单不下、不扣款
	bad := []struct {
		patch  map[string]any
		status int
		code   string
	}{
		{map[string]any{"app_id": apps.other.ID}, 409, "app_image_mismatch"},
		{map[string]any{"app_id": apps.jupyter.ID, "app_env": nil}, 400, "app_env_required"},
		{map[string]any{"app_id": apps.vllm.ID, "app_env": map[string]string{"NOPE": "x"}}, 400, "app_env_invalid"},
		{map[string]any{"app_id": 999999}, 404, "app_not_found"},
	}
	for _, b := range bad {
		req := map[string]any{}
		for k, v := range body {
			req[k] = v
		}
		for k, v := range b.patch {
			req[k] = v
		}
		if code, out := c.do("POST", "/orders", req); code != b.status || out["code"] != b.code {
			t.Fatalf("order %v = %d %v", b.patch, code, out)
		}
		booking := map[string]any{"product_id": req["product_id"], "image_id": img, "count": 1, "duration": "1d", "quantity": 1,
			"start_at": dbtest.T0.Add(3 * time.Hour).Format(time.RFC3339), "app_id": req["app_id"], "app_env": req["app_env"]}
		if code, out := c.do("POST", "/bookings", booking); code != b.status || out["code"] != b.code {
			t.Fatalf("booking %v = %d %v", b.patch, code, out)
		}
	}
	if n := e.countRows(`SELECT count(*) FROM instances`); n != instances {
		t.Fatalf("instances %d → %d after rejected orders", instances, n)
	}
	if after, _ := e.wallet.Balance(ctx, uid); after != balance {
		t.Fatalf("balance %d → %d after rejected orders", balance, after)
	}
	// 预订带应用：每台预留实例一条 queued 部署
	booking := map[string]any{"product_id": body["product_id"], "image_id": img, "count": 1, "duration": "1d", "quantity": 1,
		"start_at": dbtest.T0.Add(3 * time.Hour).Format(time.RFC3339), "app_id": apps.jupyter.ID,
		"app_env": map[string]string{"JUPYTER_TOKEN": "abcdefgh12"}}
	code, out = c.do("POST", "/bookings", booking)
	if code != 201 {
		t.Fatalf("booking with app = %d %v", code, out)
	}
	bi := out["instances"].([]any)[0].(map[string]any)
	if bi["state"] != "reserved" || len(bi["apps_summary"].([]any)) != 1 || bi["apps_summary"].([]any)[0].(map[string]any)["state"] != "queued" {
		t.Fatalf("booking instance = %v", bi)
	}
	if n := e.countRows(`SELECT count(*) FROM app_deployments WHERE instance_id = $1 AND app_id = $2`, bi["id"], apps.jupyter.ID); n != 1 {
		t.Fatalf("booking deployments = %d", n)
	}
}

func TestAppsWriteRateLimit(t *testing.T) {
	e := newAppsTestEnv(t, func(s *api.Server) { s.AppsLimit = 2 })
	apps := e.seedApps()
	c := e.client()
	id := c.runningInstance(t, e, "13800000141")
	code, dep := c.do("POST", "/instances/"+id+"/apps", map[string]any{"app_id": apps.vllm.ID})
	if code != 201 {
		t.Fatalf("create = %d %v", code, dep)
	}
	base := "/instances/" + id + "/apps/" + dep["id"].(string)
	if code, out := c.do("POST", base+"/redeploy", map[string]any{}); code != 200 { // queued + 无 env：返回当前行，仍计数
		t.Fatalf("2nd write = %d %v", code, out)
	}
	if code, out := c.do("DELETE", base, nil); code != http.StatusTooManyRequests || out["code"] != "rate_limited" {
		t.Fatalf("3rd write = %d %v", code, out)
	}
	for _, p := range []string{"/instances/" + id + "/apps", base, base + "/log", base + "/events"} {
		if code, out := c.do("GET", p, nil); code != 200 {
			t.Fatalf("read %s after limit = %d %v", p, code, out)
		}
	}
	// 别的用户不受影响；窗口一小时
	other := e.client()
	oid := other.runningInstance(t, e, "13800000142")
	if code, out := other.do("POST", "/instances/"+oid+"/apps", map[string]any{"app_id": apps.vllm.ID}); code != 201 {
		t.Fatalf("other user create = %d %v", code, out)
	}
	e.clk.Advance(time.Hour)
	if code, out := c.do("DELETE", base, nil); code != 200 || out["state"] != "removing" {
		t.Fatalf("write after an hour = %d %v", code, out)
	}
}
