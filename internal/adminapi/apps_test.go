package adminapi_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/imageref"
	"spark-cloud/internal/rental"
)

const appScript = "#!/bin/sh\ncase \"$1\" in install) ;; start) sleep 1 & echo $! > \"$SPARK_APP_DIR/service.pid\" ;; esac\n"

// appSpecJSON 是一个合法 spec（端口可选，便于同一实例上部署多个应用）；含 secret 变量 API_KEY。
func appSpecJSON(port int) map[string]any {
	return map[string]any{
		"ports": []any{map[string]any{"port": port, "label": "API", "health_path": "/health"}},
		"env": []any{
			map[string]any{"name": "MODEL_ID", "label": "模型", "type": "enum", "options": []string{"a", "b"}, "default": "a"},
			map[string]any{"name": "API_KEY", "label": "接口密钥", "type": "secret"},
		},
	}
}

func (e *env) appSpec(port int) appdeploy.Spec {
	e.t.Helper()
	b, _ := json.Marshal(appSpecJSON(port))
	var s appdeploy.Spec
	if err := json.Unmarshal(b, &s); err != nil {
		e.t.Fatal(err)
	}
	return s
}

// publishedApp 直接经 Catalog 建一个已上架应用及其已发布版本（部署相关用例的前置数据）。
func (e *env) publishedApp(slug string, port int) (appdeploy.App, appdeploy.Version) {
	e.t.Helper()
	actor := audit.Actor{AdminID: 1}
	a, err := e.appCat.CreateApp(e.ctx, actor, appdeploy.AppInput{Slug: slug, Name: slug, Category: "llm", Status: appdeploy.StatusPublished})
	if err != nil {
		e.t.Fatal(err)
	}
	v, err := e.appCat.CreateVersion(e.ctx, actor, a.ID, appdeploy.VersionInput{Version: "1.0", ImageIDs: []int64{dbtest.ImageID(e.t, e.pool)},
		Spec: e.appSpec(port), Script: appScript})
	if err != nil {
		e.t.Fatal(err)
	}
	if v, _, err = e.appCat.PublishVersion(e.ctx, actor, a.ID, v.ID); err != nil {
		e.t.Fatal(err)
	}
	return a, v
}

func (e *env) deploy(uid int64, inst rental.Instance, appID int64, env map[string]string) appdeploy.Deployment {
	e.t.Helper()
	e.clk.Advance(time.Second) // 部署按 created_at, id 排序：错开创建时间让顺序确定
	d, err := e.apps.Create(e.ctx, appdeploy.CreateRequest{UserID: uid, InstanceID: inst.ID, AppID: appID, Env: env})
	if err != nil {
		e.t.Fatalf("deploy: %v", err)
	}
	return d
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatalf("exec %q: %v", sql, err)
	}
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

func appBody(slug string) map[string]any {
	return map[string]any{"slug": slug, "name": "vLLM", "summary": "推理", "description": "说明", "category": "llm", "icon": "llm", "sort": 1}
}

func TestAdminAppCRUD(t *testing.T) {
	e := newEnv(t)
	root := e.loginAs("root", admin.RoleSuper)
	ops := e.loginAs("ops", admin.RoleOps)
	fin := e.loginAs("fin", admin.RoleFinance)

	if code, out := ops.do("POST", "/apps", appBody("vllm")); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("ops create = %d %v", code, out)
	}
	code, out := root.do("POST", "/apps", appBody("vllm"))
	if code != 201 || out["slug"] != "vllm" || out["status"] != "draft" || out["icon"] != "llm" {
		t.Fatalf("create = %d %v", code, out)
	}
	id := int64(out["id"].(float64))
	if code, out := root.do("POST", "/apps", appBody("vllm")); code != 409 {
		t.Fatalf("duplicate slug = %d %v", code, out)
	}
	if code, out := root.do("POST", "/apps", map[string]any{"slug": "x", "name": "x", "category": "llm"}); code != 400 {
		t.Fatalf("bad slug = %d %v", code, out)
	}

	upd := appBody("ignored")
	upd["name"] = "vLLM 推理"
	upd["status"] = "published"
	code, out = root.do("PUT", fmt.Sprintf("/apps/%d", id), upd)
	if code != 200 || out["name"] != "vLLM 推理" || out["slug"] != "vllm" || out["status"] != "published" {
		t.Fatalf("update = %d %v", code, out)
	}

	code, out = fin.do("GET", "/apps", nil)
	items, _ := out["items"].([]any)
	if code != 200 || len(items) != 1 {
		t.Fatalf("finance list = %d %v", code, out)
	}
	row := items[0].(map[string]any)
	if row["versions"].(float64) != 0 || row["deployments_live"].(float64) != 0 || row["name"] != "vLLM 推理" {
		t.Fatalf("list row = %v", row)
	}

	// 只能删除没有版本的 draft 应用
	if code, out := root.do("DELETE", fmt.Sprintf("/apps/%d", id), nil); code != 409 {
		t.Fatalf("delete published = %d %v", code, out)
	}
	upd["status"] = "draft"
	if code, out := root.do("PUT", fmt.Sprintf("/apps/%d", id), upd); code != 200 {
		t.Fatalf("back to draft = %d %v", code, out)
	}
	if code, out := ops.do("DELETE", fmt.Sprintf("/apps/%d", id), nil); code != 403 {
		t.Fatalf("ops delete = %d %v", code, out)
	}
	if code, out := root.do("DELETE", fmt.Sprintf("/apps/%d", id), nil); code != 200 {
		t.Fatalf("delete = %d %v", code, out)
	}
	if code, _ := root.do("PUT", fmt.Sprintf("/apps/%d", id), upd); code != 404 {
		t.Fatalf("update deleted = %d", code)
	}
	for _, action := range []string{"app.create", "app.update", "app.delete"} {
		if n := e.count(`SELECT count(*) FROM audit_logs WHERE action = $1 AND actor_type = 'admin' AND target_id = $2`, action, itoa(id)); n == 0 {
			t.Errorf("audit %s missing", action)
		}
	}
}

func TestAdminVersionValidateAndPublish(t *testing.T) {
	e := newEnv(t)
	online := e.machine("SN1")
	root := e.loginAs("root", admin.RoleSuper)
	ops := e.loginAs("ops", admin.RoleOps)
	fin := e.loginAs("fin", admin.RoleFinance)
	imageID := dbtest.ImageID(t, e.pool)
	_, out := root.do("POST", "/apps", appBody("vllm"))
	appID := int64(out["id"].(float64))
	base := fmt.Sprintf("/apps/%d/versions", appID)

	bad := map[string]any{"version": "1.0", "image_ids": []int64{imageID}, "script": appScript,
		"spec": map[string]any{"ports": []any{map[string]any{"port": 22}}}}
	code, out := root.do("POST", base+"?validate=1", bad)
	if code != 400 || codeOf(out) != "app_version_invalid" || !strings.Contains(out["message"].(string), "ports[0].port") {
		t.Fatalf("validate bad = %d %v", code, out)
	}
	if code, out := root.do("POST", base, bad); code != 400 || codeOf(out) != "app_version_invalid" {
		t.Fatalf("create bad = %d %v", code, out)
	}
	if n := e.count(`SELECT count(*) FROM app_versions`); n != 0 {
		t.Fatalf("invalid version written: %d rows", n)
	}

	good := map[string]any{"version": "1.0", "image_ids": []int64{imageID}, "script": appScript, "spec": appSpecJSON(8000), "changelog": "首版"}
	code, out = root.do("POST", base+"?validate=1", good)
	if code != 200 || out["ok"] != true || len(out["spec_hash"].(string)) != 64 {
		t.Fatalf("validate good = %d %v", code, out)
	}
	hash := out["spec_hash"].(string)
	if n := e.count(`SELECT count(*) FROM app_versions`); n != 0 {
		t.Fatal("validate=1 wrote a version")
	}
	if code, out := ops.do("POST", base, good); code != 403 {
		t.Fatalf("ops create version = %d %v", code, out)
	}
	code, out = root.do("POST", base, good)
	if code != 201 || out["status"] != "draft" || out["spec_hash"] != hash || out["script"] != nil {
		t.Fatalf("create = %d %v", code, out)
	}
	vid := int64(out["id"].(float64))
	vpath := fmt.Sprintf("%s/%d", base, vid)

	// 草稿：PUT 只校验 / 真改
	edit := map[string]any{"version": "1.0", "image_ids": []int64{imageID}, "script": appScript + "# v2\n", "spec": appSpecJSON(8000), "changelog": "首版"}
	if code, out := root.do("PUT", vpath+"?validate=1", edit); code != 200 || out["ok"] != true || out["spec_hash"] == hash {
		t.Fatalf("validate put = %d %v", code, out)
	}
	if code, out := root.do("PUT", vpath, edit); code != 200 || out["spec_hash"] == hash {
		t.Fatalf("put draft = %d %v", code, out)
	}
	code, out = fin.do("GET", vpath+"/script", nil)
	if code != 200 || out["script"] != appScript+"# v2\n" {
		t.Fatalf("script = %d %v", code, out)
	}
	if code, _ := fin.do("GET", fmt.Sprintf("/apps/%d/versions/%d/script", appID+1, vid), nil); code != 404 {
		t.Fatalf("script under wrong app = %d", code)
	}

	// 草稿不能经 PUT 改成 published（发布要走动态码）
	if code, out := root.do("PUT", vpath, map[string]any{"status": "published"}); code != 409 || codeOf(out) != "invalid_state" {
		t.Fatalf("put draft→published = %d %v", code, out)
	}
	if code, out := root.do("PUT", vpath+"?validate=1", map[string]any{"status": "published"}); code != 409 || codeOf(out) != "invalid_state" {
		t.Fatalf("validate draft→published = %d %v", code, out)
	}

	// 发布：没有动态码 → totp_required；ops 无权
	if code, out := root.do("POST", vpath+"/publish", map[string]any{}); code != 403 || codeOf(out) != "totp_required" {
		t.Fatalf("publish without totp = %d %v", code, out)
	}
	if code, out := ops.do("POST", vpath+"/publish", map[string]any{"totp": "000000"}); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("ops publish = %d %v", code, out)
	}
	// 不存在的版本：动态码校验之前就拒绝，不消耗动态码
	if code, out := root.do("POST", base+"/999999/publish", map[string]any{"totp": "000000"}); code != 404 {
		t.Fatalf("publish missing = %d %v", code, out)
	}
	code, out = root.do("POST", vpath+"/publish", map[string]any{"totp": root.totp()})
	if code != 200 {
		t.Fatalf("publish = %d %v", code, out)
	}
	ver := out["version"].(map[string]any)
	if ver["status"] != "published" || ver["published_at"] == nil || out["prepull_machines"].(float64) != 1 {
		t.Fatalf("publish out = %v", out)
	}
	cmds, err := e.fleet.PendingCommands(e.ctx, online)
	if err != nil || len(cmds) != 1 || cmds[0].Type != fleet.CmdPullImage {
		t.Fatalf("pull_image = %+v %v", cmds, err)
	}
	var diff []byte
	if err := e.pool.QueryRow(e.ctx, `SELECT diff FROM audit_logs WHERE action = 'app.version.publish' AND actor_id = $1 AND target_id = $2`,
		root.admin.ID, itoa(vid)).Scan(&diff); err != nil || !strings.Contains(string(diff), ver["spec_hash"].(string)) {
		t.Fatalf("publish audit = %s %v", diff, err)
	}
	// 再发布 → 状态不对
	if code, out := root.do("POST", vpath+"/publish", map[string]any{}); code != 409 {
		t.Fatalf("republish = %d %v", code, out)
	}

	// 已发布：改 spec → 冻结；只改 changelog（省略的字段沿用库中值）可以；下线可以
	frozen := map[string]any{"version": "1.0", "image_ids": []int64{imageID}, "script": appScript + "# v2\n", "spec": appSpecJSON(8001)}
	if code, out := root.do("PUT", vpath, frozen); code != 409 || codeOf(out) != "app_version_frozen" {
		t.Fatalf("put published spec = %d %v", code, out)
	}
	// ?validate=1 与真正修改同一规则
	if code, out := root.do("PUT", vpath+"?validate=1", frozen); code != 409 || codeOf(out) != "app_version_frozen" {
		t.Fatalf("validate published spec = %d %v", code, out)
	}
	if code, out := root.do("PUT", vpath+"?validate=1", map[string]any{"changelog": "仅改说明"}); code != 200 || out["ok"] != true ||
		out["spec_hash"] != ver["spec_hash"] {
		t.Fatalf("validate published changelog = %d %v", code, out)
	}
	if code, out := root.do("PUT", vpath, map[string]any{"status": "draft"}); code != 409 || codeOf(out) != "app_version_frozen" {
		t.Fatalf("put published→draft = %d %v", code, out)
	}
	if code, out := root.do("PUT", vpath, map[string]any{"changelog": "修正说明"}); code != 200 || out["changelog"] != "修正说明" || out["status"] != "published" {
		t.Fatalf("put published changelog = %d %v", code, out)
	}
	// 回传整个版本（含当前 status）也算没改
	if code, out := root.do("PUT", vpath, map[string]any{"version": "1.0", "image_ids": []int64{imageID}, "script": appScript + "# v2\n",
		"spec": appSpecJSON(8000), "changelog": "修正说明", "status": "published"}); code != 200 {
		t.Fatalf("put published unchanged = %d %v", code, out)
	}

	// 版本列表带覆盖率
	var ref string
	if err := e.pool.QueryRow(e.ctx, `SELECT ref FROM images WHERE id = $1`, imageID).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE machines SET cached_images = $1, cached_images_at = $2 WHERE id = $3`, []string{imageref.Key(ref)}, e.clk.Now(), online)
	e.machine("SN2") // 在线但从未上报 → 未知
	code, out = fin.do("GET", base, nil)
	if code != 200 || len(out["items"].([]any)) != 1 {
		t.Fatalf("versions = %d %v", code, out)
	}
	item := out["items"].([]any)[0].(map[string]any)
	cov := item["coverage"].([]any)[0].(map[string]any)
	if item["id"].(float64) != float64(vid) || item["script"] != nil || cov["image_id"].(float64) != float64(imageID) || cov["image_name"] == "" ||
		cov["cached"].(float64) != 1 || cov["online"].(float64) != 2 || cov["unknown"].(float64) != 1 {
		t.Fatalf("version item = %v", item)
	}

	// 复制为新版本
	code, out = root.do("POST", base, map[string]any{"version": "1.1", "copy_from": vid})
	if code != 201 || out["status"] != "draft" {
		t.Fatalf("copy = %d %v", code, out)
	}
	if code, out := ops.do("POST", vpath+"/prepull", map[string]any{}); code != 200 || out["machines"].(float64) != 2 {
		t.Fatalf("prepull = %d %v", code, out)
	}
	if code, _ := fin.do("POST", vpath+"/prepull", map[string]any{}); code != 403 {
		t.Fatalf("finance prepull = %d", code)
	}

	// 未下线版本引用的镜像不能改 ref（catalog 已拒绝，接口原样透出）
	code, out = root.do("PUT", fmt.Sprintf("/images/%d", imageID), map[string]any{"name": "x", "ref": "nvcr.io/x/other:1", "enabled": true, "sort": 1})
	if code != 409 || !strings.Contains(out["message"].(string), "应用版本") {
		t.Fatalf("image ref change = %d %v", code, out)
	}

	if code, out := root.do("PUT", vpath, map[string]any{"status": "retired"}); code != 200 || out["status"] != "retired" {
		t.Fatalf("retire = %d %v", code, out)
	}
	if code, out := root.do("PUT", vpath, map[string]any{"changelog": "x"}); code != 409 || codeOf(out) != "app_version_frozen" {
		t.Fatalf("put retired = %d %v", code, out)
	}
	if code, out := root.do("PUT", vpath+"?validate=1", map[string]any{"changelog": "x"}); code != 409 || codeOf(out) != "app_version_frozen" {
		t.Fatalf("validate retired = %d %v", code, out)
	}
	// 已下线版本不算覆盖率；草稿照算（同一镜像只统计一次）
	_, out = fin.do("GET", base, nil)
	items := out["items"].([]any)
	draft, retired := items[0].(map[string]any), items[1].(map[string]any)
	if len(items) != 2 || draft["status"] != "draft" || retired["status"] != "retired" || len(retired["coverage"].([]any)) != 0 ||
		draft["coverage"].([]any)[0].(map[string]any)["cached"].(float64) != 1 {
		t.Fatalf("versions after retire = %v", items)
	}
	for _, action := range []string{"app.version.create", "app.version.update", "app.version.prepull"} {
		if n := e.count(`SELECT count(*) FROM audit_logs WHERE action = $1 AND actor_type = 'admin'`, action); n == 0 {
			t.Errorf("audit %s missing", action)
		}
	}
	// 读脚本不审计
	if n := e.count(`SELECT count(*) FROM audit_logs WHERE action LIKE '%script%'`); n != 0 {
		t.Fatalf("script read audited")
	}
}

func TestAdminDeploymentsListAndForceRemove(t *testing.T) {
	e := newEnv(t)
	e.machine("SN1")
	e.machine("SN2")
	uid := e.renter("13600000001")
	running, stopped := e.place(uid), e.place(uid)
	e.ack()
	app, ver := e.publishedApp("vllm", 8000)
	const secret = "sk-very-secret-value"
	d1 := e.deploy(uid, running, app.ID, map[string]string{"API_KEY": secret})
	d2 := e.deploy(uid, stopped, app.ID, nil)
	// 模拟 Runner 回写：running + 已脱敏的日志尾部
	e.exec(`UPDATE app_deployments SET state = 'running', attempt = 1, log_tail = 'serving with api-key ***' WHERE id = $1`, d1.ID)
	e.exec(`UPDATE instances SET state = 'stopped' WHERE id = $1`, stopped.ID)

	ops := e.loginAs("ops", admin.RoleOps)
	fin := e.loginAs("fin", admin.RoleFinance)

	code, out := fin.do("GET", "/deployments?state=running", nil)
	if code != 200 || out["total"].(float64) != 1 || out["items"].([]any)[0].(map[string]any)["id"] != d1.ID {
		t.Fatalf("list running = %d %v", code, out)
	}
	code, out = fin.do("GET", fmt.Sprintf("/deployments?app_id=%d&user_id=%d", app.ID, uid), nil)
	if code != 200 || out["total"].(float64) != 2 {
		t.Fatalf("list by app/user = %d %v", code, out)
	}
	code, out = fin.do("GET", "/deployments?instance_id="+stopped.ID, nil)
	row := out["items"].([]any)[0].(map[string]any)
	if code != 200 || out["total"].(float64) != 1 || row["id"] != d2.ID || row["user_id"].(float64) != float64(uid) || row["app_slug"] != "vllm" ||
		row["version"] != ver.Version {
		t.Fatalf("list by instance = %d %v", code, out)
	}
	if code, _ := fin.do("GET", "/deployments?state=bogus", nil); code != 400 {
		t.Fatalf("bad state = %d", code)
	}
	if code, _ := fin.do("GET", "/deployments?app_id=x", nil); code != 400 {
		t.Fatalf("bad app_id = %d", code)
	}

	resp, out := fin.send("GET", "/deployments/"+d1.ID, nil)
	raw, _ := json.Marshal(out)
	if resp.StatusCode != 200 || strings.Contains(string(raw), secret) || strings.Contains(string(raw), "secret_env_enc") {
		t.Fatalf("detail leaks secret: %d %s", resp.StatusCode, raw)
	}
	dep := out["deployment"].(map[string]any)
	if dep["id"] != d1.ID || dep["secret_env"].(map[string]any)["API_KEY"] != true || dep["env"].(map[string]any)["API_KEY"] != nil ||
		dep["log_tail"] != "serving with api-key ***" || out["log_tail"] != nil || len(out["events"].([]any)) != 1 {
		t.Fatalf("detail = %v", out)
	}
	if code, _ := fin.do("GET", "/deployments/dep-missing", nil); code != 404 {
		t.Fatalf("missing = %d", code)
	}

	if code, out := fin.do("POST", "/deployments/"+d1.ID+"/force-remove", map[string]any{"reason": "滥用"}); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("finance force-remove = %d %v", code, out)
	}
	if code, out := ops.do("POST", "/deployments/"+d1.ID+"/force-remove", map[string]any{"reason": "  "}); code != 400 {
		t.Fatalf("force-remove without reason = %d %v", code, out)
	}
	if code, out := ops.do("POST", "/deployments/"+d1.ID+"/force-remove", map[string]any{"reason": strings.Repeat("长", 201)}); code != 400 {
		t.Fatalf("force-remove reason too long = %d %v", code, out)
	}
	code, out = ops.do("POST", "/deployments/"+d1.ID+"/force-remove", map[string]any{"reason": "挖矿"})
	if code != 200 || out["state"] != "removing" || out["desired"] != "removed" {
		t.Fatalf("force-remove running = %d %v", code, out)
	}
	code, out = ops.do("POST", "/deployments/"+d2.ID+"/force-remove", map[string]any{"reason": "挖矿"})
	if code != 200 || out["state"] != "removed" {
		t.Fatalf("force-remove stopped = %d %v", code, out)
	}
	for _, id := range []string{d1.ID, d2.ID} {
		if n := e.count(`SELECT count(*) FROM app_deploy_events WHERE deployment_id = $1 AND detail = '平台卸载：挖矿'`, id); n != 1 {
			t.Errorf("%s force-remove event = %d", id, n)
		}
		if n := e.count(`SELECT count(*) FROM audit_logs WHERE action = 'app.deployment.force_remove' AND actor_type = 'admin' AND actor_id = $1
			AND target_id = $2 AND diff->>'reason' = '挖矿'`, ops.admin.ID, id); n != 1 {
			t.Errorf("%s force-remove audit = %d", id, n)
		}
	}
	if code, out := ops.do("POST", "/deployments/dep-missing/force-remove", map[string]any{"reason": "x"}); code != 404 {
		t.Fatalf("force-remove missing = %d %v", code, out)
	}
}

func TestAdminInstanceAndDashboardIncludeApps(t *testing.T) {
	e := newEnv(t)
	mid := e.machine("SN1")
	e.machine("SN2")
	uid := e.renter("13600000001")
	inst1, inst2 := e.place(uid), e.place(uid)
	e.ack()
	a1, _ := e.publishedApp("app-a", 8000)
	a2, _ := e.publishedApp("app-b", 8001)
	a3, _ := e.publishedApp("app-c", 8002)
	installing := e.deploy(uid, inst1, a1.ID, map[string]string{"API_KEY": "sk-very-secret-value"})
	runningDep := e.deploy(uid, inst1, a2.ID, nil)
	failedNew := e.deploy(uid, inst1, a3.ID, nil)
	failedOld := e.deploy(uid, inst2, a1.ID, nil)
	now := e.clk.Now()
	e.exec(`UPDATE app_deployments SET state = 'installing', log_tail = 'downloading' WHERE id = $1`, installing.ID)
	e.exec(`UPDATE app_deployments SET state = 'running' WHERE id = $1`, runningDep.ID)
	e.exec(`UPDATE app_deployments SET state = 'failed', finished_at = $2 WHERE id = $1`, failedNew.ID, now.Add(-time.Hour))
	e.exec(`UPDATE app_deployments SET state = 'failed', finished_at = $2 WHERE id = $1`, failedOld.ID, now.Add(-25*time.Hour))
	e.exec(`UPDATE machines SET cached_images = ARRAY['spark/x:1'], cached_images_at = $2 WHERE id = $1`, mid, now)

	fin := e.loginAs("fin", admin.RoleFinance)
	code, out := fin.do("GET", "/instances/"+inst1.ID, nil)
	if code != 200 {
		t.Fatalf("instance = %d %v", code, out)
	}
	apps := out["apps"].([]any)
	if len(apps) != 3 {
		t.Fatalf("instance apps = %v", apps)
	}
	first := apps[0].(map[string]any)
	if first["id"] != installing.ID || first["state"] != "installing" || first["log_tail"] != "downloading" ||
		first["secret_env"].(map[string]any)["API_KEY"] != true || first["user_id"].(float64) != float64(uid) {
		t.Fatalf("instance app = %v", first)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "sk-very-secret-value") {
		t.Fatal("instance detail leaks secret value")
	}
	if code, out := fin.do("GET", "/instances/"+inst2.ID, nil); code != 200 || len(out["apps"].([]any)) != 1 {
		t.Fatalf("instance2 apps = %d %v", code, out)
	}

	code, out = fin.do("GET", "/dashboard", nil)
	if code != 200 || out["apps_installing"].(float64) != 1 || out["apps_running"].(float64) != 1 || out["apps_failed_24h"].(float64) != 1 {
		t.Fatalf("dashboard apps = %d %v", code, out)
	}
	e.exec(`UPDATE app_deployments SET state = 'queued' WHERE id = $1`, runningDep.ID) // queued 也算「部署中」
	if _, out := fin.do("GET", "/dashboard", nil); out["apps_installing"].(float64) != 2 || out["apps_running"].(float64) != 0 {
		t.Fatalf("dashboard queued = %v", out)
	}

	ops := e.loginAs("ops", admin.RoleOps)
	code, out = ops.do("GET", fmt.Sprintf("/machines/%d", mid), nil)
	m := out["machine"].(map[string]any)
	if code != 200 || len(m["cached_images"].([]any)) != 1 || m["cached_images"].([]any)[0] != "spark/x:1" || m["cached_images_at"] == nil {
		t.Fatalf("machine cached images = %d %v", code, m)
	}
}
