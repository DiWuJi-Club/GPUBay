package rental_test

import (
	"strings"
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
	e := newEnv(t, 2)
	_, a := e.place(catalog.D1d)
	_, b := e.place(catalog.D1d)
	e.ack(true)
	ma, mb := e.get(a.ID).MachineID, e.get(b.ID).MachineID
	if ma == mb {
		t.Fatalf("instances share machine %d", ma)
	}
	// a：上报 stopped；b：整个没上报——两条分支各自断言
	if err := e.svc.Reconcile(e.ctx, ma, report(fleet.InstanceState{InstanceID: a.ID, Phase: fleet.PhaseStopped})); err != nil {
		t.Fatal(err)
	}
	if e.alertCount("container_down", a.ID) != 1 || e.alertCount("container_down", b.ID) != 0 {
		t.Fatalf("after reported-stopped: a=%d b=%d", e.alertCount("container_down", a.ID), e.alertCount("container_down", b.ID))
	}
	if err := e.svc.Reconcile(e.ctx, mb, report()); err != nil {
		t.Fatal(err)
	}
	if e.alertCount("container_down", b.ID) != 1 {
		t.Fatal("missing container not alerted")
	}
	var cmds int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM agent_commands WHERE status IN ('pending','sent')`).Scan(&cmds)
	for _, id := range []string{a.ID, b.ID} {
		if got := e.get(id); cmds != 0 || got.State != rental.StateRunning || got.Suspended() {
			t.Fatalf("instance %s: cmds=%d state=%s", id, cmds, got.State)
		}
	}
	// 暂停中的实例由 ResumeMachine 负责告警，Reconcile 不重复（用新的告警文案路径：先清掉再看不新增）
	mustCount(t, firstOf(e.svc.SuspendMachines(e.ctx, e.machines)), nil, 2, "suspended")
	for _, m := range []int64{ma, mb} {
		if err := e.svc.Reconcile(e.ctx, m, report()); err != nil {
			t.Fatal(err)
		}
	}
	if e.alertCount("container_down", "") != 2 {
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

// 实例在原机器 A 上洗机失败（仍 wiping、A 故障）；机器 B 上报了它的容器 → B 上的孤儿清理成功也不能释放 A。
func TestReconcileOrphanWipeOnOtherMachineDoesNotReleaseWipingInstance(t *testing.T) {
	for _, ok := range []bool{true, false} {
		e := newEnv(t, 2)
		_, inst := e.place(catalog.D1d)
		e.ack(true)
		ma := e.get(inst.ID).MachineID
		mb := e.machines[0]
		if mb == ma {
			mb = e.machines[1]
		}
		_ = e.svc.Release(e.ctx, e.userID, inst.ID)
		e.ack(false) // A 上洗机失败
		if e.get(inst.ID).State != rental.StateWiping || e.machineState(ma) != fleet.StateFaulty {
			t.Fatalf("setup: state=%s machine=%s", e.get(inst.ID).State, e.machineState(ma))
		}
		if err := e.svc.Reconcile(e.ctx, mb, report(fleet.InstanceState{InstanceID: inst.ID, Phase: fleet.PhaseRunning})); err != nil {
			t.Fatal(err)
		}
		e.ack(ok)
		if e.get(inst.ID).State != rental.StateWiping || e.machineState(ma) != fleet.StateFaulty {
			t.Fatalf("ok=%v: state=%s machineA=%s", ok, e.get(inst.ID).State, e.machineState(ma))
		}
		failed := e.alertCount("orphan_wipe_failed", inst.ID)
		if (ok && failed != 0) || (!ok && failed != 1) || e.machineState(mb) == fleet.StateFaulty {
			t.Fatalf("ok=%v: orphan_wipe_failed=%d machineB=%s", ok, failed, e.machineState(mb))
		}
	}
}

// 系统实例（Token 服务节点）的容器不在：只发 warning「平台节点容器异常，节点将自动重建」（llm Controller 重建节点），
// 不发 critical「换机或退款」——上报 stopped、整个没上报、机器恢复时容器不在三条路径都一样。
func TestSystemInstanceContainerDownIsWarning(t *testing.T) {
	e := newEnv(t, 2)
	_, a := e.place(catalog.D1d)
	_, b := e.place(catalog.D1d)
	e.ack(true)
	if _, err := e.pool.Exec(e.ctx, `UPDATE instances SET system = true WHERE id IN ($1, $2)`, a.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	ma, mb := e.get(a.ID).MachineID, e.get(b.ID).MachineID
	serial := func(m int64) string {
		var s string
		if err := e.pool.QueryRow(e.ctx, `SELECT serial FROM machines WHERE id = $1`, m).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	expect := func(what, instID string, m int64) {
		t.Helper()
		var level, msg string
		var n int
		if err := e.pool.QueryRow(e.ctx, `SELECT count(*), min(level), min(message) FROM alerts WHERE kind = 'container_down' AND instance_id = $1`,
			instID).Scan(&n, &level, &msg); err != nil {
			t.Fatal(err)
		}
		if n != 1 || level != "warning" || msg != rental.AlertSystemContainerDown(serial(m), instID) || !strings.Contains(msg, "平台节点容器异常，节点将自动重建") {
			t.Fatalf("%s: alerts = %d level %q message %q", what, n, level, msg)
		}
	}
	if err := e.svc.Reconcile(e.ctx, ma, report(fleet.InstanceState{InstanceID: a.ID, Phase: fleet.PhaseStopped})); err != nil {
		t.Fatal(err)
	}
	expect("reported stopped", a.ID, ma)
	if err := e.svc.Reconcile(e.ctx, mb, report()); err != nil {
		t.Fatal(err)
	}
	expect("missing from report", b.ID, mb)
	if n := e.alertCount("", ""); n != 2 {
		t.Fatalf("alerts = %d, want 2 (no critical alert)", n)
	}
	// 机器离线后恢复、容器不在
	if _, err := e.pool.Exec(e.ctx, `DELETE FROM alerts`); err != nil {
		t.Fatal(err)
	}
	mustCount(t, firstOf(e.svc.SuspendMachines(e.ctx, e.machines)), nil, 2, "suspended")
	n, err := e.svc.ResumeMachine(e.ctx, ma, nil)
	mustCount(t, n, err, 0, "resumed with container down")
	expect("resume with container down", a.ID, ma)
}
