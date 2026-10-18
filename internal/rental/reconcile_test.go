package rental_test

import (
	"testing"
	"time"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/rental"
)

func report(states ...fleet.InstanceState) fleet.StateReport {
	return fleet.StateReport{Instances: states}
}

// alertCount 统计告警；kind 或 instanceID 为空串表示不按该项筛选。
func (e *env) alertCount(kind, instanceID string) int {
	e.t.Helper()
	var n int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM alerts WHERE ($1 = '' OR kind = $1) AND ($2 = '' OR instance_id = $2)`, kind, instanceID).Scan(&n)
	return n
}

func (e *env) openCommands(instanceID string, t fleet.CommandType) int {
	e.t.Helper()
	var n int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM agent_commands WHERE instance_id = $1 AND type = $2 AND status IN ('pending', 'sent')`, instanceID, string(t)).Scan(&n)
	return n
}

func TestReconcileOrphanContainerEnqueuesWipeOnce(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	_ = e.svc.Release(e.ctx, e.userID, inst.ID)
	e.ack(true) // released，机器 idle
	for i := 0; i < 2; i++ {
		if err := e.svc.Reconcile(e.ctx, e.machines[0], report(fleet.InstanceState{InstanceID: inst.ID, Phase: fleet.PhaseRunning})); err != nil {
			t.Fatal(err)
		}
	}
	if e.openCommands(inst.ID, fleet.CmdWipeInstance) != 1 || e.alertCount("orphan_container", inst.ID) != 1 {
		t.Fatalf("orphan: wipes=%d alerts=%d", e.openCommands(inst.ID, fleet.CmdWipeInstance), e.alertCount("orphan_container", inst.ID))
	}
	var msg, level string
	_ = e.pool.QueryRow(e.ctx, `SELECT message, level FROM alerts WHERE kind = 'orphan_container'`).Scan(&msg, &level)
	if msg != rental.AlertOrphanContainer("SN000", inst.ID) || level != "warning" {
		t.Fatalf("alert = %s %s", level, msg)
	}
	// 平台从未见过的 ID 同样清理
	if err := e.svc.Reconcile(e.ctx, e.machines[0], report(fleet.InstanceState{InstanceID: "ins-zzzzzzzz", Phase: fleet.PhaseStopped})); err != nil {
		t.Fatal(err)
	}
	if e.openCommands("ins-zzzzzzzz", fleet.CmdWipeInstance) != 1 {
		t.Fatal("unknown instance not wiped")
	}
}

// 别的机器上的实例出现在这台机器的报告里 → 也是孤儿。
func TestReconcileInstanceOfOtherMachineIsOrphan(t *testing.T) {
	e := newEnv(t, 2)
	_, inst := e.place(catalog.D1d) // 分配到 machines[0]
	e.ack(true)
	if err := e.svc.Reconcile(e.ctx, e.machines[1], report(fleet.InstanceState{InstanceID: inst.ID, Phase: fleet.PhaseRunning})); err != nil {
		t.Fatal(err)
	}
	var machineID int64
	_ = e.pool.QueryRow(e.ctx, `SELECT machine_id FROM agent_commands WHERE instance_id = $1 AND type = 'wipe_instance'`, inst.ID).Scan(&machineID)
	if machineID != e.machines[1] || e.alertCount("orphan_container", inst.ID) != 1 || e.get(inst.ID).State != rental.StateRunning {
		t.Fatalf("wipe on machine %d, alerts=%d, state=%s", machineID, e.alertCount("orphan_container", inst.ID), e.get(inst.ID).State)
	}
}

func TestReconcileInvalidInstanceIDIsAlertedAndSkipped(t *testing.T) {
	e := newEnv(t, 1)
	bad := report(fleet.InstanceState{InstanceID: "../../etc/passwd", Phase: fleet.PhaseRunning}, fleet.InstanceState{InstanceID: "INS-ABCDEFGH", Phase: fleet.PhaseRunning})
	for i := 0; i < 2; i++ {
		if err := e.svc.Reconcile(e.ctx, e.machines[0], bad); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM agent_commands`).Scan(&n)
	if n != 0 || e.alertCount("invalid_report", "") != 1 {
		t.Fatalf("commands=%d alerts=%d", n, e.alertCount("invalid_report", ""))
	}
}

func TestReconcileRunningWhileStoppedEnqueuesStop(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	e.clock.Advance(24 * time.Hour)
	e.heartbeatAll()
	mustCount(t, firstOf(e.svc.ExpireDue(e.ctx)), nil, 1, "expired")
	// 到期的 stop 指令仍 pending → 有未完成指令，不重复入队
	if err := e.svc.Reconcile(e.ctx, e.machines[0], report(fleet.InstanceState{InstanceID: inst.ID, Phase: fleet.PhaseRunning})); err != nil {
		t.Fatal(err)
	}
	if e.openCommands(inst.ID, fleet.CmdStopInstance) != 1 {
		t.Fatalf("stop commands = %d", e.openCommands(inst.ID, fleet.CmdStopInstance))
	}
	e.ack(true) // stop 完成
	// 容器又跑起来了（例如管理员手工 docker start）→ 入队 stop
	if err := e.svc.Reconcile(e.ctx, e.machines[0], report(fleet.InstanceState{InstanceID: inst.ID, Phase: fleet.PhaseRunning})); err != nil {
		t.Fatal(err)
	}
	if e.openCommands(inst.ID, fleet.CmdStopInstance) != 1 || e.get(inst.ID).State != rental.StateStopped {
		t.Fatalf("stop enqueued = %d", e.openCommands(inst.ID, fleet.CmdStopInstance))
	}
	// 报告 stopped 与记录一致 → 无动作
	if err := e.svc.Reconcile(e.ctx, e.machines[0], report(fleet.InstanceState{InstanceID: inst.ID, Phase: fleet.PhaseStopped})); err != nil {
		t.Fatal(err)
	}
	if e.openCommands(inst.ID, fleet.CmdStopInstance) != 1 {
		t.Fatal("stopped/stopped must not enqueue")
	}
}

func TestReconcileContainerDownAlertsWithoutAction(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	for _, rep := range []fleet.StateReport{report(fleet.InstanceState{InstanceID: inst.ID, Phase: fleet.PhaseStopped}), report()} {
		if err := e.svc.Reconcile(e.ctx, e.machines[0], rep); err != nil {
			t.Fatal(err)
		}
	}
	var cmds int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM agent_commands WHERE status IN ('pending','sent')`).Scan(&cmds)
	got := e.get(inst.ID)
	if cmds != 0 || got.State != rental.StateRunning || got.Suspended() || e.alertCount("container_down", inst.ID) != 1 {
		t.Fatalf("container down: cmds=%d state=%s alerts=%d", cmds, got.State, e.alertCount("container_down", inst.ID))
	}
	// 暂停中的实例由 ResumeMachine 负责告警，Reconcile 不重复
	mustCount(t, firstOf(e.svc.SuspendMachines(e.ctx, e.machines)), nil, 1, "suspended")
	if err := e.svc.Reconcile(e.ctx, e.machines[0], report()); err != nil {
		t.Fatal(err)
	}
	if e.alertCount("container_down", inst.ID) != 1 {
		t.Fatal("suspended instance alerted again by reconcile")
	}
}

func TestReconcileMissingAndMissingData(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	if err := e.svc.Reconcile(e.ctx, e.machines[0], report(fleet.InstanceState{InstanceID: inst.ID, Phase: fleet.PhaseMissingData})); err != nil {
		t.Fatal(err)
	}
	if e.alertCount("data_disk_missing", inst.ID) != 1 {
		t.Fatal("data_disk_missing not raised")
	}
	e.clock.Advance(24 * time.Hour)
	e.heartbeatAll()
	mustCount(t, firstOf(e.svc.ExpireDue(e.ctx)), nil, 1, "expired")
	e.ack(true)
	if err := e.svc.Reconcile(e.ctx, e.machines[0], report()); err != nil {
		t.Fatal(err)
	}
	if e.alertCount("container_missing", inst.ID) != 1 || e.get(inst.ID).State != rental.StateStopped {
		t.Fatalf("container_missing = %d", e.alertCount("container_missing", inst.ID))
	}
	var msg string
	_ = e.pool.QueryRow(e.ctx, `SELECT message FROM alerts WHERE kind = 'container_missing'`).Scan(&msg)
	if msg != rental.AlertContainerMissing("SN000", inst.ID) {
		t.Fatalf("message = %s", msg)
	}
}

func TestReconcileCreatingWipingAndUnknownUntouched(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d) // creating，create 指令 pending
	for _, rep := range []fleet.StateReport{report(), report(fleet.InstanceState{InstanceID: inst.ID, Phase: fleet.PhaseRunning}), report(fleet.InstanceState{InstanceID: inst.ID, Phase: fleet.PhaseUnknown})} {
		if err := e.svc.Reconcile(e.ctx, e.machines[0], rep); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM agent_commands`).Scan(&n)
	if n != 1 || e.alertCount("", "") != 0 {
		t.Fatalf("creating: commands=%d alerts=%d", n, e.alertCount("", ""))
	}
	e.ack(true)
	_ = e.svc.Release(e.ctx, e.userID, inst.ID) // wiping
	if err := e.svc.Reconcile(e.ctx, e.machines[0], report()); err != nil {
		t.Fatal(err)
	}
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM agent_commands WHERE type = 'wipe_instance'`).Scan(&n)
	if n != 1 || e.alertCount("", "") != 0 {
		t.Fatalf("wiping: wipes=%d alerts=%d", n, e.alertCount("", ""))
	}
}
