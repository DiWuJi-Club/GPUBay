package rental_test

import (
	"errors"
	"testing"
	"time"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/rental"
)

func TestGroupCalendarCountsFreeMachinesPerDay(t *testing.T) {
	e := newEnv(t, 3)
	gid := e.group("A", true)
	for _, m := range e.machines {
		e.setGroup(m, gid)
	}
	e.setMachine(e.machines[2], "maintenance", true) // 不在 total 里
	// 机器 0：T0 + 2d 11:00 CST 起租 1 天 → 占 [T0+2d+2h, T0+2d+3h+24h+7h)，跨北京时间第 3、4 天
	e.reserveRaw(e.machines[0], dbtest.T0.Add(48*time.Hour+3*time.Hour), 86400)
	from := dbtest.T0 // 2026-01-01 08:00 CST；DayStart → 1 月 1 日 00:00 CST
	days, err := e.svc.GroupCalendar(e.ctx, gid, from, 5, true)
	if err != nil || len(days) != 5 {
		t.Fatalf("calendar = %+v, %v", days, err)
	}
	want := []rental.CalendarDay{
		{Date: "2026-01-01", Free: 2, Total: 2}, {Date: "2026-01-02", Free: 2, Total: 2}, {Date: "2026-01-03", Free: 1, Total: 2},
		{Date: "2026-01-04", Free: 1, Total: 2}, {Date: "2026-01-05", Free: 2, Total: 2},
	}
	for i, d := range days {
		if d != want[i] {
			t.Fatalf("day[%d] = %+v, want %+v", i, d, want[i])
		}
	}
	hidden := e.group("留池", false)
	if _, err := e.svc.GroupCalendar(e.ctx, hidden, from, 5, true); !errors.Is(err, apperr.ErrGroupNotFound) {
		t.Fatalf("hidden group (console) = %v", err)
	}
	if _, err := e.svc.GroupCalendar(e.ctx, hidden, from, 5, false); err != nil {
		t.Fatalf("hidden group (admin) = %v", err)
	}
	if _, err := e.svc.GroupCalendar(e.ctx, gid, from, 62, true); apperr.As(err) == nil {
		t.Fatalf("days > 61 = %v", err)
	}
}

func TestMachineCalendarMasksOtherUsers(t *testing.T) {
	e := newEnv(t, 1)
	gid := e.group("A", true)
	e.setGroup(e.machines[0], gid)
	mine := e.book(dbtest.T0.Add(3 * time.Hour))
	other := e.newUser("13600000002", startFunds)
	req := e.batchReq(1, ptr(dbtest.T0.Add(72*time.Hour)))
	req.UserID = other
	if _, err := e.svc.PlaceBatch(e.ctx, req); err != nil {
		t.Fatal(err)
	}
	periods, err := e.svc.MachineCalendar(e.ctx, e.machines[0], e.userID, dbtest.T0, 10, false)
	if err != nil || len(periods) != 2 {
		t.Fatalf("periods = %+v, %v", periods, err)
	}
	if !periods[0].Mine || periods[0].InstanceID != mine.ID || periods[1].Mine || periods[1].InstanceID != "" || periods[1].UserID != 0 {
		t.Fatalf("masking = %+v", periods)
	}
	adminView, _ := e.svc.MachineCalendar(e.ctx, e.machines[0], 0, dbtest.T0, 10, true)
	if adminView[1].InstanceID == "" || adminView[1].UserID != other {
		t.Fatalf("admin view = %+v", adminView)
	}
	// 窗口外的时段不返回
	if later, _ := e.svc.MachineCalendar(e.ctx, e.machines[0], e.userID, dbtest.T0.Add(10*24*time.Hour), 5, false); len(later) != 0 {
		t.Fatalf("outside window = %+v", later)
	}
	ms, err := e.svc.GroupMachines(e.ctx, gid, true)
	if err != nil || len(ms) != 1 || ms[0].Label != "机器 #"+itoa64(e.machines[0]) || ms[0].BusyUntil == nil || !ms[0].BusyUntil.Equal(dbtest.T0.Add(72*time.Hour+24*time.Hour+rental.Tail)) {
		t.Fatalf("group machines = %+v, %v", ms, err)
	}
}

func TestAvailabilityMatchesPlacement(t *testing.T) {
	e := newEnv(t, 3)
	start := dbtest.T0.Add(5 * time.Hour)
	e.reserveRaw(e.machines[0], start, 86400)
	q := rental.AvailabilityQuery{ProductID: e.productID, StartAt: &start, Duration: catalog.D1d, Quantity: 1}
	av, err := e.svc.Availability(e.ctx, q)
	if err != nil || av.Available != 2 || !av.Lower.Equal(start.Add(-rental.PrepLead)) {
		t.Fatalf("availability = %+v, %v", av, err)
	}
	for i := 0; i < av.Available; i++ {
		if _, err := e.svc.PlaceBatch(e.ctx, e.batchReq(1, &start)); err != nil {
			t.Fatalf("booking %d should succeed: %v", i, err)
		}
	}
	if _, err := e.svc.PlaceBatch(e.ctx, e.batchReq(1, &start)); codeOf(err) != "no_capacity" {
		t.Fatalf("one more must fail: %v", err)
	}
	if av, _ = e.svc.Availability(e.ctx, q); av.Available != 0 {
		t.Fatalf("after booking = %+v", av)
	}
	imm := rental.AvailabilityQuery{ProductID: e.productID, Duration: catalog.D1d, Quantity: 1}
	if av, _ = e.svc.Availability(e.ctx, imm); av.Available != 0 { // 三台都在 1 天 + 7h 内有预订
		t.Fatalf("immediate = %+v", av)
	}
	imm.Quantity = 91
	if _, err := e.svc.Availability(e.ctx, imm); !errors.Is(err, apperr.ErrBookingTooLong) {
		t.Fatalf("quantity 91 = %v", err)
	}
}
