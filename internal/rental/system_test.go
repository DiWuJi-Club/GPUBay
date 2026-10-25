package rental_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/access/upstream"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/rental"
)

// poolGroupID 迁移建好的 Token 服务池（purpose='llm'）。
func (e *env) poolGroupID() int64 {
	e.t.Helper()
	var id int64
	if err := e.pool.QueryRow(e.ctx, `SELECT id FROM machine_groups WHERE purpose = 'llm'`).Scan(&id); err != nil {
		e.t.Fatalf("pool group: %v", err)
	}
	return id
}

// toPool 把机器加入池机组（走 fleet.AssignMachines，与后台一致）。
func (e *env) toPool(ids ...int64) int64 {
	e.t.Helper()
	pool := e.poolGroupID()
	if err := e.fleet.AssignMachines(e.ctx, audit.Actor{AdminID: 1}, pool, ids); err != nil {
		e.t.Fatalf("AssignMachines: %v", err)
	}
	return pool
}

func (e *env) placeSystem(machineID, groupID int64) rental.Instance {
	e.t.Helper()
	inst, err := e.svc.PlaceSystem(e.ctx, rental.SystemPlaceRequest{MachineID: machineID, GroupID: groupID, ImageID: e.imageID, Note: "llm node node-test qwen3-4b"})
	if err != nil {
		e.t.Fatalf("PlaceSystem(%d): %v", machineID, err)
	}
	return inst
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

func wantCode(t *testing.T, err error, code, what string) {
	t.Helper()
	if ae := apperr.As(err); ae == nil || ae.Code != code {
		t.Fatalf("%s: want %s, got %v", what, code, err)
	}
}

func TestPlaceSystemRequiresPoolIdleOnline(t *testing.T) {
	e := newEnv(t, 6)
	m := e.machines
	hooks := &recHooks{}
	e.svc.SetAppHooks(hooks)
	pool := e.poolGroupID()

	// 未配置系统账号
	bare := rental.NewService(rental.Deps{Pool: e.pool, Clock: e.clock})
	if _, err := bare.PlaceSystem(e.ctx, rental.SystemPlaceRequest{MachineID: m[0], GroupID: pool, ImageID: e.imageID}); err == nil ||
		!strings.Contains(err.Error(), "未配置系统账号") {
		t.Fatalf("no system user: %v", err)
	}

	// 不在池
	_, err := e.svc.PlaceSystem(e.ctx, rental.SystemPlaceRequest{MachineID: m[0], GroupID: pool, ImageID: e.imageID})
	wantCode(t, err, "machine_unavailable", "not in pool")
	e.toPool(m[1], m[2], m[3], m[4], m[5])
	// 机组不符
	_, err = e.svc.PlaceSystem(e.ctx, rental.SystemPlaceRequest{MachineID: m[5], GroupID: pool + 1000, ImageID: e.imageID})
	wantCode(t, err, "machine_unavailable", "group mismatch")
	// 机器所在机组与参数一致、但不是池（普通可见机组）
	normal := e.group("普通机组", true)
	e.setGroup(m[0], normal)
	_, err = e.svc.PlaceSystem(e.ctx, rental.SystemPlaceRequest{MachineID: m[0], GroupID: normal, ImageID: e.imageID})
	wantCode(t, err, "machine_unavailable", "normal group")
	// 不存在的机器
	_, err = e.svc.PlaceSystem(e.ctx, rental.SystemPlaceRequest{MachineID: 999999, GroupID: pool, ImageID: e.imageID})
	wantCode(t, err, "machine_unavailable", "no such machine")
	// 非 idle
	if _, err := e.pool.Exec(e.ctx, `UPDATE machines SET state = 'maintenance' WHERE id = $1`, m[1]); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.PlaceSystem(e.ctx, rental.SystemPlaceRequest{MachineID: m[1], GroupID: pool, ImageID: e.imageID})
	wantCode(t, err, "machine_unavailable", "maintenance")
	// 离线
	if _, err := e.pool.Exec(e.ctx, `UPDATE machines SET agent_online = false WHERE id = $1`, m[2]); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.PlaceSystem(e.ctx, rental.SystemPlaceRequest{MachineID: m[2], GroupID: pool, ImageID: e.imageID})
	wantCode(t, err, "machine_unavailable", "offline")
	// 有活动实例（机器仍是 idle：直接写库造一个 stopped 实例）
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO instances (id, user_id, machine_id, product_id, image_id, state, ssh_password_enc, stopped_at, created_at, updated_at)
		VALUES ('ins-busy0001', $1, $2, $3, $4, 'stopped', '\x00', $5, $5, $5)`, e.userID, m[3], e.productID, e.imageID, e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.PlaceSystem(e.ctx, rental.SystemPlaceRequest{MachineID: m[3], GroupID: pool, ImageID: e.imageID})
	wantCode(t, err, "machine_unavailable", "active instance")
	// 未来（10 年内）的 reserved 时段
	e.reserveRaw(m[4], e.clock.Now().Add(30*24*time.Hour), 86400)
	_, err = e.svc.PlaceSystem(e.ctx, rental.SystemPlaceRequest{MachineID: m[4], GroupID: pool, ImageID: e.imageID})
	wantCode(t, err, "machine_unavailable", "future reservation")

	inst := e.placeSystem(m[5], pool)
	if !inst.System || inst.UserID != e.systemID || inst.ExpiresAt != nil || inst.PendingSeconds != 0 || inst.PasswordAuth ||
		inst.State != rental.StateCreating || inst.MachineID != m[5] || inst.SSHHostPubkey == "" {
		t.Fatalf("system instance = %+v", inst)
	}
	lower, upper, status := e.lease(inst.ID)
	if status != "active" || !lower.Equal(e.clock.Now()) || upper.Sub(lower) != rental.SystemLeaseHorizon {
		t.Fatalf("lease = [%v, %v) %s", lower, upper, status)
	}
	if st := e.machineState(m[5]); st != fleet.StateAllocated {
		t.Fatalf("machine state = %s", st)
	}
	cmds, err := e.fleet.PendingCommands(e.ctx, m[5])
	if err != nil || len(cmds) != 1 || cmds[0].Type != fleet.CmdCreateInstance || cmds[0].InstanceID != inst.ID {
		t.Fatalf("commands = %+v, %v", cmds, err)
	}
	// 计划 10：与其它开通路径同一形态的 Create 指令（显式端口、整机一份）
	var p fleet.CreateInstancePayload
	if err := json.Unmarshal(cmds[0].Payload, &p); err != nil || p.ImageRef == "" || p.SSHPort != testSSHBasePort || p.ShareSlots != 1 {
		t.Fatalf("create payload = %+v, %v", p, err)
	}
	if n := e.count(`SELECT count(*) FROM orders`); n != 0 {
		t.Fatalf("orders = %d", n)
	}
	if n := e.count(`SELECT count(*) FROM wallet_ledger WHERE user_id = $1`, e.systemID); n != 0 {
		t.Fatalf("system ledger = %d", n)
	}
	if n := e.count(`SELECT count(*) FROM instance_events WHERE instance_id = $1 AND reason LIKE 'system instance for llm node%'`, inst.ID); n != 1 {
		t.Fatalf("events = %d", n)
	}
	// 同一台机器不能再放第二个系统实例
	_, err = e.svc.PlaceSystem(e.ctx, rental.SystemPlaceRequest{MachineID: m[5], GroupID: pool, ImageID: e.imageID})
	wantCode(t, err, "machine_unavailable", "already allocated")
	// 不调 QueueInstall；Create 成功后照常调 OnInstanceRunning
	if calls := hooks.snapshot(); len(calls) != 0 {
		t.Fatalf("hooks before running = %v", calls)
	}
	e.ackFor(inst.ID, fleet.CmdCreateInstance, true)
	if calls := hooks.snapshot(); !slices.Equal(calls, []string{"running:" + inst.ID}) {
		t.Fatalf("hooks after running = %v", calls)
	}
}

func TestSystemInstanceNeverExpiresOrBills(t *testing.T) {
	e := newEnv(t, 1)
	m := e.machines[0]
	pool := e.toPool(m)
	inst := e.placeSystem(m, pool)
	e.ackFor(inst.ID, fleet.CmdCreateInstance, true)
	got := e.get(inst.ID)
	if got.State != rental.StateRunning || got.ExpiresAt != nil || got.PendingSeconds != 0 {
		t.Fatalf("after create = %+v", got)
	}
	lower0, upper0, _ := e.lease(inst.ID)
	if upper0.Sub(lower0) != rental.SystemLeaseHorizon {
		t.Fatalf("lease after running = [%v, %v)", lower0, upper0)
	}
	for day := 0; day < 100; day++ {
		e.clock.Advance(24 * time.Hour)
		e.heartbeatAll()
		if err := e.svc.Sweep(e.ctx); err != nil {
			t.Fatalf("day %d sweep: %v", day, err)
		}
		for _, f := range []func() (int, error){
			func() (int, error) { return e.svc.ExpireDue(e.ctx) },
			func() (int, error) { return e.svc.SendReminders(e.ctx) },
			func() (int, error) { return e.svc.AutoRenewDue(e.ctx) },
			func() (int, error) { return e.svc.RefreshCappedLeases(e.ctx) },
		} {
			if n, err := f(); err != nil || n != 0 {
				t.Fatalf("day %d: n=%d err=%v", day, n, err)
			}
		}
	}
	got = e.get(inst.ID)
	if got.State != rental.StateRunning || got.ExpiresAt != nil || got.Suspended() {
		t.Fatalf("after 100 days = %+v", got)
	}
	if lower, upper, status := e.lease(inst.ID); !lower.Equal(lower0) || !upper.Equal(upper0) || status != "active" {
		t.Fatalf("lease after 100 days = [%v, %v) %s", lower, upper, status)
	}
	if n := e.count(`SELECT count(*) FROM orders`); n != 0 {
		t.Fatalf("orders = %d", n)
	}
	if n := e.count(`SELECT count(*) FROM wallet_ledger WHERE user_id = $1`, e.systemID); n != 0 {
		t.Fatalf("ledger = %d", n)
	}
	if calls := e.notifier.Calls(); len(calls) != 0 {
		t.Fatalf("sms = %v", calls)
	}
	if n := e.count(`SELECT count(*) FROM alerts`); n != 0 {
		t.Fatalf("alerts = %d", n)
	}

	// 机器失联：只写 suspended_at，不算 paused_remaining、不动 expires_at 与 lease
	if n, err := e.svc.SuspendMachines(e.ctx, []int64{m}); err != nil || n != 1 {
		t.Fatalf("suspend: n=%d err=%v", n, err)
	}
	got = e.get(inst.ID)
	if got.SuspendedAt == nil || got.PausedRemaining != nil || got.ExpiresAt != nil {
		t.Fatalf("suspended = %+v", got)
	}
	if _, upper, _ := e.lease(inst.ID); !upper.Equal(upper0) {
		t.Fatalf("lease upper after suspend = %v", upper)
	}
	e.clock.Advance(time.Hour)
	if n, err := e.svc.ResumeMachine(e.ctx, m, []string{inst.ID}); err != nil || n != 1 {
		t.Fatalf("resume: n=%d err=%v", n, err)
	}
	got = e.get(inst.ID)
	if got.SuspendedAt != nil || got.PausedRemaining != nil || got.ExpiresAt != nil || got.State != rental.StateRunning {
		t.Fatalf("resumed = %+v", got)
	}
	if _, upper, _ := e.lease(inst.ID); !upper.Equal(upper0) {
		t.Fatalf("lease upper after resume = %v", upper)
	}
	if n := e.count(`SELECT count(*) FROM wallet_ledger WHERE user_id = $1`, e.systemID); n != 0 {
		t.Fatalf("ledger after resume = %d", n)
	}

	// 真实失联路径：心跳停过 OfflineTimeout，清扫器判离线并暂停；恢复在线 + 报告 running 后清扫器恢复
	e.heartbeatAll()
	e.clock.Advance(rental.OfflineTimeout + time.Minute)
	for i := 0; i < 3; i++ {
		if err := e.svc.Sweep(e.ctx); err != nil {
			t.Fatalf("offline sweep %d: %v", i, err)
		}
		e.clock.Advance(time.Minute)
	}
	got = e.get(inst.ID)
	if got.SuspendedAt == nil || got.PausedRemaining != nil || got.ExpiresAt != nil || got.State != rental.StateRunning {
		t.Fatalf("offline = %+v", got)
	}
	if lower, upper, status := e.lease(inst.ID); !lower.Equal(lower0) || !upper.Equal(upper0) || status != "active" {
		t.Fatalf("lease while offline = [%v, %v) %s", lower, upper, status)
	}
	e.heartbeatAll()
	e.reportRunning(m, inst.ID)
	if err := e.svc.Sweep(e.ctx); err != nil {
		t.Fatalf("online sweep: %v", err)
	}
	got = e.get(inst.ID)
	if got.SuspendedAt != nil || got.PausedRemaining != nil || got.ExpiresAt != nil || got.State != rental.StateRunning {
		t.Fatalf("back online = %+v", got)
	}
	if lower, upper, status := e.lease(inst.ID); !lower.Equal(lower0) || !upper.Equal(upper0) || status != "active" {
		t.Fatalf("lease after online = [%v, %v) %s", lower, upper, status)
	}
	if n := e.count(`SELECT count(*) FROM orders`); n != 0 {
		t.Fatalf("orders after offline = %d", n)
	}
	if n := e.count(`SELECT count(*) FROM wallet_ledger WHERE user_id = $1`, e.systemID); n != 0 {
		t.Fatalf("ledger after offline = %d", n)
	}
	if calls := e.notifier.Calls(); len(calls) != 0 {
		t.Fatalf("sms after offline = %v", calls)
	}

	// ReleaseSystem 后机器回到 idle
	if err := e.svc.ReleaseSystem(e.ctx, inst.ID); err != nil {
		t.Fatal(err)
	}
	e.ackFor(inst.ID, fleet.CmdWipeInstance, true)
	if got := e.get(inst.ID); got.State != rental.StateReleased {
		t.Fatalf("state = %s", got.State)
	}
	if st := e.machineState(m); st != fleet.StateIdle {
		t.Fatalf("machine = %s", st)
	}
}

func TestReleaseSystemFromEachState(t *testing.T) {
	e := newEnv(t, 3)
	pool := e.toPool(e.machines...)
	hooks := &recHooks{}
	e.svc.SetAppHooks(hooks)

	creating := e.placeSystem(e.machines[0], pool)
	running := e.placeSystem(e.machines[1], pool)
	stopped := e.placeSystem(e.machines[2], pool)
	e.ackFor(running.ID, fleet.CmdCreateInstance, true)
	e.ackFor(stopped.ID, fleet.CmdCreateInstance, true)
	if _, err := e.pool.Exec(e.ctx, `UPDATE instances SET state = 'stopped', stopped_at = $2 WHERE id = $1`, stopped.ID, e.clock.Now()); err != nil {
		t.Fatal(err)
	}

	for i, inst := range []rental.Instance{creating, running, stopped} {
		mid := e.machines[i]
		if err := e.svc.ReleaseSystem(e.ctx, inst.ID); err != nil {
			t.Fatalf("ReleaseSystem(%d): %v", i, err)
		}
		if got := e.get(inst.ID); got.State != rental.StateWiping {
			t.Fatalf("%d: state = %s", i, got.State)
		}
		cmds, err := e.fleet.PendingCommands(e.ctx, mid)
		if err != nil || len(cmds) != 1 || cmds[0].Type != fleet.CmdWipeInstance {
			t.Fatalf("%d: pending = %+v, %v", i, cmds, err)
		}
		if !slices.Contains(hooks.snapshot(), "gone:"+inst.ID+":实例已释放") {
			t.Fatalf("%d: hooks = %v", i, hooks.snapshot())
		}
		e.ackFor(inst.ID, fleet.CmdWipeInstance, true)
		if got := e.get(inst.ID); got.State != rental.StateReleased {
			t.Fatalf("%d: after wipe = %s", i, got.State)
		}
		if st := e.machineState(mid); st != fleet.StateIdle {
			t.Fatalf("%d: machine = %s", i, st)
		}
		if _, _, status := e.lease(inst.ID); status != "ended" {
			t.Fatalf("%d: lease = %s", i, status)
		}
	}
	// creating 起点：待执行的 Create 被取消
	if n := e.count(`SELECT count(*) FROM agent_commands WHERE instance_id = $1 AND type = 'create_instance' AND status = 'failed' AND result LIKE 'cancelled:%'`, creating.ID); n != 1 {
		t.Fatalf("create cancelled = %d", n)
	}
	// 已释放 → invalid_state；不存在 → not_found
	wantCode(t, e.svc.ReleaseSystem(e.ctx, creating.ID), "invalid_state", "released")
	if err := e.svc.ReleaseSystem(e.ctx, "ins-nothere1"); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	// 非系统实例 → invalid_state
	e.newMachine("SNUSER")
	_, user := e.place(catalog.D1d)
	wantCode(t, e.svc.ReleaseSystem(e.ctx, user.ID), "invalid_state", "user instance")
	if got := e.get(user.ID); got.State != rental.StateCreating {
		t.Fatalf("user instance touched: %s", got.State)
	}
}

func TestSystemInstanceRejectsAdminOps(t *testing.T) {
	e := newEnv(t, 2)
	pool := e.toPool(e.machines[0])
	inst := e.placeSystem(e.machines[0], pool)
	e.ackFor(inst.ID, fleet.CmdCreateInstance, true)
	actor := audit.Actor{AdminID: 9}

	_, err := e.svc.RefundRelease(e.ctx, actor, inst.ID, 0, "x")
	wantCode(t, err, "invalid_state", "RefundRelease")
	_, err = e.svc.AdminExtend(e.ctx, actor, inst.ID, 3600, "x")
	wantCode(t, err, "invalid_state", "AdminExtend")
	_, err = e.svc.AdminMigrate(e.ctx, actor, inst.ID, "x")
	wantCode(t, err, "invalid_state", "AdminMigrate")
	_, err = e.svc.Cancel(e.ctx, e.systemID, inst.ID, "1.1.1.1")
	wantCode(t, err, "invalid_state", "Cancel")
	_, err = e.svc.AdminCancelBooking(e.ctx, actor, inst.ID, "x")
	wantCode(t, err, "invalid_state", "AdminCancelBooking")
	_, err = e.svc.AdminReassign(e.ctx, actor, inst.ID, nil, "x")
	wantCode(t, err, "invalid_state", "AdminReassign")
	_, err = e.svc.AdminReassign(e.ctx, actor, inst.ID, &e.machines[1], "x")
	wantCode(t, err, "invalid_state", "AdminReassign to")
	_, err = e.svc.SetAutoRenew(e.ctx, e.systemID, inst.ID, true, catalog.D1d, "1.1.1.1")
	wantCode(t, err, "invalid_state", "SetAutoRenew")
	_, err = e.svc.Renew(e.ctx, e.systemID, inst.ID, catalog.D1d, "")
	wantCode(t, err, "invalid_state", "Renew")
	wantCode(t, e.svc.Release(e.ctx, e.systemID, inst.ID), "invalid_state", "Release")

	got := e.get(inst.ID)
	if got.State != rental.StateRunning || got.ExpiresAt != nil || got.AutoRenew || got.MachineID != e.machines[0] {
		t.Fatalf("after rejected ops = %+v", got)
	}
	if n := e.count(`SELECT count(*) FROM orders`); n != 0 {
		t.Fatalf("orders = %d", n)
	}
	if n := e.count(`SELECT count(*) FROM wallet_ledger WHERE user_id = $1`, e.systemID); n != 0 {
		t.Fatalf("ledger = %d", n)
	}
}

func TestPoolMachinesInvisibleToRental(t *testing.T) {
	e := newEnv(t, 2)
	poolMachine, other := e.machines[0], e.machines[1]
	pool := e.toPool(poolMachine)
	actor := audit.Actor{AdminID: 9}

	// 另一台暂时不可用：此刻唯一空闲在线的机器在池里
	if _, err := e.pool.Exec(e.ctx, `UPDATE machines SET state = 'maintenance' WHERE id = $1`, other); err != nil {
		t.Fatal(err)
	}
	if n, err := e.svc.AvailableNow(e.ctx, e.productID); err != nil || n != 0 {
		t.Fatalf("AvailableNow = %d, %v", n, err)
	}
	_, _, err := e.svc.Place(e.ctx, rental.PlaceRequest{UserID: e.userID, ProductID: e.productID, ImageID: e.imageID, Duration: catalog.D1d})
	if !errors.Is(err, apperr.ErrNoCapacity) {
		t.Fatalf("Place: %v", err)
	}
	start := e.clock.Now().Add(48 * time.Hour)
	_, err = e.svc.PlaceBatch(e.ctx, rental.BatchRequest{UserID: e.userID, ProductID: e.productID, ImageID: e.imageID, Count: 1,
		Duration: catalog.D1d, StartAt: &start})
	wantCode(t, err, "no_capacity", "booking any group")
	_, err = e.svc.PlaceBatch(e.ctx, rental.BatchRequest{UserID: e.userID, ProductID: e.productID, ImageID: e.imageID, Count: 1,
		Duration: catalog.D1d, StartAt: &start, GroupID: &pool})
	wantCode(t, err, "group_not_found", "booking pool group")
	_, err = e.svc.PlaceBatch(e.ctx, rental.BatchRequest{UserID: e.userID, ProductID: e.productID, ImageID: e.imageID, Count: 1,
		Duration: catalog.D1d, MachineID: &poolMachine})
	wantCode(t, err, "machine_unavailable", "named pool machine")

	// 另一台恢复：用户实例落在它上面；后台换机找不到别的机器（池机器不可选）
	if _, err := e.pool.Exec(e.ctx, `UPDATE machines SET state = 'idle' WHERE id = $1`, other); err != nil {
		t.Fatal(err)
	}
	_, inst := e.place(catalog.D1d)
	if inst.MachineID != other {
		t.Fatalf("placed on %d", inst.MachineID)
	}
	e.ackFor(inst.ID, fleet.CmdCreateInstance, true)
	_, err = e.svc.AdminMigrate(e.ctx, actor, inst.ID, "bad gpu")
	if !errors.Is(err, apperr.ErrNoCapacity) {
		t.Fatalf("AdminMigrate: %v", err)
	}
	// 预订改派（自动 / 指定池机器）都拿不到池机器
	booked := e.reserveRaw(other, e.clock.Now().Add(10*24*time.Hour), 86400)
	_, err = e.svc.AdminReassign(e.ctx, actor, booked, nil, "x")
	wantCode(t, err, "machine_unavailable", "auto reassign")
	_, err = e.svc.AdminReassign(e.ctx, actor, booked, &poolMachine, "x")
	wantCode(t, err, "machine_unavailable", "reassign to pool machine")
	if n := e.count(`SELECT count(*) FROM machine_leases WHERE machine_id = $1`, poolMachine); n != 0 {
		t.Fatalf("pool machine leases = %d", n)
	}

	// 移出池后立刻能租
	if err := e.fleet.UnassignMachines(e.ctx, actor, pool, []int64{poolMachine}); err != nil {
		t.Fatal(err)
	}
	_, inst2 := e.place(catalog.D1d)
	if inst2.MachineID != poolMachine {
		t.Fatalf("after unassign placed on %d", inst2.MachineID)
	}
}

func TestUpstreamTarget(t *testing.T) {
	e := newEnv(t, 1)
	m := e.machines[0]
	inst := e.placeSystem(m, e.toPool(m))
	mach, err := e.fleet.Get(e.ctx, e.pool, m)
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.UpstreamTarget(e.ctx, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 端口与 InstanceSSHPort / Create 指令一致（计划 10 按实例端口）
	want := upstream.Target{InstanceID: inst.ID, LanIP: mach.LanIP, HostPubkey: inst.SSHHostPubkey, Port: testSSHBasePort}
	if got != want || got.LanIP == "" || got.HostPubkey == "" || got.Port != e.svc.InstanceSSHPort(inst.ID) {
		t.Fatalf("target = %+v, want %+v", got, want)
	}
	if _, err := e.svc.UpstreamTarget(e.ctx, "ins-nothere1"); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestListLiveSystemInstances(t *testing.T) {
	e := newEnv(t, 3)
	pool := e.toPool(e.machines[0], e.machines[1])
	a := e.placeSystem(e.machines[0], pool)
	b := e.placeSystem(e.machines[1], pool)
	e.ackFor(b.ID, fleet.CmdCreateInstance, true)
	_, user := e.place(catalog.D1d) // 落在池外的第三台
	ids := func() []string {
		t.Helper()
		list, err := e.svc.ListLiveSystemInstances(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, i := range list {
			if !i.System {
				t.Fatalf("non-system instance listed: %+v", i)
			}
			out = append(out, i.ID)
		}
		return out
	}
	want := []string{a.ID, b.ID}
	slices.Sort(want)
	if got := ids(); !slices.Equal(got, want) {
		t.Fatalf("live = %v, want %v (user %s excluded)", got, want, user.ID)
	}
	// 洗机中仍算未结束；洗完（released）不再列出
	if err := e.svc.ReleaseSystem(e.ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if got := ids(); !slices.Equal(got, want) {
		t.Fatalf("live while wiping = %v", got)
	}
	e.ackFor(a.ID, fleet.CmdWipeInstance, true)
	if got := ids(); !slices.Equal(got, []string{b.ID}) {
		t.Fatalf("live after wipe = %v", got)
	}
}
