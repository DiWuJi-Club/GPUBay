package appdeploy_test

// 计划 11 的 appdeploy 扩展点：PrepareInstall / QueueInstall 带工作流、换机复制集合、RedeployInTx、视图 workflows 字段、目录 supports_workflows。

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/rental"
)

// workflowSpec 是支持工作流的版本 spec（goodSpec + supports_workflows）。
func workflowSpec(port int) appdeploy.Spec {
	s := specPorts(port)
	s.SupportsWorkflows = true
	return s
}

// comfyWorkflow 直接插一个 published 工作流及其 published 版本（comfywf 的目录服务不在本包）。
func (e *env) comfyWorkflow(slug string) (wfID, verID int64) {
	e.t.Helper()
	now := e.clk.Now()
	if err := e.pool.QueryRow(e.ctx, `INSERT INTO comfy_workflows (slug, name, category, status, created_at, updated_at)
		VALUES ($1, $1, 'txt2img', 'published', $2, $2) RETURNING id`, slug, now).Scan(&wfID); err != nil {
		e.t.Fatal(err)
	}
	if err := e.pool.QueryRow(e.ctx, `INSERT INTO comfy_workflow_versions (workflow_id, version, status, graph, manifest, manifest_hash, published_at, created_at, updated_at)
		VALUES ($1, '1.0.0', 'published', '{}', '{}', 'h', $2, $2, $2) RETURNING id`, wfID, now).Scan(&verID); err != nil {
		e.t.Fatal(err)
	}
	return wfID, verID
}

type depWorkflow struct {
	WorkflowID, VersionID int64
	State                 string
	AddedAt               time.Time
	InstalledAt           *time.Time
}

func (e *env) depWorkflows(depID string) []depWorkflow {
	e.t.Helper()
	rows, err := e.pool.Query(e.ctx, `SELECT workflow_id, workflow_version_id, state, added_at, installed_at FROM deployment_workflows
		WHERE deployment_id = $1 ORDER BY workflow_id`, depID)
	if err != nil {
		e.t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (depWorkflow, error) {
		var w depWorkflow
		err := r.Scan(&w.WorkflowID, &w.VersionID, &w.State, &w.AddedAt, &w.InstalledAt)
		return w, err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func TestPrepareInstallWorkflows(t *testing.T) {
	e := newEnv(t, 2)
	plain, _ := e.publishedApp("vllm", goodSpec(), goodScript, []int64{e.imageID})
	comfy, comfyVer := e.publishedApp("comfyui", workflowSpec(8188), goodScript, []int64{e.imageID})
	w1, v1 := e.comfyWorkflow("sdxl-txt2img")
	w2, v2 := e.comfyWorkflow("flux-schnell")

	var gotIDs []int64
	var resolveErr error
	svc := e.serviceWith(func(d *appdeploy.Deps) {
		d.ResolveWorkflows = func(_ context.Context, q db.Querier, ids []int64) ([]appdeploy.WorkflowRef, error) {
			if q == nil {
				t.Fatal("ResolveWorkflows got nil querier")
			}
			gotIDs = slices.Clone(ids)
			if resolveErr != nil {
				return nil, resolveErr
			}
			return []appdeploy.WorkflowRef{{WorkflowID: w1, VersionID: v1}, {WorkflowID: w2, VersionID: v2}}, nil
		}
	})
	ids := []int64{w1, w2}

	// 版本不支持工作流 → workflow_app_unsupported（不调 ResolveWorkflows）
	if _, err := svc.PrepareInstall(e.ctx, e.pool, plain.ID, e.imageID, model4B, ids); !errors.Is(err, apperr.ErrWorkflowAppUnsupported) || gotIDs != nil {
		t.Fatalf("unsupported version = %v (resolve called with %v)", err, gotIDs)
	}
	// 没接 ResolveWorkflows → workflow_app_unsupported
	if _, err := e.svc.PrepareInstall(e.ctx, e.pool, comfy.ID, e.imageID, model4B, ids); !errors.Is(err, apperr.ErrWorkflowAppUnsupported) {
		t.Fatalf("no resolver = %v", err)
	}
	// ResolveWorkflows 的错误原样返回
	resolveErr = apperr.ErrWorkflowLimit
	if _, err := svc.PrepareInstall(e.ctx, e.pool, comfy.ID, e.imageID, model4B, ids); !errors.Is(err, apperr.ErrWorkflowLimit) || !slices.Equal(gotIDs, ids) {
		t.Fatalf("resolver error = %v (ids %v)", err, gotIDs)
	}
	resolveErr = nil
	plan, err := svc.PrepareInstall(e.ctx, e.pool, comfy.ID, e.imageID, model4B, ids)
	if err != nil {
		t.Fatal(err)
	}

	inst := e.reservedInstance()
	e.clk.Advance(time.Minute)
	now := e.clk.Now()
	e.hookTx(inst.ID, func(tx pgx.Tx) error { return svc.QueueInstall(e.ctx, tx, inst, plan, now) })
	live, err := svc.ListLive(e.ctx, inst.ID)
	if err != nil || len(live) != 1 || live[0].AppVersionID != comfyVer.ID {
		t.Fatalf("live = %+v, %v", live, err)
	}
	d := live[0]
	got := e.depWorkflows(d.ID)
	if len(got) != 2 {
		t.Fatalf("deployment_workflows = %+v", got)
	}
	for _, w := range got {
		if w.State != "pending" || !w.AddedAt.Equal(now) || w.InstalledAt != nil || (w.WorkflowID == w1) != (w.VersionID == v1) {
			t.Fatalf("workflow row = %+v", w)
		}
	}
	if ev := e.lastEvent(d.ID); ev.FromState != appdeploy.StateQueued || ev.ToState != appdeploy.StateQueued || ev.Detail != "预装工作流 2 个" {
		t.Fatalf("event = %+v", ev)
	}
	logs := e.auditLogs("instance.app_deploy", inst.ID)
	if len(logs) != 1 || !strings.Contains(string(logs[0].Diff), `"workflow_ids"`) {
		t.Fatalf("audit = %+v", logs)
	}

	// 不带工作流：不调 ResolveWorkflows、不写集合、没有额外事件
	gotIDs = nil
	plan, err = svc.PrepareInstall(e.ctx, e.pool, comfy.ID, e.imageID, model4B, nil)
	if err != nil || gotIDs != nil {
		t.Fatalf("plain preinstall = %v (resolve %v)", err, gotIDs)
	}
	inst2 := e.reservedInstance()
	e.hookTx(inst2.ID, func(tx pgx.Tx) error { return svc.QueueInstall(e.ctx, tx, inst2, plan, now) })
	live, _ = svc.ListLive(e.ctx, inst2.ID)
	if len(live) != 1 || len(e.depWorkflows(live[0].ID)) != 0 || e.count(`SELECT count(*) FROM app_deploy_events WHERE deployment_id = $1`, live[0].ID) != 1 {
		t.Fatalf("plain preinstall wrote workflows / events: %+v", live)
	}
}

// D14：换机复制部署时连带复制工作流集合（全部 pending、added_at = now）；旧集合原样留在 lost 的部署上；不迁移的部署没有集合。
func TestOnInstanceMigratedCopiesWorkflows(t *testing.T) {
	e := newEnv(t, 2)
	comfy, _ := e.publishedApp("comfyui", workflowSpec(8188), goodScript, []int64{e.imageID})
	w1, v1 := e.comfyWorkflow("sdxl-txt2img")
	w2, v2 := e.comfyWorkflow("flux-schnell")
	old := e.runningInstance()
	d := e.setDep(e.create(old, comfy.ID, model4B).ID, appdeploy.StateRunning, appdeploy.DesiredRunning)
	t0 := e.clk.Now()
	e.exec(`INSERT INTO deployment_workflows (deployment_id, workflow_id, workflow_version_id, state, added_at, installed_at) VALUES
		($1, $2, $3, 'installed', $6, $6), ($1, $4, $5, 'failed', $6, NULL)`, d.ID, w1, v1, w2, v2, t0)
	moved := e.instanceOf(e.userID, rental.StateCreating)

	e.clk.Advance(time.Minute)
	now := e.clk.Now()
	e.hookTx(old.ID, func(tx pgx.Tx) error { return e.svc.OnInstanceMigrated(e.ctx, tx, old.ID, moved.ID, now) })
	news, err := e.svc.ListForInstance(e.ctx, moved.ID)
	if err != nil || len(news) != 1 {
		t.Fatalf("new deployments = %+v, %v", news, err)
	}
	got := e.depWorkflows(news[0].ID)
	if len(got) != 2 {
		t.Fatalf("copied set = %+v", got)
	}
	for _, w := range got {
		if w.State != "pending" || !w.AddedAt.Equal(now) || w.InstalledAt != nil {
			t.Fatalf("copied row = %+v", w)
		}
	}
	if got[0].WorkflowID != min(w1, w2) || got[0].VersionID != map[int64]int64{w1: v1, w2: v2}[got[0].WorkflowID] {
		t.Fatalf("copied rows = %+v", got)
	}
	if oldSet := e.depWorkflows(d.ID); len(oldSet) != 2 || !oldSet[0].AddedAt.Equal(t0) || oldSet[0].State == "pending" || oldSet[1].State == "pending" {
		t.Fatalf("old set changed: %+v", oldSet)
	}
}

// RedeployInTx 与 Redeploy 等价：两条同状态的部署，一条走 Redeploy、一条在调用方自己的事务里（LockOwned → LockDeployment）走 RedeployInTx。
func TestRedeployInTxMatchesRedeploy(t *testing.T) {
	e := newEnv(t, 3)
	app, _ := e.publishedApp("comfyui", workflowSpec(8188), goodScript, []int64{e.imageID})
	v2 := e.publish(app.ID, "1.1.0", workflowSpec(8188), goodScript+"# v2\n", []int64{e.imageID})
	instA, instB := e.runningInstance(), e.runningInstance()
	a := e.setDep(e.create(instA, app.ID, model4B).ID, appdeploy.StateFailed, appdeploy.DesiredRunning)
	b := e.setDep(e.create(instB, app.ID, model4B).ID, appdeploy.StateFailed, appdeploy.DesiredRunning)
	e.clk.Advance(time.Minute)
	req := func(inst rental.Instance, d appdeploy.Deployment) appdeploy.RedeployRequest {
		return appdeploy.RedeployRequest{UserID: e.userID, InstanceID: inst.ID, DeploymentID: d.ID, VersionID: &v2.ID, IP: "9.9.9.9"}
	}
	ra, err := e.svc.Redeploy(e.ctx, req(instA, a))
	if err != nil {
		t.Fatal(err)
	}
	var rb appdeploy.Deployment
	if err := db.WithTx(e.ctx, e.pool, func(tx pgx.Tx) error {
		inst, err := rental.LockOwned(e.ctx, tx, e.userID, instB.ID)
		if err != nil {
			return err
		}
		d, err := appdeploy.LockDeployment(e.ctx, tx, b.ID)
		if err != nil {
			return err
		}
		rb, err = e.svc.RedeployInTx(e.ctx, tx, inst, d, req(instB, b), e.clk.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		before, after appdeploy.Deployment
	}{{a, ra}, {b, rb}} {
		got := e.dep(c.after.ID)
		if got.State != appdeploy.StateQueued || got.AppVersionID != v2.ID || got.SpecHash != v2.SpecHash || got.ErrorCode != "" ||
			got.RowVersion != c.before.RowVersion+1 || got.RowVersion != c.after.RowVersion || !got.NextPollAt.Equal(e.clk.Now()) {
			t.Fatalf("after redeploy = %+v", got)
		}
		if ev := e.lastEvent(got.ID); ev.FromState != appdeploy.StateFailed || ev.ToState != appdeploy.StateQueued || ev.Detail != "重新部署 v1.1.0" {
			t.Fatalf("event = %+v", ev)
		}
		logs := e.auditLogs("instance.app_redeploy", got.InstanceID)
		if len(logs) != 1 || logs[0].IP != "9.9.9.9" || logs[0].ActorID != e.userID {
			t.Fatalf("audit = %+v", logs)
		}
	}
	// 状态校验同样在 RedeployInTx 里：installing → app_invalid_state
	c := e.setDep(e.create(e.runningInstance(), app.ID, model4B).ID, appdeploy.StateInstalling, appdeploy.DesiredRunning)
	err = db.WithTx(e.ctx, e.pool, func(tx pgx.Tx) error {
		inst, err := rental.LockOwned(e.ctx, tx, e.userID, c.InstanceID)
		if err != nil {
			return err
		}
		d, err := appdeploy.LockDeployment(e.ctx, tx, c.ID)
		if err != nil {
			return err
		}
		_, err = e.svc.RedeployInTx(e.ctx, tx, inst, d, appdeploy.RedeployRequest{UserID: e.userID}, e.clk.Now())
		return err
	})
	if !errors.Is(err, apperr.ErrAppInvalidState) {
		t.Fatalf("installing = %v", err)
	}
	if _, err := appdeploy.LockDeployment(e.ctx, e.pool, "dep-nope"); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("LockDeployment missing = %v", err)
	}
}

// 视图的 workflows：comfyui（slug）或版本支持工作流的部署恒为数组（钩子没给 / 没接 = []），其它部署省略该字段；摘要不取。
func TestViewsWorkflows(t *testing.T) {
	e := newEnv(t, 1)
	comfy, _ := e.publishedApp("comfyui", specPorts(8188), goodScript, []int64{e.imageID}) // 1.0.0：不支持工作流，但 slug 是 comfyui
	other, _ := e.publishedApp("sdnext", workflowSpec(7860), goodScript, []int64{e.imageID})
	plain, _ := e.publishedApp("vllm", goodSpec(), goodScript, []int64{e.imageID})
	inst := e.runningInstance()
	dc, do, dp := e.create(inst, comfy.ID, model4B), e.create(inst, other.ID, model4B), e.create(inst, plain.ID, model4B)

	var calls [][]string
	svc := e.serviceWith(func(d *appdeploy.Deps) {
		d.WorkflowViews = func(_ context.Context, ids []string) (map[string][]json.RawMessage, error) {
			calls = append(calls, slices.Clone(ids))
			return map[string][]json.RawMessage{dc.ID: {json.RawMessage(`{"workflow_id":1,"slug":"sdxl-txt2img"}`)}}, nil
		}
	})
	views, err := svc.Views(e.ctx, []appdeploy.Deployment{dc, do, dp})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || !slices.Equal(calls[0], []string{dc.ID, do.ID}) {
		t.Fatalf("WorkflowViews calls = %v", calls)
	}
	js := func(v appdeploy.View) string { b, _ := json.Marshal(v); return string(b) }
	if s := js(views[0]); !strings.Contains(s, `"workflows":[{"workflow_id":1,"slug":"sdxl-txt2img"}]`) {
		t.Fatalf("comfyui view = %s", s)
	}
	if s := js(views[1]); !strings.Contains(s, `"workflows":[]`) || !strings.Contains(s, `"supports_workflows":true`) {
		t.Fatalf("supporting app view = %s", s)
	}
	if s := js(views[2]); strings.Contains(s, `"workflows"`) || strings.Contains(s, "supports_workflows") {
		t.Fatalf("plain view = %s", s)
	}
	// 没接钩子：comfyui 仍是 []
	views, err = e.svc.Views(e.ctx, []appdeploy.Deployment{dc, dp})
	if err != nil || !strings.Contains(js(views[0]), `"workflows":[]`) || strings.Contains(js(views[1]), `"workflows"`) {
		t.Fatalf("no hook views = %v %v", views, err)
	}
	// 摘要不取工作流
	calls = nil
	if _, err := svc.Summaries(e.ctx, []string{inst.ID}); err != nil || calls != nil {
		t.Fatalf("Summaries = %v, calls %v", err, calls)
	}
	// 钩子出错 → 视图出错（不静默丢字段）
	bad := e.serviceWith(func(d *appdeploy.Deps) {
		d.WorkflowViews = func(context.Context, []string) (map[string][]json.RawMessage, error) { return nil, errors.New("boom") }
	})
	if _, err := bad.Views(e.ctx, []appdeploy.Deployment{dc}); err == nil {
		t.Fatal("WorkflowViews error swallowed")
	}
	if _, err := bad.Views(e.ctx, []appdeploy.Deployment{dp}); err != nil {
		t.Fatalf("plain deployment must not call WorkflowViews: %v", err)
	}
}

// 目录视图的 version 带 supports_workflows（控制台据此显示「预装工作流」）。
func TestCatalogViewSupportsWorkflows(t *testing.T) {
	e := newEnv(t, 0)
	e.publishedApp("comfyui", workflowSpec(8188), goodScript, []int64{e.imageID})
	e.publishedApp("vllm", goodSpec(), goodScript, []int64{e.imageID})
	items, err := e.svc.CatalogViews(e.ctx, false)
	if err != nil || len(items) != 2 {
		t.Fatalf("catalog = %+v, %v", items, err)
	}
	for _, it := range items {
		b, _ := json.Marshal(it.Version)
		want := `"supports_workflows":` + map[string]string{"comfyui": "true", "vllm": "false"}[it.Slug]
		if !strings.Contains(string(b), want) {
			t.Fatalf("%s version = %s", it.Slug, b)
		}
	}
}
