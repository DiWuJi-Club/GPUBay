package agentgw

import (
	"sync"
	"time"

	"spark-cloud/internal/platform/clock"
)

// ipLimiter 按来源 IP 做固定窗口计数（Enroll 每分钟 10 次）；窗口按注入时钟计算。
type ipLimiter struct {
	limit  int
	window time.Duration
	clk    clock.Clock
	mu     sync.Mutex
	seen   map[string]*ipWindow
}

type ipWindow struct {
	start time.Time
	n     int
}

func newIPLimiter(limit int, window time.Duration, clk clock.Clock) *ipLimiter {
	return &ipLimiter{limit: limit, window: window, clk: clk, seen: map[string]*ipWindow{}}
}

func (l *ipLimiter) allow(ip string) bool {
	now := l.clk.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.seen[ip]
	if w == nil || now.Sub(w.start) >= l.window {
		w = &ipWindow{start: now}
		l.seen[ip] = w
		if len(l.seen) > 10000 { // 防止内存无限增长
			for k, v := range l.seen {
				if now.Sub(v.start) >= l.window {
					delete(l.seen, k)
				}
			}
		}
	}
	w.n++
	return w.n <= l.limit
}
