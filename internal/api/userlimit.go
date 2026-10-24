package api

import (
	"sync"
	"time"
)

// userLimiter 进程内按用户的滑动窗口限流：authorize（票据签发，每分钟，也限住了 nonce 表的增长速度）与
// 应用部署类写操作（每小时，规格 §6.2）。window 零值 = 一分钟。
type userLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[int64][]time.Time
}

func (l *userLimiter) allow(userID int64, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hits == nil {
		l.hits = map[int64][]time.Time{}
	}
	window := l.window
	if window <= 0 {
		window = time.Minute
	}
	// 与 ipLimiter 相同的思路：每次调用（含被拒的）顺手清掉一个窗口已过期的其它用户键，map 不随历史用户数无界增长（摊销 O(1)）
	for k, v := range l.hits {
		if k != userID && (len(v) == 0 || now.Sub(v[len(v)-1]) >= window) {
			delete(l.hits, k)
			break
		}
	}
	ts := l.hits[userID]
	i := 0
	for i < len(ts) && now.Sub(ts[i]) >= window {
		i++
	}
	ts = ts[i:]
	if len(ts) >= l.limit {
		l.hits[userID] = ts
		return false
	}
	l.hits[userID] = append(ts, now)
	return true
}

// size 当前跟踪的用户数（测试用）。
func (l *userLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.hits)
}
