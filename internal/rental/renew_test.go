package rental_test

import (
	"errors"
	"testing"
	"time"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/rental"
)

// expireAndStop 让实例到期并模拟 Agent 完成停机。
func expireAndStop(e *env) {
	e.t.Helper()
	e.clock.Advance(24 * time.Hour)
	if _, err := e.svc.ExpireDue(e.ctx); err != nil {
		e.t.Fatal(err)
	}
	e.ack(true)
}

func TestRenewRunningExtendsExpiry(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	ord, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1w, "")
	if err != nil || ord.Kind != "renew" || ord.Amount != 41400 {
		t.Fatalf("renew = %+v, %v", ord, err)
	}
	got := e.get(inst.ID)
	want := dbtest.T0.Add(8 * 24 * time.Hour)
	if !got.ExpiresAt.Equal(want) {
		t.Fatalf("expires = %v, want %v", got.ExpiresAt, want)
	}
	if b := e.balance(e.userID); b != startFunds-6900-41400 {
		t.Fatalf("balance = %d", b)
	}
	if _, upper, _ := e.lease(inst.ID); !upper.Equal(want.Add(rental.RetainAfterStop + rental.WipeBuffer)) {
		t.Fatalf("lease upper = %v", upper)
	}
}

func TestRenewWhileCreatingAddsPendingTime(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	if _, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1d, ""); err != nil {
		t.Fatal(err)
	}
	if got := e.get(inst.ID); got.PendingSeconds != 2*86400 {
		t.Fatalf("pending = %d", got.PendingSeconds)
	}
	e.clock.Advance(3 * time.Minute)
	e.ack(true)
	if got := e.get(inst.ID); !got.ExpiresAt.Equal(e.clock.Now().Add(48 * time.Hour)) {
		t.Fatalf("expires = %v", got.ExpiresAt)
	}
}

func TestCreateFailureRefundsRenewalsToo(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	if _, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1w, ""); err != nil {
		t.Fatal(err)
	}
	e.ack(false)
	if b := e.balance(e.userID); b != startFunds {
		t.Fatalf("balance = %d, want full refund of both orders", b)
	}
}

func TestRenewStoppedRestartsWithFreshLease(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	expireAndStop(e)
	e.clock.Advance(2 * time.Hour)

	if _, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1d, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1d, ""); err != nil {
		t.Fatal(err)
	}
	got := e.get(inst.ID)
	if got.State != rental.StateStopped || got.PendingSeconds != 2*86400 {
		t.Fatalf("after renew = %+v", got)
	}
	if n := countPending(e, e.machines[0], fleet.CmdStartInstance); n != 1 {
		t.Fatalf("start commands = %d, want 1", n)
	}

	e.clock.Advance(7 * time.Hour) // 有待开始的续费，不能被 6 小时清扫洗掉
	if n, err := e.svc.WipeStale(e.ctx); err != nil || n != 0 {
		t.Fatalf("WipeStale = %d, %v", n, err)
	}

	e.ack(true)
	got = e.get(inst.ID)
	if got.State != rental.StateRunning || !got.ExpiresAt.Equal(e.clock.Now().Add(48*time.Hour)) || got.StoppedAt != nil {
		t.Fatalf("after start = %+v", got)
	}
}

func TestRenewAfterWipeStartedIsRejectedWithoutCharge(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	expireAndStop(e)
	e.clock.Advance(rental.RetainAfterStop)
	if _, err := e.svc.WipeStale(e.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1d, ""); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("want invalid state, got %v", err)
	}
	if b := e.balance(e.userID); b != startFunds-6900 {
		t.Fatalf("balance = %d, renewal must not be charged", b)
	}
}

func TestRenewWhileSuspendedExtendsFrozenTime(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	if _, err := e.svc.SuspendMachines(e.ctx, e.machines); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1d, ""); err != nil {
		t.Fatal(err)
	}
	if got := e.get(inst.ID); got.Remaining(e.clock.Now()) != 48*time.Hour {
		t.Fatalf("remaining = %v", got.Remaining(e.clock.Now()))
	}
}

func TestRenewValidation(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	other := e.newUser("13600000004", startFunds)
	if _, err := e.svc.Renew(e.ctx, other, inst.ID, catalog.D1d, ""); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("other user want not found, got %v", err)
	}
	if _, err := e.svc.Renew(e.ctx, e.userID, inst.ID, "1h", ""); apperr.As(err) == nil || apperr.As(err).Code != "bad_request" {
		t.Fatalf("bad duration want bad_request, got %v", err)
	}
	poor := e.newUser("13600000005", 0)
	_, _ = e.pool.Exec(e.ctx, `UPDATE instances SET user_id = $1 WHERE id = $2`, poor, inst.ID)
	before := e.get(inst.ID).ExpiresAt
	if _, err := e.svc.Renew(e.ctx, poor, inst.ID, catalog.D1d, ""); !errors.Is(err, apperr.ErrInsufficientBalance) {
		t.Fatalf("want insufficient balance, got %v", err)
	}
	if after := e.get(inst.ID).ExpiresAt; !after.Equal(*before) {
		t.Fatalf("expires changed %v -> %v", before, after)
	}
}

func TestReleaseRunningWipesWithoutRefund(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	if err := e.svc.Release(e.ctx, e.userID, inst.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.get(inst.ID); got.State != rental.StateWiping {
		t.Fatalf("state = %s", got.State)
	}
	if b := e.balance(e.userID); b != startFunds-6900 {
		t.Fatalf("balance = %d, release must not refund", b)
	}
	e.ack(true)
	if got := e.get(inst.ID); got.State != rental.StateReleased {
		t.Fatalf("state = %s", got.State)
	}
	if got := e.machineState(e.machines[0]); got != fleet.StateIdle {
		t.Fatalf("machine = %s", got)
	}
	if err := e.svc.Release(e.ctx, e.userID, inst.ID); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("release twice want invalid state, got %v", err)
	}
	// 机器回到空闲后可以被再次租用
	if _, inst2 := e.place(catalog.D1d); inst2.MachineID != inst.MachineID {
		t.Fatalf("expected machine reuse")
	}
}

func TestReleaseCreatingRejected(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	if err := e.svc.Release(e.ctx, e.userID, inst.ID); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("want invalid state, got %v", err)
	}
}

func TestWipeFailureQuarantinesMachine(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	_ = e.svc.Release(e.ctx, e.userID, inst.ID)
	e.ack(false)
	if got := e.get(inst.ID); got.State != rental.StateWiping {
		t.Fatalf("state = %s, want wiping", got.State)
	}
	if got := e.machineState(e.machines[0]); got != fleet.StateFaulty {
		t.Fatalf("machine = %s, want faulty", got)
	}
	var n int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM alerts WHERE kind = 'wipe_failed'`).Scan(&n)
	if n != 1 {
		t.Fatalf("alerts = %d", n)
	}
}

func TestLeaseExclusionRejectsOverlap(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	_, err := e.pool.Exec(e.ctx, `INSERT INTO machine_leases (machine_id, instance_id, period, status, created_at, updated_at)
		VALUES ($1, $2, tstzrange($3, $4), 'reserved', $3, $3)`,
		inst.MachineID, inst.ID, dbtest.T0.Add(time.Hour), dbtest.T0.Add(2*time.Hour))
	if err == nil {
		t.Fatal("overlapping lease must be rejected")
	}
}

func TestRenewAfterFailedStartRequeuesStart(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	expireAndStop(e)
	if _, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1d, ""); err != nil {
		t.Fatal(err)
	}
	e.ack(false) // 启动失败，仍为 stopped
	if got := e.get(inst.ID); got.State != rental.StateStopped || got.PendingSeconds != 86400 {
		t.Fatalf("after failed start = %+v", got)
	}
	if _, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1d, ""); err != nil {
		t.Fatal(err)
	}
	if n := countPending(e, e.machines[0], fleet.CmdStartInstance); n != 1 {
		t.Fatalf("start commands = %d, want 1", n)
	}
	if got := e.get(inst.ID); got.PendingSeconds != 172800 {
		t.Fatalf("pending = %d", got.PendingSeconds)
	}
	e.ack(true)
	got := e.get(inst.ID)
	if got.State != rental.StateRunning || !got.ExpiresAt.Equal(e.clock.Now().Add(48*time.Hour)) {
		t.Fatalf("after start = %+v", got)
	}
}

func TestRenewExpiredButUnsweptExtendsFromNow(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	e.clock.Advance(25 * time.Hour)
	if _, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1d, ""); err != nil {
		t.Fatal(err)
	}
	if got := e.get(inst.ID); !got.ExpiresAt.Equal(e.clock.Now().Add(24 * time.Hour)) {
		t.Fatalf("expires = %v", got.ExpiresAt)
	}
}
