package rental_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
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
	if len(cmds) != 1 || cmds[0].Type != fleet.CmdCreateInstance || json.Unmarshal(cmds[0].Payload, &p) != nil || p.ImageRef == "" {
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
