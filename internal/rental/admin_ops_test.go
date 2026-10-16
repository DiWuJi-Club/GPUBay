package rental_test

import (
	"errors"
	"testing"
	"time"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/rental"
)

var opsActor = audit.Actor{AdminID: 5, IP: "10.0.0.5"}

func codeOf(err error) string {
	if ae := apperr.As(err); ae != nil {
		return ae.Code
	}
	if err != nil {
		return "raw: " + err.Error()
	}
	return ""
}

func (e *env) auditCount(action string) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM audit_logs WHERE action = $1 AND actor_id = $2`, action, opsActor.AdminID).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestAdminExtendRunning(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	e.clock.Advance(23*time.Hour + 30*time.Minute) // 剩余 30 分钟
	got, err := e.svc.AdminExtend(e.ctx, opsActor, inst.ID, 2*3600, "机器故障补偿")
	if err != nil {
		t.Fatal(err)
	}
	if !got.ExpiresAt.Equal(dbtest.T0.Add(26*time.Hour)) || got.Reminded1h || !got.Reminded24h {
		t.Fatalf("instance = %+v", got)
	}
	if b := e.balance(e.userID); b != startFunds-6900 {
		t.Fatalf("extend must be free, balance = %d", b)
	}
	var orders int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM orders WHERE instance_id = $1`, inst.ID).Scan(&orders)
	if orders != 1 || e.auditCount("instance.extend") != 1 {
		t.Fatalf("orders = %d audit = %d", orders, e.auditCount("instance.extend"))
	}
}

func TestAdminExtendSuspendedAddsToFrozenTime(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	if _, err := e.svc.SuspendMachines(e.ctx, e.machines); err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.AdminExtend(e.ctx, opsActor, inst.ID, 600, "补偿")
	if err != nil || got.Remaining(e.clock.Now()) != 24*time.Hour+10*time.Minute {
		t.Fatalf("remaining = %v, %v", got.Remaining(e.clock.Now()), err)
	}
}

func TestAdminExtendCreatingAddsPending(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	got, err := e.svc.AdminExtend(e.ctx, opsActor, inst.ID, 3600, "补偿")
	if err != nil || got.PendingSeconds != 86400+3600 {
		t.Fatalf("pending = %d, %v", got.PendingSeconds, err)
	}
}

func TestAdminExtendStoppedRestarts(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	expireAndStop(e)
	if _, err := e.svc.AdminExtend(e.ctx, opsActor, inst.ID, 3600, "补偿"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AdminExtend(e.ctx, opsActor, inst.ID, 600, "再补偿"); err != nil {
		t.Fatal(err)
	}
	if n := countPending(e, e.machines[0], fleet.CmdStartInstance); n != 1 {
		t.Fatalf("start commands = %d, want 1", n)
	}
	e.ack(true)
	got := e.get(inst.ID)
	if got.State != rental.StateRunning || got.Remaining(e.clock.Now()) != 70*time.Minute {
		t.Fatalf("after restart: state=%s remaining=%v", got.State, got.Remaining(e.clock.Now()))
	}
}

func TestAdminExtendValidation(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	for _, c := range []struct {
		secs   int64
		reason string
	}{{59, "x"}, {rental.MaxExtendSeconds + 1, "x"}, {3600, "  "}} {
		if _, err := e.svc.AdminExtend(e.ctx, opsActor, inst.ID, c.secs, c.reason); codeOf(err) != "bad_request" {
			t.Fatalf("extend(%d, %q) = %v", c.secs, c.reason, err)
		}
	}
	if _, err := e.svc.AdminExtend(e.ctx, opsActor, "ins-missing", 3600, "x"); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
	e.ack(true)
	if err := e.svc.Release(e.ctx, e.userID, inst.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AdminExtend(e.ctx, opsActor, inst.ID, 3600, "x"); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("wiping = %v", err)
	}
}

func TestAdminRewipeAfterWipeFailure(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	if err := e.svc.AdminRewipe(e.ctx, opsActor, inst.ID); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("rewipe running = %v", err)
	}
	if err := e.svc.Release(e.ctx, e.userID, inst.ID); err != nil {
		t.Fatal(err)
	}
	e.ack(false) // 洗机失败：机器隔离为 faulty
	if e.machineState(e.machines[0]) != fleet.StateFaulty || e.get(inst.ID).State != rental.StateWiping {
		t.Fatalf("after wipe failure: machine=%s instance=%s", e.machineState(e.machines[0]), e.get(inst.ID).State)
	}
	// 结束维护前必须先处理完实例
	if err := e.svc.SetMachineMaintenance(e.ctx, opsActor, e.machines[0], false); codeOf(err) != "invalid_state" {
		t.Fatalf("end maintenance with wiping instance = %v", err)
	}
	if err := e.svc.AdminRewipe(e.ctx, opsActor, inst.ID); err != nil {
		t.Fatal(err)
	}
	if n := countPending(e, e.machines[0], fleet.CmdWipeInstance); n != 1 {
		t.Fatalf("wipe commands = %d", n)
	}
	e.ack(true)
	if got := e.get(inst.ID); got.State != rental.StateReleased {
		t.Fatalf("state = %s", got.State)
	}
	if e.machineState(e.machines[0]) != fleet.StateFaulty {
		t.Fatal("machine must stay faulty until ops ends maintenance")
	}
	if err := e.svc.SetMachineMaintenance(e.ctx, opsActor, e.machines[0], false); err != nil {
		t.Fatal(err)
	}
	if e.machineState(e.machines[0]) != fleet.StateIdle || e.auditCount("instance.rewipe") != 1 || e.auditCount("machine.maintenance") != 1 {
		t.Fatalf("machine=%s rewipe audit=%d maintenance audit=%d", e.machineState(e.machines[0]),
			e.auditCount("instance.rewipe"), e.auditCount("machine.maintenance"))
	}
}

func TestSetMachineMaintenanceOn(t *testing.T) {
	e := newEnv(t, 1)
	if err := e.svc.SetMachineMaintenance(e.ctx, opsActor, e.machines[0], true); err != nil {
		t.Fatal(err)
	}
	if e.machineState(e.machines[0]) != fleet.StateMaintenance {
		t.Fatal("not in maintenance")
	}
	if err := e.svc.SetMachineMaintenance(e.ctx, opsActor, 9999, true); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("unknown machine = %v", err)
	}
}
