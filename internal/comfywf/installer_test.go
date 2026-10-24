package comfywf

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/secretbox"
)

// instEnv 是安装服务测试的环境：目录（catEnv）+ appdeploy（钩子已接）+ Installer；comfyui 应用带 1.0.0（不支持工作流）与 1.1.0（支持）。
type instEnv struct {
	*catEnv
	appsCat  *appdeploy.Catalog
	apps     *appdeploy.Service
	hooks    *Hooks
	in       *Installer
	uid      int64
	imageID  int64
	comfy    int64 // apps.id
	comfyOld int64 // 1.0.0：supports_workflows = false
	comfyNew int64 // 1.1.0：supports_workflows = true
	vllm     int64
	vllmVer  int64
	n        int
}

func newInstEnv(t *testing.T) *instEnv {
	t.Helper()
	ce := newCatEnv(t)
	box, err := secretbox.New(dbtest.Key)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	f := fleet.NewService(ce.pool, ce.clk)
	imgs := catalog.NewService(ce.pool).WithClock(ce.clk)
	e := &instEnv{catEnv: ce, appsCat: appdeploy.NewCatalog(ce.pool, ce.clk, f, imgs, log),
		uid: dbtest.CreateUser(t, ce.pool, "13800000009", true), imageID: dbtest.ImageID(t, ce.pool)}
	e.hooks = NewHooks(ce.pool, ce.cat)
	deps := appdeploy.Deps{Pool: ce.pool, Clock: ce.clk, Box: box, Log: log, Catalog: e.appsCat, Images: imgs, Fleet: f}
	e.hooks.Wire(&deps)
	e.apps = appdeploy.NewService(deps)
	e.in = NewInstaller(ce.pool, ce.clk, ce.cat, e.apps, e.appsCat, log)

	e.comfy = e.app("comfyui")
	e.comfyOld = e.appVersion(e.comfy, "1.0.0", false, StatusPublished, dbtest.T0)
	e.comfyNew = e.appVersion(e.comfy, "1.1.0", true, StatusPublished, dbtest.T0.Add(time.Hour))
	e.vllm = e.app("vllm")
	e.vllmVer = e.appVersion(e.vllm, "1.0.0", false, StatusPublished, dbtest.T0)
	e.clk.Advance(2 * time.Hour)
	return e
}

func (e *instEnv) app(slug string) int64 {
	e.t.Helper()
	var id int64
	if err := e.pool.QueryRow(e.ctx, `INSERT INTO apps (slug, name, category, status, created_at, updated_at)
		VALUES ($1, $1, 'image', 'published', $2, $2) RETURNING id`, slug, dbtest.T0).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// appVersion 直接插应用版本（端口 8188；supports 决定 spec.supports_workflows）。
func (e *instEnv) appVersion(appID int64, ver string, supports bool, status string, publishedAt time.Time) int64 {
	e.t.Helper()
	spec := `{"ports":[{"port":8188}]}`
	if supports {
		spec = `{"ports":[{"port":8188}],"supports_workflows":true}`
	}
	var pub *time.Time
	if status != StatusDraft {
		pub = &publishedAt
	}
	var id int64
	if err := e.pool.QueryRow(e.ctx, `INSERT INTO app_versions (app_id, version, status, image_ids, spec, script, spec_hash, published_at, created_at, updated_at)
		VALUES ($1, $2, $3, ARRAY[$4::bigint], $5, 'true', $6, $7, $8, $8) RETURNING id`,
		appID, ver, status, e.imageID, spec, fmt.Sprintf("hash-%d-%s", appID, ver), pub, dbtest.T0).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// dep 建一台机器 + running 实例 + 部署（指定应用版本与状态），返回部署与实例 ID。
func (e *instEnv) dep(appID, verID int64, state string) (string, string) {
	e.t.Helper()
	e.n++
	id, ins := fmt.Sprintf("dep-in%010d", e.n), fmt.Sprintf("ins-in%06d", e.n)
	mid := dbtest.CreateMachine(e.t, e.pool, fmt.Sprintf("SN-IN%03d", e.n))
	e.exec(`INSERT INTO instances (id, user_id, machine_id, product_id, image_id, state, ssh_password_enc, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'running', 'x', $6, $6)`, ins, e.uid, mid, dbtest.ProductID(e.t, e.pool), e.imageID, dbtest.T0)
	e.exec(`INSERT INTO app_deployments (id, instance_id, user_id, app_id, app_version_id, state, port, spec_hash, next_poll_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, 8188, (SELECT spec_hash FROM app_versions WHERE id = $5), $7, $7, $7)`,
		id, ins, e.uid, appID, verID, state, dbtest.T0)
	return id, ins
}

// pubWF 建一个已上架工作流及其已发布版本 1.0.0（sdxl 测试数据，带 api_prompt）。
func (e *instEnv) pubWF(slug string) (Workflow, Version) {
	e.t.Helper()
	w := e.workflow(slug, StatusPublished)
	return w, e.published(w.ID, "1.0.0")
}

func (e *instEnv) get(depID string) appdeploy.Deployment {
	e.t.Helper()
	d, err := e.apps.Get(e.ctx, depID)
	if err != nil {
		e.t.Fatal(err)
	}
	return d
}

type dwRow struct {
	WorkflowID, VersionID int64
	State                 string
	InstalledAt           *time.Time
}

func (e *instEnv) rows(depID string) []dwRow {
	e.t.Helper()
	rows, err := e.pool.Query(e.ctx, `SELECT workflow_id, workflow_version_id, state, installed_at FROM deployment_workflows
		WHERE deployment_id = $1 ORDER BY workflow_id`, depID)
	if err != nil {
		e.t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (dwRow, error) {
		var w dwRow
		err := r.Scan(&w.WorkflowID, &w.VersionID, &w.State, &w.InstalledAt)
		return w, err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *instEnv) events(depID string) []string {
	e.t.Helper()
	rows, err := e.pool.Query(e.ctx, `SELECT detail FROM app_deploy_events WHERE deployment_id = $1 ORDER BY id`, depID)
	if err != nil {
		e.t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *instEnv) add(dep, ins string, wfID int64, vid *int64) (Result, error) {
	return e.in.Add(e.ctx, AddRequest{UserID: e.uid, InstanceID: ins, DeploymentID: dep, WorkflowID: wfID, VersionID: vid, IP: "1.2.3.4"})
}

func TestInstallerAddRunning(t *testing.T) {
	e := newInstEnv(t)
	w, v := e.pubWF("sdxl-txt2img")
	dep, ins := e.dep(e.comfy, e.comfyNew, appdeploy.StateRunning)
	before := e.get(dep)

	res, err := e.add(dep, ins, w.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := res.Deployment
	if d.State != appdeploy.StateQueued || d.AppVersionID != e.comfyNew || d.RowVersion <= before.RowVersion || d.SpecHash != before.SpecHash {
		t.Fatalf("deployment = %+v", d)
	}
	if len(res.Workflows) != 1 || res.Workflows[0].WorkflowID != w.ID || res.Workflows[0].VersionID != v.ID || res.Workflows[0].State != WFStatePending ||
		res.Workflows[0].InstalledAt != nil || !res.Workflows[0].QuickRun || res.Workflows[0].UpdateAvailable {
		t.Fatalf("workflows = %+v", res.Workflows)
	}
	ev := e.events(dep)
	if !slices.Contains(ev, "添加工作流 sdxl-txt2img v1.0.0") || !slices.Contains(ev, "重新部署 v1.1.0") {
		t.Fatalf("events = %q", ev)
	}
	if n := e.count(`SELECT count(*) FROM audit_logs WHERE action = 'instance.workflow_add' AND target_id = $1 AND actor_id = $2 AND ip = '1.2.3.4'
		AND diff->>'deployment' = $3 AND (diff->>'workflow_id')::bigint = $4 AND (diff->>'version_id')::bigint = $5`, ins, e.uid, dep, w.ID, v.ID); n != 1 {
		t.Fatalf("audit = %d", n)
	}
	// 重复添加 → workflow_exists
	_, err = e.add(dep, ins, w.ID, nil)
	wantCode(t, err, "workflow_exists")
}

func TestInstallerStateMatrix(t *testing.T) {
	e := newInstEnv(t)
	w, _ := e.pubWF("sdxl-txt2img")
	for _, st := range []string{appdeploy.StateStopped, appdeploy.StateFailed} {
		dep, ins := e.dep(e.comfy, e.comfyNew, st)
		before := e.get(dep)
		res, err := e.add(dep, ins, w.ID, nil)
		if err != nil {
			t.Fatalf("%s: %v", st, err)
		}
		if res.Deployment.State != appdeploy.StateQueued || res.Deployment.RowVersion != before.RowVersion+1 {
			t.Fatalf("%s: deployment = %+v (before row_version %d)", st, res.Deployment, before.RowVersion)
		}
	}
	// queued：只写集合，row_version + 1（Runner 正在打的包作废），不重新部署
	dep, ins := e.dep(e.comfy, e.comfyNew, appdeploy.StateQueued)
	before := e.get(dep)
	res, err := e.add(dep, ins, w.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deployment.State != appdeploy.StateQueued || res.Deployment.RowVersion != before.RowVersion+1 || len(res.Workflows) != 1 {
		t.Fatalf("queued: %+v / %+v", res.Deployment, res.Workflows)
	}
	if ev := e.events(dep); len(ev) != 1 || ev[0] != "添加工作流 sdxl-txt2img v1.0.0" {
		t.Fatalf("queued events = %q", ev)
	}
	for _, st := range []string{appdeploy.StateInstalling, appdeploy.StateRemoving} {
		dep, ins := e.dep(e.comfy, e.comfyNew, st)
		_, err := e.add(dep, ins, w.ID, nil)
		wantCode(t, err, "app_invalid_state")
		if len(e.rows(dep)) != 0 {
			t.Fatalf("%s: row written", st)
		}
	}
}

func TestInstallerAddErrors(t *testing.T) {
	e := newInstEnv(t)
	w, v := e.pubWF("sdxl-txt2img")
	dep, ins := e.dep(e.comfy, e.comfyNew, appdeploy.StateRunning)

	// 不存在 / 草稿 / 已下线 → workflow_not_found
	_, err := e.add(dep, ins, 99999, nil)
	wantCode(t, err, "workflow_not_found")
	draft := e.workflow("draft-wf", StatusDraft)
	e.published(draft.ID, "1.0.0")
	_, err = e.add(dep, ins, draft.ID, nil)
	wantCode(t, err, "workflow_not_found")
	retired, _ := e.pubWF("retired-wf")
	e.exec(`UPDATE comfy_workflows SET status = 'retired' WHERE id = $1`, retired.ID)
	_, err = e.add(dep, ins, retired.ID, nil)
	wantCode(t, err, "workflow_not_found")

	// 上架但没有已发布版本 → workflow_unavailable；指定草稿版本 → workflow_unavailable
	bare := e.workflow("bare-wf", StatusPublished)
	dv := e.version(bare.ID, "0.1.0")
	_, err = e.add(dep, ins, bare.ID, nil)
	wantCode(t, err, "workflow_unavailable")
	_, err = e.add(dep, ins, bare.ID, &dv.ID)
	wantCode(t, err, "workflow_unavailable")
	// 指定别的工作流的版本 → workflow_not_found
	other, _ := e.pubWF("other-wf")
	_, err = e.add(dep, ins, other.ID, &v.ID)
	wantCode(t, err, "workflow_not_found")

	// 非 comfyui 部署 → workflow_app_unsupported
	vdep, vins := e.dep(e.vllm, e.vllmVer, appdeploy.StateRunning)
	_, err = e.add(vdep, vins, w.ID, nil)
	wantCode(t, err, "workflow_app_unsupported")
	// 部署不在该实例上 / 别人的实例 → not_found
	_, err = e.add(vdep, ins, w.ID, nil)
	wantCode(t, err, "not_found")
	_, err = e.in.Add(e.ctx, AddRequest{UserID: e.uid + 1000, InstanceID: ins, DeploymentID: dep, WorkflowID: w.ID})
	wantCode(t, err, "not_found")

	// 上限 8
	full, fins := e.dep(e.comfy, e.comfyNew, appdeploy.StateRunning)
	for i := 0; i < MaxWorkflowsPerDeployment; i++ {
		_, fv := e.pubWF(fmt.Sprintf("wf-%d", i))
		e.attach(full, fv, WFStateInstalled)
	}
	_, err = e.add(full, fins, w.ID, nil)
	wantCode(t, err, "workflow_limit")
	if len(e.rows(dep)) != 0 {
		t.Fatalf("rows written on errors: %+v", e.rows(dep))
	}
}

func TestInstallerAppVersionSelection(t *testing.T) {
	e := newInstEnv(t)
	w, _ := e.pubWF("sdxl-txt2img")
	// 更新的但不支持工作流的版本不会被选中
	e.appVersion(e.comfy, "1.0.1", false, StatusPublished, dbtest.T0.Add(90*time.Minute))

	// 1.0.0 部署：升到最新的支持版本，spec_hash 随之变
	dep, ins := e.dep(e.comfy, e.comfyOld, appdeploy.StateRunning)
	before := e.get(dep)
	res, err := e.add(dep, ins, w.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deployment.AppVersionID != e.comfyNew || res.Deployment.SpecHash == before.SpecHash || res.Deployment.State != appdeploy.StateQueued {
		t.Fatalf("upgrade: %+v", res.Deployment)
	}

	// 支持工作流的当前版本已下线：改用最新的已发布支持版本
	newer := e.appVersion(e.comfy, "1.2.0", true, StatusPublished, dbtest.T0.Add(100*time.Minute))
	e.exec(`UPDATE app_versions SET status = 'retired' WHERE id = $1`, e.comfyNew)
	dep2, ins2 := e.dep(e.comfy, e.comfyNew, appdeploy.StateStopped)
	res, err = e.add(dep2, ins2, w.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deployment.AppVersionID != newer {
		t.Fatalf("retired current: version %d, want %d", res.Deployment.AppVersionID, newer)
	}
	// 支持且仍发布的当前版本不换（即使有更新的支持版本）
	e.exec(`UPDATE app_versions SET status = 'published' WHERE id = $1`, e.comfyNew)
	dep3, ins3 := e.dep(e.comfy, e.comfyNew, appdeploy.StateRunning)
	if res, err = e.add(dep3, ins3, w.ID, nil); err != nil || res.Deployment.AppVersionID != e.comfyNew {
		t.Fatalf("keep current: %+v, %v", res.Deployment, err)
	}

	// queued 的 1.0.0 部署（不重新部署，版本换不了）→ workflow_app_unsupported
	qdep, qins := e.dep(e.comfy, e.comfyOld, appdeploy.StateQueued)
	_, err = e.add(qdep, qins, w.ID, nil)
	wantCode(t, err, "workflow_app_unsupported")

	// 没有任何已发布的支持版本 → workflow_app_unsupported
	e.exec(`UPDATE app_versions SET status = 'retired' WHERE app_id = $1 AND (spec->>'supports_workflows')::boolean`, e.comfy)
	dep4, ins4 := e.dep(e.comfy, e.comfyOld, appdeploy.StateRunning)
	_, err = e.add(dep4, ins4, w.ID, nil)
	wantCode(t, err, "workflow_app_unsupported")
}

func TestInstallerUpdate(t *testing.T) {
	e := newInstEnv(t)
	w, v1 := e.pubWF("sdxl-txt2img")
	dep, ins := e.dep(e.comfy, e.comfyNew, appdeploy.StateRunning)
	e.attach(dep, v1, WFStateInstalled)
	e.exec(`UPDATE deployment_workflows SET installed_at = $2 WHERE deployment_id = $1`, dep, dbtest.T0)
	upd := func(vid *int64) (Result, error) {
		return e.in.Update(e.ctx, UpdateRequest{UserID: e.uid, InstanceID: ins, DeploymentID: dep, WorkflowID: w.ID, VersionID: vid, IP: "1.2.3.4"})
	}

	// 同版本 → 不变（不重新部署）
	before := e.get(dep)
	res, err := upd(nil)
	if err != nil || res.Deployment.RowVersion != before.RowVersion || res.Deployment.State != appdeploy.StateRunning {
		t.Fatalf("same version: %+v, %v", res.Deployment, err)
	}

	e.clk.Advance(time.Minute)
	v2 := e.published(w.ID, "1.1.0")
	// 视图：有新版本
	ws, err := e.in.ListInstalled(e.ctx, dep)
	if err != nil || len(ws) != 1 || !ws[0].UpdateAvailable || ws[0].LatestVersionID == nil || *ws[0].LatestVersionID != v2.ID || ws[0].LatestVersion != "1.1.0" {
		t.Fatalf("installed = %+v, %v", ws, err)
	}
	res, err = upd(nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deployment.State != appdeploy.StateQueued || len(res.Workflows) != 1 || res.Workflows[0].VersionID != v2.ID ||
		res.Workflows[0].State != WFStatePending || res.Workflows[0].InstalledAt != nil || res.Workflows[0].UpdateAvailable {
		t.Fatalf("update: %+v / %+v", res.Deployment, res.Workflows)
	}
	if ev := e.events(dep); !slices.Contains(ev, "更新工作流 sdxl-txt2img v1.0.0 → v1.1.0") {
		t.Fatalf("events = %q", ev)
	}
	if n := e.count(`SELECT count(*) FROM audit_logs WHERE action = 'instance.workflow_update' AND (diff->>'version_id')::bigint = $1
		AND (diff->>'from_version_id')::bigint = $2`, v2.ID, v1.ID); n != 1 {
		t.Fatalf("audit = %d", n)
	}
	// 显式换回旧版本（queued：只写集合）
	before = e.get(dep)
	res, err = upd(&v1.ID)
	if err != nil || res.Workflows[0].VersionID != v1.ID || res.Deployment.RowVersion != before.RowVersion+1 {
		t.Fatalf("downgrade: %+v, %v", res, err)
	}

	// 没装的工作流 → workflow_not_found
	other, _ := e.pubWF("other-wf")
	_, err = e.in.Update(e.ctx, UpdateRequest{UserID: e.uid, InstanceID: ins, DeploymentID: dep, WorkflowID: other.ID})
	wantCode(t, err, "workflow_not_found")
	// 工作流下线后更新 → workflow_unavailable
	e.exec(`UPDATE comfy_workflows SET status = 'retired' WHERE id = $1`, w.ID)
	_, err = upd(&v2.ID)
	wantCode(t, err, "workflow_unavailable")
}

func TestInstallerRemove(t *testing.T) {
	e := newInstEnv(t)
	w1, v1 := e.pubWF("sdxl-txt2img")
	w2, v2 := e.pubWF("flux-schnell")
	dep, ins := e.dep(e.comfy, e.comfyNew, appdeploy.StateRunning)
	e.attach(dep, v1, WFStateInstalled)
	e.attach(dep, v2, WFStateFailed)
	rm := func(dep, ins string, wfID int64) (Result, error) {
		return e.in.Remove(e.ctx, RemoveRequest{UserID: e.uid, InstanceID: ins, DeploymentID: dep, WorkflowID: wfID, IP: "1.2.3.4"})
	}

	res, err := rm(dep, ins, w1.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 重新部署；其余失败的工作流随本次部署重装（failed → pending）
	if res.Deployment.State != appdeploy.StateQueued || len(res.Workflows) != 1 || res.Workflows[0].WorkflowID != w2.ID || res.Workflows[0].State != WFStatePending {
		t.Fatalf("remove: %+v / %+v", res.Deployment, res.Workflows)
	}
	if ev := e.events(dep); !slices.Contains(ev, "移除工作流 sdxl-txt2img") || !slices.Contains(ev, "重新部署 v1.1.0") {
		t.Fatalf("events = %q", ev)
	}
	if n := e.count(`SELECT count(*) FROM audit_logs WHERE action = 'instance.workflow_remove' AND (diff->>'workflow_id')::bigint = $1`, w1.ID); n != 1 {
		t.Fatalf("audit = %d", n)
	}
	// 没装 → workflow_not_found
	_, err = rm(dep, ins, w1.ID)
	wantCode(t, err, "workflow_not_found")

	// queued：只删行 + row_version + 1
	before := e.get(dep)
	res, err = rm(dep, ins, w2.ID)
	if err != nil || len(res.Workflows) != 0 || res.Deployment.RowVersion != before.RowVersion+1 || res.Deployment.State != appdeploy.StateQueued {
		t.Fatalf("queued remove: %+v, %v", res, err)
	}
	if ev := e.events(dep); strings.Count(strings.Join(ev, "\n"), "重新部署") != 1 {
		t.Fatalf("events = %q", ev)
	}

	// 部署版本不支持工作流且没有可用的支持版本：仍可移除（只删行，不重新部署）
	e.exec(`UPDATE app_versions SET status = 'retired' WHERE id = $1`, e.comfyNew)
	odep, oins := e.dep(e.comfy, e.comfyOld, appdeploy.StateRunning)
	e.attach(odep, v1, WFStatePending)
	before = e.get(odep)
	res, err = rm(odep, oins, w1.ID)
	if err != nil || len(res.Workflows) != 0 || res.Deployment.State != appdeploy.StateRunning || res.Deployment.RowVersion != before.RowVersion+1 {
		t.Fatalf("unsupported remove: %+v, %v", res, err)
	}
}

// 集合里两个工作流对同一自定义节点钉了不同 commit → 添加时就拒绝（否则部署包生成失败）。
func TestInstallerNodeConflict(t *testing.T) {
	e := newInstEnv(t)
	insertNodeVersion := func(slug, commit string) Workflow {
		w := e.workflow(slug, StatusPublished)
		n := usduNode()
		n.Commit = commit
		m, err := json.Marshal(Manifest{Nodes: []NodeSpec{n}})
		if err != nil {
			t.Fatal(err)
		}
		e.exec(`INSERT INTO comfy_workflow_versions (workflow_id, version, status, graph, manifest, manifest_hash, published_at, created_at, updated_at)
			VALUES ($1, '1.0.0', 'published', '{}', $2, 'h', $3, $3, $3)`, w.ID, m, e.clk.Now())
		return w
	}
	a := insertNodeVersion("node-a", testCommit)
	b := insertNodeVersion("node-b", strings.Repeat("b", 40))
	dep, ins := e.dep(e.comfy, e.comfyNew, appdeploy.StateRunning)
	if _, err := e.add(dep, ins, a.ID, nil); err != nil {
		t.Fatal(err)
	}
	_, err := e.add(dep, ins, b.ID, nil)
	wantCode(t, err, "workflow_version_invalid")
	if len(e.rows(dep)) != 1 {
		t.Fatalf("rows = %+v", e.rows(dep))
	}
	// 预装同样拒绝
	_, err = e.hooks.ResolveWorkflows(e.ctx, e.pool, []int64{a.ID, b.ID})
	wantCode(t, err, "workflow_version_invalid")
}

func TestOnTransition(t *testing.T) {
	e := newInstEnv(t)
	_, v1 := e.pubWF("sdxl-txt2img")
	_, v2 := e.pubWF("flux-schnell")
	_, v3 := e.pubWF("wan-video")
	dep, _ := e.dep(e.comfy, e.comfyNew, appdeploy.StateInstalling)
	e.attach(dep, v1, WFStatePending)
	e.attach(dep, v2, WFStateFailed)
	e.attach(dep, v3, WFStateInstalled)
	e.exec(`UPDATE deployment_workflows SET installed_at = $2 WHERE deployment_id = $1 AND workflow_id = $3`, dep, dbtest.T0, v3.WorkflowID)
	tx := func(fn func(pgx.Tx) error) {
		t.Helper()
		if err := db.WithTx(e.ctx, e.pool, fn); err != nil {
			t.Fatal(err)
		}
	}
	d := e.get(dep)
	now := e.clk.Now()

	// 其它目标状态不动
	tx(func(q pgx.Tx) error { return e.hooks.OnTransition(e.ctx, q, d, appdeploy.StateStopped, now) })
	if r := e.rows(dep); r[0].State != WFStatePending || r[1].State != WFStateFailed {
		t.Fatalf("stopped: %+v", r)
	}
	// failed：pending → failed
	tx(func(q pgx.Tx) error { return e.hooks.OnTransition(e.ctx, q, d, appdeploy.StateFailed, now) })
	if r := e.rows(dep); r[0].State != WFStateFailed || r[1].State != WFStateFailed || r[2].State != WFStateInstalled {
		t.Fatalf("failed: %+v", r)
	}
	// running：pending / failed → installed（installed_at = now）；已装的不改 installed_at
	e.exec(`UPDATE deployment_workflows SET state = 'pending' WHERE deployment_id = $1 AND workflow_id = $2`, dep, v1.WorkflowID)
	tx(func(q pgx.Tx) error { return e.hooks.OnTransition(e.ctx, q, d, appdeploy.StateRunning, now) })
	r := e.rows(dep)
	for i, row := range r {
		if row.State != WFStateInstalled || row.InstalledAt == nil {
			t.Fatalf("running[%d]: %+v", i, row)
		}
	}
	if !r[0].InstalledAt.Equal(now) || !r[1].InstalledAt.Equal(now) || !r[2].InstalledAt.Equal(dbtest.T0) {
		t.Fatalf("installed_at: %v %v %v", r[0].InstalledAt, r[1].InstalledAt, r[2].InstalledAt)
	}

	// 部署版本不支持工作流（包里没有 workflows/）：running 不把 pending 标成 installed
	odep, _ := e.dep(e.comfy, e.comfyOld, appdeploy.StateInstalling)
	e.attach(odep, v1, WFStatePending)
	od := e.get(odep)
	tx(func(q pgx.Tx) error { return e.hooks.OnTransition(e.ctx, q, od, appdeploy.StateRunning, now) })
	if r := e.rows(odep); r[0].State != WFStatePending {
		t.Fatalf("unsupported running: %+v", r)
	}
	// 非 comfyui 部署、没有行：无操作
	vdep, _ := e.dep(e.vllm, e.vllmVer, appdeploy.StateInstalling)
	tx(func(q pgx.Tx) error { return e.hooks.OnTransition(e.ctx, q, e.get(vdep), appdeploy.StateRunning, now) })
}

func TestBundleExtra(t *testing.T) {
	e := newInstEnv(t)
	_, v1 := e.pubWF("sdxl-txt2img")
	_, v2 := e.pubWF("flux-schnell")
	dep, _ := e.dep(e.comfy, e.comfyNew, appdeploy.StateQueued)
	d := e.get(dep)
	newVer, err := e.appsCat.GetVersion(e.ctx, e.comfyNew)
	if err != nil {
		t.Fatal(err)
	}

	// 空集合：仍给 workflows/manifest.json（{"workflows":[]}），app.sh 据此清掉残留的 spark-*.json
	files, err := e.hooks.BundleExtra(e.ctx, d, newVer)
	if err != nil || len(files) != 1 || files[0].Name != BundleManifestName || strings.TrimSpace(string(files[0].Body)) != `{"workflows":[]}` {
		t.Fatalf("empty = %+v, %v", files, err)
	}

	e.attach(dep, v1, WFStateInstalled)
	e.attach(dep, v2, WFStatePending)
	files, err = e.hooks.BundleExtra(e.ctx, d, newVer)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range files {
		names = append(names, f.Name)
	}
	if !slices.Equal(names, []string{"workflows/manifest.json", "workflows/flux-schnell.json", "workflows/sdxl-txt2img.json"}) {
		t.Fatalf("names = %q", names)
	}
	var m struct {
		Workflows []struct {
			Slug, Version, File string
			Models              []ModelSpec
		}
	}
	if err := json.Unmarshal(files[0].Body, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Workflows) != 2 || m.Workflows[0].Slug != "flux-schnell" || m.Workflows[1].File != "sdxl-txt2img.json" || len(m.Workflows[1].Models) != 1 {
		t.Fatalf("manifest = %s", files[0].Body)
	}
	if !json.Valid(files[2].Body) || len(files[2].Body) == 0 {
		t.Fatalf("graph = %q", files[2].Body)
	}
	if strings.Contains(string(files[0].Body), "api_prompt") {
		t.Fatal("manifest carries api_prompt")
	}

	// 不支持工作流的版本：不追加任何文件
	oldVer, err := e.appsCat.GetVersion(e.ctx, e.comfyOld)
	if err != nil {
		t.Fatal(err)
	}
	if files, err := e.hooks.BundleExtra(e.ctx, d, oldVer); err != nil || files != nil {
		t.Fatalf("unsupported = %+v, %v", files, err)
	}
}

func TestResolveWorkflows(t *testing.T) {
	e := newInstEnv(t)
	w1, v1 := e.pubWF("sdxl-txt2img")
	w2, _ := e.pubWF("flux-schnell")
	e.clk.Advance(time.Minute)
	v2b := e.published(w2.ID, "1.1.0")

	refs, err := e.hooks.ResolveWorkflows(e.ctx, e.pool, []int64{w1.ID, w2.ID, w1.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(refs, []appdeploy.WorkflowRef{{WorkflowID: w1.ID, VersionID: v1.ID}, {WorkflowID: w2.ID, VersionID: v2b.ID}}) {
		t.Fatalf("refs = %+v", refs)
	}
	if refs, err := e.hooks.ResolveWorkflows(e.ctx, e.pool, nil); err != nil || refs != nil {
		t.Fatalf("empty = %+v, %v", refs, err)
	}
	many := make([]int64, MaxWorkflowsPerDeployment+1)
	for i := range many {
		many[i] = int64(i + 1)
	}
	_, err = e.hooks.ResolveWorkflows(e.ctx, e.pool, many)
	wantCode(t, err, "workflow_limit")
	draft := e.workflow("draft-wf", StatusDraft)
	_, err = e.hooks.ResolveWorkflows(e.ctx, e.pool, []int64{w1.ID, draft.ID})
	wantCode(t, err, "workflow_not_found")
	_, err = e.hooks.ResolveWorkflows(e.ctx, e.pool, []int64{424242})
	wantCode(t, err, "workflow_not_found")
	bare := e.workflow("bare-wf", StatusPublished)
	_, err = e.hooks.ResolveWorkflows(e.ctx, e.pool, []int64{bare.ID})
	wantCode(t, err, "workflow_unavailable")
}

func TestInstalledViews(t *testing.T) {
	e := newInstEnv(t)
	w1, v1 := e.pubWF("sdxl-txt2img")
	w2 := e.workflow("no-quick", StatusPublished)
	in := goodVersion(t, "1.0.0")
	in.APIPrompt = nil
	in.Manifest.Inputs, in.Manifest.Outputs = nil, nil
	v2, err := e.cat.CreateVersion(e.ctx, e.actor(), w2.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	if v2, err = e.cat.PublishVersion(e.ctx, e.actor(), w2.ID, v2.ID); err != nil {
		t.Fatal(err)
	}
	dep, _ := e.dep(e.comfy, e.comfyNew, appdeploy.StateRunning)
	empty, _ := e.dep(e.comfy, e.comfyNew, appdeploy.StateRunning)
	e.attach(dep, v1, WFStateInstalled)
	e.attach(dep, v2, WFStatePending)

	ws, err := e.in.ListInstalled(e.ctx, dep)
	if err != nil || len(ws) != 2 {
		t.Fatalf("installed = %+v, %v", ws, err)
	}
	// 按 slug 排序
	if ws[0].Slug != "no-quick" || ws[0].QuickRun || ws[1].Slug != "sdxl-txt2img" || !ws[1].QuickRun || ws[1].Category != CategoryTxt2Img ||
		ws[1].Name != w1.Name || ws[1].Version != "1.0.0" || ws[1].UpdateAvailable || ws[1].LatestVersionID == nil || *ws[1].LatestVersionID != v1.ID {
		t.Fatalf("installed = %+v", ws)
	}
	if ws, err := e.in.ListInstalled(e.ctx, empty); err != nil || ws == nil || len(ws) != 0 {
		t.Fatalf("empty = %#v, %v", ws, err)
	}
	// 工作流下线：没有「最新版本」，不提示更新
	e.exec(`UPDATE comfy_workflows SET status = 'retired' WHERE id = $1`, w1.ID)
	ws, _ = e.in.ListInstalled(e.ctx, dep)
	if ws[1].LatestVersionID != nil || ws[1].UpdateAvailable || ws[1].LatestVersion != "" {
		t.Fatalf("retired = %+v", ws[1])
	}

	views, err := e.hooks.WorkflowViews(e.ctx, []string{dep, empty})
	if err != nil || len(views[dep]) != 2 || len(views[empty]) != 0 {
		t.Fatalf("views = %v, %v", views, err)
	}
	var obj map[string]any
	if err := json.Unmarshal(views[dep][1], &obj); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"workflow_id", "slug", "name", "category", "version_id", "version", "state", "latest_version_id", "latest_version",
		"update_available", "quick_run", "added_at", "installed_at"} {
		if _, ok := obj[k]; !ok {
			t.Fatalf("view lacks %s: %s", k, views[dep][1])
		}
	}
	if len(obj) != 13 {
		t.Fatalf("view keys = %v", obj)
	}

	// 接到 appdeploy：部署视图带 workflows
	d := e.get(dep)
	vs, err := e.apps.Views(e.ctx, []appdeploy.Deployment{d, e.get(empty)})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(vs)
	if !strings.Contains(string(b), `"slug":"sdxl-txt2img"`) || !strings.Contains(string(b), `"workflows":[]`) || strings.Contains(string(b), "api_prompt") {
		t.Fatalf("deployment views = %s", b)
	}
}
