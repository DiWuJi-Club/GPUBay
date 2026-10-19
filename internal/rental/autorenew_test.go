package rental_test

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/rental"
)

func (e *env) autoRenewOn(id string, d catalog.Duration) {
	e.t.Helper()
	if _, err := e.svc.SetAutoRenew(e.ctx, e.userID, id, true, d, "1.1.1.1"); err != nil {
		e.t.Fatalf("SetAutoRenew: %v", err)
	}
}

func TestAutoRenewChargesOneHourBeforeExpiry(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	e.autoRenewOn(inst.ID, catalog.D1d)
	if got := e.get(inst.ID); !got.AutoRenew || got.AutoRenewDuration != catalog.D1d || e.auditCountOf("instance.auto_renew") != 1 {
		t.Fatalf("after switch = %+v", got)
	}
	e.clock.Advance(22*time.Hour + 59*time.Minute)
	n, err := e.svc.AutoRenewDue(e.ctx)
	mustCount(t, n, err, 0, "before lead")
	e.clock.Advance(time.Minute)
	n, err = e.svc.AutoRenewDue(e.ctx)
	mustCount(t, n, err, 1, "at lead")
	got := e.get(inst.ID)
	if !got.ExpiresAt.Equal(dbtest.T0.Add(48*time.Hour)) || got.AutoRenewFailedAt != nil || got.AutoRenewFailedReason != "" {
		t.Fatalf("after auto renew = %+v", got)
	}
	orders, _ := e.svc.ListOrders(e.ctx, e.userID, 10)
	if len(orders) != 2 || orders[0].Kind != "renew" || !orders[0].Auto || orders[0].Amount != 6900 || orders[0].Quantity != 1 {
		t.Fatalf("orders = %+v", orders)
	}
	if ev := e.lastEvent(inst.ID); ev.Reason != "auto renew "+orders[0].ID+" (1d)" {
		t.Fatalf("event = %+v", ev)
	}
	if _, upper, _ := e.lease(inst.ID); !upper.Equal(dbtest.T0.Add(48*time.Hour + rental.Tail)) {
		t.Fatalf("lease upper = %v", upper)
	}
	n, err = e.svc.AutoRenewDue(e.ctx)
	mustCount(t, n, err, 0, "not due again")
	// 到期扫描不会停机（到期已推后）
	e.clock.Advance(time.Hour)
	if n, _ := e.svc.ExpireDue(e.ctx); n != 0 {
		t.Fatal("must not expire after auto renew")
	}
}

func TestAutoRenewInsufficientBalanceRetriesAndNotifiesOnce(t *testing.T) {
	e := newEnv(t, 1)
	poor := e.newUser("13600000002", 7000)
	e.userID = poor
	_, inst := e.place(catalog.D1d) // 余额剩 100
	e.ack(true)
	e.autoRenewOn(inst.ID, catalog.D1d)
	e.clock.Advance(23 * time.Hour)
	if _, err := e.svc.AutoRenewDue(e.ctx); err != nil {
		t.Fatal(err)
	}
	got := e.get(inst.ID)
	if got.AutoRenewFailedAt == nil || !got.AutoRenewFailedAt.Equal(e.clock.Now()) || got.AutoRenewFailedReason != "insufficient_balance" || !got.ExpiresAt.Equal(dbtest.T0.Add(24*time.Hour)) {
		t.Fatalf("after failure = %+v", got)
	}
	sms := func() int {
		n := 0
		for _, c := range e.notifier.Calls() {
			if strings.HasPrefix(c, "auto_renew_failed:"+inst.ID+":余额不足") {
				n++
			}
		}
		return n
	}
	if sms() != 1 || e.lastEvent(inst.ID).Reason != "auto renew failed: insufficient_balance" {
		t.Fatalf("sms = %d, event = %+v", sms(), e.lastEvent(inst.ID))
	}
	e.clock.Advance(30 * time.Second)
	n, err := e.svc.AutoRenewDue(e.ctx)
	mustCount(t, n, err, 0, "retry interval not elapsed")
	e.clock.Advance(31 * time.Second)
	n, err = e.svc.AutoRenewDue(e.ctx)
	mustCount(t, n, err, 1, "retried")
	if sms() != 1 || e.balance(poor) != 100 {
		t.Fatalf("repeat failure must not resend sms or charge: sms=%d balance=%d", sms(), e.balance(poor))
	}
	dbtest.Fund(t, e.pool, poor, 7000)
	e.clock.Advance(time.Minute)
	n, err = e.svc.AutoRenewDue(e.ctx)
	mustCount(t, n, err, 1, "after top-up")
	got = e.get(inst.ID)
	if got.AutoRenewFailedAt != nil || got.AutoRenewFailedReason != "" || !got.ExpiresAt.Equal(dbtest.T0.Add(48*time.Hour)) || e.balance(poor) != 200 {
		t.Fatalf("after success = %+v balance=%d", got, e.balance(poor))
	}
}

func TestAutoRenewFallsBackThenBlocks(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true) // expires T0 + 24h
	e.autoRenewOn(inst.ID, catalog.D1m)
	// 下一预订：可续至 = T0 + 24h + 8d → cap = +7h → S = cap + 1h = T0 + 24h + 8d + 8h
	e.reserveRaw(e.machines[0], dbtest.T0.Add(24*time.Hour+8*24*time.Hour+8*time.Hour), 3600)
	e.clock.Advance(23 * time.Hour)
	balance := e.balance(e.userID)
	if n, err := e.svc.AutoRenewDue(e.ctx); err != nil || n != 1 {
		t.Fatalf("first = %d, %v", n, err)
	}
	got := e.get(inst.ID)
	if !got.ExpiresAt.Equal(dbtest.T0.Add(24*time.Hour+7*24*time.Hour)) || e.balance(e.userID) != balance-41400 {
		t.Fatalf("expected fallback to 1w: %+v balance=%d", got, e.balance(e.userID))
	}
	if !strings.HasSuffix(e.lastEvent(inst.ID).Reason, "(1w)") {
		t.Fatalf("event = %+v", e.lastEvent(inst.ID))
	}
	// 再过 7 天：余量 1 天 → 降到 1d
	e.clock.Advance(7 * 24 * time.Hour)
	balance = e.balance(e.userID)
	if n, err := e.svc.AutoRenewDue(e.ctx); err != nil || n != 1 {
		t.Fatalf("second = %d, %v", n, err)
	}
	if got := e.get(inst.ID); !got.ExpiresAt.Equal(dbtest.T0.Add(24*time.Hour+8*24*time.Hour)) || e.balance(e.userID) != balance-6900 {
		t.Fatalf("expected fallback to 1d: %+v", got)
	}
	// 再过 1 天：余量 0 → renew_blocked，失败标记 + 短信；10 分钟内不重试；手动续费同样被挡且不扣款
	e.clock.Advance(24 * time.Hour)
	balance = e.balance(e.userID)
	if _, err := e.svc.AutoRenewDue(e.ctx); err != nil {
		t.Fatal(err)
	}
	got = e.get(inst.ID)
	if got.AutoRenewFailedReason != "renew_blocked" || e.balance(e.userID) != balance || !slices.Contains(e.notifier.Calls(), "auto_renew_failed:"+inst.ID+":机器已有后续预订") {
		t.Fatalf("blocked = %+v calls=%v", got, e.notifier.Calls())
	}
	e.clock.Advance(5 * time.Minute)
	n, err := e.svc.AutoRenewDue(e.ctx)
	mustCount(t, n, err, 0, "blocked retry within 10m")
	e.clock.Advance(6 * time.Minute)
	n, err = e.svc.AutoRenewDue(e.ctx)
	mustCount(t, n, err, 1, "blocked retry after 10m")
	if _, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1d, ""); err == nil || e.balance(e.userID) != balance {
		t.Fatalf("manual renew must be blocked without charge: %v", err)
	}
	// 一直被挡直到到期（规格 S15/S19）：期间的重试不重发短信；到期停机后不再尝试，全程只发一条
	e.clock.Advance(49 * time.Minute) // = 到期时刻
	if _, err := e.svc.AutoRenewDue(e.ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := e.svc.ExpireDue(e.ctx); err != nil || n != 1 {
		t.Fatalf("expire = %d, %v", n, err)
	}
	e.clock.Advance(11 * time.Minute)
	n, err = e.svc.AutoRenewDue(e.ctx)
	mustCount(t, n, err, 0, "stopped after expiry")
	sms := 0
	for _, c := range e.notifier.Calls() {
		if strings.HasPrefix(c, "auto_renew_failed:"+inst.ID+":") {
			sms++
		}
	}
	if sms != 1 || e.balance(e.userID) != balance || e.get(inst.ID).State != rental.StateStopped {
		t.Fatalf("blocked through expiry: sms=%d balance=%d state=%s", sms, e.balance(e.userID), e.get(inst.ID).State)
	}
}

func TestAutoRenewSkipsStoppedAndSuspended(t *testing.T) {
	e := newEnv(t, 2)
	_, a := e.place(catalog.D1d)
	_, b := e.place(catalog.D1d)
	e.ack(true)
	e.autoRenewOn(a.ID, catalog.D1d)
	e.autoRenewOn(b.ID, catalog.D1d)
	// 到期前 1h：b 所在机器失联 → b 暂停（running + suspended_at，剩余 1h 冻结）；a 暂时关掉自动续费免得被续
	if _, err := e.pool.Exec(e.ctx, `UPDATE instances SET auto_renew = false WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(23 * time.Hour)
	if _, err := e.svc.SuspendMachines(e.ctx, []int64{b.MachineID}); err != nil {
		t.Fatal(err)
	}
	if got := e.get(b.ID); got.State != rental.StateRunning || got.SuspendedAt == nil {
		t.Fatalf("b must be suspended: %+v", got)
	}
	n, err := e.svc.AutoRenewDue(e.ctx)
	mustCount(t, n, err, 0, "suspended skipped")
	if got := e.get(b.ID); !got.ExpiresAt.Equal(dbtest.T0.Add(24 * time.Hour)) {
		t.Fatalf("suspended instance must not be auto-renewed: %+v", got)
	}
	// 到期：a 停机（b 暂停中不被判到期）；再打开 a 的自动续费也不会续
	e.clock.Advance(time.Hour)
	if n, err := e.svc.ExpireDue(e.ctx); err != nil || n != 1 {
		t.Fatalf("expire = %d, %v", n, err)
	}
	if _, err := e.pool.Exec(e.ctx, `UPDATE instances SET auto_renew = true WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	n, err = e.svc.AutoRenewDue(e.ctx)
	mustCount(t, n, err, 0, "stopped skipped")
	if e.get(a.ID).State != rental.StateStopped {
		t.Fatal("stopped instance must not be auto-renewed")
	}
}

func TestAutoRenewRaceWithNewReservationRollsBackChargeAndMarksBlocked(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true) // expires = T0 + 24h，lease 上界 T0 + 31h
	e.autoRenewOn(inst.ID, catalog.D1d)
	var once sync.Once
	restore := rental.SetRenewHookForTest(func() {
		once.Do(func() { e.reserveRaw(e.machines[0], dbtest.T0.Add(32*time.Hour), 86400) }) // 选档之后才提交的同机预订
	})
	defer restore()
	e.clock.Advance(23 * time.Hour)
	balance := e.balance(e.userID)
	if _, err := e.svc.AutoRenewDue(e.ctx); err != nil {
		t.Fatal(err)
	}
	got := e.get(inst.ID)
	if e.balance(e.userID) != balance || !got.ExpiresAt.Equal(dbtest.T0.Add(24*time.Hour)) || got.AutoRenewFailedReason != "renew_blocked" {
		t.Fatalf("race must roll back charge and extension and mark blocked: balance=%d %+v", e.balance(e.userID), got)
	}
	if orders, _ := e.svc.ListOrders(e.ctx, e.userID, 10); len(orders) != 1 {
		t.Fatalf("no auto renew order may survive: %+v", orders)
	}
	if !slices.Contains(e.notifier.Calls(), "auto_renew_failed:"+inst.ID+":机器已有后续预订") {
		t.Fatalf("calls = %v", e.notifier.Calls())
	}
}
