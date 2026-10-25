package comfywf

import (
	"testing"
	"time"

	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/paging"
)

func TestListInstalls(t *testing.T) {
	e := newCatEnv(t)
	w := e.workflow("sdxl", StatusPublished)
	other := e.workflow("other", StatusPublished)
	v1 := e.published(w.ID, "1.0")
	v2 := e.published(w.ID, "1.1")
	ov := e.published(other.ID, "1.0")
	newDep := e.deployFixture()
	d1, d2, d3, d4 := newDep("running"), newDep("running"), newDep("removed"), newDep("running")
	e.attach(d1, v1, "installed")
	e.exec(`UPDATE deployment_workflows SET installed_at = $2 WHERE deployment_id = $1`, d1, dbtest.T0.Add(time.Minute))
	e.attach(d2, v2, "pending")
	e.exec(`UPDATE deployment_workflows SET added_at = $2 WHERE deployment_id = $1`, d2, dbtest.T0.Add(time.Hour))
	e.attach(d3, v1, "installed") // 已卸载部署的历史行也列出（deployment_state = removed）
	e.exec(`UPDATE deployment_workflows SET added_at = $2 WHERE deployment_id = $1`, d3, dbtest.T0.Add(-time.Hour))
	e.attach(d4, ov, "installed") // 别的工作流

	items, total, err := e.cat.ListInstalls(e.ctx, w.ID, paging.Page{Number: 1, Size: 2})
	if err != nil || total != 3 || len(items) != 2 {
		t.Fatalf("page 1 = %+v %d %v", items, total, err)
	}
	// 按 added_at 倒序
	if items[0].DeploymentID != d2 || items[0].Version != "1.1" || items[0].VersionID != v2.ID || items[0].State != "pending" ||
		items[0].InstalledAt != nil || items[0].DeploymentState != "running" {
		t.Fatalf("first = %+v", items[0])
	}
	if items[1].DeploymentID != d1 || items[1].State != "installed" || items[1].InstalledAt == nil || items[1].Phone != "13800000001" ||
		items[1].UserID == 0 || items[1].InstanceID == "" || !items[1].AddedAt.Equal(dbtest.T0) {
		t.Fatalf("second = %+v", items[1])
	}
	items, total, err = e.cat.ListInstalls(e.ctx, w.ID, paging.Page{Number: 2, Size: 2})
	if err != nil || total != 3 || len(items) != 1 || items[0].DeploymentID != d3 || items[0].DeploymentState != "removed" {
		t.Fatalf("page 2 = %+v %d %v", items, total, err)
	}
	// 越界页：空列表、总数照给
	items, total, err = e.cat.ListInstalls(e.ctx, w.ID, paging.Page{Number: 9, Size: 2})
	if err != nil || total != 3 || items == nil || len(items) != 0 {
		t.Fatalf("page 9 = %+v %d %v", items, total, err)
	}
	if _, _, err := e.cat.ListInstalls(e.ctx, 999999, paging.Page{}); errCode(err) != "workflow_not_found" {
		t.Fatalf("missing workflow = %v", err)
	}
}

func TestAdminCounts(t *testing.T) {
	e := newCatEnv(t)
	w := e.workflow("sdxl", StatusPublished)
	v := e.published(w.ID, "1.0")
	newDep := e.deployFixture()
	live, pending, removed, lost := newDep("running"), newDep("running"), newDep("removed"), newDep("lost")
	e.attach(live, v, "installed")
	e.attach(pending, v, "pending")
	e.attach(removed, v, "installed")
	e.attach(lost, v, "installed")
	now := e.clk.Now()
	var uid int64
	if err := e.pool.QueryRow(e.ctx, `SELECT user_id FROM app_deployments WHERE id = $1`, live).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	for i, at := range []time.Time{now.Add(-time.Minute), now.Add(-23 * time.Hour), now.Add(-25 * time.Hour)} {
		e.exec(`INSERT INTO comfy_workflow_runs (id, deployment_id, user_id, workflow_version_id, prompt_id, state, created_at)
			VALUES ($1, $2, $3, $4, 'p', 'done', $5)`, "run-00000000000"+string(rune('a'+i)), live, uid, v.ID, at)
	}
	installs, runs, err := e.cat.AdminCounts(e.ctx, now)
	if err != nil || installs != 1 || runs != 2 {
		t.Fatalf("counts = %d %d %v", installs, runs, err)
	}
}

func TestUpdateWorkflowEmptyStatusKeeps(t *testing.T) {
	e := newCatEnv(t)
	w := e.workflow("sdxl", StatusPublished)
	u, err := e.cat.UpdateWorkflow(e.ctx, e.actor(), w.ID, WorkflowInput{Name: "新名字", Category: CategoryTxt2Img})
	if err != nil || u.Status != StatusPublished || u.Name != "新名字" {
		t.Fatalf("update without status = %+v %v", u, err)
	}
	u, err = e.cat.UpdateWorkflow(e.ctx, e.actor(), w.ID, WorkflowInput{Name: "x", Category: CategoryTxt2Img, Status: StatusRetired})
	if err != nil || u.Status != StatusRetired {
		t.Fatalf("update to retired = %+v %v", u, err)
	}
	if _, err := e.cat.UpdateWorkflow(e.ctx, e.actor(), w.ID, WorkflowInput{Name: "x", Category: CategoryTxt2Img, Status: "hidden"}); errCode(err) != "bad_request" {
		t.Fatalf("bad status = %v", err)
	}
	// 新建时空状态仍是 draft
	if d := e.workflow("other", ""); d.Status != StatusDraft {
		t.Fatalf("create without status = %+v", d)
	}
}

func TestCheckPublishable(t *testing.T) {
	e := newCatEnv(t)
	w := e.workflow("sdxl", StatusPublished)
	v := e.version(w.ID, "1.0")
	if err := e.cat.CheckPublishable(e.ctx, w.ID, v.ID); err != nil {
		t.Fatalf("good draft: %v", err)
	}
	// 库里的内容不再能通过校验（例如规则变严 / 手工改库）→ workflow_version_invalid，不改任何东西
	e.exec(`UPDATE comfy_workflow_versions SET manifest = jsonb_set(manifest, '{models}', '[]') WHERE id = $1`, v.ID)
	wantCode(t, e.cat.CheckPublishable(e.ctx, w.ID, v.ID), "workflow_version_invalid")
	if n := e.count(`SELECT count(*) FROM comfy_workflow_versions WHERE id = $1 AND status = 'draft'`, v.ID); n != 1 {
		t.Fatal("CheckPublishable changed the version")
	}
	p := e.published(w.ID, "1.1")
	wantCode(t, e.cat.CheckPublishable(e.ctx, w.ID, p.ID), "invalid_state")
	wantCode(t, e.cat.CheckPublishable(e.ctx, w.ID, 999999), "not_found")
	other := e.workflow("other", "")
	wantCode(t, e.cat.CheckPublishable(e.ctx, other.ID, p.ID), "not_found")
}
