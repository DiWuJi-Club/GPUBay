package appdeploy_test

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/rental"
)

// hookTx 在持实例行锁（FOR UPDATE）的事务里执行 fn，与 rental 调用钩子的上下文一致。
func (e *env) hookTx(instanceID string, fn func(tx pgx.Tx) error) {
	e.t.Helper()
	if err := db.WithTx(e.ctx, e.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(e.ctx, `SELECT 1 FROM instances WHERE id = $1 FOR UPDATE`, instanceID); err != nil {
			return err
		}
		return fn(tx)
	}); err != nil {
		e.t.Fatalf("hook tx: %v", err)
	}
}

// setDep 直接改部署的状态 / 期望（模拟 Runner 推进过的行），并弄脏运行时字段以便断言被清空。
func (e *env) setDep(id, state, desired string) appdeploy.Deployment {
	e.t.Helper()
	e.exec(`UPDATE app_deployments SET state = $2, desired = $3, phase = 'download', progress = 40, message = 'x', health = 'ok',
		unreachable_count = 2, crash_count = 1, attempt = 1, next_poll_at = $4 WHERE id = $1`, id, state, desired, e.clk.Now().Add(time.Hour))
	if state == appdeploy.StateFailed {
		e.exec(`UPDATE app_deployments SET error_code = 'script_failed' WHERE id = $1`, id)
	}
	return e.dep(id)
}

func (e *env) lastEvent(depID string) appdeploy.Event {
	e.t.Helper()
	evs, err := e.svc.Events(e.ctx, depID, 1)
	if err != nil || len(evs) == 0 {
		e.t.Fatalf("events %s: %v", depID, err)
	}
	return evs[0]
}

var model4B = map[string]string{"MODEL_ID": "Qwen/Qwen3-4B"}

func TestOnInstanceRunningRequeues(t *testing.T) {
	e := newEnv(t, 2)
	a, _ := e.publishedApp("vllm", specPorts(8000), goodScript, []int64{e.imageID})
	b, _ := e.publishedApp("webui", specPorts(8080), goodScript, []int64{e.imageID})
	c, _ := e.publishedApp("jupyter", specPorts(8888), goodScript, []int64{e.imageID})
	d4, _ := e.publishedApp("comfyui", specPorts(8188), goodScript, []int64{e.imageID})

	// 实例 A：running / installing（P1）/ stopped，desired 都是 running
	instA := e.runningInstance()
	running := e.setDep(e.create(instA, a.ID, model4B).ID, appdeploy.StateRunning, appdeploy.DesiredRunning)
	installing := e.setDep(e.create(instA, b.ID, model4B).ID, appdeploy.StateInstalling, appdeploy.DesiredRunning)
	stopped := e.setDep(e.create(instA, c.ID, model4B).ID, appdeploy.StateStopped, appdeploy.DesiredRunning)

	// 实例 B：停机时卸载的（removed + cleanup_pending）、用户停止的、failed、queued
	instB := e.runningInstance()
	cleanup := e.create(instB, d4.ID, model4B)
	e.exec(`UPDATE app_deployments SET state = 'removed', desired = 'removed', cleanup_pending = true, next_poll_at = $2 WHERE id = $1`,
		cleanup.ID, e.clk.Now().Add(time.Hour))
	cleanup = e.dep(cleanup.ID)
	userStopped := e.setDep(e.create(instB, a.ID, model4B).ID, appdeploy.StateStopped, appdeploy.DesiredStopped)
	failed := e.setDep(e.create(instB, b.ID, model4B).ID, appdeploy.StateFailed, appdeploy.DesiredRunning)
	queued := e.create(instB, c.ID, model4B)
	e.exec(`UPDATE app_deployments SET next_poll_at = $2 WHERE id = $1`, queued.ID, e.clk.Now().Add(time.Hour)) // 例如 Runner 推后的等待
	queued = e.dep(queued.ID)

	e.clk.Advance(time.Minute)
	now := e.clk.Now()
	for _, id := range []string{instA.ID, instB.ID} {
		e.hookTx(id, func(tx pgx.Tx) error { return e.svc.OnInstanceRunning(e.ctx, tx, id, now) })
	}

	for _, before := range []appdeploy.Deployment{running, installing, stopped} {
		d := e.dep(before.ID)
		if d.State != appdeploy.StateQueued || d.Desired != appdeploy.DesiredRunning || d.Phase != "" || d.Progress != 0 || d.Message != "" ||
			d.Health != appdeploy.HealthUnknown || d.ErrorCode != "" || d.UnreachableCount != 0 || d.CrashCount != 0 ||
			!d.NextPollAt.Equal(now) || d.RowVersion != before.RowVersion+1 || d.Attempt != before.Attempt {
			t.Fatalf("%s (was %s) after running hook = %+v", d.ID, before.State, d)
		}
		if ev := e.lastEvent(d.ID); ev.FromState != before.State || ev.ToState != appdeploy.StateQueued || ev.Detail != "实例已启动，正在恢复应用" {
			t.Fatalf("event for %s = %+v", before.State, ev)
		}
	}
	for _, before := range []appdeploy.Deployment{userStopped, failed} {
		if d := e.dep(before.ID); d.State != before.State || d.RowVersion != before.RowVersion || !d.NextPollAt.Equal(before.NextPollAt) {
			t.Fatalf("%s must stay untouched: %+v", before.State, d)
		}
	}
	if d := e.dep(cleanup.ID); !d.CleanupPending || d.State != appdeploy.StateRemoved || !d.NextPollAt.Equal(now) || d.RowVersion != cleanup.RowVersion+1 {
		t.Fatalf("cleanup row = %+v", d)
	}
	if d := e.dep(queued.ID); d.State != appdeploy.StateQueued || !d.NextPollAt.Equal(now) || d.RowVersion != queued.RowVersion+1 {
		t.Fatalf("queued row = %+v", d)
	}
	if n := e.count(`SELECT count(*) FROM app_deploy_events WHERE deployment_id = ANY($1)`, []string{queued.ID, userStopped.ID, failed.ID, cleanup.ID}); n != 4 {
		t.Fatalf("untouched rows got events: %d (want only the 4 create events)", n)
	}
}

func TestOnInstanceGoneRemovesAndKeepsTerminal(t *testing.T) {
	e := newEnv(t, 1)
	a, _ := e.publishedApp("vllm", specPorts(8000), goodScript, []int64{e.imageID})
	b, _ := e.publishedApp("webui", specPorts(8080), goodScript, []int64{e.imageID})
	c, _ := e.publishedApp("jupyter", specPorts(8888), goodScript, []int64{e.imageID})
	d4, _ := e.publishedApp("comfyui", specPorts(8188), goodScript, []int64{e.imageID})
	inst := e.runningInstance()

	lost := e.create(inst, c.ID, model4B)
	e.exec(`UPDATE app_deployments SET state = 'lost' WHERE id = $1`, lost.ID)
	lost = e.dep(lost.ID)
	cleanup := e.create(inst, d4.ID, model4B)
	e.exec(`UPDATE app_deployments SET state = 'removed', desired = 'removed', cleanup_pending = true WHERE id = $1`, cleanup.ID)
	cleanup = e.dep(cleanup.ID)
	running := e.setDep(e.create(inst, a.ID, model4B).ID, appdeploy.StateRunning, appdeploy.DesiredRunning)
	e.addAppPort(running, 8000)
	queued := e.create(inst, b.ID, model4B)
	e.addUserPort(inst.ID, 22000)

	e.clk.Advance(time.Minute)
	now := e.clk.Now()
	e.hookTx(inst.ID, func(tx pgx.Tx) error { return e.svc.OnInstanceGone(e.ctx, tx, inst.ID, "实例已释放", now) })

	for _, before := range []appdeploy.Deployment{running, queued} {
		d := e.dep(before.ID)
		if d.State != appdeploy.StateRemoved || d.Desired != appdeploy.DesiredRemoved || d.ErrorCode != appdeploy.ErrCodeInstanceGone ||
			d.FinishedAt == nil || !d.FinishedAt.Equal(now) || d.CleanupPending || d.RowVersion != before.RowVersion+1 {
			t.Fatalf("%s after gone = %+v", before.State, d)
		}
		if ev := e.lastEvent(d.ID); ev.FromState != before.State || ev.ToState != appdeploy.StateRemoved || ev.Detail != "实例已释放" {
			t.Fatalf("event = %+v", ev)
		}
	}
	if n := e.count(`SELECT count(*) FROM instance_ports WHERE deployment_id IS NOT NULL`); n != 0 {
		t.Fatalf("app ports left: %d", n)
	}
	if n := e.count(`SELECT count(*) FROM instance_ports WHERE instance_id = $1 AND port = 22000`, inst.ID); n != 1 {
		t.Fatal("user port must stay")
	}
	if d := e.dep(lost.ID); d.State != appdeploy.StateLost || d.RowVersion != lost.RowVersion {
		t.Fatalf("lost changed: %+v", d)
	}
	// 实例已消失：停机时卸载留下的目录随实例一起没了，不再等 Runner 清理
	if d := e.dep(cleanup.ID); d.CleanupPending || d.State != appdeploy.StateRemoved || d.RowVersion != cleanup.RowVersion+1 {
		t.Fatalf("cleanup row = %+v", d)
	}
	// 再调一次：没有非终态部署，什么都不做
	e.hookTx(inst.ID, func(tx pgx.Tx) error { return e.svc.OnInstanceGone(e.ctx, tx, inst.ID, "实例已释放", now) })
	if d := e.dep(running.ID); d.RowVersion != running.RowVersion+1 {
		t.Fatalf("second call touched removed row: %+v", d)
	}
}

func TestOnInstanceMigratedCopies(t *testing.T) {
	e := newEnv(t, 2)
	vllm, ver := e.publishedApp("vllm", goodSpec(), goodScript, []int64{e.imageID})
	webui, _ := e.publishedApp("webui", specPorts(8080), goodScript, []int64{e.imageID})
	comfy, _ := e.publishedApp("comfyui", specPorts(8188), goodScript, []int64{e.imageID})
	jupyter, _ := e.publishedApp("jupyter", specPorts(8888), goodScript, []int64{e.imageID})
	old := e.runningInstance()

	removed := e.create(old, jupyter.ID, model4B)
	e.exec(`UPDATE app_deployments SET state = 'removed', desired = 'removed' WHERE id = $1`, removed.ID)
	removed = e.dep(removed.ID)
	run := e.setDep(e.create(old, vllm.ID, map[string]string{"MODEL_ID": "Qwen/Qwen3-4B", "API_KEY": "sk-secret-12345", "NOTE": "hi"}).ID,
		appdeploy.StateRunning, appdeploy.DesiredRunning)
	e.addAppPort(run, 8000)
	failed := e.setDep(e.create(old, webui.ID, model4B).ID, appdeploy.StateFailed, appdeploy.DesiredRunning)
	e.addAppPort(failed, 8080)
	removing := e.setDep(e.create(old, comfy.ID, model4B).ID, appdeploy.StateRemoving, appdeploy.DesiredRemoved)
	e.addUserPort(old.ID, 22000)

	// 新实例（creating），端口按 AdminMigrate 的方式复制（deployment_id NULL）
	moved := e.instanceOf(e.userID, rental.StateCreating)
	e.exec(`INSERT INTO instance_ports (instance_id, port, visibility, forced_private, note, created_at, updated_at)
		SELECT $2, port, visibility, forced_private, note, $3, $3 FROM instance_ports WHERE instance_id = $1`, old.ID, moved.ID, e.clk.Now())

	e.clk.Advance(time.Minute)
	now := e.clk.Now()
	e.hookTx(old.ID, func(tx pgx.Tx) error { return e.svc.OnInstanceMigrated(e.ctx, tx, old.ID, moved.ID, now) })

	for _, before := range []appdeploy.Deployment{run, failed, removing} {
		d := e.dep(before.ID)
		if d.State != appdeploy.StateLost || d.FinishedAt == nil || !d.FinishedAt.Equal(now) || d.RowVersion != before.RowVersion+1 {
			t.Fatalf("old %s after migrate = %+v", before.State, d)
		}
		if ev := e.lastEvent(d.ID); ev.FromState != before.State || ev.ToState != appdeploy.StateLost || ev.Detail != "实例已更换机器" {
			t.Fatalf("event = %+v", ev)
		}
	}
	if d := e.dep(removed.ID); d.State != appdeploy.StateRemoved || d.RowVersion != removed.RowVersion {
		t.Fatalf("removed changed: %+v", d)
	}
	// P7：lost 的部署不再被 instance_ports 引用；旧实例的端口行本身保留
	if n := e.count(`SELECT count(*) FROM instance_ports WHERE deployment_id IS NOT NULL`); n != 0 {
		t.Fatalf("ports still reference old deployments: %d", n)
	}
	if n := e.count(`SELECT count(*) FROM instance_ports WHERE instance_id = $1`, old.ID); n != 3 {
		t.Fatalf("old instance ports = %d", n)
	}
	// 不迁移的 failed webui 在旧实例上占的 8080：新实例上复制来的那行删掉（不留无主孤儿），审计一条；8000 与用户端口保留
	if n := e.count(`SELECT count(*) FROM instance_ports WHERE instance_id = $1 AND port = ANY($2)`, moved.ID, []int{8000, 22000}); n != 2 ||
		e.count(`SELECT count(*) FROM instance_ports WHERE instance_id = $1`, moved.ID) != 2 {
		t.Fatalf("new instance ports after migrate = %d", e.count(`SELECT count(*) FROM instance_ports WHERE instance_id = $1`, moved.ID))
	}
	if logs := e.auditLogs("instance.port_delete", moved.ID); len(logs) != 1 {
		t.Fatalf("port_delete audit on new instance = %+v", logs)
	}

	news, err := e.svc.ListForInstance(e.ctx, moved.ID)
	if err != nil || len(news) != 1 {
		t.Fatalf("new deployments = %+v, %v", news, err)
	}
	nd := news[0]
	if nd.ID == run.ID || nd.AppID != vllm.ID || nd.AppVersionID != ver.ID || nd.State != appdeploy.StateQueued || nd.Desired != appdeploy.DesiredRunning ||
		nd.Attempt != 0 || nd.UserID != run.UserID || nd.Port != run.Port || nd.SpecHash != run.SpecHash || !maps.Equal(nd.Env, run.Env) ||
		!bytes.Equal(nd.SecretEnvEnc, run.SecretEnvEnc) || !nd.NextPollAt.Equal(now) || nd.Phase != "" || nd.Health != appdeploy.HealthUnknown {
		t.Fatalf("copied deployment = %+v (from %+v)", nd, run)
	}
	if e.secrets(nd)["API_KEY"] != "sk-secret-12345" {
		t.Fatal("secret not carried over")
	}
	if ev := e.lastEvent(nd.ID); ev.FromState != "" || ev.ToState != appdeploy.StateQueued || ev.Detail != "从 "+old.ID+" 迁移，模型需重新下载" {
		t.Fatalf("new event = %+v", ev)
	}
	// 新部署 running 时认领复制来的端口行（不重复插入）
	if err := db.WithTx(e.ctx, e.pool, func(tx pgx.Tx) error {
		code, err := appdeploy.RegisterPorts(e.ctx, tx, nd, goodSpec().Ports, "vllm", now)
		if err == nil && code != "" {
			err = fmt.Errorf("errCode %s", code)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*) FROM instance_ports WHERE instance_id = $1 AND port = 8000 AND deployment_id = $2`, moved.ID, nd.ID); n != 1 {
		t.Fatal("copied port row not claimed")
	}
	if n := e.count(`SELECT count(*) FROM instance_ports WHERE instance_id = $1`, moved.ID); n != 2 {
		t.Fatalf("new instance ports = %d", n)
	}
	// 在新实例上重新部署没迁移的 failed 应用：端口不冲突
	if _, err := e.svc.Create(e.ctx, appdeploy.CreateRequest{UserID: e.userID, InstanceID: moved.ID, AppID: webui.ID, Env: model4B}); err != nil {
		t.Fatalf("re-deploy webui on new instance: %v", err)
	}
}

// TestOnInstanceMigratedKeepsStopped 裁定：用户停止的部署（desired = stopped）换机后仍是 stopped，不自动启动；「启动」走 Start → queued。
func TestOnInstanceMigratedKeepsStopped(t *testing.T) {
	e := newEnv(t, 2)
	vllm, _ := e.publishedApp("vllm", goodSpec(), goodScript, []int64{e.imageID})
	webui, _ := e.publishedApp("webui", specPorts(8080), goodScript, []int64{e.imageID})
	old := e.runningInstance()
	stopped := e.setDep(e.create(old, vllm.ID, map[string]string{"MODEL_ID": "Qwen/Qwen3-4B", "API_KEY": "sk-secret-12345"}).ID,
		appdeploy.StateStopped, appdeploy.DesiredStopped)
	e.addAppPort(stopped, 8000)
	stopping := e.setDep(e.create(old, webui.ID, model4B).ID, appdeploy.StateRunning, appdeploy.DesiredStopped) // 已请求停止、Runner 还没执行
	e.addAppPort(stopping, 8080)
	moved := e.instanceOf(e.userID, rental.StateCreating)
	e.exec(`INSERT INTO instance_ports (instance_id, port, visibility, forced_private, note, created_at, updated_at)
		SELECT $2, port, visibility, forced_private, note, $3, $3 FROM instance_ports WHERE instance_id = $1`, old.ID, moved.ID, e.clk.Now())

	e.clk.Advance(time.Minute)
	now := e.clk.Now()
	e.hookTx(old.ID, func(tx pgx.Tx) error { return e.svc.OnInstanceMigrated(e.ctx, tx, old.ID, moved.ID, now) })

	news, err := e.svc.ListForInstance(e.ctx, moved.ID)
	if err != nil || len(news) != 2 {
		t.Fatalf("new deployments = %+v, %v", news, err)
	}
	for _, nd := range news {
		if nd.State != appdeploy.StateStopped || nd.Desired != appdeploy.DesiredStopped || nd.Attempt != 0 {
			t.Fatalf("copied stopped deployment = %+v", nd)
		}
		if ev := e.lastEvent(nd.ID); ev.FromState != "" || ev.ToState != appdeploy.StateStopped || ev.Detail != "从 "+old.ID+" 迁移（已停止，启动时重新下载模型）" {
			t.Fatalf("event = %+v", ev)
		}
	}
	if e.secrets(news[0])["API_KEY"] != "sk-secret-12345" && e.secrets(news[1])["API_KEY"] != "sk-secret-12345" {
		t.Fatal("secret not carried over")
	}
	// 端口行随迁移的部署保留（等启动后认领）
	if n := e.count(`SELECT count(*) FROM instance_ports WHERE instance_id = $1 AND deployment_id IS NULL`, moved.ID); n != 2 {
		t.Fatalf("new instance ports = %d", n)
	}
	// 新实例开通（running 钩子）不启动它们
	e.exec(`UPDATE instances SET state = 'running' WHERE id = $1`, moved.ID)
	e.hookTx(moved.ID, func(tx pgx.Tx) error { return e.svc.OnInstanceRunning(e.ctx, tx, moved.ID, now) })
	for _, nd := range news {
		if d := e.dep(nd.ID); d.State != appdeploy.StateStopped || d.Desired != appdeploy.DesiredStopped {
			t.Fatalf("stopped copy auto-started: %+v", d)
		}
	}
	// 用户「启动」→ queued / desired running（完整部署）
	d, err := e.svc.Start(e.ctx, e.userID, moved.ID, news[0].ID, "")
	if err != nil || d.State != appdeploy.StateQueued || d.Desired != appdeploy.DesiredRunning {
		t.Fatalf("Start = %+v, %v", d, err)
	}
}

func TestPrepareAndQueueInstall(t *testing.T) {
	e := newEnv(t, 1)
	draft := e.app("draft", "")
	e.version(draft.ID, "1.0.0")
	app, ver := e.publishedApp("vllm", goodSpec(), goodScript, []int64{e.imageID})
	other, _ := e.publishedApp("webui", specPorts(8080), goodScript, []int64{e.otherImage()})

	prep := func(appID, imageID int64, env map[string]string) (rental.InstallPlan, error) {
		return e.svc.PrepareInstall(e.ctx, e.pool, appID, imageID, env, nil)
	}
	for _, id := range []int64{draft.ID, 999999} {
		if _, err := prep(id, e.imageID, nil); !errors.Is(err, apperr.ErrAppNotFound) {
			t.Fatalf("app %d: %v", id, err)
		}
	}
	if _, err := prep(other.ID, e.imageID, nil); apperr.As(err) == nil || apperr.As(err).Code != "app_image_mismatch" || apperr.As(err).Status != 409 {
		t.Fatalf("image mismatch = %v", err)
	}
	for _, bad := range []map[string]string{{"MODEL_ID": "bogus"}, {"API_KEY": "short"}, {"UNKNOWN": "x"}, {"MAX_MODEL_LEN": "1"}} {
		ae := apperr.As(func() error { _, err := prep(app.ID, e.imageID, bad); return err }())
		if ae == nil || ae.Status != 400 || (ae.Code != "app_env_invalid" && ae.Code != "app_env_required") {
			t.Fatalf("env %v = %v", bad, ae)
		}
	}
	plan, err := prep(app.ID, e.imageID, map[string]string{"MODEL_ID": "Qwen/Qwen3-4B", "API_KEY": "sk-secret-12345"})
	if err != nil {
		t.Fatal(err)
	}

	inst := e.reservedInstance()
	// 同一应用一条停机时卸载、等清理的旧行：新部署会用同一目录，取消它的清理（与 Create 一致）
	stale := e.create(inst, app.ID, model4B)
	e.exec(`UPDATE app_deployments SET state = 'removed', desired = 'removed', cleanup_pending = true WHERE id = $1`, stale.ID)
	stale = e.dep(stale.ID)

	e.clk.Advance(time.Minute)
	now := e.clk.Now()
	e.hookTx(inst.ID, func(tx pgx.Tx) error { return e.svc.QueueInstall(e.ctx, tx, inst, plan, now) })
	live, err := e.svc.ListLive(e.ctx, inst.ID)
	if err != nil || len(live) != 1 {
		t.Fatalf("live = %+v, %v", live, err)
	}
	d := live[0]
	if d.State != appdeploy.StateQueued || d.UserID != inst.UserID || d.AppID != app.ID || d.AppVersionID != ver.ID || d.Attempt != 0 ||
		d.Env["MODEL_ID"] != "Qwen/Qwen3-4B" || d.Env["API_KEY"] != "" || e.secrets(d)["API_KEY"] != "sk-secret-12345" || !d.NextPollAt.Equal(now) {
		t.Fatalf("queued = %+v", d)
	}
	if ev := e.lastEvent(d.ID); ev.ToState != appdeploy.StateQueued || ev.Detail != "预装：vllm 1.0.0，实例开通后自动部署" {
		t.Fatalf("event = %+v", ev)
	}
	if logs := e.auditLogs("instance.app_deploy", inst.ID); len(logs) != 2 {
		t.Fatalf("audit = %+v", logs)
	}
	if s := e.dep(stale.ID); s.CleanupPending || s.RowVersion != stale.RowVersion+1 {
		t.Fatalf("stale cleanup not cancelled: %+v", s)
	}
	// 不是 PrepareInstall 的计划 → 错误（编程错误，不静默跳过）
	if err := db.WithTx(e.ctx, e.pool, func(tx pgx.Tx) error { return e.svc.QueueInstall(e.ctx, tx, inst, "bogus", now) }); err == nil {
		t.Fatal("foreign plan accepted")
	}
}

// TestPlaceWithAppEndToEnd 用真实钩子走 rental：下单预装 → 开通 → 释放；预装校验失败在扣款前以 4xx 拒绝。
func TestPlaceWithAppEndToEnd(t *testing.T) {
	e := newEnv(t, 3)
	e.rental.SetAppHooks(e.svc)
	app, _ := e.publishedApp("vllm", goodSpec(), goodScript, []int64{e.imageID})
	mismatch, _ := e.publishedApp("webui", specPorts(8080), goodScript, []int64{e.otherImage()})
	balance := func() int64 {
		var b int64
		if err := e.pool.QueryRow(e.ctx, `SELECT balance FROM wallet_accounts WHERE user_id = $1`, e.userID).Scan(&b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	req := func(appID int64, env map[string]string) rental.BatchRequest {
		return rental.BatchRequest{UserID: e.userID, ProductID: e.productID, ImageID: e.imageID, Count: 2, Duration: catalog.D1d, AppID: &appID, AppEnv: env}
	}

	before, insts := balance(), e.count(`SELECT count(*) FROM instances`)
	for _, c := range []struct {
		appID  int64
		env    map[string]string
		code   string
		status int
	}{
		{999999, nil, "app_not_found", 404},
		{mismatch.ID, nil, "app_image_mismatch", 409},
		{app.ID, map[string]string{"MODEL_ID": "bogus"}, "app_env_invalid", 400},
		{app.ID, map[string]string{"API_KEY": "short"}, "app_env_invalid", 400},
	} {
		_, err := e.rental.PlaceBatch(e.ctx, req(c.appID, c.env))
		if ae := apperr.As(err); ae == nil || ae.Code != c.code || ae.Status != c.status {
			t.Fatalf("app %d env %v = %v, want %s", c.appID, c.env, err, c.code)
		}
	}
	if balance() != before || e.count(`SELECT count(*) FROM instances`) != insts || e.count(`SELECT count(*) FROM app_deployments`) != 0 {
		t.Fatal("rejected preinstall order left side effects")
	}

	res, err := e.rental.PlaceBatch(e.ctx, req(app.ID, map[string]string{"MODEL_ID": "Qwen/Qwen3-4B", "API_KEY": "sk-secret-12345"}))
	if err != nil || len(res.Instances) != 2 {
		t.Fatalf("PlaceBatch = %+v, %v", res, err)
	}
	if balance() >= before {
		t.Fatal("order not charged")
	}
	for _, inst := range res.Instances {
		live, err := e.svc.ListLive(e.ctx, inst.ID)
		if err != nil || len(live) != 1 || live[0].State != appdeploy.StateQueued || e.secrets(live[0])["API_KEY"] != "sk-secret-12345" {
			t.Fatalf("instance %s deployments = %+v, %v", inst.ID, live, err)
		}
	}

	// 开通成功 → running 钩子（queued 不变，next_poll_at = now）；释放 → removed(instance_gone)
	inst := res.Instances[0]
	e.clk.Advance(time.Minute)
	cmds, err := e.fleet.PendingCommands(e.ctx, inst.MachineID)
	if err != nil {
		t.Fatal(err)
	}
	created := 0
	for _, c := range cmds {
		if c.InstanceID == inst.ID && c.Type == fleet.CmdCreateInstance {
			if err := e.rental.HandleCommandResult(e.ctx, c.ID, true, ""); err != nil {
				t.Fatal(err)
			}
			created++
		}
	}
	if created != 1 {
		t.Fatalf("create commands = %+v", cmds)
	}
	live, _ := e.svc.ListLive(e.ctx, inst.ID)
	if len(live) != 1 || live[0].State != appdeploy.StateQueued || live[0].NextPollAt.After(e.clk.Now()) {
		t.Fatalf("after create = %+v", live)
	}
	if err := e.rental.Release(e.ctx, e.userID, inst.ID); err != nil {
		t.Fatal(err)
	}
	all, _ := e.svc.ListForInstance(e.ctx, inst.ID)
	if len(all) != 1 || all[0].State != appdeploy.StateRemoved || all[0].ErrorCode != appdeploy.ErrCodeInstanceGone {
		t.Fatalf("after release = %+v", all)
	}
}
