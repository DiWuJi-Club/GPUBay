package rental_test

import (
	"encoding/json"
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

func TestAdminMigrateMovesRemainingTime(t *testing.T) {
	e := newEnv(t, 2)
	_, old := e.place(catalog.D1d)
	e.ack(true)
	e.clock.Advance(2 * time.Hour)
	balance := e.balance(e.userID)

	moved, err := e.svc.AdminMigrate(e.ctx, opsActor, old.ID, "GPU 报错")
	if err != nil {
		t.Fatal(err)
	}
	if moved.ID == old.ID || moved.MachineID != e.machines[1] || moved.State != rental.StateCreating ||
		moved.PendingSeconds != 22*3600 || moved.UserID != e.userID || moved.ImageID != old.ImageID {
		t.Fatalf("new instance = %+v", moved)
	}
	if got := e.get(old.ID); got.State != rental.StateWiping {
		t.Fatalf("old state = %s", got.State)
	}
	if e.balance(e.userID) != balance {
		t.Fatal("migration must be free")
	}
	var orders, transfers int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM orders WHERE user_id = $1`, e.userID).Scan(&orders)
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM instance_transfers WHERE from_instance_id = $1 AND to_instance_id = $2 AND seconds = 79200`,
		old.ID, moved.ID).Scan(&transfers)
	if orders != 1 || transfers != 1 || e.auditCount("instance.migrate") != 1 {
		t.Fatalf("orders=%d transfers=%d audit=%d", orders, transfers, e.auditCount("instance.migrate"))
	}
	cmds, _ := e.fleet.PendingCommands(e.ctx, e.machines[1])
	var p fleet.CreateInstancePayload
	if len(cmds) != 1 || cmds[0].Type != fleet.CmdCreateInstance || json.Unmarshal(cmds[0].Payload, &p) != nil || p.ImageRef == "" ||
		p.SSHPort != testSSHBasePort || p.ShareSlots != 1 {
		t.Fatalf("create command = %+v", cmds)
	}

	e.clock.Advance(time.Minute)
	e.ack(true) // 新实例开通、原实例洗机完成
	if got := e.get(moved.ID); got.State != rental.StateRunning || got.Remaining(e.clock.Now()) != 22*time.Hour {
		t.Fatalf("moved after create: state=%s remaining=%v", got.State, got.Remaining(e.clock.Now()))
	}
	if e.get(old.ID).State != rental.StateReleased || e.machineState(e.machines[0]) != fleet.StateIdle {
		t.Fatalf("old not released: %s / %s", e.get(old.ID).State, e.machineState(e.machines[0]))
	}
}

func TestAdminMigrateSuspendedUsesFrozenTime(t *testing.T) {
	e := newEnv(t, 2)
	_, old := e.place(catalog.D1d)
	e.ack(true)
	e.clock.Advance(time.Hour)
	if _, err := e.svc.SuspendMachines(e.ctx, e.machines[:1]); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(5 * time.Hour) // 失联期间不计时
	moved, err := e.svc.AdminMigrate(e.ctx, opsActor, old.ID, "机器失联")
	if err != nil || moved.PendingSeconds != 23*3600 {
		t.Fatalf("pending = %d, %v", moved.PendingSeconds, err)
	}
}

func TestAdminMigrateNoCapacityChangesNothing(t *testing.T) {
	e := newEnv(t, 1)
	_, old := e.place(catalog.D1d)
	e.ack(true)
	if _, err := e.svc.AdminMigrate(e.ctx, opsActor, old.ID, "GPU 报错"); !errors.Is(err, apperr.ErrNoCapacity) {
		t.Fatalf("no capacity = %v", err)
	}
	if got := e.get(old.ID); got.State != rental.StateRunning {
		t.Fatalf("old state = %s", got.State)
	}
	if e.auditCount("instance.migrate") != 0 {
		t.Fatal("failed migration must not be audited")
	}
}

func TestAdminMigrateValidation(t *testing.T) {
	e := newEnv(t, 2)
	_, inst := e.place(catalog.D1d)
	if _, err := e.svc.AdminMigrate(e.ctx, opsActor, inst.ID, "x"); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("creating = %v", err)
	}
	e.ack(true)
	if _, err := e.svc.AdminMigrate(e.ctx, opsActor, inst.ID, " "); codeOf(err) != "bad_request" {
		t.Fatalf("empty reason = %v", err)
	}
	if _, err := e.svc.AdminMigrate(e.ctx, opsActor, "ins-missing", "x"); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
}

func TestAdminMigrateWorksWithDisabledImage(t *testing.T) {
	e := newEnv(t, 2)
	_, old := e.place(catalog.D1d)
	e.ack(true)
	if _, err := e.pool.Exec(e.ctx, `UPDATE images SET enabled = false`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AdminMigrate(e.ctx, opsActor, old.ID, "镜像已下架也要能换机"); err != nil {
		t.Fatal(err)
	}
}

func TestAdminMigrateCopiesAutoRenewTakesNewMachineGroup(t *testing.T) {
	e := newEnv(t, 2)
	gid := e.group("A", true)
	target := e.group("B", true)
	_, old := e.place(catalog.D1d)
	e.ack(true)
	for _, mid := range e.machines {
		if mid == old.MachineID {
			e.setGroup(mid, gid)
		} else {
			e.setGroup(mid, target)
		}
	}
	if _, err := e.pool.Exec(e.ctx, `UPDATE instances SET auto_renew = true, auto_renew_duration = '1w', group_id = $2 WHERE id = $1`, old.ID, gid); err != nil {
		t.Fatal(err)
	}
	moved, err := e.svc.AdminMigrate(e.ctx, opsActor, old.ID, "GPU 报错")
	if err != nil {
		t.Fatal(err)
	}
	got := e.get(moved.ID)
	if !got.AutoRenew || got.AutoRenewDuration != catalog.D1w || got.GroupID == nil || *got.GroupID != target || got.Booked() || got.ProvisionedAt != nil || got.BatchID != nil {
		t.Fatalf("moved = %+v", got)
	}
	lower, upper, status := e.lease(moved.ID)
	if status != "active" || !lower.Equal(e.clock.Now()) || !upper.Equal(e.clock.Now().Add(rental.CreateTimeout+24*time.Hour+rental.Tail)) {
		t.Fatalf("lease = [%v, %v) %s", lower, upper, status)
	}
}

func TestAdminMigrateToUngroupedMachineClearsGroup(t *testing.T) {
	e := newEnv(t, 2)
	actor := audit.Actor{AdminID: 1, IP: "127.0.0.1"}
	gid := e.group("A", true)
	_, old := e.place(catalog.D1d)
	e.ack(true)
	if _, err := e.pool.Exec(e.ctx, `UPDATE instances SET group_id = $2 WHERE id = $1`, old.ID, gid); err != nil {
		t.Fatal(err)
	}
	moved, err := e.svc.AdminMigrate(e.ctx, opsActor, old.ID, "GPU 报错")
	if err != nil {
		t.Fatal(err)
	}
	if got := e.get(moved.ID); got.GroupID != nil {
		t.Fatalf("moved group_id = %v, want nil", *got.GroupID)
	}
	e.ack(true) // 新实例开通完成，旧实例洗机完成
	if got := e.get(old.ID); got.State != rental.StateReleased {
		t.Fatalf("old state = %s", got.State)
	}
	if err := e.svc.AdminDeleteGroup(e.ctx, actor, gid); err != nil {
		t.Fatalf("delete old group after migrate = %v", err)
	}
}

// 终审 M4：早开通（S 之前已 running）的预订实例在 S 之前换机，不能少算已付时长：
// 新实例的待开始时长 = 原到期（S + D）− now，到期不早于 S + D（与普通换机一样，开通耗时算平台的）；
// instance_transfers.seconds 只记已付的 D（新实例开通失败时按它退款，早开通的免费时段不折现）。
func TestAdminMigrateEarlyProvisionedBookingKeepsPaidTime(t *testing.T) {
	e := newEnv(t, 2)
	start := dbtest.T0.Add(3 * time.Hour)
	old := e.book(start)
	e.clock.Advance(2 * time.Hour) // S − 1h：开通
	e.heartbeatAll()
	if n, err := e.svc.ProvisionDue(e.ctx); err != nil || n != 1 {
		t.Fatalf("provision = %d, %v", n, err)
	}
	e.clock.Advance(5 * time.Minute)
	e.ack(true)
	if got := e.get(old.ID); got.State != rental.StateRunning || !got.ExpiresAt.Equal(start.Add(24*time.Hour)) {
		t.Fatalf("early provisioned = %+v", got)
	}
	e.clock.Advance(25 * time.Minute) // S − 30min 换机
	moved, err := e.svc.AdminMigrate(e.ctx, opsActor, old.ID, "GPU 报错")
	if err != nil {
		t.Fatal(err)
	}
	if moved.PendingSeconds != 24*3600+30*60 {
		t.Fatalf("pending = %d, want %d", moved.PendingSeconds, 24*3600+30*60)
	}
	var transferred int64
	if err := e.pool.QueryRow(e.ctx, `SELECT seconds FROM instance_transfers WHERE to_instance_id = $1`, moved.ID).Scan(&transferred); err != nil || transferred != 86400 {
		t.Fatalf("transfer seconds = %d, %v", transferred, err)
	}
	if _, upper, _ := e.lease(moved.ID); upper.Before(start.Add(24*time.Hour + rental.Tail)) {
		t.Fatalf("lease upper %v must cover S + D + Tail", upper)
	}
	e.clock.Advance(5 * time.Minute)
	e.ack(true)
	got := e.get(moved.ID)
	if got.State != rental.StateRunning || got.ExpiresAt.Before(start.Add(24*time.Hour)) {
		t.Fatalf("moved expires %v before S + D %v", got.ExpiresAt, start.Add(24*time.Hour))
	}
}
