package rental_test

import (
	"context"
	"errors"
	"fmt"
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

// 即时分支：savepoint 里先 MarkAllocated 再插 lease，撞排他约束回滚 savepoint 后机器回到 idle（不会卡在 allocated）；
// 外层事务对这台机器的行锁不随 savepoint 回滚释放：提交前另一个连接 pickMachines(lock=true) 看不到它，提交后才看得到。
func TestPlaceOnMachinesImmediateRetryRevertsMachineAndKeepsRowLock(t *testing.T) {
	e := newEnv(t, 2)
	spec := rental.PickSpec{ProductID: e.productID, Immediate: true}
	spec.Lower, spec.Upper = rental.ImmediatePeriod(e.clock.Now(), 86400)
	var collided int64
	// 另一个连接对 collided 加锁探测：窗口远在 10 天后（与任何时段都不冲突），只有行锁能让它看不到这台机器
	probe := func() []int64 {
		t.Helper()
		far := rental.PickSpec{ProductID: e.productID, MachineID: &collided}
		far.Lower, far.Upper = rental.BookingPeriod(dbtest.T0.Add(10*24*time.Hour), 86400)
		var got []int64
		if err := db.WithTx(e.ctx, e.pool, func(tx pgx.Tx) error {
			var err error
			got, err = rental.PickMachines(e.ctx, tx, far, 10, true)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}
	var placed []int64
	err := db.WithTx(e.ctx, e.pool, func(tx pgx.Tx) error {
		ids, err := rental.PlaceOnMachines(e.ctx, tx, spec, 1, func(sp pgx.Tx, mid int64) error {
			if _, err := e.fleet.MarkAllocated(e.ctx, sp, mid); err != nil {
				return err
			}
			id, err := insertRawInstance(e.ctx, sp, e, mid)
			if err != nil {
				return err
			}
			if collided == 0 { // 预筛之后另一个连接在这台机器上提交了重叠的预订
				collided = mid
				e.reserveRaw(mid, dbtest.T0.Add(5*time.Hour), 3600)
			}
			_, err = rental.InsertLease(e.ctx, sp, mid, id, spec.Lower, spec.Upper, "active", e.clock.Now())
			return err
		})
		if err != nil {
			return err
		}
		placed = ids
		if got := probe(); len(got) != 0 {
			return fmt.Errorf("row lock on machine %d released by savepoint rollback: probe saw %v", collided, got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(placed) != 1 || placed[0] == collided {
		t.Fatalf("placed = %v, collided = %d", placed, collided)
	}
	if st := e.machineState(collided); st != "idle" {
		t.Fatalf("collided machine = %s, want idle (MarkAllocated rolled back with the savepoint)", st)
	}
	if st := e.machineState(placed[0]); st != "allocated" {
		t.Fatalf("placed machine = %s", st)
	}
	if got := probe(); !slices.Equal(got, []int64{collided}) { // 对照：外层事务提交后锁释放，探测能看到
		t.Fatalf("probe after commit = %v", got)
	}
}

// 补取有上限：首轮 + 3 轮补取，每次都撞约束 → 恰好尝试 4 台后 no_capacity（机器再多也不继续）。
func TestPlaceOnMachinesRefillBound(t *testing.T) {
	e := newEnv(t, 6)
	start := dbtest.T0.Add(12 * time.Hour)
	spec := rental.PickSpec{ProductID: e.productID}
	spec.Lower, spec.Upper = rental.BookingPeriod(start, 86400)
	var tried []int64
	err := db.WithTx(e.ctx, e.pool, func(tx pgx.Tx) error {
		_, err := rental.PlaceOnMachines(e.ctx, tx, spec, 1, func(sp pgx.Tx, mid int64) error {
			tried = append(tried, mid)
			id, err := insertRawInstance(e.ctx, sp, e, mid)
			if err != nil {
				return err
			}
			e.reserveRaw(mid, start.Add(2*time.Hour), 3600)
			_, err = rental.InsertLease(e.ctx, sp, mid, id, spec.Lower, spec.Upper, "reserved", e.clock.Now())
			return err
		})
		return err
	})
	if codeOf(err) != "no_capacity" {
		t.Fatalf("want no_capacity, got %v", err)
	}
	if len(tried) != 4 || len(sortedCopy(tried)) != len(slices.Compact(sortedCopy(tried))) {
		t.Fatalf("tried %v, want 4 distinct machines (1 + 3 refills)", tried)
	}
}

// 预订分支的 no_capacity 消息：一台都没有 / 只有部分台数（即时分支保持旧的 ErrNoCapacity，见上）。
func TestBookingNoCapacityMessage(t *testing.T) {
	e := newEnv(t, 2)
	start := dbtest.T0.Add(12 * time.Hour)
	_, err := e.svc.PlaceBatch(e.ctx, e.batchReq(3, &start))
	if ae := apperr.As(err); ae == nil || ae.Code != "no_capacity" || ae.Status != 409 || ae.Message != "该时段只有 2 台可用，请减少数量或更换时段" {
		t.Fatalf("partial = %#v", ae)
	}
	if _, err := e.svc.PlaceBatch(e.ctx, e.batchReq(2, &start)); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.PlaceBatch(e.ctx, e.batchReq(1, &start))
	if ae := apperr.As(err); ae == nil || ae.Code != "no_capacity" || ae.Message != "该时段暂无可用机器" {
		t.Fatalf("none = %#v", ae)
	}
}

// SKIP LOCKED 跳过的机器其实空闲时要重挑，而不是报 no_capacity：只有 1 台机器，事务 A 在它上面放一个 T0+5d 的预订
// （持有行锁、未提交），B 要同一台机器上另一个不重叠的时段（T0+12h 的预订 / 即时 Place）。B 第一次挑选被跳过 →
// 不加锁计数显示有空 → 进入重挑（钩子在此时让 A 提交或回滚）→ 退避后重挑成功。修复前 B 直接 no_capacity。
func TestPlaceRetriesWhenFreeMachineOnlyBrieflyLocked(t *testing.T) {
	for _, tc := range []struct {
		name      string
		immediate bool
		commitA   bool
	}{
		{"booking/A commits", false, true},
		{"booking/A rolls back", false, false},
		{"immediate Place/A commits", true, true},
		{"immediate Place/A rolls back", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, 1)
			mid := e.machines[0]
			txA, err := e.pool.Begin(e.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer txA.Rollback(e.ctx) //nolint:errcheck
			far := rental.PickSpec{ProductID: e.productID}
			far.Lower, far.Upper = rental.BookingPeriod(dbtest.T0.Add(5*24*time.Hour), 86400)
			if _, err := rental.PlaceOnMachines(e.ctx, txA, far, 1, func(sp pgx.Tx, m int64) error {
				id, err := insertRawInstance(e.ctx, sp, e, m)
				if err != nil {
					return err
				}
				_, err = rental.InsertLease(e.ctx, sp, m, id, far.Lower, far.Upper, "reserved", e.clock.Now())
				return err
			}); err != nil {
				t.Fatal(err)
			}

			retried := make(chan struct{})
			aDone := make(chan struct{})
			var once bool
			defer rental.SetLockRetryHookForTest(func() {
				if !once { // 只在第一次重挑时让 A 结束；之后的重挑（不该发生）直接放行
					once = true
					close(retried)
					<-aDone
				}
			})()
			type result struct {
				machine int64
				err     error
			}
			bDone := make(chan result, 1)
			go func() {
				if tc.immediate {
					_, inst, err := e.svc.Place(e.ctx, rental.PlaceRequest{UserID: e.userID, ProductID: e.productID, ImageID: e.imageID, Duration: catalog.D1d})
					bDone <- result{inst.MachineID, err}
					return
				}
				start := dbtest.T0.Add(12 * time.Hour)
				res, err := e.svc.PlaceBatch(e.ctx, e.batchReq(1, &start))
				var m int64
				if err == nil {
					m = res.Instances[0].MachineID
				}
				bDone <- result{m, err}
			}()

			select {
			case <-retried:
			case r := <-bDone:
				t.Fatalf("B finished while A still held the row lock (no retry): machine=%d err=%v", r.machine, r.err)
			case <-time.After(30 * time.Second):
				t.Fatal("B neither retried nor finished")
			}
			if tc.commitA {
				err = txA.Commit(e.ctx)
			} else {
				err = txA.Rollback(e.ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			close(aDone)
			r := <-bDone
			if r.err != nil || r.machine != mid {
				t.Fatalf("B after A ended: machine=%d err=%v, want machine %d", r.machine, r.err, mid)
			}
			var leases int
			if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM machine_leases WHERE machine_id = $1 AND status IN ('reserved', 'active')`, mid).Scan(&leases); err != nil {
				t.Fatal(err)
			}
			want := 1 // B 的
			if tc.commitA {
				want = 2 // A 的 + B 的
			}
			if leases != want {
				t.Fatalf("leases on machine = %d, want %d", leases, want)
			}
		})
	}
}

// 重挑有界、且只在「其实有空」时发生：A 一直不结束 → B 恰好重挑 3 次后 no_capacity（不会无限等）；
// 机器真被占了（不加锁计数也不够）→ 一次都不重挑，直接 no_capacity。
func TestPlaceLockRetryBounds(t *testing.T) {
	e := newEnv(t, 1)
	retries := 0
	defer rental.SetLockRetryHookForTest(func() { retries++ })()
	start := dbtest.T0.Add(12 * time.Hour)

	txA, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer txA.Rollback(e.ctx) //nolint:errcheck
	if _, err := txA.Exec(e.ctx, `SELECT id FROM machines WHERE id = $1 FOR NO KEY UPDATE`, e.machines[0]); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.PlaceBatch(e.ctx, e.batchReq(1, &start))
	if codeOf(err) != "no_capacity" || retries != 3 {
		t.Fatalf("lock never released: err=%v retries=%d, want no_capacity after 3 retries", err, retries)
	}
	if err := txA.Rollback(e.ctx); err != nil {
		t.Fatal(err)
	}

	retries = 0
	e.reserveRaw(e.machines[0], start, 86400)
	_, err = e.svc.PlaceBatch(e.ctx, e.batchReq(1, &start))
	if codeOf(err) != "no_capacity" || retries != 0 {
		t.Fatalf("machine really taken: err=%v retries=%d, want no_capacity without retry", err, retries)
	}
}

// 计划 9：Token 服务池（purpose='llm'，visible=false）里的机器不进「不限机组」的候选集，即时与预订分支都不选。
func TestPickMachinesSkipsLLMPool(t *testing.T) {
	e := newEnv(t, 2)
	pooled, free := e.machines[0], e.machines[1]
	e.toPool(pooled)
	now := e.clock.Now()
	for _, immediate := range []bool{true, false} {
		spec := rental.PickSpec{ProductID: e.productID, Immediate: immediate, Lower: now, Upper: now.Add(24 * time.Hour)}
		if got := pick(e, spec); !slices.Equal(got, []int64{free}) {
			t.Fatalf("immediate=%v: candidates = %v, want [%d]", immediate, got, free)
		}
		if n, err := rental.CountAvailable(e.ctx, e.pool, spec); err != nil || n != 1 {
			t.Fatalf("immediate=%v: count = %d, %v", immediate, n, err)
		}
	}
}
