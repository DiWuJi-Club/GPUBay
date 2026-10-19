package rental_test

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/rental"
)

// TestConcurrentBatchesConsistent：10 台机器、51 个并发批次，分三组争三个互不重叠的时段，每组需求都远超容量：
//   - 第 0 组：即时订单，每批 1 台（17 批，需求 17 台）；
//   - 第 1 组：T0+3d 开始的预订，每批 2 台（17 批，需求 34 台）；
//   - 第 2 组：T0+6d 开始的预订，每批 3 台（17 批，需求 51 台）。
//
// 每批 1d × 1～2：组内任意两批的时段都包含 [开始, 开始+1d)，两两重叠；三组之间不重叠（即时最晚到 T0+55h20m，
// 第 1 组最早 T0+71h、最晚到 T0+127h，第 2 组最早 T0+143h）。所以每组能放下的台数恰好是 ⌊10/count⌋×count。
// 修复前（SKIP LOCKED 跳过别人只是短暂锁住的空闲机器就报 no_capacity）同样的争用下成功批次远低于容量；
// 修复后每组成功批次 × count 必须恰好等于 min(容量, 需求)（一台假的 no_capacity 都不允许）。
// 其余不变式：同机 reserved / active 时段两两不交；批次内机器两两不同；台数 = 订单数 = count；
// 失败批次不留任何实例 / 订单 / 批次行且余额不变；成功批次余额恰好扣 6900 × count × quantity；
// reserved 实例恰有一条 reserved lease；失败只允许 no_capacity；整体在 60 秒内完成（没有死锁）。
func TestConcurrentBatchesConsistent(t *testing.T) {
	const funds = 100_000_000
	const machines = 10
	e := newEnv(t, machines)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rng := rand.New(rand.NewSource(42))
	type job struct {
		uid   int64
		group int
		req   rental.BatchRequest
	}
	groupCount := [3]int{1, 2, 3}
	groupStart := [3]*time.Time{nil, ptr(dbtest.T0.Add(3 * 24 * time.Hour)), ptr(dbtest.T0.Add(6 * 24 * time.Hour))}
	jobs := make([]job, 51)
	var demand [3]int
	for i := range jobs {
		g := i % 3
		uid := e.newUser(fmt.Sprintf("1370000%04d", i), funds)
		req := rental.BatchRequest{UserID: uid, ProductID: e.productID, ImageID: e.imageID, Count: groupCount[g],
			Duration: catalog.D1d, Quantity: 1 + rng.Intn(2), StartAt: groupStart[g], IdempotencyKey: fmt.Sprintf("c-%d", i)}
		demand[g] += req.Count
		jobs[i] = job{uid: uid, group: g, req: req}
	}
	type outcome struct {
		res rental.BatchResult
		err error
	}
	results := make([]outcome, len(jobs))
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			res, err := e.svc.PlaceBatch(ctx, j.req)
			results[i] = outcome{res, err}
		}(i, j)
	}
	wg.Wait()
	if ctx.Err() != nil {
		t.Fatalf("batches did not finish within 60s: %v", ctx.Err())
	}

	okCount, failCodes := 0, map[string]int{}
	var placed [3]int
	for i, r := range results {
		j := jobs[i]
		var instances, orders, batches int
		if err := e.pool.QueryRow(e.ctx, `SELECT (SELECT count(*) FROM instances WHERE user_id = $1), (SELECT count(*) FROM orders WHERE user_id = $1),
			(SELECT count(*) FROM rental_batches WHERE user_id = $1)`, j.uid).Scan(&instances, &orders, &batches); err != nil {
			t.Fatal(err)
		}
		bal := e.balance(j.uid)
		if r.err != nil {
			ae := apperr.As(r.err)
			if ae == nil {
				t.Fatalf("job %d: unexpected error: %v", i, r.err)
			}
			failCodes[ae.Code]++
			if instances != 0 || orders != 0 || batches != 0 || bal != funds {
				t.Errorf("job %d failed (%s) but left instances=%d orders=%d batches=%d balance=%d", i, ae.Code, instances, orders, batches, bal)
			}
			continue
		}
		okCount++
		placed[j.group] += j.req.Count
		res := r.res
		seen := map[int64]bool{}
		for _, inst := range res.Instances {
			if seen[inst.MachineID] {
				t.Errorf("job %d: batch %s has two instances on machine %d", i, res.Batch.ID, inst.MachineID)
			}
			seen[inst.MachineID] = true
		}
		n := j.req.Count
		if res.Batch.Count != n || len(res.Instances) != n || len(res.Orders) != n || instances != n || orders != n || batches != 1 {
			t.Errorf("job %d: count %d but batch=%d returned %d instances / %d orders, stored %d instances / %d orders / %d batches",
				i, n, res.Batch.Count, len(res.Instances), len(res.Orders), instances, orders, batches)
		}
		if want := int64(funds - 6900*n*j.req.Quantity); bal != want {
			t.Errorf("job %d: balance = %d, want %d", i, bal, want)
		}
	}
	for code := range failCodes {
		if code != "no_capacity" {
			t.Fatalf("unexpected failure code %s: %v", code, failCodes)
		}
	}
	// 每组：成功台数 = min(容量, 需求)。容量 = ⌊机器数/count⌋×count（组内两两重叠、组间不重叠，见函数注释）。
	for g, c := range groupCount {
		capacity := machines / c * c
		if want := min(capacity, demand[g]); placed[g] != want {
			t.Errorf("group %d (count %d): placed %d machines, want min(capacity %d, demand %d) = %d (ok=%d fails=%v)",
				g, c, placed[g], capacity, demand[g], want, okCount, failCodes)
		}
	}
	var overlaps, badBatches, badReserved int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM machine_leases a JOIN machine_leases b ON a.machine_id = b.machine_id AND a.id < b.id
		WHERE a.status IN ('reserved', 'active') AND b.status IN ('reserved', 'active') AND a.period && b.period`).Scan(&overlaps); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM rental_batches b WHERE (SELECT count(*) FROM orders o WHERE o.batch_id = b.id) <> b.count
		OR (SELECT count(*) FROM instances i WHERE i.batch_id = b.id) <> b.count
		OR (SELECT count(DISTINCT i.machine_id) FROM instances i WHERE i.batch_id = b.id) <> b.count`).Scan(&badBatches); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM instances i WHERE i.state = 'reserved'
		AND (SELECT count(*) FROM machine_leases l WHERE l.instance_id = i.id AND l.status = 'reserved') <> 1`).Scan(&badReserved); err != nil {
		t.Fatal(err)
	}
	if overlaps != 0 || badBatches != 0 || badReserved != 0 {
		t.Fatalf("overlaps=%d badBatches=%d badReserved=%d (ok=%d fails=%v)", overlaps, badBatches, badReserved, okCount, failCodes)
	}
	t.Logf("ok=%d fails=%v placed=%v", okCount, failCodes, placed)
}
