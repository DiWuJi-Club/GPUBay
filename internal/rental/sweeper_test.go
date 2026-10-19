package rental_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/rental"
)

func countPending(e *env, machineID int64, t fleet.CommandType) int {
	e.t.Helper()
	cmds, err := e.fleet.PendingCommands(e.ctx, machineID)
	if err != nil {
		e.t.Fatal(err)
	}
	n := 0
	for _, c := range cmds {
		if c.Type == t {
			n++
		}
	}
	return n
}

func mustCount(t *testing.T, n int, err error, want int, what string) {
	t.Helper()
	if err != nil || n != want {
		t.Fatalf("%s = %d, %v; want %d", what, n, err, want)
	}
}

func TestExpireDueStopsExactlyAtExpiry(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)

	e.clock.Advance(24*time.Hour - time.Minute)
	n, err := e.svc.ExpireDue(e.ctx)
	mustCount(t, n, err, 0, "expired before due")

	e.clock.Advance(time.Minute)
	n, err = e.svc.ExpireDue(e.ctx)
	mustCount(t, n, err, 1, "expired at due")
	got := e.get(inst.ID)
	if got.State != rental.StateStopped || got.StoppedAt == nil || !got.StoppedAt.Equal(e.clock.Now()) {
		t.Fatalf("instance = %+v", got)
	}
	if countPending(e, e.machines[0], fleet.CmdStopInstance) != 1 {
		t.Fatal("stop command not enqueued")
	}
	n, err = e.svc.ExpireDue(e.ctx)
	mustCount(t, n, err, 0, "expire twice")
}

func TestWipeStaleAfterSixHoursThenRelease(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	e.clock.Advance(24 * time.Hour)
	_, _ = e.svc.ExpireDue(e.ctx)

	e.clock.Advance(6*time.Hour - time.Minute)
	n, err := e.svc.WipeStale(e.ctx)
	mustCount(t, n, err, 0, "wiped before 6h")

	e.clock.Advance(time.Minute)
	n, err = e.svc.WipeStale(e.ctx)
	mustCount(t, n, err, 1, "wiped at 6h")
	if got := e.get(inst.ID); got.State != rental.StateWiping {
		t.Fatalf("state = %s", got.State)
	}
	// 未回报的 stop 指令被取消，只剩 wipe
	if countPending(e, e.machines[0], fleet.CmdStopInstance) != 0 || countPending(e, e.machines[0], fleet.CmdWipeInstance) != 1 {
		t.Fatal("expected only a wipe command pending")
	}
	e.ack(true)
	if got := e.get(inst.ID); got.State != rental.StateReleased {
		t.Fatalf("state = %s, want released", got.State)
	}
	if got := e.machineState(e.machines[0]); got != fleet.StateIdle {
		t.Fatalf("machine = %s, want idle", got)
	}
	if _, _, status := e.lease(inst.ID); status != "ended" {
		t.Fatalf("lease status = %s", status)
	}
}

func TestCreatingTimeoutRefundsAndIgnoresLateAck(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	cmds, _ := e.fleet.PendingCommands(e.ctx, e.machines[0])

	e.clock.Advance(rental.CreateTimeout - time.Second)
	n, err := e.svc.FailStuckCreating(e.ctx)
	mustCount(t, n, err, 0, "failed before timeout")

	e.clock.Advance(time.Second)
	n, err = e.svc.FailStuckCreating(e.ctx)
	mustCount(t, n, err, 1, "failed at timeout")
	if got := e.get(inst.ID); got.State != rental.StateFailed {
		t.Fatalf("state = %s", got.State)
	}
	if got := e.balance(e.userID); got != startFunds {
		t.Fatalf("balance = %d, want refund", got)
	}
	// Agent 迟到的成功回报不能复活实例，也不能重复退款
	if err := e.svc.HandleCommandResult(e.ctx, cmds[0].ID, true, "late"); err != nil {
		t.Fatal(err)
	}
	if got := e.get(inst.ID); got.State != rental.StateFailed {
		t.Fatalf("after late ack state = %s", got.State)
	}
	if got := e.balance(e.userID); got != startFunds {
		t.Fatalf("after late ack balance = %d", got)
	}
}

func TestOfflineMachinePausesLeaseAndResumes(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true) // 计时从 T0 开始，到期 T0+24h

	e.clock.Advance(time.Hour)
	e.heartbeatAll()
	e.clock.Advance(10 * time.Minute) // 10 分钟无心跳
	if err := e.svc.Sweep(e.ctx); err != nil {
		t.Fatal(err)
	}
	got := e.get(inst.ID)
	frozen := 24*time.Hour - 70*time.Minute
	if !got.Suspended() || got.Remaining(e.clock.Now()) != frozen {
		t.Fatalf("after offline: suspended=%v remaining=%v, want %v", got.Suspended(), got.Remaining(e.clock.Now()), frozen)
	}

	e.clock.Advance(48 * time.Hour) // 失联期间远超原到期时间，也不能被停机
	if err := e.svc.Sweep(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.get(inst.ID); got.State != rental.StateRunning {
		t.Fatalf("state during outage = %s, want running", got.State)
	}

	came, err := e.fleet.Heartbeat(e.ctx, e.machines[0])
	if err != nil || !came {
		t.Fatalf("heartbeat came=%v err=%v", came, err)
	}
	n, err := e.svc.ResumeMachine(e.ctx, e.machines[0], []string{inst.ID})
	mustCount(t, n, err, 1, "resumed")
	got = e.get(inst.ID)
	if got.Suspended() || got.ExpiresAt == nil || !got.ExpiresAt.Equal(e.clock.Now().Add(frozen)) {
		t.Fatalf("after resume = %+v", got)
	}
	var alerts int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM alerts WHERE kind = 'machine_offline'`).Scan(&alerts)
	if alerts != 1 {
		t.Fatalf("machine_offline alerts = %d", alerts)
	}
}

func TestRemindersSentOncePerThreshold(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1w)
	e.ack(true)

	n, err := e.svc.SendReminders(e.ctx)
	mustCount(t, n, err, 0, "reminders at start")

	e.clock.Advance(6*24*time.Hour + time.Minute) // 剩余 23h59m
	n, err = e.svc.SendReminders(e.ctx)
	mustCount(t, n, err, 1, "24h reminder")
	n, err = e.svc.SendReminders(e.ctx)
	mustCount(t, n, err, 0, "24h reminder repeated")

	e.clock.Advance(22*time.Hour + 59*time.Minute) // 剩余 1h
	n, err = e.svc.SendReminders(e.ctx)
	mustCount(t, n, err, 1, "1h reminder")

	calls := e.notifier.Calls()
	if len(calls) != 2 || calls[0] != inst.ID+":23h59m0s" || calls[1] != inst.ID+":1h0m0s" {
		t.Fatalf("notifications = %v", calls)
	}
}

func TestDayLeaseOnlyGetsOneHourReminder(t *testing.T) {
	e := newEnv(t, 1)
	e.place(catalog.D1d)
	e.ack(true)
	n, err := e.svc.SendReminders(e.ctx)
	mustCount(t, n, err, 0, "reminder right after start")
	e.clock.Advance(23 * time.Hour)
	n, err = e.svc.SendReminders(e.ctx)
	mustCount(t, n, err, 1, "1h reminder")
}

func TestConcurrentExpireDoesNotDoubleStop(t *testing.T) {
	e := newEnv(t, 1)
	e.place(catalog.D1d)
	e.ack(true)
	e.clock.Advance(24 * time.Hour)

	var wg sync.WaitGroup
	var mu sync.Mutex
	total := 0
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := e.svc.ExpireDue(e.ctx)
			if err != nil {
				t.Errorf("ExpireDue: %v", err)
			}
			mu.Lock()
			total += n
			mu.Unlock()
		}()
	}
	wg.Wait()
	if total != 1 || countPending(e, e.machines[0], fleet.CmdStopInstance) != 1 {
		t.Fatalf("total stopped = %d", total)
	}
}

func TestSweepRepairsMissedSuspend(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	e.clock.Advance(time.Hour)
	e.heartbeatAll()
	e.clock.Advance(10 * time.Minute)
	// 离线转换已被消费，但暂停没有发生
	if off, err := e.fleet.MarkOfflineStale(e.ctx, rental.OfflineTimeout); err != nil || len(off) != 1 {
		t.Fatalf("mark offline = %v, %v", off, err)
	}
	if err := e.svc.Sweep(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.get(inst.ID); !got.Suspended() {
		t.Fatalf("instance not suspended: %+v", got)
	}
}

func TestSweepResumesOnlineSuspended(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	e.clock.Advance(time.Hour)
	frozen := 23 * time.Hour
	n, err := e.svc.SuspendMachines(e.ctx, e.machines)
	mustCount(t, n, err, 1, "suspended")
	e.clock.Advance(2 * time.Hour)
	e.heartbeatAll()
	// 没有 StateReport：清扫器不恢复（不知道容器是否正常）
	if err := e.svc.Sweep(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.get(inst.ID); !got.Suspended() {
		t.Fatalf("resumed without state report: %+v", got)
	}
	e.reportRunning(e.machines[0], inst.ID)
	if err := e.svc.Sweep(e.ctx); err != nil {
		t.Fatal(err)
	}
	got := e.get(inst.ID)
	if got.Suspended() || got.ExpiresAt == nil || !got.ExpiresAt.Equal(e.clock.Now().Add(frozen)) {
		t.Fatalf("after sweep = %+v", got)
	}
}

// 容器不在（报告为空）→ 保持暂停并告警一次；第二次报告变 running 才恢复。
func TestResumeMachineKeepsPausedWhenContainerDown(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	e.clock.Advance(time.Hour)
	mustCount(t, firstOf(e.svc.SuspendMachines(e.ctx, e.machines)), nil, 1, "suspended")
	for i := 0; i < 2; i++ {
		n, err := e.svc.ResumeMachine(e.ctx, e.machines[0], nil)
		mustCount(t, n, err, 0, "resumed with container down")
	}
	if got := e.get(inst.ID); !got.Suspended() {
		t.Fatalf("must stay suspended: %+v", got)
	}
	var alerts int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM alerts WHERE kind = 'container_down' AND instance_id = $1 AND message = $2`,
		inst.ID, rental.AlertContainerDown("SN000", inst.ID)).Scan(&alerts)
	if alerts != 1 {
		t.Fatalf("container_down alerts = %d, want exactly 1", alerts)
	}
	n, err := e.svc.ResumeMachine(e.ctx, e.machines[0], []string{"ins-other000", inst.ID})
	mustCount(t, n, err, 1, "resumed when running")
	if got := e.get(inst.ID); got.Suspended() {
		t.Fatalf("not resumed: %+v", got)
	}
}

// 清扫器兜底：用 last_state_report 里 running 的实例列表恢复；报告里没有的保持暂停并告警。
func TestSweepResumeUsesLastStateReport(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	e.clock.Advance(time.Hour)
	mustCount(t, firstOf(e.svc.SuspendMachines(e.ctx, e.machines)), nil, 1, "suspended")
	e.heartbeatAll()
	if err := e.fleet.AttachAgentConn(e.ctx, e.machines[0], "c1"); err != nil {
		t.Fatal(err)
	}
	if err := e.fleet.RecordStateReport(e.ctx, e.machines[0], fleet.StateReport{Instances: []fleet.InstanceState{{InstanceID: inst.ID, Phase: fleet.PhaseStopped}}}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Sweep(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.get(inst.ID); !got.Suspended() {
		t.Fatalf("stopped container must not resume: %+v", got)
	}
	var alerts int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM alerts WHERE kind = 'container_down'`).Scan(&alerts)
	if alerts != 1 {
		t.Fatalf("container_down alerts = %d", alerts)
	}
	e.reportRunning(e.machines[0], inst.ID)
	if err := e.svc.Sweep(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.get(inst.ID); got.Suspended() {
		t.Fatalf("not resumed: %+v", got)
	}
}

func TestFailTimedOutCommands(t *testing.T) {
	e := newEnv(t, 2)
	_, inst := e.place(catalog.D1d)
	if err := e.fleet.AttachAgentConn(e.ctx, e.machines[0], "c1"); err != nil {
		t.Fatal(err)
	}
	claimed, err := e.fleet.ClaimPending(e.ctx, []string{"c1"})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}
	e.clock.Advance(rental.CreateCommandTimeout - time.Second)
	e.heartbeatAll()
	mustCount(t, firstOf(e.svc.FailTimedOutCommands(e.ctx)), nil, 0, "before timeout")
	e.clock.Advance(time.Second)
	mustCount(t, firstOf(e.svc.FailTimedOutCommands(e.ctx)), nil, 1, "at timeout")
	got := e.get(inst.ID)
	if got.State != rental.StateFailed || e.balance(e.userID) != startFunds || e.machineState(e.machines[0]) != fleet.StateFaulty {
		t.Fatalf("after timeout: state=%s balance=%d machine=%s", got.State, e.balance(e.userID), e.machineState(e.machines[0]))
	}
	cmds, _ := e.fleet.CommandsForInstance(e.ctx, inst.ID, 10)
	if cmds[0].Status != "failed" || cmds[0].Result != "timeout after 15m0s" {
		t.Fatalf("command = %+v", cmds[0])
	}
	var reason string
	_ = e.pool.QueryRow(e.ctx, `SELECT reason FROM instance_events WHERE instance_id = $1 AND to_state = 'failed'`, inst.ID).Scan(&reason)
	if reason != "开通失败: timeout after 15m0s" {
		t.Fatalf("event reason = %q", reason)
	}

	// 离线机器的 sent 指令不计时；stop 超时 5 分钟 → stop_failed 告警
	_, inst2 := e.place(catalog.D1d)
	e.ack(true)
	e.clock.Advance(24 * time.Hour)
	e.heartbeatAll()
	mustCount(t, firstOf(e.svc.ExpireDue(e.ctx)), nil, 1, "expired")
	_ = e.fleet.AttachAgentConn(e.ctx, e.machines[1], "c2")
	if claimed, _ := e.fleet.ClaimPending(e.ctx, []string{"c2"}); len(claimed) != 1 || claimed[0].Type != fleet.CmdStopInstance {
		t.Fatalf("claimed stop = %+v", claimed)
	}
	e.clock.Advance(rental.StopCommandTimeout + time.Minute)
	if _, err := e.fleet.MarkOfflineStale(e.ctx, rental.OfflineTimeout); err != nil {
		t.Fatal(err)
	}
	mustCount(t, firstOf(e.svc.FailTimedOutCommands(e.ctx)), nil, 0, "offline machine must not time out")
	e.heartbeatAll()
	mustCount(t, firstOf(e.svc.FailTimedOutCommands(e.ctx)), nil, 1, "stop timed out once online")
	var alerts int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM alerts WHERE kind = 'stop_failed' AND instance_id = $1`, inst2.ID).Scan(&alerts)
	if alerts != 1 || e.get(inst2.ID).State != rental.StateStopped {
		t.Fatalf("stop timeout: alerts=%d state=%s", alerts, e.get(inst2.ID).State)
	}
	if got := rental.CommandTimeout(fleet.CmdWipeInstance); got != 30*time.Minute {
		t.Fatalf("wipe timeout = %v", got)
	}
	if got := rental.CommandTimeout(fleet.CmdPullImage); got != 60*time.Minute {
		t.Fatalf("pull timeout = %v", got)
	}
}

// 指令 sent 后机器离线超过超时时长再重连：重连（AttachAgentConn）重启超时计时，补推之前清扫不得判超时（规格 §3.1.5）。
func TestReconnectRestartsCommandTimeout(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	if err := e.fleet.AttachAgentConn(e.ctx, e.machines[0], "c1"); err != nil {
		t.Fatal(err)
	}
	if claimed, err := e.fleet.ClaimPending(e.ctx, []string{"c1"}); err != nil || len(claimed) != 1 {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}
	e.clock.Advance(rental.CreateCommandTimeout + time.Hour)
	if _, err := e.fleet.MarkOfflineStale(e.ctx, rental.OfflineTimeout); err != nil {
		t.Fatal(err)
	}
	// 重连：先登记归属，再心跳上线（与网关 Connect 顺序一致），补推尚未发生
	if err := e.fleet.AttachAgentConn(e.ctx, e.machines[0], "c2"); err != nil {
		t.Fatal(err)
	}
	e.heartbeatAll()
	mustCount(t, firstOf(e.svc.FailTimedOutCommands(e.ctx)), nil, 0, "reconnect window")
	e.clock.Advance(rental.CreateCommandTimeout - time.Second)
	e.heartbeatAll()
	mustCount(t, firstOf(e.svc.FailTimedOutCommands(e.ctx)), nil, 0, "before timeout counted from reconnect")
	e.clock.Advance(time.Second)
	mustCount(t, firstOf(e.svc.FailTimedOutCommands(e.ctx)), nil, 1, "timeout counted from reconnect")
	if got := e.get(inst.ID); got.State != rental.StateFailed {
		t.Fatalf("state = %s", got.State)
	}
}

// 流在离线期间没断（未重连）：离线前的 StateReport 不能被清扫器用来恢复计时。
func TestSweepDoesNotResumeFromPreOfflineReport(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	e.reportRunning(e.machines[0], inst.ID)
	e.clock.Advance(rental.OfflineTimeout + time.Minute)
	if err := e.svc.Sweep(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.get(inst.ID); !got.Suspended() {
		t.Fatalf("must be suspended while offline: %+v", got)
	}
	e.heartbeatAll() // 同一条流恢复心跳，尚未发来新的 StateReport
	if err := e.svc.Sweep(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.get(inst.ID); !got.Suspended() {
		t.Fatalf("resumed from a pre-offline report: %+v", got)
	}
	if err := e.fleet.RecordStateReport(e.ctx, e.machines[0], fleet.StateReport{Instances: []fleet.InstanceState{{InstanceID: inst.ID, Phase: fleet.PhaseRunning}}}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Sweep(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.get(inst.ID); got.Suspended() {
		t.Fatalf("fresh report must resume: %+v", got)
	}
}

// 暂停期间续费：新租期的提醒标记按续费后的剩余时长重算，不能沿用旧租期已发过的标记。
func TestRenewWhileSuspendedResetsReminders(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	e.clock.Advance(23 * time.Hour)
	n, err := e.svc.SendReminders(e.ctx)
	mustCount(t, n, err, 1, "1h reminder")
	n, err = e.svc.SuspendMachines(e.ctx, e.machines)
	mustCount(t, n, err, 1, "suspended")
	if _, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1m, ""); err != nil {
		t.Fatal(err)
	}
	e.heartbeatAll()
	n, err = e.svc.ResumeMachine(e.ctx, e.machines[0], []string{inst.ID})
	mustCount(t, n, err, 1, "resumed")
	got := e.get(inst.ID)
	if want := e.clock.Now().Add(30*24*time.Hour + time.Hour); !got.ExpiresAt.Equal(want) {
		t.Fatalf("expires = %v, want %v", got.ExpiresAt, want)
	}
	n, err = e.svc.SendReminders(e.ctx)
	mustCount(t, n, err, 0, "reminders right after resume")
	e.clock.Advance(29*24*time.Hour + time.Hour) // 距新到期 24 小时
	n, err = e.svc.SendReminders(e.ctx)
	mustCount(t, n, err, 1, "24h reminder for the new term")
}

// failEventsFor 让写入某实例状态事件时报错，用来模拟清扫中单行处理失败。
func failEventsFor(e *env, instanceID string) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, `
		CREATE FUNCTION fail_events() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.instance_id = '`+instanceID+`' THEN RAISE EXCEPTION 'injected failure'; END IF;
			RETURN NEW;
		END $$;
		CREATE TRIGGER fail_events BEFORE INSERT ON instance_events FOR EACH ROW EXECUTE FUNCTION fail_events();`); err != nil {
		e.t.Fatal(err)
	}
}

// 清扫逐行独立事务：一行失败不影响同批其他行。
func TestSweepRowFailureDoesNotBlockOthers(t *testing.T) {
	e := newEnv(t, 2)
	_, bad := e.place(catalog.D1d)
	_, good := e.place(catalog.D1d)
	e.ack(true)
	e.clock.Advance(24 * time.Hour)
	failEventsFor(e, bad.ID)

	n, err := e.svc.ExpireDue(e.ctx)
	if err == nil || n != 1 {
		t.Fatalf("ExpireDue = %d, %v; want 1 and an error", n, err)
	}
	if got := e.get(good.ID); got.State != rental.StateStopped {
		t.Fatalf("good instance = %s, want stopped", got.State)
	}
	if got := e.get(bad.ID); got.State != rental.StateRunning {
		t.Fatalf("bad instance = %s, want running (rolled back)", got.State)
	}

	e.clock.Advance(rental.RetainAfterStop)
	n, err = e.svc.WipeStale(e.ctx)
	mustCount(t, n, err, 1, "wipe good despite bad row")
}

func TestFailStuckCreatingRowFailureDoesNotBlockOthers(t *testing.T) {
	e := newEnv(t, 2)
	_, bad := e.place(catalog.D1d)
	_, good := e.place(catalog.D1d)
	e.clock.Advance(rental.CreateTimeout)
	failEventsFor(e, bad.ID)
	n, err := e.svc.FailStuckCreating(e.ctx)
	if err == nil || n != 1 {
		t.Fatalf("FailStuckCreating = %d, %v; want 1 and an error", n, err)
	}
	if got := e.get(good.ID); got.State != rental.StateFailed {
		t.Fatalf("good instance = %s, want failed", got.State)
	}
	if got := e.get(bad.ID); got.State != rental.StateCreating {
		t.Fatalf("bad instance = %s, want creating", got.State)
	}
}

func TestRunSweeperRunsExtraTasks(t *testing.T) {
	e := newEnv(t, 0)
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	var calls []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.svc.RunSweeper(ctx, time.Hour,
			func(context.Context) error { calls = append(calls, "a"); return errors.New("boom") },
			func(context.Context) error { calls = append(calls, "b"); cancel(); return nil })
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("RunSweeper did not stop")
	}
	if len(calls) != 2 || calls[0] != "a" || calls[1] != "b" {
		t.Fatalf("calls = %v (a failing task must not stop later tasks)", calls)
	}
}

func TestRunSweeperExtraTaskTimesOut(t *testing.T) {
	e := newEnv(t, 0)
	defer rental.SetExtraTaskTimeout(50 * time.Millisecond)()
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	var taskErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.svc.RunSweeper(ctx, time.Hour,
			func(c context.Context) error { <-c.Done(); taskErr = c.Err(); return c.Err() },
			func(context.Context) error { cancel(); return nil })
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a hung extra task blocked the sweeper")
	}
	if !errors.Is(taskErr, context.DeadlineExceeded) {
		t.Fatalf("hung task ctx err = %v, want deadline exceeded", taskErr)
	}
}

// 恢复后按冻结剩余时长推算的到期若越过同机下一条预订，到期封顶于 lease 上界 − Tail（与 refreshLease 的封顶一致）。
func TestResumeMachineCapsExpiryAtNextBooking(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)                                                     // 到期 T0+24h，lease 上界 T0+31h
	e.reserveRaw(e.machines[0], dbtest.T0.Add(33*time.Hour), 86400) // 下一条预订的 lease 下界 T0+32h

	e.clock.Advance(time.Hour)
	e.heartbeatAll()
	e.clock.Advance(10 * time.Minute)
	if err := e.svc.Sweep(e.ctx); err != nil {
		t.Fatal(err)
	}
	if !e.get(inst.ID).Suspended() {
		t.Fatal("not suspended")
	}
	e.clock.Advance(5 * time.Hour) // 冻结 22h50m；恢复后按 now + 22h50m = T0+29h 到期，+Tail 越过 T0+32h
	if _, err := e.fleet.Heartbeat(e.ctx, e.machines[0]); err != nil {
		t.Fatal(err)
	}
	balance := e.balance(e.userID)
	n, err := e.svc.ResumeMachine(e.ctx, e.machines[0], []string{inst.ID})
	mustCount(t, n, err, 1, "resumed")
	got := e.get(inst.ID)
	wantExpiry := dbtest.T0.Add(32*time.Hour - rental.Tail)
	// 封顶吃掉 T0+29h − T0+25h = 4h：故障停机是平台原因，按单价补偿退回余额
	if refund := int64(4*3600) * 6900 / 86400; e.balance(e.userID) != balance+refund {
		t.Fatalf("balance = %d, want compensation %d", e.balance(e.userID), refund)
	}
	if ev := e.lastEvent(inst.ID); ev.Reason != "resume capped compensated: 14400s refund 1150" {
		t.Fatalf("last event = %q", ev.Reason)
	}
	if evs, err := e.svc.UserEvents(e.ctx, e.userID, inst.ID); err != nil || evs[0].Kind != "compensated" || evs[0].Detail != "故障恢复后可用时长不足，补偿已退回余额" {
		t.Fatalf("user events = %+v, %v", evs, err)
	}
	if got.Suspended() || got.ExpiresAt == nil || !got.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("expires_at = %v, want %v", got.ExpiresAt, wantExpiry)
	}
	if _, upper, _ := e.lease(inst.ID); !upper.Equal(dbtest.T0.Add(32 * time.Hour)) {
		t.Fatalf("lease upper = %v", upper)
	}
}

// 同一轮 Sweep：到期前 1 小时的自动续费先于到期停机；到点的预订在同一轮里开通。
func TestSweepOrderProvisionsAndAutoRenewsBeforeExpiry(t *testing.T) {
	e := newEnv(t, 2)
	_, running := e.place(catalog.D1d)
	e.ack(true)
	e.autoRenewOn(running.ID, catalog.D1d)
	booked := e.book(dbtest.T0.Add(25 * time.Hour)) // 开通时刻 T0 + 24h
	e.clock.Advance(24 * time.Hour)                   // running 恰好到期；自动续费本该在 1 小时前触发，这里让两者同一轮
	e.heartbeatAll()
	if err := e.svc.Sweep(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.get(running.ID); got.State != rental.StateRunning || !got.ExpiresAt.Equal(dbtest.T0.Add(48*time.Hour)) {
		t.Fatalf("auto renew must run before expiry: %+v", got)
	}
	if got := e.get(booked.ID); got.State != rental.StateCreating {
		t.Fatalf("booking must be provisioned in the same sweep: %+v", got)
	}
}
