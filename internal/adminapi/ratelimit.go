package adminapi

import (
	"net/http"
	"sync"
	"time"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/httpx"
)

const loginLimit = 20 // 同一 IP 每分钟最多 20 次登录相关请求

// ipLimiter 是进程内按 IP 的滑动窗口计数器（后台只在内网单实例部署）。
type ipLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	clock  clock.Clock
	hits   map[string][]time.Time
}

func newIPLimiter(limit int, window time.Duration, clk clock.Clock) *ipLimiter {
	return &ipLimiter{limit: limit, window: window, clock: clk, hits: map[string][]time.Time{}}
}

func (l *ipLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock.Now()
	cutoff := now.Add(-l.window)
	recent := l.hits[ip][:0]
	for _, t := range l.hits[ip] {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	if len(recent) >= l.limit {
		l.hits[ip] = recent
		return false
	}
	l.hits[ip] = append(recent, now)
	if len(l.hits) > 10_000 { // 防止大量不同来源撑大内存：清掉窗口外的记录
		for k, v := range l.hits {
			if len(v) == 0 || !v[len(v)-1].After(cutoff) {
				delete(l.hits, k)
			}
		}
	}
	return true
}

func (s *Server) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.limiter.allow(httpx.ClientIP(r)) {
			s.writeError(w, r, apperr.ErrRateLimited)
			return
		}
		next.ServeHTTP(w, r)
	})
}
