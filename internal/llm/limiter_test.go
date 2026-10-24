package llm_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"spark-cloud/internal/llm"
	"spark-cloud/internal/platform/dbtest"
)

func rtLimitErr(t *testing.T, err error) *llm.LimitError {
	t.Helper()
	var le *llm.LimitError
	if !errors.As(err, &le) {
		t.Fatalf("want *LimitError, got %v", err)
	}
	if le.Code != llm.CodeRateLimitExceeded {
		t.Fatalf("code = %q", le.Code)
	}
	return le
}

// 60 次通过、第 61 次拒绝且 RetryAfter = 最早一次离开窗口的剩余秒数；滑过后恢复。
func TestLimiterRPMWindow(t *testing.T) {
	l := llm.NewLimiter()
	lim := llm.Limits{RPM: 60, TPM: 1 << 30, Concurrency: 1000}
	t0 := dbtest.T0
	for i := 0; i < 60; i++ {
		rel, err := l.Acquire(1, lim, 1, t0.Add(time.Duration(i)*500*time.Millisecond))
		if err != nil {
			t.Fatalf("hit %d: %v", i, err)
		}
		rel()
	}
	// 第一次在 t0，此刻 t0+40.3s → 还要 19.7s 才离开窗口，向上取整 20s
	_, err := l.Acquire(1, lim, 1, t0.Add(40300*time.Millisecond))
	if le := rtLimitErr(t, err); le.RetryAfter != 20*time.Second || le.Limit != "rpm" {
		t.Fatalf("retry after = %v, limit = %q", le.RetryAfter, le.Limit)
	}
	// 被拒的请求不占 RPM：其它用户不受影响
	if rel, err := l.Acquire(2, lim, 1, t0.Add(41*time.Second)); err != nil {
		t.Fatalf("other user: %v", err)
	} else {
		rel()
	}
	// t0+59.9s：仍满，剩 0.1s → RetryAfter 至少 1s
	_, err = l.Acquire(1, lim, 1, t0.Add(59900*time.Millisecond))
	if le := rtLimitErr(t, err); le.RetryAfter != time.Second {
		t.Fatalf("retry after = %v", le.RetryAfter)
	}
	// t0+60s：t0 那次滑出窗口，放行一次；紧接着再一次（t0+0.5s 那次还在）拒绝
	rel, err := l.Acquire(1, lim, 1, t0.Add(60*time.Second))
	if err != nil {
		t.Fatalf("after slide: %v", err)
	}
	rel()
	_, err = l.Acquire(1, lim, 1, t0.Add(60*time.Second))
	if le := rtLimitErr(t, err); le.RetryAfter != time.Second {
		t.Fatalf("retry after = %v", le.RetryAfter)
	}
	// 整个窗口过去：全部恢复
	for i := 0; i < 60; i++ {
		rel, err := l.Acquire(1, lim, 1, t0.Add(3*time.Minute))
		if err != nil {
			t.Fatalf("new window hit %d: %v", i, err)
		}
		rel()
	}
}

// TPM：过去 60 秒已计 token + 本次估算 > 上限 → 拒绝；估算本身不计入 TPM，结束时按实际 AddTokens。
func TestLimiterTPMIncludesEstimate(t *testing.T) {
	l := llm.NewLimiter()
	lim := llm.Limits{RPM: 1000, TPM: 1000, Concurrency: 100}
	t0 := dbtest.T0
	rel, err := l.Acquire(1, lim, 1000, t0) // 恰好等于上限：放行
	if err != nil {
		t.Fatalf("est == limit: %v", err)
	}
	rel()
	// 估算不进窗口：仍可再估 1000
	rel, err = l.Acquire(1, lim, 1000, t0.Add(time.Second))
	if err != nil {
		t.Fatalf("estimate must not be recorded: %v", err)
	}
	rel()
	l.AddTokens(1, 600, t0.Add(10*time.Second))
	l.AddTokens(1, 300, t0.Add(20*time.Second))
	// 900 已计 + 估算 101 > 1000 → 拒绝；要等 t0+10s 那 600 滑出（t0+70s），此刻 t0+30s → 40s
	_, err = l.Acquire(1, lim, 101, t0.Add(30*time.Second))
	if le := rtLimitErr(t, err); le.Limit != "tpm" || le.RetryAfter != 40*time.Second {
		t.Fatalf("limit = %q, retry after = %v", le.Limit, le.RetryAfter)
	}
	// 估算 100 → 恰好 1000：放行
	rel, err = l.Acquire(1, lim, 100, t0.Add(30*time.Second))
	if err != nil {
		t.Fatalf("900 + 100: %v", err)
	}
	rel()
	// 600 滑出后：300 + 700 放行
	rel, err = l.Acquire(1, lim, 700, t0.Add(70*time.Second))
	if err != nil {
		t.Fatalf("after slide: %v", err)
	}
	rel()
	// 单次估算超过上限：按上限封顶 → 窗口里还有 300@t0+20s 时拒绝，等它滑出（t0+80s）；窗口清空后放行
	_, err = l.Acquire(1, lim, 5000, t0.Add(70*time.Second))
	if le := rtLimitErr(t, err); le.RetryAfter != 10*time.Second {
		t.Fatalf("oversized estimate retry after = %v", le.RetryAfter)
	}
	rel, err = l.Acquire(1, lim, 5000, t0.Add(80*time.Second))
	if err != nil {
		t.Fatalf("oversized estimate with empty window must be admitted: %v", err)
	}
	rel()
	// 其它用户互不影响
	if rel, err := l.Acquire(2, lim, 1000, t0.Add(30*time.Second)); err != nil {
		t.Fatalf("other user: %v", err)
	} else {
		rel()
	}
}

// 并发：满了拒绝（RetryAfter 1s）、release 后恢复；release 幂等；被拒的请求不记 RPM。
func TestLimiterConcurrencyRelease(t *testing.T) {
	l := llm.NewLimiter()
	lim := llm.Limits{RPM: 3, TPM: 1 << 30, Concurrency: 2}
	t0 := dbtest.T0
	r1, err := l.Acquire(1, lim, 1, t0)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := l.Acquire(1, lim, 1, t0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = l.Acquire(1, lim, 1, t0)
	if le := rtLimitErr(t, err); le.Limit != "concurrency" || le.RetryAfter != time.Second {
		t.Fatalf("limit = %q, retry after = %v", le.Limit, le.RetryAfter)
	}
	r1()
	r1() // 幂等：不得多减
	r3, err := l.Acquire(1, lim, 1, t0)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	// 并发仍满（r2、r3）：再来一个是并发拒绝而不是 RPM
	_, err = l.Acquire(1, lim, 1, t0)
	if le := rtLimitErr(t, err); le.Limit != "concurrency" {
		t.Fatalf("limit = %q", le.Limit)
	}
	r2()
	r3()
	// 已记 3 次 RPM（r1 r2 r3），被拒的两次不算 → 第 4 次是 RPM 拒绝
	_, err = l.Acquire(1, lim, 1, t0)
	if le := rtLimitErr(t, err); le.Limit != "rpm" {
		t.Fatalf("limit = %q", le.Limit)
	}

	// 未配置（≤0）的字段取默认值：并发 8
	l2 := llm.NewLimiter()
	var rels []func()
	for i := 0; i < llm.DefaultConcurrency; i++ {
		rel, err := l2.Acquire(9, llm.Limits{}, 1, t0)
		if err != nil {
			t.Fatalf("default concurrency %d: %v", i, err)
		}
		rels = append(rels, rel)
	}
	if _, err := l2.Acquire(9, llm.Limits{}, 1, t0); err == nil {
		t.Fatal("default concurrency not enforced")
	}
	for _, r := range rels {
		r()
	}
}

// 在途预算估算：累加、各自 release 一次、幂等、按用户隔离。
func TestLimiterEstimateReserve(t *testing.T) {
	l := llm.NewLimiter()
	if got := l.InflightEstimate(1); got != 0 {
		t.Fatalf("initial = %d", got)
	}
	a := l.ReserveEstimate(1, 30)
	b := l.ReserveEstimate(1, 12)
	c := l.ReserveEstimate(2, 5)
	if got := l.InflightEstimate(1); got != 42 {
		t.Fatalf("user1 = %d", got)
	}
	a()
	a()
	if got := l.InflightEstimate(1); got != 12 {
		t.Fatalf("after release = %d", got)
	}
	if got := l.InflightEstimate(2); got != 5 {
		t.Fatalf("user2 = %d", got)
	}
	b()
	c()
	if l.InflightEstimate(1) != 0 || l.InflightEstimate(2) != 0 {
		t.Fatal("not back to zero")
	}
}

// -race：并发 Acquire / release / AddTokens / ReserveEstimate 后计数回零、并发上限从不被突破。
func TestLimiterConcurrentUse(t *testing.T) {
	l := llm.NewLimiter()
	lim := llm.Limits{RPM: 1 << 30, TPM: 1 << 30, Concurrency: 4}
	t0 := dbtest.T0
	var mu sync.Mutex
	var cur, peak [2]int
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				now := t0.Add(time.Duration(i) * time.Millisecond)
				rel, err := l.Acquire(int64(g%2), lim, 10, now)
				if err != nil {
					continue
				}
				est := l.ReserveEstimate(int64(g%2), 3)
				mu.Lock()
				cur[g%2]++
				peak[g%2] = max(peak[g%2], cur[g%2])
				mu.Unlock()
				l.AddTokens(int64(g%2), 5, now)
				mu.Lock()
				cur[g%2]--
				mu.Unlock()
				est()
				rel()
			}
		}(g)
	}
	wg.Wait()
	if peak[0] > 4 || peak[1] > 4 {
		t.Fatalf("concurrency limit broken: peak = %v", peak)
	}
	if l.InflightEstimate(0) != 0 || l.InflightEstimate(1) != 0 {
		t.Fatal("estimates leaked")
	}
	// 释放干净后两个用户都能再拿满并发
	for u := int64(0); u < 2; u++ {
		var rels []func()
		for i := 0; i < 4; i++ {
			rel, err := l.Acquire(u, lim, 1, t0.Add(time.Hour))
			if err != nil {
				t.Fatalf("user %d slot %d: %v", u, i, err)
			}
			rels = append(rels, rel)
		}
		for _, r := range rels {
			r()
		}
	}
}
