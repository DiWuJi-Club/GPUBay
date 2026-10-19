package rental_test

import (
	"testing"
	"time"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/rental"
)

func TestFailedInstanceLeaseIsNeverEmpty(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(false) // 开通失败：lease 在同一时刻关闭，上界必须 ≥ 下界 + 1 秒
	lower, upper, status := e.lease(inst.ID)
	if status != "ended" || !upper.Equal(lower.Add(time.Second)) {
		t.Fatalf("lease = [%v, %v) %s", lower, upper, status)
	}
}

func TestRefreshLeaseCapsAtNextReservation(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true) // running，expires = T0 + 24h，lease = [T0, T0 + 31h)
	// 同机下一条预订：T0 + 40h 开始 → lease 下界 T0 + 39h
	resv := e.reserveRaw(e.machines[0], dbtest.T0.Add(40*time.Hour), 86400)

	// 失联 5 小时后暂停：剩余 19h 冻结；desired = now + 19h + 7h = T0 + 31h（未碰到封顶）
	e.clock.Advance(5 * time.Hour)
	if _, err := e.svc.SuspendMachines(e.ctx, e.machines); err != nil {
		t.Fatal(err)
	}
	if _, upper, _ := e.lease(inst.ID); !upper.Equal(dbtest.T0.Add(31 * time.Hour)) {
		t.Fatalf("upper after suspend = %v", upper)
	}
	// 再过 10 小时（仍失联）：desired = T0 + 41h，被封顶到 T0 + 39h
	e.clock.Advance(10 * time.Hour)
	n, err := e.svc.RefreshCappedLeases(e.ctx)
	mustCount(t, n, err, 1, "refresh capped")
	if _, upper, _ := e.lease(inst.ID); !upper.Equal(dbtest.T0.Add(39 * time.Hour)) {
		t.Fatalf("capped upper = %v, want %v", upper, dbtest.T0.Add(39*time.Hour))
	}
	// 预订取消后封顶消失
	if _, err := e.pool.Exec(e.ctx, `UPDATE machine_leases SET status = 'cancelled' WHERE instance_id = $1`, resv); err != nil {
		t.Fatal(err)
	}
	n, err = e.svc.RefreshCappedLeases(e.ctx)
	mustCount(t, n, err, 1, "refresh after cancel")
	if _, upper, _ := e.lease(inst.ID); !upper.Equal(dbtest.T0.Add(41 * time.Hour)) {
		t.Fatalf("uncapped upper = %v", upper)
	}
	// 预订实例自己的 lease 不被 refreshLease 改写
	if lower, upper, status := e.lease(resv); status != "cancelled" || !lower.Equal(dbtest.T0.Add(39*time.Hour)) || !upper.Equal(dbtest.T0.Add(40*time.Hour+24*time.Hour+rental.Tail)) {
		t.Fatalf("reserved lease touched: [%v, %v) %s", lower, upper, status)
	}
}

func TestRefreshCappedLeasesAdvancesWiping(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	if err := e.svc.Release(e.ctx, e.userID, inst.ID); err != nil {
		t.Fatal(err)
	}
	if _, upper, _ := e.lease(inst.ID); !upper.Equal(e.clock.Now().Add(rental.WipeBuffer)) {
		t.Fatalf("wiping upper = %v", upper)
	}
	e.clock.Advance(2 * time.Hour) // Wipe 一直没回报：上界必须跟着时间走
	n, err := e.svc.RefreshCappedLeases(e.ctx)
	mustCount(t, n, err, 1, "refresh wiping")
	if _, upper, _ := e.lease(inst.ID); !upper.Equal(e.clock.Now().Add(rental.WipeBuffer)) {
		t.Fatalf("advanced upper = %v", upper)
	}
}

// cancelled 与 released / failed 一样是终态：时段关闭（ended），不会被改写成 active [lower, now+1s)。
func TestRefreshLeaseClosesCancelled(t *testing.T) {
	e := newEnv(t, 1)
	resv := e.reserveRaw(e.machines[0], dbtest.T0.Add(40*time.Hour), 86400)
	// 模拟取消时 lease 仍为 active（例如改派过渡期），refreshLease 必须把它结束
	if _, err := e.pool.Exec(e.ctx, `UPDATE instances SET state = 'cancelled', cancelled_at = $2 WHERE id = $1`, resv, e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(e.ctx, `UPDATE machine_leases SET status = 'active' WHERE instance_id = $1`, resv); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.RefreshLease(e.ctx, resv); err != nil {
		t.Fatal(err)
	}
	lower, upper, status := e.lease(resv)
	if status != "ended" || !lower.Equal(dbtest.T0.Add(39*time.Hour)) || !upper.Equal(lower.Add(time.Second)) {
		t.Fatalf("cancelled lease = [%v, %v) %s", lower, upper, status)
	}
}

// 预订实例在 reserved 与首次开通的 creating 期间时段固定，refreshLease 不改写。
func TestRefreshLeaseLeavesBookedUntouched(t *testing.T) {
	e := newEnv(t, 1)
	resv := e.reserveRaw(e.machines[0], dbtest.T0.Add(40*time.Hour), 86400)
	wantLower, wantUpper := dbtest.T0.Add(39*time.Hour), dbtest.T0.Add(64*time.Hour+rental.Tail)
	check := func(wantStatus string) {
		t.Helper()
		if err := e.svc.RefreshLease(e.ctx, resv); err != nil {
			t.Fatal(err)
		}
		if lower, upper, status := e.lease(resv); status != wantStatus || !lower.Equal(wantLower) || !upper.Equal(wantUpper) {
			t.Fatalf("booked lease rewritten: [%v, %v) %s", lower, upper, status)
		}
	}
	check("reserved")
	// 首次开通（creating）期间，即使 lease 已转 active 也不按 occupiedUntil 改写
	e.clock.Advance(39 * time.Hour)
	if _, err := e.pool.Exec(e.ctx, `UPDATE instances SET state = 'creating', provisioned_at = $2 WHERE id = $1`, resv, e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(e.ctx, `UPDATE machine_leases SET status = 'active' WHERE instance_id = $1`, resv); err != nil {
		t.Fatal(err)
	}
	check("active")
}
