package rental_test

import (
	"errors"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
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

var errRollback = errors.New("rollback")

// placeable 在一个回滚的事务里真正放置：先放 want 台必须成功，再放 want+1 台必须 no_capacity（D24）。
func placeable(t *testing.T, e *env, spec rental.PickSpec, want int) {
	t.Helper()
	try := func(n int) error {
		err := db.WithTx(e.ctx, e.pool, func(tx pgx.Tx) error {
			_, err := rental.PlaceOnMachines(e.ctx, tx, spec, n, func(sp pgx.Tx, mid int64) error {
				id, err := insertRawInstance(e.ctx, sp, e, mid)
				if err != nil {
					return err
				}
				_, err = rental.InsertLease(e.ctx, sp, mid, id, spec.Lower, spec.Upper, "reserved", e.clock.Now())
				return err
			})
			if err != nil {
				return err
			}
			return errRollback
		})
		if errors.Is(err, errRollback) {
			return nil
		}
		return err
	}
	if want > 0 {
		if err := try(want); err != nil {
			t.Fatalf("placing %d (available) failed: %v", want, err)
		}
	}
	if err := try(want + 1); codeOf(err) != "no_capacity" {
		t.Fatalf("placing %d (available+1) = %v, want no_capacity", want+1, err)
	}
}

// D24 性质测试：随机预订 / 机器状态 / 机组下，Availability 的台数 = pickMachines 真正能放下的台数。
func TestAvailabilityPropertyMatchesPlacement(t *testing.T) {
	e := newEnv(t, 6)
	rng := rand.New(rand.NewPCG(24, 2026))
	visible, hidden := e.group("A", true), e.group("留池", false)
	states := []string{"idle", "idle", "idle", "allocated", "maintenance"}
	durations := []catalog.Duration{catalog.D1d, catalog.D1w}
	for round := 0; round < 25; round++ {
		// 打乱机器：状态、在线、机组
		for _, m := range e.machines {
			e.setMachine(m, states[rng.IntN(len(states))], rng.IntN(4) != 0)
			switch rng.IntN(3) {
			case 0:
				e.setGroup(m, visible)
			case 1:
				e.setGroup(m, hidden)
			default:
				if _, err := e.pool.Exec(e.ctx, `UPDATE machines SET group_id = NULL WHERE id = $1`, m); err != nil {
					t.Fatal(err)
				}
			}
		}
		// 再随机加几条预订（与已有时段冲突的跳过）
		for k := 0; k < 3; k++ {
			m := e.machines[rng.IntN(len(e.machines))]
			start := dbtest.T0.Add(time.Duration(2+rng.IntN(20*24)) * time.Hour)
			secs := int64(86400 * (1 + rng.IntN(3)))
			var free bool
			if err := e.pool.QueryRow(e.ctx, `SELECT NOT EXISTS (SELECT 1 FROM machine_leases WHERE machine_id = $1 AND status IN ('reserved', 'active')
				AND period && tstzrange($2, $3))`, m, start.Add(-rental.PrepLead), start.Add(time.Duration(secs)*time.Second+rental.Tail)).Scan(&free); err != nil {
				t.Fatal(err)
			}
			if free {
				e.reserveRaw(m, start, secs)
			}
		}
		for probe := 0; probe < 4; probe++ {
			q := rental.AvailabilityQuery{ProductID: e.productID, Duration: durations[rng.IntN(len(durations))], Quantity: 1 + rng.IntN(2)}
			if rng.IntN(3) != 0 {
				s := dbtest.T0.Add(time.Duration(2+rng.IntN(20*24)) * time.Hour)
				q.StartAt = &s
			}
			if rng.IntN(2) == 0 {
				q.GroupID = &visible
			}
			av, err := e.svc.Availability(e.ctx, q)
			if err != nil {
				t.Fatalf("round %d: availability(%+v) = %v", round, q, err)
			}
			spec := rental.PickSpec{ProductID: q.ProductID, GroupID: q.GroupID, Immediate: q.StartAt == nil, Lower: av.Lower, Upper: av.Upper}
			ids, err := rental.PickMachines(e.ctx, e.pool, spec, 100, false)
			if err != nil || len(ids) != av.Available {
				t.Fatalf("round %d: pick = %v, %v; available = %d", round, ids, err, av.Available)
			}
			placeable(t, e, spec, av.Available)
		}
	}
}
