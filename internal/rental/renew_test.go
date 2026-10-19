package rental_test

import (
	"errors"
	"sync"
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

func TestRenewInfoReflectsNextReservation(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true) // expires = T0 + 24h
	info, err := e.svc.RenewInfoFor(e.ctx, e.get(inst.ID))
	if err != nil || info.RenewableUntil != nil || len(info.Options) != 3 || info.Options[0].QuantityMax != 90 || info.Options[1].QuantityMax != 12 || info.Options[2].QuantityMax != 3 {
		t.Fatalf("no reservation info = %+v, %v", info, err)
	}
	e.reserveRaw(e.machines[0], dbtest.T0.Add(72*time.Hour), 86400) // 下一预订 S = T0 + 3d → cap = S − 1h，可续至 = S − 8h = T0 + 64h
	info, _ = e.svc.RenewInfoFor(e.ctx, e.get(inst.ID))
	until := dbtest.T0.Add(64 * time.Hour)
	if info.RenewableUntil == nil || !info.RenewableUntil.Equal(until) {
		t.Fatalf("renewable_until = %v, want %v", info.RenewableUntil, until)
	}
	// base = T0 + 24h，余量 40h：1d 最多 1、1w / 1m 为 0
	if info.Options[0].QuantityMax != 1 || info.Options[1].QuantityMax != 0 || info.Options[2].QuantityMax != 0 {
		t.Fatalf("options = %+v", info.Options)
	}
}

func TestRenewBlockedByReservationDoesNotCharge(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	e.reserveRaw(e.machines[0], dbtest.T0.Add(72*time.Hour), 86400)
	balance := e.balance(e.userID)
	_, _, upperBefore := e.lease(inst.ID)
	_, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1w, "")
	ae := apperr.As(err)
	if ae == nil || ae.Code != "renew_blocked" || ae.Message != "该机器自 2026-01-04 08:00 起已有预订，本次最多可续至 2026-01-04 00:00" {
		t.Fatalf("renew 1w = %v", err)
	}
	got := e.get(inst.ID)
	if e.balance(e.userID) != balance || !got.ExpiresAt.Equal(dbtest.T0.Add(24*time.Hour)) {
		t.Fatal("blocked renewal must not charge or extend")
	}
	if _, _, upper := e.lease(inst.ID); upper != upperBefore {
		t.Fatal("lease must not change")
	}
	// 后台延长 7 天同样被挡；1 天可以
	if _, err := e.svc.AdminExtend(e.ctx, opsActor, inst.ID, 7*86400, "补偿"); apperr.As(err) == nil || apperr.As(err).Code != "renew_blocked" {
		t.Fatalf("extend 7d = %v", err)
	}
	if _, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1d, ""); err != nil {
		t.Fatalf("renew 1d = %v", err)
	}
	if !e.get(inst.ID).ExpiresAt.Equal(dbtest.T0.Add(48 * time.Hour)) {
		t.Fatal("1d renewal must extend")
	}
	// 停机期续费：base = now + 20m + pending；停机在 T0 + 48h，续 2d → T0 + 96h20m > 可续至 T0 + 64h → renew_blocked
	e.clock.Advance(48 * time.Hour)
	if _, err := e.svc.ExpireDue(e.ctx); err != nil {
		t.Fatal(err)
	}
	e.ack(true)
	if _, err := e.svc.RenewQuantity(e.ctx, e.userID, inst.ID, catalog.D1d, 2, ""); apperr.As(err) == nil || apperr.As(err).Code != "renew_blocked" {
		t.Fatalf("stopped renew 2d = %v", err)
	}
	if e.get(inst.ID).PendingSeconds != 0 {
		t.Fatal("blocked stopped renewal must not add pending time")
	}
}

// 评审 M2：renewFits 通过之后、本事务提交之前，另一连接提交了同机紧随的预订 → 必须 renew_blocked 且不扣款。
func TestRenewRaceWithNewReservationIsBlocked(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true) // expires = T0 + 24h，lease 上界 T0 + 31h
	restore := rental.SetRenewHookForTest(func() {
		e.reserveRaw(e.machines[0], dbtest.T0.Add(32*time.Hour), 86400) // 下界 T0 + 31h：紧贴当前 lease，续 1d 就越界
	})
	defer restore()
	balance := e.balance(e.userID)
	_, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1d, "")
	if ae := apperr.As(err); ae == nil || ae.Code != "renew_blocked" {
		t.Fatalf("want renew_blocked, got %v", err)
	}
	got := e.get(inst.ID)
	if e.balance(e.userID) != balance || !got.ExpiresAt.Equal(dbtest.T0.Add(24*time.Hour)) {
		t.Fatalf("race must roll back charge and expiry: balance=%d expires=%v", e.balance(e.userID), got.ExpiresAt)
	}
	if _, upper, _ := e.lease(inst.ID); !upper.Equal(dbtest.T0.Add(31 * time.Hour)) {
		t.Fatalf("lease upper = %v", upper)
	}
	restore()
	if _, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1d, ""); apperr.As(err) == nil || apperr.As(err).Code != "renew_blocked" {
		t.Fatalf("after the reservation exists renewFits itself must block: %v", err)
	}
}

func TestRenewReservedExtendsLeaseUnlessBlocked(t *testing.T) {
	e := newEnv(t, 1)
	first := e.book(dbtest.T0.Add(3 * time.Hour))                          // lease [T0+2h, T0+34h)
	next := e.reserveRaw(e.machines[0], dbtest.T0.Add(35*time.Hour), 3600) // lease 下界 T0 + 34h：紧随其后
	if _, err := e.svc.Renew(e.ctx, e.userID, first.ID, catalog.D1d, ""); apperr.As(err) == nil || apperr.As(err).Code != "renew_blocked" {
		t.Fatalf("reserved renew blocked = %v", err)
	}
	if _, err := e.pool.Exec(e.ctx, `UPDATE machine_leases SET status = 'cancelled' WHERE instance_id = $1`, next); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Renew(e.ctx, e.userID, first.ID, catalog.D1d, ""); err != nil {
		t.Fatalf("reserved renew after cancel = %v", err)
	}
	if got := e.get(first.ID); got.PendingSeconds != 2*86400 {
		t.Fatalf("pending = %d", got.PendingSeconds)
	}
	if _, upper, _ := e.lease(first.ID); !upper.Equal(dbtest.T0.Add(3*time.Hour + 48*time.Hour + rental.Tail)) {
		t.Fatalf("lease upper = %v", upper)
	}
}

// Task 5 评审：renewFits 通过后同机紧随的预订才提交 → reserved 分支 addTime 撞排他约束，映射为 renew_blocked，不扣款。
func TestRenewReservedRaceHitsExclusionIsBlocked(t *testing.T) {
	e := newEnv(t, 1)
	first := e.book(dbtest.T0.Add(3 * time.Hour)) // lease [T0+2h, T0+34h)
	restore := rental.SetRenewHookForTest(func() {
		e.reserveRaw(e.machines[0], dbtest.T0.Add(35*time.Hour), 3600) // 下界 T0 + 34h
	})
	defer restore()
	balance := e.balance(e.userID)
	_, err := e.svc.Renew(e.ctx, e.userID, first.ID, catalog.D1d, "")
	if ae := apperr.As(err); ae == nil || ae.Code != "renew_blocked" || ae.Message != "该机器已有后续预订，无法续费" {
		t.Fatalf("want generic renew_blocked, got %v", err)
	}
	if e.balance(e.userID) != balance || e.get(first.ID).PendingSeconds != 86400 {
		t.Fatal("blocked reserved renewal must not charge or add time")
	}
	if _, upper, _ := e.lease(first.ID); !upper.Equal(dbtest.T0.Add(3*time.Hour + 24*time.Hour + rental.Tail)) {
		t.Fatalf("lease upper = %v", upper)
	}
}

// D7：预订已开通（booked + creating）时续费 / 后台延长不得误报 renew_blocked，且 lease 上界随之推后。
func TestRenewBookedCreatingExtendsLease(t *testing.T) {
	e := newEnv(t, 1)
	start := dbtest.T0.Add(3 * time.Hour)
	inst := e.book(start)
	e.clock.Advance(2 * time.Hour)
	e.heartbeatAll()
	n, err := e.svc.ProvisionDue(e.ctx)
	mustCount(t, n, err, 1, "provision")
	if _, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1d, ""); err != nil {
		t.Fatalf("renew booked creating = %v", err)
	}
	if _, err := e.svc.AdminExtend(e.ctx, opsActor, inst.ID, 3600, "补偿"); err != nil {
		t.Fatalf("extend booked creating = %v", err)
	}
	got := e.get(inst.ID)
	if got.State != rental.StateCreating || got.PendingSeconds != 2*86400+3600 {
		t.Fatalf("instance = %+v", got)
	}
	if _, upper, status := e.lease(inst.ID); status != "active" || !upper.Equal(start.Add(49*time.Hour+rental.Tail)) {
		t.Fatalf("lease = %v %s", upper, status)
	}
	// 同机紧随的预订：续费被挡
	e.reserveRaw(e.machines[0], start.Add(49*time.Hour+rental.Tail+rental.PrepLead), 3600)
	if _, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1d, ""); apperr.As(err) == nil || apperr.As(err).Code != "renew_blocked" {
		t.Fatalf("renew booked creating blocked = %v", err)
	}
}

func TestAdminExtendReserved(t *testing.T) {
	e := newEnv(t, 1)
	start := dbtest.T0.Add(3 * time.Hour)
	inst := e.book(start)
	if _, err := e.svc.AdminExtend(e.ctx, opsActor, inst.ID, 7200, "补偿"); err != nil {
		t.Fatalf("extend reserved = %v", err)
	}
	if got := e.get(inst.ID); got.State != rental.StateReserved || got.PendingSeconds != 86400+7200 {
		t.Fatalf("instance = %+v", got)
	}
	if _, upper, status := e.lease(inst.ID); status != "reserved" || !upper.Equal(start.Add(26*time.Hour+rental.Tail)) {
		t.Fatalf("lease = %v %s", upper, status)
	}
}

func TestRenewQuantityNegativeIsBadRequest(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	_, err := e.svc.RenewQuantity(e.ctx, e.userID, inst.ID, catalog.D1d, -1, "")
	if ae := apperr.As(err); ae == nil || ae.Code != "bad_request" || ae.Message != "数量不正确" {
		t.Fatalf("quantity -1 = %v", err)
	}
}

// Task 5 评审：同一键并发提交两次 PlaceBatch，只建一个批次、扣一次款。
func TestConcurrentPlaceBatchSameKey(t *testing.T) {
	e := newEnv(t, 4)
	start := dbtest.T0.Add(3 * time.Hour)
	var wg sync.WaitGroup
	var mu sync.Mutex
	batches, insts := map[string]bool{}, map[string]bool{}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := e.batchReq(2, &start)
			req.IdempotencyKey = "batch-key"
			res, err := e.svc.PlaceBatch(e.ctx, req)
			if err != nil {
				t.Errorf("PlaceBatch: %v", err)
				return
			}
			mu.Lock()
			batches[res.Batch.ID] = true
			for _, in := range res.Instances {
				insts[in.ID] = true
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(batches) != 1 || len(insts) != 2 {
		t.Fatalf("batches = %v, instances = %v", batches, insts)
	}
	if b := e.balance(e.userID); b != startFunds-2*6900 {
		t.Fatalf("balance = %d, want charged once per machine", b)
	}
}
