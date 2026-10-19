package rental_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/randx"
	"spark-cloud/internal/rental"
)

// group 直接写库建一个机组并返回 id。
func (e *env) group(name string, visible bool) int64 {
	e.t.Helper()
	var id int64
	if err := e.pool.QueryRow(e.ctx, `INSERT INTO machine_groups (name, visible, created_at, updated_at) VALUES ($1, $2, $3, $3) RETURNING id`,
		name, visible, e.clock.Now()).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) setGroup(machineID, groupID int64) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, `UPDATE machines SET group_id = $2 WHERE id = $1`, machineID, groupID); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) setMachine(machineID int64, state string, online bool) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, `UPDATE machines SET state = $2, agent_online = $3 WHERE id = $1`, machineID, state, online); err != nil {
		e.t.Fatal(err)
	}
}

func pick(e *env, spec rental.PickSpec) []int64 {
	e.t.Helper()
	ids, err := rental.PickMachines(e.ctx, e.pool, spec, 10, false)
	if err != nil {
		e.t.Fatal(err)
	}
	return ids
}

func TestPickMachinesCandidateSet(t *testing.T) {
	e := newEnv(t, 6)
	ungrouped, visibleM, hiddenM, maint, busy, offline := e.machines[0], e.machines[1], e.machines[2], e.machines[3], e.machines[4], e.machines[5]
	vis := e.group("A", true)
	hid := e.group("留池", false)
	e.setGroup(visibleM, vis)
	e.setGroup(hiddenM, hid)
	e.setMachine(maint, "maintenance", true)
	e.setMachine(offline, "idle", false)
	// busy：一台运行中的实例（lease 到 T0 + 31h）
	e.setMachine(busy, "allocated", true)
	instID := randx.ID("ins-", 8)
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO instances (id, user_id, machine_id, product_id, image_id, state, ssh_password_enc, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'running', $6, $7, $7)`, instID, e.userID, busy, e.productID, e.imageID, []byte("x"), dbtest.T0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO machine_leases (machine_id, instance_id, period, status, created_at, updated_at)
		VALUES ($1, $2, tstzrange($3, $4), 'active', $3, $3)`, busy, instID, dbtest.T0, dbtest.T0.Add(31*time.Hour)); err != nil {
		t.Fatal(err)
	}

	far := rental.PickSpec{ProductID: e.productID}
	far.Lower, far.Upper = rental.BookingPeriod(dbtest.T0.Add(48*time.Hour), 86400)
	got := pick(e, far)
	slices.Sort(got)
	if want := []int64{ungrouped, visibleM, busy, offline}; !slices.Equal(got, sortedCopy(want)) {
		t.Fatalf("booking, no group = %v, want %v (busy's lease ends before the window; offline allowed; hidden/maintenance excluded)", got, want)
	}
	near := far
	near.Lower, near.Upper = rental.BookingPeriod(dbtest.T0.Add(10*time.Hour), 86400)
	if got := pick(e, near); slices.Contains(got, busy) {
		t.Fatalf("overlapping window must exclude busy: %v", got)
	}
	imm := rental.PickSpec{ProductID: e.productID, Immediate: true}
	imm.Lower, imm.Upper = rental.ImmediatePeriod(dbtest.T0, 86400)
	got = pick(e, imm)
	slices.Sort(got)
	if want := []int64{ungrouped, visibleM}; !slices.Equal(got, sortedCopy(want)) {
		t.Fatalf("immediate = %v, want %v", got, want)
	}
	g := far
	g.GroupID = &vis
	if got := pick(e, g); !slices.Equal(got, []int64{visibleM}) {
		t.Fatalf("group vis = %v", got)
	}
	g.GroupID = &hid
	if got := pick(e, g); !slices.Equal(got, []int64{hiddenM}) {
		t.Fatalf("group hidden (explicit) = %v", got)
	}
	m := far
	m.MachineID = &hiddenM
	if got := pick(e, m); len(got) != 0 {
		t.Fatalf("hidden machine without its group must be excluded: %v", got)
	}
	m.GroupID = &hid
	if got := pick(e, m); !slices.Equal(got, []int64{hiddenM}) {
		t.Fatalf("hidden machine with its group = %v", got)
	}
	ex := far
	ex.Exclude = []int64{ungrouped, visibleM, busy}
	if got := pick(e, ex); !slices.Equal(got, []int64{offline}) {
		t.Fatalf("exclude = %v", got)
	}
	if n, err := rental.CountAvailable(e.ctx, e.pool, far); err != nil || n != 4 {
		t.Fatalf("count = %d, %v", n, err)
	}
}

func sortedCopy(xs []int64) []int64 {
	out := append([]int64(nil), xs...)
	slices.Sort(out)
	return out
}

func TestPickMachinesBestFit(t *testing.T) {
	e := newEnv(t, 2)
	// m0 上有一段到 T0 + 10h 结束的历史时段，m1 没有：从 T0 + 12h 起的窗口应优先 m0（最紧贴）
	instID := randx.ID("ins-", 8)
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO instances (id, user_id, machine_id, product_id, image_id, state, ssh_password_enc, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'released', $6, $7, $7)`, instID, e.userID, e.machines[0], e.productID, e.imageID, []byte("x"), dbtest.T0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO machine_leases (machine_id, instance_id, period, status, created_at, updated_at)
		VALUES ($1, $2, tstzrange($3, $4), 'ended', $3, $3)`, e.machines[0], instID, dbtest.T0, dbtest.T0.Add(10*time.Hour)); err != nil {
		t.Fatal(err)
	}
	spec := rental.PickSpec{ProductID: e.productID}
	spec.Lower, spec.Upper = rental.BookingPeriod(dbtest.T0.Add(12*time.Hour), 86400)
	if got := pick(e, spec); !slices.Equal(got, []int64{e.machines[0], e.machines[1]}) {
		t.Fatalf("best fit order = %v", got)
	}
	// m1 上加一段到 T0 + 11h 的时段：m1 更紧贴，排到前面
	inst2 := randx.ID("ins-", 8)
	_, _ = e.pool.Exec(e.ctx, `INSERT INTO instances (id, user_id, machine_id, product_id, image_id, state, ssh_password_enc, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'released', $6, $7, $7)`, inst2, e.userID, e.machines[1], e.productID, e.imageID, []byte("x"), dbtest.T0)
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO machine_leases (machine_id, instance_id, period, status, created_at, updated_at)
		VALUES ($1, $2, tstzrange($3, $4), 'ended', $3, $3)`, e.machines[1], inst2, dbtest.T0, dbtest.T0.Add(11*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := pick(e, spec); !slices.Equal(got, []int64{e.machines[1], e.machines[0]}) {
		t.Fatalf("best fit order after = %v", got)
	}
}

// insertRawInstance 在给定 Querier 上插一个 reserved 实例行（供 PlaceOnMachines 的回调使用）。
func insertRawInstance(ctx context.Context, q db.Querier, e *env, machineID int64) (string, error) {
	id := randx.ID("ins-", 8)
	_, err := q.Exec(ctx, `INSERT INTO instances (id, user_id, machine_id, product_id, image_id, state, pending_seconds, ssh_password_enc, scheduled_start_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'reserved', 86400, $6, $7, $8, $8)`, id, e.userID, machineID, e.productID, e.imageID, []byte("x"), dbtest.T0.Add(12*time.Hour), e.clock.Now())
	return id, err
}

func TestPlaceOnMachinesRetriesOnExclusionViolation(t *testing.T) {
	e := newEnv(t, 2)
	start := dbtest.T0.Add(12 * time.Hour)
	spec := rental.PickSpec{ProductID: e.productID}
	spec.Lower, spec.Upper = rental.BookingPeriod(start, 86400)
	first := true
	var placed []string
	err := db.WithTx(e.ctx, e.pool, func(tx pgx.Tx) error {
		ids, err := rental.PlaceOnMachines(e.ctx, tx, spec, 1, func(sp pgx.Tx, mid int64) error {
			id, err := insertRawInstance(e.ctx, sp, e, mid)
			if err != nil {
				return err
			}
			if first { // 另一个连接在我们预筛之后、插 lease 之前提交了重叠的预订（READ COMMITTED 快照看不到）
				first = false
				e.reserveRaw(mid, start.Add(2*time.Hour), 3600)
			}
			if _, err := rental.InsertLease(e.ctx, sp, mid, id, spec.Lower, spec.Upper, "reserved", e.clock.Now()); err != nil {
				return err
			}
			placed = append(placed, id)
			return nil
		})
		if err != nil {
			return err
		}
		if len(ids) != 1 {
			return errors.New("want exactly one machine")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(placed) != 1 {
		t.Fatalf("placed = %v", placed)
	}
	var n int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM instances WHERE state = 'reserved'`).Scan(&n)
	if n != 2 { // 1 个来自 reserveRaw、1 个来自成功的那台；撞约束那台的实例行随 savepoint 回滚
		t.Fatalf("reserved instances = %d, want 2", n)
	}

	// 换一个窗口，两台都在插 lease 前被别人占掉（每轮补取都撞）：整单 no_capacity，事务回滚后什么都不留
	before := n
	start2 := dbtest.T0.Add(5 * 24 * time.Hour)
	spec2 := rental.PickSpec{ProductID: e.productID}
	spec2.Lower, spec2.Upper = rental.BookingPeriod(start2, 86400)
	err = db.WithTx(e.ctx, e.pool, func(tx pgx.Tx) error {
		_, err := rental.PlaceOnMachines(e.ctx, tx, spec2, 2, func(sp pgx.Tx, mid int64) error {
			id, err := insertRawInstance(e.ctx, sp, e, mid)
			if err != nil {
				return err
			}
			e.reserveRaw(mid, start2.Add(3*time.Hour), 3600)
			_, err = rental.InsertLease(e.ctx, sp, mid, id, spec2.Lower, spec2.Upper, "reserved", e.clock.Now())
			return err
		})
		return err
	})
	if ae := apperr.As(err); ae == nil || ae.Code != "no_capacity" {
		t.Fatalf("want no_capacity, got %v", err)
	}
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM instances WHERE state = 'reserved'`).Scan(&n)
	if n != before+2 { // 只多了两条 reserveRaw 造的冲突预订；本事务的实例行全部回滚
		t.Fatalf("reserved instances = %d, want %d", n, before+2)
	}
}

func TestImmediateNoCapacityKeepsLegacyError(t *testing.T) {
	e := newEnv(t, 0)
	_, _, err := e.svc.Place(e.ctx, rental.PlaceRequest{UserID: e.userID, ProductID: e.productID, ImageID: e.imageID, Duration: catalog.D1d})
	if !errors.Is(err, apperr.ErrNoCapacity) {
		t.Fatalf("want ErrNoCapacity, got %v", err)
	}
}

func TestAvailableNowIsLeaseAware(t *testing.T) {
	e := newEnv(t, 2)
	if n, _ := e.svc.AvailableNow(e.ctx, e.productID); n != 2 {
		t.Fatalf("available = %d", n)
	}
	// 机器 0 上 10 小时后有预订：即时租 1 天（含 20 分钟开通 + 7 小时尾段）放不下
	e.reserveRaw(e.machines[0], dbtest.T0.Add(10*time.Hour), 86400)
	if n, _ := e.svc.AvailableNow(e.ctx, e.productID); n != 1 {
		t.Fatalf("available with near reservation = %d, want 1", n)
	}
}
