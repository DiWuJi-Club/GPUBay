package rental_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/rental"
)

func (e *env) alerts(kind string) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM alerts WHERE kind = $1`, kind).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) lastEvent(id string) rental.Event {
	e.t.Helper()
	events, err := e.svc.InstanceEvents(e.ctx, id)
	if err != nil || len(events) == 0 {
		e.t.Fatalf("events: %v", err)
	}
	return events[len(events)-1]
}

// ackOn 同 ack，但只回报一台机器上未完成的指令。
func (e *env) ackOn(machineID int64, ok bool) {
	e.t.Helper()
	cmds, err := e.fleet.PendingCommands(e.ctx, machineID)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, c := range cmds {
		if err := e.svc.HandleCommandResult(e.ctx, c.ID, ok, "test"); err != nil {
			e.t.Fatalf("HandleCommandResult(%s): %v", c.Type, err)
		}
	}
}

func TestProvisionDueStartsOneHourBefore(t *testing.T) {
	e := newEnv(t, 1)
	start := dbtest.T0.Add(3 * time.Hour)
	inst := e.book(start)
	e.clock.Advance(time.Hour + 59*time.Minute)
	n, err := e.svc.ProvisionDue(e.ctx)
	mustCount(t, n, err, 0, "provision before T-1h")
	e.clock.Advance(time.Minute)
	e.heartbeatAll()
	n, err = e.svc.ProvisionDue(e.ctx)
	mustCount(t, n, err, 1, "provision at T-1h")
	got := e.get(inst.ID)
	if got.State != rental.StateCreating || got.ProvisionedAt == nil || !got.ProvisionedAt.Equal(e.clock.Now()) || got.PendingSeconds != 86400 {
		t.Fatalf("after provision = %+v", got)
	}
	if e.machineState(e.machines[0]) != fleet.StateAllocated || countPending(e, e.machines[0], fleet.CmdCreateInstance) != 1 {
		t.Fatal("machine must be allocated with a create command")
	}
	if _, _, status := e.lease(inst.ID); status != "active" {
		t.Fatalf("lease = %s", status)
	}
	if ev := e.lastEvent(inst.ID); ev.FromState != rental.StateReserved || ev.ToState != rental.StateCreating || ev.Reason != "scheduled start 2026-01-01 11:00" {
		t.Fatalf("event = %+v", ev)
	}
	n, err = e.svc.ProvisionDue(e.ctx)
	mustCount(t, n, err, 0, "provision twice")

	// 早开通：5 分钟后 Agent 回报 running，到期 = S + 1d（不是 now + 1d），这段时间可用但不计费
	e.clock.Advance(5 * time.Minute)
	e.ack(true)
	got = e.get(inst.ID)
	if got.State != rental.StateRunning || !got.ExpiresAt.Equal(start.Add(24*time.Hour)) {
		t.Fatalf("after create = %+v", got)
	}
	if ev := e.lastEvent(inst.ID); ev.Reason != "agent: created (scheduled 2026-01-01 11:00)" {
		t.Fatalf("event = %+v", ev)
	}
	if _, upper, _ := e.lease(inst.ID); !upper.Equal(start.Add(24*time.Hour + rental.Tail)) {
		t.Fatalf("lease upper = %v", upper)
	}
	if r := e.get(inst.ID).Remaining(e.clock.Now()); r != 24*time.Hour {
		t.Fatalf("remaining before S must not tick: %v", r)
	}
	e.clock.Advance(55 * time.Minute) // 到 S
	if r := e.get(inst.ID).Remaining(e.clock.Now()); r != 24*time.Hour {
		t.Fatalf("remaining at S = %v", r)
	}
	e.clock.Advance(time.Hour)
	if r := e.get(inst.ID).Remaining(e.clock.Now()); r != 23*time.Hour {
		t.Fatalf("remaining after S = %v", r)
	}
}

func TestProvisionLateStartIsCompensatedAndCapped(t *testing.T) {
	e := newEnv(t, 1)
	start := dbtest.T0.Add(3 * time.Hour)
	inst := e.book(start)
	// 同机紧随其后的预订：下界 = 第一单上界 = S + 1d + 7h
	next := e.book(start.Add(24*time.Hour + rental.Tail + rental.PrepLead))
	e.clock.Advance(2 * time.Hour)
	e.heartbeatAll()
	if n, err := e.svc.ProvisionDue(e.ctx); err != nil || n != 1 {
		t.Fatalf("provision = %d, %v", n, err)
	}
	e.clock.Advance(time.Hour + 30*time.Minute) // Agent 晚了 30 分钟：now = S + 30m
	balance := e.balance(e.userID)
	e.ack(true)
	got := e.get(inst.ID)
	// base = S + 30m，想要 S + 30m + 24h；封顶 cap − Tail = (S + 31h) − 7h = S + 24h；差额 30 分钟
	if !got.ExpiresAt.Equal(start.Add(24 * time.Hour)) {
		t.Fatalf("expires = %v, want %v (capped)", got.ExpiresAt, start.Add(24*time.Hour))
	}
	refund := int64(1800) * 6900 / 86400
	if b := e.balance(e.userID); b != balance+refund {
		t.Fatalf("balance = %d, want compensation %d", b, refund)
	}
	events, _ := e.svc.InstanceEvents(e.ctx, inst.ID)
	var reasons []string
	for _, ev := range events {
		reasons = append(reasons, ev.Reason)
	}
	if !slices.Contains(reasons, "agent: created (scheduled 2026-01-01 11:00, late by 30m0s)") || !slices.Contains(reasons, "late start compensated: 1800s refund 143") {
		t.Fatalf("events = %v", reasons)
	}
	if _, upper, _ := e.lease(inst.ID); !upper.Equal(start.Add(24*time.Hour + rental.Tail)) {
		t.Fatalf("lease upper = %v", upper)
	}
	if e.get(next.ID).State != rental.StateReserved {
		t.Fatal("next booking untouched")
	}
}

func TestProvisionReassignsWhenMachineUnavailable(t *testing.T) {
	e := newEnv(t, 2)
	start := dbtest.T0.Add(3 * time.Hour)
	req := e.batchReq(1, &start)
	req.MachineID = &e.machines[0]
	res, err := e.svc.PlaceBatch(e.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	inst := res.Instances[0]
	e.setMachine(e.machines[0], "maintenance", true)
	e.clock.Advance(2 * time.Hour)
	e.heartbeatAll()
	if n, err := e.svc.ProvisionDue(e.ctx); err != nil || n != 1 {
		t.Fatalf("provision = %d, %v", n, err)
	}
	got := e.get(inst.ID)
	if got.State != rental.StateCreating || got.MachineID != e.machines[1] || got.ReassignCount != 1 {
		t.Fatalf("after reassign = %+v", got)
	}
	if e.machineState(e.machines[1]) != fleet.StateAllocated || countPending(e, e.machines[1], fleet.CmdCreateInstance) != 1 {
		t.Fatal("new machine must be allocated with create")
	}
	var leases []string
	rows, _ := e.pool.Query(e.ctx, `SELECT status FROM machine_leases WHERE instance_id = $1 ORDER BY id`, inst.ID)
	for rows.Next() {
		var st string
		_ = rows.Scan(&st)
		leases = append(leases, st)
	}
	rows.Close()
	if !slices.Equal(leases, []string{"cancelled", "active"}) {
		t.Fatalf("leases = %v", leases)
	}
	events, _ := e.svc.InstanceEvents(e.ctx, inst.ID)
	found := false
	for _, ev := range events {
		if ev.Reason == "reassigned "+itoa64(e.machines[0])+" -> "+itoa64(e.machines[1])+": machine unavailable at start" {
			found = true
		}
	}
	if !found {
		t.Fatalf("reassign event missing: %+v", events)
	}
}

func itoa64(n int64) string { return fmt.Sprintf("%d", n) }

func TestProvisionWaitsThenCancelsAfterGrace(t *testing.T) {
	e := newEnv(t, 1)
	start := dbtest.T0.Add(3 * time.Hour)
	inst := e.book(start)
	e.setMachine(e.machines[0], "maintenance", true)
	e.clock.Advance(2 * time.Hour)
	if n, err := e.svc.ProvisionDue(e.ctx); err != nil || n != 1 {
		t.Fatalf("provision = %d, %v", n, err)
	}
	if e.get(inst.ID).State != rental.StateReserved || e.alerts("booking_at_risk") != 1 {
		t.Fatalf("expected reserved + at-risk alert, got %s / %d", e.get(inst.ID).State, e.alerts("booking_at_risk"))
	}
	e.clock.Advance(time.Hour + 29*time.Minute) // S + 29m：仍在宽限期
	if _, err := e.svc.ProvisionDue(e.ctx); err != nil {
		t.Fatal(err)
	}
	if e.get(inst.ID).State != rental.StateReserved || e.alerts("booking_at_risk") != 1 {
		t.Fatal("still waiting, alert not duplicated")
	}
	e.clock.Advance(2 * time.Minute) // S + 31m：取消、全退、告警、短信
	if _, err := e.svc.ProvisionDue(e.ctx); err != nil {
		t.Fatal(err)
	}
	got := e.get(inst.ID)
	if got.State != rental.StateCancelled || e.balance(e.userID) != startFunds || e.alerts("booking_unfulfilled") != 1 {
		t.Fatalf("after grace = %+v balance=%d", got, e.balance(e.userID))
	}
	if _, _, status := e.lease(inst.ID); status != "cancelled" {
		t.Fatalf("lease = %s", status)
	}
	if !slices.Contains(e.notifier.Calls(), "booking_cancelled:"+inst.ID+":无法安排机器") {
		t.Fatalf("sms = %v", e.notifier.Calls())
	}
	if ev := e.lastEvent(inst.ID); ev.Reason != "unfulfilled: no machine by grace, refund 6900" {
		t.Fatalf("event = %+v", ev)
	}
}

func TestProvisionCancelsWhenUserCannotRent(t *testing.T) {
	e := newEnv(t, 1)
	inst := e.book(dbtest.T0.Add(3 * time.Hour))
	if _, err := e.pool.Exec(e.ctx, `UPDATE users SET status = 'banned' WHERE id = $1`, e.userID); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(2 * time.Hour)
	if n, err := e.svc.ProvisionDue(e.ctx); err != nil || n != 1 {
		t.Fatalf("provision = %d, %v", n, err)
	}
	if got := e.get(inst.ID); got.State != rental.StateCancelled || e.balance(e.userID) != startFunds {
		t.Fatalf("banned user booking = %+v", got)
	}
	if ev := e.lastEvent(inst.ID); ev.Reason != "cancelled: user cannot rent, refund 6900" {
		t.Fatalf("event = %+v", ev)
	}
	if !slices.Contains(e.notifier.Calls(), "booking_cancelled:"+inst.ID+":账号不可租用") {
		t.Fatalf("sms = %v", e.notifier.Calls())
	}
}

func TestCreateFailureReassignsBookedInstance(t *testing.T) {
	e := newEnv(t, 2)
	start := dbtest.T0.Add(3 * time.Hour)
	req := e.batchReq(1, &start)
	req.MachineID = &e.machines[0]
	res, _ := e.svc.PlaceBatch(e.ctx, req)
	inst := res.Instances[0]
	e.clock.Advance(2 * time.Hour)
	e.heartbeatAll()
	_, _ = e.svc.ProvisionDue(e.ctx)
	before := e.get(inst.ID)
	balance := e.balance(e.userID)
	e.ackOn(e.machines[0], false) // 只回报机器 0 上的 Create 失败（e.ack 会接着把改派到机器 1 的 Create 也判失败）
	got := e.get(inst.ID)
	if got.State != rental.StateCreating || got.MachineID != e.machines[1] || got.ReassignCount != 1 || got.PendingSeconds != 86400 ||
		got.ProvisionedAt == nil || got.SSHHostPubkey == before.SSHHostPubkey {
		t.Fatalf("after create failure = %+v (before %+v)", got, before)
	}
	if e.balance(e.userID) != balance {
		t.Fatal("booked instance must not be refunded on create failure")
	}
	if e.machineState(e.machines[0]) != fleet.StateFaulty || e.alerts("create_failed") != 1 {
		t.Fatal("old machine faulty + create_failed alert expected")
	}
	if countPending(e, e.machines[1], fleet.CmdCreateInstance) != 1 || e.machineState(e.machines[1]) != fleet.StateAllocated {
		t.Fatal("create must be re-enqueued on the new machine")
	}
	if _, _, status := e.lease(inst.ID); status != "active" {
		t.Fatalf("lease = %s", status)
	}
	if ev := e.lastEvent(inst.ID); ev.FromState != rental.StateCreating || ev.ToState != rental.StateCreating ||
		ev.Reason != "reassigned "+itoa64(e.machines[0])+" -> "+itoa64(e.machines[1])+": create failed" {
		t.Fatalf("event = %+v", ev)
	}
	e.ack(true)
	if e.get(inst.ID).State != rental.StateRunning {
		t.Fatal("expected running on the new machine")
	}
}

func TestCreateFailureNoCandidateGoesBackToReserved(t *testing.T) {
	e := newEnv(t, 1)
	start := dbtest.T0.Add(3 * time.Hour)
	inst := e.book(start)
	e.clock.Advance(2 * time.Hour)
	e.heartbeatAll()
	_, _ = e.svc.ProvisionDue(e.ctx)
	e.ack(false)
	got := e.get(inst.ID)
	if got.State != rental.StateReserved || got.ProvisionedAt != nil || got.MachineID != e.machines[0] || got.PendingSeconds != 86400 {
		t.Fatalf("after create failure without candidates = %+v", got)
	}
	var n int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM machine_leases WHERE instance_id = $1 AND status = 'reserved'`, inst.ID).Scan(&n)
	if n != 1 || e.alerts("booking_at_risk") != 1 || e.machineState(e.machines[0]) != fleet.StateFaulty {
		t.Fatalf("lease reserved=%d at_risk=%d machine=%s", n, e.alerts("booking_at_risk"), e.machineState(e.machines[0]))
	}
	if ev := e.lastEvent(inst.ID); ev.FromState != rental.StateCreating || ev.ToState != rental.StateReserved || ev.Reason != "create failed, waiting for machine" {
		t.Fatalf("event = %+v", ev)
	}
	// 运维修好机器（结束维护回到 idle）后下一轮重新开通
	e.setMachine(e.machines[0], "idle", true)
	e.clock.Advance(time.Minute)
	e.heartbeatAll()
	if n, err := e.svc.ProvisionDue(e.ctx); err != nil || n != 1 {
		t.Fatalf("re-provision = %d, %v", n, err)
	}
	if e.get(inst.ID).State != rental.StateCreating {
		t.Fatal("expected creating again")
	}
}

func TestCreateTimeoutUsesProvisionedAt(t *testing.T) {
	e := newEnv(t, 2)
	start := dbtest.T0.Add(30 * time.Hour)
	inst := e.book(start)
	e.clock.Advance(29 * time.Hour)
	e.heartbeatAll()
	_, _ = e.svc.ProvisionDue(e.ctx)
	e.clock.Advance(rental.CreateTimeout - time.Minute)
	e.heartbeatAll()
	n, err := e.svc.FailStuckCreating(e.ctx)
	mustCount(t, n, err, 0, "timeout before provisioned_at + CreateTimeout (created_at is 29h old)")
	e.clock.Advance(2 * time.Minute)
	n, err = e.svc.FailStuckCreating(e.ctx)
	mustCount(t, n, err, 1, "timeout after")
	got := e.get(inst.ID)
	if got.State != rental.StateCreating || got.MachineID != e.machines[1] || got.ReassignCount != 1 {
		t.Fatalf("after timeout = %+v", got)
	}
}

func TestCreateFailurePastGraceCancelsAfterMachineIsFaulty(t *testing.T) {
	e := newEnv(t, 1)
	start := dbtest.T0.Add(3 * time.Hour)
	inst := e.book(start)
	e.clock.Advance(2 * time.Hour)
	e.heartbeatAll()
	_, _ = e.svc.ProvisionDue(e.ctx)
	e.clock.Advance(time.Hour + 31*time.Minute) // S + 31m：已过宽限期
	e.ackOn(e.machines[0], false)
	got := e.get(inst.ID)
	if got.State != rental.StateCancelled || e.balance(e.userID) != startFunds || e.alerts("booking_unfulfilled") != 1 {
		t.Fatalf("after create failure past grace = %+v balance=%d", got, e.balance(e.userID))
	}
	// cancelInstance 不碰机器：creating 实例被平台取消前，钉住的机器必须已被隔离（不能留在 allocated 被下一单直接拿走）
	if e.machineState(e.machines[0]) != fleet.StateFaulty || e.alerts("create_failed") != 1 {
		t.Fatalf("machine = %s, create_failed = %d", e.machineState(e.machines[0]), e.alerts("create_failed"))
	}
	if _, _, status := e.lease(inst.ID); status != "cancelled" {
		t.Fatalf("lease = %s", status)
	}
	if countPending(e, e.machines[0], fleet.CmdCreateInstance) != 0 {
		t.Fatal("no create may remain queued")
	}
	if ev := e.lastEvent(inst.ID); ev.FromState != rental.StateCreating || ev.Reason != "unfulfilled: create failed past grace, refund 6900" {
		t.Fatalf("event = %+v", ev)
	}
	if !slices.Contains(e.notifier.Calls(), "booking_cancelled:"+inst.ID+":无法安排机器") {
		t.Fatalf("sms = %v", e.notifier.Calls())
	}
}

// breakRefund 预先占用该实例首单的第一条退款流水号（<order>-r1），让平台取消的 refundAcross 撞唯一索引失败。
func (e *env) breakRefund(instanceID string) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO wallet_ledger (user_id, amount, balance_after, kind, ref_type, ref_id, remark, created_at)
		SELECT user_id, 1, 0, 'refund', 'order', id || '-r1', 'test', $2 FROM orders WHERE instance_id = $1`, instanceID, e.clock.Now()); err != nil {
		e.t.Fatal(err)
	}
}

func TestProvisionCancelFailureRaisesAlert(t *testing.T) {
	e := newEnv(t, 1)
	inst := e.book(dbtest.T0.Add(3 * time.Hour))
	e.setMachine(e.machines[0], "maintenance", true)
	e.breakRefund(inst.ID)
	e.clock.Advance(3*time.Hour + 31*time.Minute) // 过了宽限期：平台取消，但退款失败
	if _, err := e.svc.ProvisionDue(e.ctx); err == nil {
		t.Fatal("expected cancel error")
	}
	if e.get(inst.ID).State != rental.StateReserved || e.alerts("booking_cancel_failed") != 1 || e.alerts("booking_unfulfilled") != 0 {
		t.Fatalf("state=%s cancel_failed=%d", e.get(inst.ID).State, e.alerts("booking_cancel_failed"))
	}
	if len(e.notifier.Calls()) != 0 {
		t.Fatalf("no sms on failed cancel: %v", e.notifier.Calls())
	}
	_, _ = e.svc.ProvisionDue(e.ctx) // 下一轮再失败：告警不重复
	if e.alerts("booking_cancel_failed") != 1 {
		t.Fatal("alert must be raised once")
	}
}

func TestCreateFailureCancelFailureRaisesAlert(t *testing.T) {
	e := newEnv(t, 1)
	inst := e.book(dbtest.T0.Add(3 * time.Hour))
	e.clock.Advance(2 * time.Hour)
	e.heartbeatAll()
	_, _ = e.svc.ProvisionDue(e.ctx)
	e.breakRefund(inst.ID)
	e.clock.Advance(time.Hour + 31*time.Minute)
	cmds, err := e.fleet.PendingCommands(e.ctx, e.machines[0])
	if err != nil || len(cmds) != 1 {
		t.Fatalf("pending = %v, %v", cmds, err)
	}
	if err := e.svc.HandleCommandResult(e.ctx, cmds[0].ID, false, "boom"); err == nil {
		t.Fatal("expected cancel error")
	}
	if e.get(inst.ID).State != rental.StateCreating || e.alerts("booking_cancel_failed") != 1 {
		t.Fatalf("state=%s cancel_failed=%d", e.get(inst.ID).State, e.alerts("booking_cancel_failed"))
	}
	// 超时清扫走同一条路径（FailStuckCreating 的 onErr），告警按内容去重
	if _, err := e.svc.FailStuckCreating(e.ctx); err == nil {
		t.Fatal("expected cancel error from timeout path")
	}
	if e.alerts("booking_cancel_failed") != 1 {
		t.Fatal("alert must be raised once")
	}
}

// 早开通窗口内失联又恢复：从 max(now, S) 恢复计时，已付时段不被缩短；暂停期间 lease 上界也不收缩。
func TestBookedSuspendResumeKeepsPaidWindow(t *testing.T) {
	for _, tc := range []struct {
		name     string
		resumeAt time.Duration // 相对 S
	}{{"resume before S", -20 * time.Minute}, {"resume after S", 2 * time.Hour}} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, 1)
			start := dbtest.T0.Add(3 * time.Hour)
			inst := e.book(start)
			e.clock.Advance(2 * time.Hour) // S − 1h
			e.heartbeatAll()
			mustCount(t, firstOf(e.svc.ProvisionDue(e.ctx)), nil, 1, "provision")
			e.ack(true)
			e.clock.Advance(10 * time.Minute) // S − 50m
			mustCount(t, firstOf(e.svc.SuspendMachines(e.ctx, e.machines)), nil, 1, "suspended")
			paid := start.Add(24 * time.Hour)
			if got := e.get(inst.ID); got.PausedRemaining == nil || *got.PausedRemaining != 86400 {
				t.Fatalf("paused = %+v", got)
			}
			if _, upper, _ := e.lease(inst.ID); !upper.Equal(paid.Add(rental.Tail)) {
				t.Fatalf("lease upper while suspended = %v, want %v", upper, paid.Add(rental.Tail))
			}
			e.clock.Advance(start.Add(tc.resumeAt).Sub(e.clock.Now()))
			n, err := e.svc.ResumeMachine(e.ctx, e.machines[0], []string{inst.ID})
			mustCount(t, n, err, 1, "resumed")
			want := paid
			if tc.resumeAt > 0 {
				want = e.clock.Now().Add(24 * time.Hour)
			}
			if got := e.get(inst.ID); !got.ExpiresAt.Equal(want) {
				t.Fatalf("expires = %v, want %v", got.ExpiresAt, want)
			}
			if _, upper, _ := e.lease(inst.ID); !upper.Equal(want.Add(rental.Tail)) {
				t.Fatalf("lease upper = %v", upper)
			}
		})
	}
}
