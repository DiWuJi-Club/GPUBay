package rental_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/rental"
)

type recHooks struct {
	mu    sync.Mutex
	calls []string
	plan  string
}

func (h *recHooks) rec(s string) { h.mu.Lock(); h.calls = append(h.calls, s); h.mu.Unlock() }
func (h *recHooks) OnInstanceRunning(_ context.Context, _ pgx.Tx, id string, _ time.Time) error {
	h.rec("running:" + id)
	return nil
}
func (h *recHooks) OnInstanceGone(_ context.Context, _ pgx.Tx, id, reason string, _ time.Time) error {
	h.rec("gone:" + id + ":" + reason)
	return nil
}
func (h *recHooks) OnInstanceMigrated(_ context.Context, _ pgx.Tx, oldID, newID string, _ time.Time) error {
	h.rec("migrated:" + oldID + "->" + newID)
	return nil
}
func (h *recHooks) PrepareInstall(_ context.Context, _ db.Querier, appID, imageID int64, env map[string]string, workflowIDs []int64) (rental.InstallPlan, error) {
	if h.plan == "fail" {
		return nil, fmt.Errorf("plan failed")
	}
	p := fmt.Sprintf("plan:%d:%d:%s", appID, imageID, env["MODEL_ID"])
	if len(workflowIDs) > 0 {
		p += fmt.Sprintf(":wf%v", workflowIDs)
	}
	return p, nil
}
func (h *recHooks) QueueInstall(_ context.Context, _ pgx.Tx, inst rental.Instance, plan rental.InstallPlan, _ time.Time) error {
	h.rec("queue:" + inst.ID + ":" + plan.(string))
	return nil
}

func (h *recHooks) snapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.calls...)
}

// ackFor 模拟 Agent 回报某实例的某类指令。
func (e *env) ackFor(instanceID string, typ fleet.CommandType, ok bool) {
	e.t.Helper()
	n := 0
	for _, mid := range e.machines {
		cmds, err := e.fleet.PendingCommands(e.ctx, mid)
		if err != nil {
			e.t.Fatal(err)
		}
		for _, c := range cmds {
			if c.InstanceID != instanceID || c.Type != typ {
				continue
			}
			if err := e.svc.HandleCommandResult(e.ctx, c.ID, ok, "test"); err != nil {
				e.t.Fatalf("HandleCommandResult(%s): %v", c.Type, err)
			}
			n++
		}
	}
	if n == 0 {
		e.t.Fatalf("no pending %s for %s", typ, instanceID)
	}
}

func (e *env) agentCreated(id string) { e.t.Helper(); e.ackFor(id, fleet.CmdCreateInstance, true) }
func (e *env) agentCreateFailed(id string) {
	e.t.Helper()
	e.ackFor(id, fleet.CmdCreateInstance, false)
}
func (e *env) agentStarted(id string) { e.t.Helper(); e.ackFor(id, fleet.CmdStartInstance, true) }

// sweep 到期停机并模拟 Agent 完成所有停机指令。
func (e *env) sweep() {
	e.t.Helper()
	if _, err := e.svc.ExpireDue(e.ctx); err != nil {
		e.t.Fatal(err)
	}
	for _, mid := range e.machines {
		cmds, err := e.fleet.PendingCommands(e.ctx, mid)
		if err != nil {
			e.t.Fatal(err)
		}
		for _, c := range cmds {
			if c.Type == fleet.CmdStopInstance {
				if err := e.svc.HandleCommandResult(e.ctx, c.ID, true, "test"); err != nil {
					e.t.Fatal(err)
				}
			}
		}
	}
}

func (e *env) renew(id string, d catalog.Duration) {
	e.t.Helper()
	if _, err := e.svc.Renew(e.ctx, e.userID, id, d, ""); err != nil {
		e.t.Fatalf("Renew: %v", err)
	}
}

func (e *env) release(id string) {
	e.t.Helper()
	if err := e.svc.Release(e.ctx, e.userID, id); err != nil {
		e.t.Fatalf("Release: %v", err)
	}
}

func (e *env) migrate(id string) rental.Instance {
	e.t.Helper()
	moved, err := e.svc.AdminMigrate(e.ctx, opsActor, id, "GPU 报错")
	if err != nil {
		e.t.Fatalf("AdminMigrate: %v", err)
	}
	return moved
}

func (e *env) countInstances() int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM instances WHERE user_id = $1`, e.userID).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestAppHooksCalledOnLifecycle(t *testing.T) {
	e := newEnv(t, 2)
	h := &recHooks{}
	e.svc.SetAppHooks(h)
	_, inst := e.place(catalog.D1d)
	e.agentCreated(inst.ID) // Create 成功 → startLease
	if got := h.snapshot(); len(got) != 1 || got[0] != "running:"+inst.ID {
		t.Fatalf("after create: %v", got)
	}
	// 到期停机再续费 Start 成功 → 再一次 running
	e.clock.Advance(25 * time.Hour)
	e.sweep()
	if got := h.snapshot(); len(got) != 1 {
		t.Fatalf("stop must not call hooks: %v", got)
	}
	e.renew(inst.ID, catalog.D1d)
	e.agentStarted(inst.ID)
	if got := h.snapshot(); len(got) != 2 || got[1] != "running:"+inst.ID {
		t.Fatalf("after renew start: %v", got)
	}
	// 释放 → beginWipe → gone；洗机完成（wiping → released）不再调
	e.release(inst.ID)
	if got := h.snapshot(); len(got) != 3 || got[2] != "gone:"+inst.ID+":实例已释放" {
		t.Fatalf("after release: %v", got)
	}
	e.ackFor(inst.ID, fleet.CmdWipeInstance, true)
	if got := h.snapshot(); len(got) != 3 {
		t.Fatalf("wiped must not call hooks again: %v", got)
	}
	// 开通失败 → gone（开通失败）
	_, inst2 := e.place(catalog.D1d)
	e.clock.Advance(time.Minute)
	e.agentCreateFailed(inst2.ID)
	if got := h.snapshot(); len(got) != 4 || got[3] != "gone:"+inst2.ID+":开通失败" {
		t.Fatalf("after create failure: %v", got)
	}
}

func TestAppHooksCreateTimeoutAndStaleWipe(t *testing.T) {
	e := newEnv(t, 2)
	h := &recHooks{}
	e.svc.SetAppHooks(h)
	_, inst := e.place(catalog.D1d)
	e.clock.Advance(rental.CreateTimeout + time.Minute)
	if _, err := e.svc.FailStuckCreating(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.snapshot(); len(got) != 1 || got[0] != "gone:"+inst.ID+":开通失败" {
		t.Fatalf("after create timeout: %v", got)
	}
	// 停机超过保留期 → WipeStale → beginWipe
	_, inst2 := e.place(catalog.D1d)
	e.agentCreated(inst2.ID)
	e.clock.Advance(25 * time.Hour)
	e.sweep()
	e.clock.Advance(rental.RetainAfterStop + time.Minute)
	if _, err := e.svc.WipeStale(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.snapshot(); got[len(got)-1] != "gone:"+inst2.ID+":实例已释放" {
		t.Fatalf("after stale wipe: %v", got)
	}
}

func TestAppHooksMigrateOrder(t *testing.T) {
	e := newEnv(t, 2)
	h := &recHooks{}
	e.svc.SetAppHooks(h)
	_, inst := e.place(catalog.D1d)
	e.agentCreated(inst.ID)
	moved := e.migrate(inst.ID)
	want := []string{"running:" + inst.ID, "migrated:" + inst.ID + "->" + moved.ID, "gone:" + inst.ID + ":实例已释放"}
	if got := h.snapshot(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	e.agentCreated(moved.ID)
	if got := h.snapshot(); got[len(got)-1] != "running:"+moved.ID {
		t.Fatalf("new instance running not hooked: %v", got)
	}
}

func TestPlaceBatchWithApp(t *testing.T) {
	e := newEnv(t, 3)
	h := &recHooks{}
	e.svc.SetAppHooks(h)
	appID := int64(7)
	res, err := e.svc.PlaceBatch(e.ctx, rental.BatchRequest{UserID: e.userID, ProductID: e.productID, ImageID: e.imageID, Count: 2, Duration: catalog.D1d,
		AppID: &appID, AppEnv: map[string]string{"MODEL_ID": "Qwen/Qwen3-4B"}, IdempotencyKey: "k1"})
	if err != nil || len(res.Instances) != 2 {
		t.Fatalf("batch = %+v %v", res, err)
	}
	queued := map[string]bool{}
	for _, c := range h.snapshot() {
		if rest, ok := strings.CutPrefix(c, "queue:"); ok {
			id, plan, _ := strings.Cut(rest, ":")
			if plan != fmt.Sprintf("plan:7:%d:Qwen/Qwen3-4B", e.imageID) {
				t.Fatalf("plan = %q", plan)
			}
			queued[id] = true
		}
	}
	if len(queued) != 2 || !queued[res.Instances[0].ID] || !queued[res.Instances[1].ID] {
		t.Fatalf("queue calls = %v", h.snapshot())
	}
	// 幂等回放不再调钩子
	n := len(h.snapshot())
	if _, err := e.svc.PlaceBatch(e.ctx, rental.BatchRequest{UserID: e.userID, ProductID: e.productID, ImageID: e.imageID, Count: 2, Duration: catalog.D1d,
		AppID: &appID, AppEnv: map[string]string{"MODEL_ID": "Qwen/Qwen3-4B"}, IdempotencyKey: "k1"}); err != nil || len(h.snapshot()) != n {
		t.Fatalf("replay = %v calls %d→%d", err, n, len(h.snapshot()))
	}
	// 不带 app_id 不调预装钩子
	if _, err := e.svc.PlaceBatch(e.ctx, rental.BatchRequest{UserID: e.userID, ProductID: e.productID, ImageID: e.imageID, Count: 1, Duration: catalog.D1d}); err != nil {
		t.Fatal(err)
	}
	if len(h.snapshot()) != n {
		t.Fatalf("order without app queued an install: %v", h.snapshot())
	}
	// 预装校验失败整单不下：没有实例、没有扣款
	h.plan = "fail"
	before := e.balance(e.userID)
	if _, err := e.svc.PlaceBatch(e.ctx, rental.BatchRequest{UserID: e.userID, ProductID: e.productID, ImageID: e.imageID, Count: 1, Duration: catalog.D1d, AppID: &appID}); err == nil {
		t.Fatal("failed plan accepted")
	}
	if e.balance(e.userID) != before || e.countInstances() != 3 {
		t.Fatalf("partial order: balance %d→%d instances %d", before, e.balance(e.userID), e.countInstances())
	}
	// 没有钩子时带 app_id → app_not_found
	e.svc.SetAppHooks(nil)
	if _, _, err := e.svc.Place(e.ctx, rental.PlaceRequest{UserID: e.userID, ProductID: e.productID, ImageID: e.imageID, Duration: catalog.D1d, AppID: &appID}); err == nil || err.Error() != "app_not_found: 应用不存在或未上架" {
		t.Fatalf("no hooks = %v", err)
	}
	if e.balance(e.userID) != before || e.countInstances() != 3 {
		t.Fatalf("no-hooks order charged: balance %d→%d instances %d", before, e.balance(e.userID), e.countInstances())
	}
}

func TestPlaceWithAppPassesThrough(t *testing.T) {
	e := newEnv(t, 1)
	h := &recHooks{}
	e.svc.SetAppHooks(h)
	appID := int64(9)
	_, inst, err := e.svc.Place(e.ctx, rental.PlaceRequest{UserID: e.userID, ProductID: e.productID, ImageID: e.imageID, Duration: catalog.D1d,
		AppID: &appID, AppEnv: map[string]string{"MODEL_ID": "m"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := h.snapshot(); len(got) != 1 || got[0] != fmt.Sprintf("queue:%s:plan:9:%d:m", inst.ID, e.imageID) {
		t.Fatalf("calls = %v", got)
	}
}

func TestBookingWithAppCancelAndProvision(t *testing.T) {
	e := newEnv(t, 2)
	h := &recHooks{}
	e.svc.SetAppHooks(h)
	appID := int64(3)
	book := func(start time.Time) rental.Instance {
		req := e.batchReq(1, &start)
		req.AppID = &appID
		res, err := e.svc.PlaceBatch(e.ctx, req)
		if err != nil {
			t.Fatalf("book: %v", err)
		}
		return res.Instances[0]
	}
	// 预订带预装：reserved 实例也写一条 queued 部署
	b1 := book(dbtest.T0.Add(3 * time.Hour))
	if got := h.snapshot(); len(got) != 1 || !strings.HasPrefix(got[0], "queue:"+b1.ID+":") {
		t.Fatalf("booking queue: %v", got)
	}
	// 取消预订 → gone（预订取消）
	if _, err := e.svc.Cancel(e.ctx, e.userID, b1.ID, ""); err != nil {
		t.Fatal(err)
	}
	if got := h.snapshot(); got[len(got)-1] != "gone:"+b1.ID+":预订取消" {
		t.Fatalf("after cancel: %v", got)
	}
	// 预订首次开通成功（startBooked）→ running
	b2 := book(dbtest.T0.Add(3 * time.Hour))
	e.clock.Advance(2 * time.Hour)
	e.heartbeatAll()
	if n, err := e.svc.ProvisionDue(e.ctx); err != nil || n != 1 {
		t.Fatalf("ProvisionDue = %d, %v", n, err)
	}
	e.agentCreated(b2.ID)
	if got := h.snapshot(); got[len(got)-1] != "running:"+b2.ID {
		t.Fatalf("after booked start: %v", got)
	}
}

func TestAppEnvWithoutAppRejected(t *testing.T) {
	e := newEnv(t, 1)
	h := &recHooks{}
	e.svc.SetAppHooks(h)
	before := e.balance(e.userID)
	env := map[string]string{"MODEL_ID": "m"}
	_, err := e.svc.PlaceBatch(e.ctx, rental.BatchRequest{UserID: e.userID, ProductID: e.productID, ImageID: e.imageID, Count: 1, Duration: catalog.D1d, AppEnv: env})
	if ae := apperr.As(err); ae == nil || ae.Code != "bad_request" || ae.Status != 400 {
		t.Fatalf("batch app_env without app_id = %v", err)
	}
	_, _, err = e.svc.Place(e.ctx, rental.PlaceRequest{UserID: e.userID, ProductID: e.productID, ImageID: e.imageID, Duration: catalog.D1d, AppEnv: env})
	if ae := apperr.As(err); ae == nil || ae.Code != "bad_request" {
		t.Fatalf("place app_env without app_id = %v", err)
	}
	if e.balance(e.userID) != before || e.countInstances() != 0 || len(h.snapshot()) != 0 {
		t.Fatalf("rejected order left side effects: balance %d→%d instances %d calls %v", before, e.balance(e.userID), e.countInstances(), h.snapshot())
	}
	// 空 map 等同于没填
	if _, _, err := e.svc.Place(e.ctx, rental.PlaceRequest{UserID: e.userID, ProductID: e.productID, ImageID: e.imageID, Duration: catalog.D1d, AppEnv: map[string]string{}}); err != nil {
		t.Fatalf("empty app_env = %v", err)
	}
}

// 机器失联暂停 / 恢复不是实例生命周期变化（容器没停，进程可能还在）：不调任何钩子。
func TestSuspendResumeCallNoHooks(t *testing.T) {
	e := newEnv(t, 1)
	h := &recHooks{}
	e.svc.SetAppHooks(h)
	_, inst := e.place(catalog.D1d)
	e.agentCreated(inst.ID)
	n := len(h.snapshot())
	if got, err := e.svc.SuspendMachines(e.ctx, []int64{inst.MachineID}); err != nil || got != 1 {
		t.Fatalf("SuspendMachines = %d, %v", got, err)
	}
	if got := e.get(inst.ID); !got.Suspended() {
		t.Fatalf("not suspended: %+v", got)
	}
	e.clock.Advance(time.Hour)
	if got, err := e.svc.ResumeMachine(e.ctx, inst.MachineID, []string{inst.ID}); err != nil || got != 1 {
		t.Fatalf("ResumeMachine = %d, %v", got, err)
	}
	if got := e.get(inst.ID); got.Suspended() || got.State != rental.StateRunning {
		t.Fatalf("not resumed: %+v", got)
	}
	if got := h.snapshot(); len(got) != n {
		t.Fatalf("suspend / resume called hooks: %v", got[n:])
	}
}

// 计划 11：预装工作流 ID 原样交给 PrepareInstall（Place 与 PlaceBatch 都是）；没有 app_id 时整单 400、无副作用。
func TestPlaceWithAppWorkflows(t *testing.T) {
	e := newEnv(t, 2)
	h := &recHooks{}
	e.svc.SetAppHooks(h)
	appID := int64(5)
	_, inst, err := e.svc.Place(e.ctx, rental.PlaceRequest{UserID: e.userID, ProductID: e.productID, ImageID: e.imageID, Duration: catalog.D1d,
		AppID: &appID, AppEnv: map[string]string{"MODEL_ID": "none"}, AppWorkflowIDs: []int64{3, 1}})
	if err != nil {
		t.Fatal(err)
	}
	if got := h.snapshot(); len(got) != 1 || got[0] != fmt.Sprintf("queue:%s:plan:5:%d:none:wf[3 1]", inst.ID, e.imageID) {
		t.Fatalf("place calls = %v", got)
	}
	res, err := e.svc.PlaceBatch(e.ctx, rental.BatchRequest{UserID: e.userID, ProductID: e.productID, ImageID: e.imageID, Count: 1, Duration: catalog.D1d,
		AppID: &appID, AppWorkflowIDs: []int64{2}})
	if err != nil {
		t.Fatal(err)
	}
	if got := h.snapshot(); len(got) != 2 || got[1] != fmt.Sprintf("queue:%s:plan:5:%d::wf[2]", res.Instances[0].ID, e.imageID) {
		t.Fatalf("batch calls = %v", got)
	}

	before, n := e.balance(e.userID), e.countInstances()
	_, err = e.svc.PlaceBatch(e.ctx, rental.BatchRequest{UserID: e.userID, ProductID: e.productID, ImageID: e.imageID, Count: 1, Duration: catalog.D1d,
		AppWorkflowIDs: []int64{1}})
	if ae := apperr.As(err); ae == nil || ae.Code != "bad_request" || ae.Status != 400 || ae.Message != "预装工作流需要同时选择 ComfyUI 应用" {
		t.Fatalf("batch workflows without app_id = %v", err)
	}
	_, _, err = e.svc.Place(e.ctx, rental.PlaceRequest{UserID: e.userID, ProductID: e.productID, ImageID: e.imageID, Duration: catalog.D1d,
		AppWorkflowIDs: []int64{1}})
	if ae := apperr.As(err); ae == nil || ae.Code != "bad_request" {
		t.Fatalf("place workflows without app_id = %v", err)
	}
	if e.balance(e.userID) != before || e.countInstances() != n || len(h.snapshot()) != 2 {
		t.Fatalf("rejected order left side effects: balance %d→%d instances %d→%d calls %v", before, e.balance(e.userID), n, e.countInstances(), h.snapshot())
	}
}
