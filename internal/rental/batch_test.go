package rental_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/rental"
)

func (e *env) batchReq(count int, start *time.Time) rental.BatchRequest {
	return rental.BatchRequest{UserID: e.userID, ProductID: e.productID, ImageID: e.imageID, Count: count, Duration: catalog.D1d, Quantity: 1, StartAt: start}
}

func ptr[T any](v T) *T { return &v }

// codeOf 已在 admin_ops_test.go 里定义（同包），这里直接复用，不要重复声明。

func TestBatchValidationMatrix(t *testing.T) {
	e := newEnv(t, 8)                                                          // 接受的用例各自真的下单：机器要够，窗口彼此错开或落在不同机器
	dbtest.Fund(t, e.pool, e.userID, 5_000_000)                                // 1m×3（479,700 分）+ 1d×90（621,000 分）+ 其余单日单，startFunds 不够
	h := func(d time.Duration) *time.Time { x := dbtest.T0.Add(d); return &x } // T0 是整点
	cases := []struct {
		name string
		mut  func(r *rental.BatchRequest)
		code string
	}{
		{"exactly 2h accepted", func(r *rental.BatchRequest) { r.StartAt = h(2 * time.Hour) }, ""},
		{"too soon", func(r *rental.BatchRequest) { r.StartAt = h(time.Hour) }, "booking_too_soon"},
		{"day 60 accepted", func(r *rental.BatchRequest) { r.StartAt = h(60 * 24 * time.Hour) }, ""},
		{"day 60 + 1h rejected", func(r *rental.BatchRequest) { r.StartAt = h(60*24*time.Hour + time.Hour) }, "booking_too_far"},
		{"not hour", func(r *rental.BatchRequest) { r.StartAt = h(3*time.Hour + 30*time.Minute) }, "booking_not_hour"},
		{"1m x3 accepted", func(r *rental.BatchRequest) { r.StartAt = h(3 * time.Hour); r.Duration, r.Quantity = catalog.D1m, 3 }, ""},
		{"1m x4 rejected", func(r *rental.BatchRequest) { r.StartAt = h(3 * time.Hour); r.Duration, r.Quantity = catalog.D1m, 4 }, "booking_too_long"},
		{"1d x90 accepted immediate", func(r *rental.BatchRequest) { r.Duration, r.Quantity = catalog.D1d, 90 }, ""},
		{"1d x91 rejected", func(r *rental.BatchRequest) { r.Duration, r.Quantity = catalog.D1d, 91 }, "booking_too_long"},
		{"count 9", func(r *rental.BatchRequest) { r.Count = 9 }, "count_invalid"},
		{"count 0", func(r *rental.BatchRequest) { r.Count = 0 }, "count_invalid"},
		{"machine with count 2", func(r *rental.BatchRequest) { r.MachineID = ptr(e.machines[0]); r.Count = 2 }, "bad_request"},
		{"key with #", func(r *rental.BatchRequest) { r.IdempotencyKey = "abc#1" }, "bad_request"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := e.batchReq(1, nil)
			c.mut(&req)
			_, err := e.svc.PlaceBatch(e.ctx, req)
			if c.code == "" {
				if err != nil {
					t.Fatalf("want ok, got %v", err)
				}
				return
			}
			if got := codeOf(err); got != c.code {
				t.Fatalf("want %s, got %v", c.code, err)
			}
		})
	}
	// 非 UTC 时区传入的整点仍是整点（北京时间 11:00 = 03:00 UTC）
	loc := time.FixedZone("CST", 8*3600)
	req := e.batchReq(1, ptr(time.Date(2026, 1, 1, 11, 0, 0, 0, loc)))
	if _, err := e.svc.PlaceBatch(e.ctx, req); err != nil {
		t.Fatalf("CST hour = %v", err)
	}
	// 5 分钟容差：now = T0 + 5m 时 S = T0 + 2h（距开始 1h55m）接受；再过 1 秒拒绝
	e.clock.Advance(5 * time.Minute)
	if _, err := e.svc.PlaceBatch(e.ctx, e.batchReq(1, h(2*time.Hour))); err != nil {
		t.Fatalf("within tolerance = %v", err)
	}
	e.clock.Advance(time.Second)
	if _, err := e.svc.PlaceBatch(e.ctx, e.batchReq(1, h(2*time.Hour))); !errors.Is(err, apperr.ErrBookingTooSoon) {
		t.Fatalf("past tolerance = %v", err)
	}
}

func TestPlaceBatchBookingReservesWithoutAllocating(t *testing.T) {
	e := newEnv(t, 3)
	start := dbtest.T0.Add(3 * time.Hour)
	req := e.batchReq(2, &start)
	req.AutoRenew, req.AutoRenewDuration = true, catalog.D1w
	res, err := e.svc.PlaceBatch(e.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Batch == nil || res.Batch.Count != 2 || len(res.Orders) != 2 || len(res.Instances) != 2 {
		t.Fatalf("result = %+v", res)
	}
	for i, inst := range res.Instances {
		if inst.State != rental.StateReserved || !inst.Booked() || !inst.ScheduledStartAt.Equal(start) || inst.PendingSeconds != 86400 ||
			inst.BatchID == nil || *inst.BatchID != res.Batch.ID || !inst.AutoRenew || inst.AutoRenewDuration != catalog.D1w || inst.ExpiresAt != nil {
			t.Fatalf("instance[%d] = %+v", i, inst)
		}
		if ord := res.Orders[i]; ord.InstanceID != inst.ID || ord.Amount != 6900 || ord.Quantity != 1 || ord.BatchID == nil || *ord.BatchID != res.Batch.ID {
			t.Fatalf("order[%d] = %+v", i, ord)
		}
		if e.machineState(inst.MachineID) != fleet.StateIdle {
			t.Fatalf("booked machine must stay idle, got %s", e.machineState(inst.MachineID))
		}
		lower, upper, status := e.lease(inst.ID)
		if status != "reserved" || !lower.Equal(start.Add(-rental.PrepLead)) || !upper.Equal(start.Add(24*time.Hour+rental.Tail)) {
			t.Fatalf("lease = [%v, %v) %s", lower, upper, status)
		}
		if n := countPending(e, inst.MachineID, fleet.CmdCreateInstance); n != 0 {
			t.Fatalf("booking must not enqueue create, got %d", n)
		}
	}
	if res.Instances[0].MachineID == res.Instances[1].MachineID {
		t.Fatal("two machines expected")
	}
	if b := e.balance(e.userID); b != startFunds-2*6900 {
		t.Fatalf("balance = %d", b)
	}
	got, err := e.svc.GetBatch(e.ctx, e.userID, res.Batch.ID)
	if err != nil || len(got.Orders) != 2 || len(got.Instances) != 2 || got.Orders[1].InstanceID != got.Instances[1].ID {
		t.Fatalf("GetBatch = %+v, %v", got, err)
	}
	if _, err := e.svc.GetBatch(e.ctx, e.userID+1, res.Batch.ID); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("other user = %v", err)
	}
	// 第三台在同一时段还能订；第四台不够：整单失败，消息带台数，且前面成功的不受影响
	if _, err := e.svc.PlaceBatch(e.ctx, e.batchReq(2, &start)); codeOf(err) != "no_capacity" || !strings.Contains(err.Error(), "只有 1 台可用") {
		t.Fatalf("want no_capacity with count, got %v", err)
	}
	var reserved int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM instances WHERE state = 'reserved'`).Scan(&reserved)
	if reserved != 2 || e.balance(e.userID) != startFunds-2*6900 {
		t.Fatalf("partial batch leaked: reserved=%d balance=%d", reserved, e.balance(e.userID))
	}
}

func TestPlaceBatchImmediateIsAtomic(t *testing.T) {
	e := newEnv(t, 3)
	res, err := e.svc.PlaceBatch(e.ctx, e.batchReq(3, nil))
	if err != nil || len(res.Instances) != 3 {
		t.Fatalf("batch = %+v, %v", res, err)
	}
	for _, inst := range res.Instances {
		if inst.State != rental.StateCreating || e.machineState(inst.MachineID) != fleet.StateAllocated || countPending(e, inst.MachineID, fleet.CmdCreateInstance) != 1 {
			t.Fatalf("instance = %+v", inst)
		}
	}
	e.ack(true)
	for _, inst := range res.Instances {
		if e.get(inst.ID).State != rental.StateRunning {
			t.Fatal("expected running")
		}
	}
	// 余额不够付 2 台：整单回滚，机器仍空闲
	poor := e.newUser("13600000002", 7000)
	e.machines = append(e.machines, dbtest.CreateMachine(t, e.pool, "SN-X"), dbtest.CreateMachine(t, e.pool, "SN-Y"))
	req := e.batchReq(2, nil)
	req.UserID = poor
	if _, err := e.svc.PlaceBatch(e.ctx, req); !errors.Is(err, apperr.ErrInsufficientBalance) {
		t.Fatalf("want insufficient balance, got %v", err)
	}
	if e.machineState(e.machines[3]) != fleet.StateIdle || e.machineState(e.machines[4]) != fleet.StateIdle || e.balance(poor) != 7000 {
		t.Fatal("insufficient balance must roll back everything")
	}
}

func TestBatchIdempotencyReplayAndConflict(t *testing.T) {
	e := newEnv(t, 2)
	start := dbtest.T0.Add(3 * time.Hour)
	req := e.batchReq(2, &start)
	req.IdempotencyKey = "k1"
	first, err := e.svc.PlaceBatch(e.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.svc.PlaceBatch(e.ctx, req)
	if err != nil || again.Batch.ID != first.Batch.ID || len(again.Orders) != 2 || again.Instances[0].ID != first.Instances[0].ID || again.Instances[1].ID != first.Instances[1].ID {
		t.Fatalf("replay = %+v, %v", again, err) // 两边都按实例 ID 排序，顺序必须一致
	}
	if b := e.balance(e.userID); b != startFunds-2*6900 {
		t.Fatalf("replay charged again: %d", b)
	}
	var keys []string
	rows, _ := e.pool.Query(e.ctx, `SELECT idempotency_key FROM orders WHERE batch_id = $1 ORDER BY idempotency_key`, first.Batch.ID)
	for rows.Next() {
		var k string
		_ = rows.Scan(&k)
		keys = append(keys, k)
	}
	rows.Close()
	if len(keys) != 2 || keys[0] != "k1#1" || keys[1] != "k1#2" {
		t.Fatalf("sub-order keys = %v", keys)
	}
	// 同一个键走旧接口 → 冲突；走续费 → 冲突
	if _, _, err := e.svc.Place(e.ctx, rental.PlaceRequest{UserID: e.userID, ProductID: e.productID, ImageID: e.imageID, Duration: catalog.D1d, IdempotencyKey: "k1"}); codeOf(err) != "idempotency_conflict" {
		t.Fatalf("legacy with batch key = %v", err)
	}
	if _, err := e.svc.Renew(e.ctx, e.userID, first.Instances[0].ID, catalog.D1d, "k1"); codeOf(err) != "idempotency_conflict" {
		t.Fatalf("renew with batch key = %v", err)
	}
}

func TestLegacyOrderIdempotencyUnchanged(t *testing.T) {
	e := newEnv(t, 1)
	req := rental.PlaceRequest{UserID: e.userID, ProductID: e.productID, ImageID: e.imageID, Duration: catalog.D1d, IdempotencyKey: "legacy-1"}
	ord, inst, err := e.svc.Place(e.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	ord2, inst2, err := e.svc.Place(e.ctx, req)
	if err != nil || ord2.ID != ord.ID || inst2.ID != inst.ID {
		t.Fatalf("replay = %+v %+v %v", ord2, inst2, err)
	}
	if ord.BatchID != nil || ord.Quantity != 1 || ord.Auto {
		t.Fatalf("legacy order = %+v", ord)
	}
	var batches int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM rental_batches`).Scan(&batches)
	if batches != 0 {
		t.Fatalf("legacy path must not create batches, got %d", batches)
	}
	var key string
	_ = e.pool.QueryRow(e.ctx, `SELECT idempotency_key FROM orders WHERE id = $1`, ord.ID).Scan(&key)
	if key != "legacy-1" {
		t.Fatalf("legacy key stored as %q", key)
	}
	req.IdempotencyKey = "legacy#1"
	if _, _, err := e.svc.Place(e.ctx, req); codeOf(err) != "bad_request" {
		t.Fatalf("key with # = %v", err)
	}
	// 键被批次占用后旧接口报冲突（上面的 TestBatchIdempotencyReplayAndConflict 覆盖反向）
}

func TestBookingLimitPerUser(t *testing.T) {
	e := newEnv(t, 1)
	for i := 0; i < rental.MaxOpenReservationsPerUser; i++ {
		start := dbtest.T0.Add(3*time.Hour + time.Duration(i)*48*time.Hour)
		if _, err := e.svc.PlaceBatch(e.ctx, e.batchReq(1, &start)); err != nil {
			t.Fatalf("booking %d: %v", i, err)
		}
	}
	start := dbtest.T0.Add(3*time.Hour + 16*48*time.Hour)
	if _, err := e.svc.PlaceBatch(e.ctx, e.batchReq(1, &start)); !errors.Is(err, apperr.ErrBookingLimit) {
		t.Fatalf("17th = %v", err)
	}
}

func TestSpecificMachineUnavailable(t *testing.T) {
	e := newEnv(t, 2)
	start := dbtest.T0.Add(3 * time.Hour)
	e.reserveRaw(e.machines[0], start, 86400)
	req := e.batchReq(1, &start)
	req.MachineID = &e.machines[0]
	if _, err := e.svc.PlaceBatch(e.ctx, req); !errors.Is(err, apperr.ErrMachineUnavailable) {
		t.Fatalf("want machine_unavailable, got %v", err)
	}
	req.MachineID = &e.machines[1]
	if res, err := e.svc.PlaceBatch(e.ctx, req); err != nil || res.Instances[0].MachineID != e.machines[1] {
		t.Fatalf("specific machine = %+v, %v", res, err)
	}
}

func TestRenewQuantityChargesMultiple(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	ord, err := e.svc.RenewQuantity(e.ctx, e.userID, inst.ID, catalog.D1d, 3, "")
	if err != nil || ord.Amount != 3*6900 || ord.Quantity != 3 {
		t.Fatalf("renew x3 = %+v, %v", ord, err)
	}
	if got := e.get(inst.ID); !got.ExpiresAt.Equal(dbtest.T0.Add(4 * 24 * time.Hour)) {
		t.Fatalf("expires = %v", got.ExpiresAt)
	}
	if _, err := e.svc.RenewQuantity(e.ctx, e.userID, inst.ID, catalog.D1m, 4, ""); !errors.Is(err, apperr.ErrBookingTooLong) {
		t.Fatalf("1m x4 = %v", err)
	}
}

func TestRenewReservedExtendsPendingAndLease(t *testing.T) {
	e := newEnv(t, 1)
	start := dbtest.T0.Add(3 * time.Hour)
	res, err := e.svc.PlaceBatch(e.ctx, e.batchReq(1, &start))
	if err != nil {
		t.Fatal(err)
	}
	id := res.Instances[0].ID
	if _, err := e.svc.RenewQuantity(e.ctx, e.userID, id, catalog.D1d, 2, ""); err != nil {
		t.Fatalf("renew reserved = %v", err)
	}
	if got := e.get(id); got.State != rental.StateReserved || got.PendingSeconds != 3*86400 {
		t.Fatalf("instance = %+v", got)
	}
	lower, upper, status := e.lease(id)
	if status != "reserved" || !lower.Equal(start.Add(-rental.PrepLead)) || !upper.Equal(start.Add(3*24*time.Hour+rental.Tail)) {
		t.Fatalf("lease = [%v, %v) %s", lower, upper, status)
	}
}
