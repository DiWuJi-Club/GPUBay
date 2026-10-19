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

// TestConcurrentBatchesConsistent：20 台机器、50 个并发批次（每批 1～3 台、开始时刻在 3 天内的随机整点、1d × 1～2；五分之一即时）。
// 机器数取 20 而不是 10：并发分配用 SKIP LOCKED，10 台时实测成功批次 5～11 个，检出力不稳定；20 台稳定在 14～22 个（含 -race）。
// 窗口收窄到 3 天，让大量批次争同一批机器：既有足够多的成功批次（≥ 10）检查一致性，也必然有 no_capacity。
// 事后：同机 reserved / active 时段两两不交；批次内机器两两不同；台数 = 订单数 = count；
// 失败批次不留任何实例 / 订单 / 批次行且余额不变；成功批次余额恰好扣 6900 × count × quantity；
// reserved 实例恰有一条 reserved lease；失败只允许 no_capacity；整体在 60 秒内完成（没有死锁）。
func TestConcurrentBatchesConsistent(t *testing.T) {
	const funds = 100_000_000
	e := newEnv(t, 20)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rng := rand.New(rand.NewSource(42))
	type job struct {
		uid int64
		req rental.BatchRequest
	}
	jobs := make([]job, 50)
	for i := range jobs {
		uid := e.newUser(fmt.Sprintf("1370000%04d", i), funds)
		start := dbtest.T0.Add(time.Duration(3+rng.Intn(3*24)) * time.Hour)
		req := rental.BatchRequest{UserID: uid, ProductID: e.productID, ImageID: e.imageID, Count: 1 + rng.Intn(3),
			Duration: catalog.D1d, Quantity: 1 + rng.Intn(2), StartAt: &start, IdempotencyKey: fmt.Sprintf("c-%d", i)}
		if i%5 == 0 { // 五分之一是即时订单
			req.StartAt = nil
		}
		jobs[i] = job{uid: uid, req: req}
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
	// 争用必须真实存在：成功足够多（一致性检查有对象），失败也必须出现（否则窗口没挤到）
	if okCount < 10 || failCodes["no_capacity"] == 0 {
		t.Fatalf("contention not meaningful: ok=%d fails=%v", okCount, failCodes)
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
	t.Logf("ok=%d fails=%v", okCount, failCodes)
}
