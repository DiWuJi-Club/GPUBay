package llm

import (
	"sync"
	"time"
)

// LimitWindow 是 RPM / TPM 的滑动窗口长度。
const LimitWindow = time.Minute

// CodeRateLimitExceeded 是网关 429 的 error.code（规格 §6.1 第 5 步：并发 / RPM / TPM 共用）。
const CodeRateLimitExceeded = "rate_limit_exceeded"

// Limits 是一个用户的限额。字段 ≤ 0 视为未配置，取默认值（DefaultRPM / DefaultTPM / DefaultConcurrency）。
type Limits struct{ RPM, TPM, Concurrency int }

func (l Limits) withDefaults() Limits {
	if l.RPM <= 0 {
		l.RPM = DefaultRPM
	}
	if l.TPM <= 0 {
		l.TPM = DefaultTPM
	}
	if l.Concurrency <= 0 {
		l.Concurrency = DefaultConcurrency
	}
	return l
}

// LimitError 是 Limiter.Acquire 的拒绝：Code 固定 rate_limit_exceeded；Limit ∈ concurrency / rpm / tpm；
// RetryAfter 为整秒、≥ 1s（网关写 Retry-After 头）。
type LimitError struct {
	Code       string
	Limit      string
	RetryAfter time.Duration
	Message    string
}

func (e *LimitError) Error() string { return e.Message }

type tokenHit struct {
	at time.Time
	n  int
}

type userLimit struct {
	hits        []time.Time // RPM：窗口内放行的请求时刻（升序）
	tokens      []tokenHit  // TPM：窗口内请求结束时记的实际 token（升序）
	tokenSum    int64       // Σ tokens[].n
	inflight    int         // 在途请求数
	inflightEst int64       // 预算预检的在途估算（分）
}

func (u *userLimit) prune(now time.Time) {
	cut := now.Add(-LimitWindow)
	i := 0
	for i < len(u.hits) && !u.hits[i].After(cut) {
		i++
	}
	u.hits = u.hits[i:]
	j := 0
	for j < len(u.tokens) && !u.tokens[j].at.After(cut) {
		u.tokenSum -= int64(u.tokens[j].n)
		j++
	}
	u.tokens = u.tokens[j:]
}

func (u *userLimit) empty() bool {
	return len(u.hits) == 0 && len(u.tokens) == 0 && u.inflight == 0 && u.inflightEst == 0
}

// gcEvery：每这么多次写操作扫一遍全表，回收窗口已空、无在途的用户条目。
const gcEvery = 1024

// Limiter 是进程内的按用户限流（规格 §1.1 #8、T4、T15：多副本时各算一份）：
// 并发、RPM 滑动窗口、TPM 滑动窗口（已计实际 token + 本次估算），以及预算预检的在途估算。
// 时间由调用方传入（clock.Clock），本类型不读系统时钟。并发安全。
type Limiter struct {
	mu     sync.Mutex
	users  map[int64]*userLimit
	ops    int
	lastAt time.Time // 最近一次带时间的操作，GC 用
}

func NewLimiter() *Limiter { return &Limiter{users: map[int64]*userLimit{}} }

// user 取（或建）用户条目；调用方持锁。
func (l *Limiter) user(userID int64) *userLimit {
	u := l.users[userID]
	if u == nil {
		u = &userLimit{}
		l.users[userID] = u
	}
	return u
}

// tick 记录一次操作并按需 GC；调用方持锁。
func (l *Limiter) tick(now time.Time) {
	if now.After(l.lastAt) {
		l.lastAt = now
	}
	l.ops++
	if l.ops < gcEvery {
		return
	}
	l.ops = 0
	for id, u := range l.users {
		u.prune(l.lastAt)
		if u.empty() {
			delete(l.users, id)
		}
	}
}

// ceilSeconds 向上取整到秒，至少 1 秒。
func ceilSeconds(d time.Duration) time.Duration {
	if d <= time.Second {
		return time.Second
	}
	return (d + time.Second - 1) / time.Second * time.Second
}

// Acquire 按「并发 → RPM → TPM（含 estTokens）」三检。通过：在途 +1、记一次 RPM，返回 release（在途 −1，幂等）。
// 拒绝：*LimitError（被拒的请求不记 RPM）。estTokens 只参与本次 TPM 判断，不入窗口；实际 token 在结束时 AddTokens。
func (l *Limiter) Acquire(userID int64, lim Limits, estTokens int, now time.Time) (release func(), err error) {
	lim = lim.withDefaults()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tick(now)
	u := l.user(userID)
	u.prune(now)

	if u.inflight >= lim.Concurrency {
		return nil, &LimitError{Code: CodeRateLimitExceeded, Limit: "concurrency", RetryAfter: time.Second,
			Message: "并发请求数超过上限，请稍后再试"}
	}
	if len(u.hits) >= lim.RPM {
		// 要放行，窗口内最多留 RPM−1 次：等第 len−RPM+1 早的那次滑出
		oldest := u.hits[len(u.hits)-lim.RPM]
		return nil, &LimitError{Code: CodeRateLimitExceeded, Limit: "rpm",
			RetryAfter: ceilSeconds(oldest.Add(LimitWindow).Sub(now)),
			Message:    "每分钟请求数超过上限，请稍后再试"}
	}
	est := int64(max(estTokens, 0))
	if u.tokenSum+est > int64(lim.TPM) {
		// 等最早的若干条滑出直到放得下；全滑出仍放不下（单次估算超限）→ 一个整窗口
		wait := LimitWindow
		sum := u.tokenSum
		for _, h := range u.tokens {
			sum -= int64(h.n)
			if sum+est <= int64(lim.TPM) {
				wait = h.at.Add(LimitWindow).Sub(now)
				break
			}
		}
		return nil, &LimitError{Code: CodeRateLimitExceeded, Limit: "tpm", RetryAfter: ceilSeconds(wait),
			Message: "每分钟 token 数超过上限，请稍后再试"}
	}

	u.inflight++
	// 保持升序：并发请求读时钟与拿锁的先后可能交错，晚到的旧时刻按当前最晚时刻记
	if k := len(u.hits); k > 0 && now.Before(u.hits[k-1]) {
		now = u.hits[k-1]
	}
	u.hits = append(u.hits, now)
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			u := l.user(userID)
			u.inflight--
		})
	}, nil
}

// AddTokens 在请求结束时按实际 token（prompt + completion）记入 TPM 窗口。
func (l *Limiter) AddTokens(userID int64, n int, now time.Time) {
	if n <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tick(now)
	u := l.user(userID)
	u.prune(now)
	// 保持升序（同 Acquire）
	if k := len(u.tokens); k > 0 && now.Before(u.tokens[k-1].at) {
		now = u.tokens[k-1].at
	}
	u.tokens = append(u.tokens, tokenHit{at: now, n: n})
	u.tokenSum += int64(n)
}

// ReserveEstimate 把本次请求的预算估算（分）计入用户的在途估算，返回 release（减回，幂等）。
func (l *Limiter) ReserveEstimate(userID int64, fen int64) (release func()) {
	l.mu.Lock()
	l.user(userID).inflightEst += fen
	l.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.user(userID).inflightEst -= fen
		})
	}
}

// InflightEstimate 返回用户当前在途估算总额（分），预算预检用。
func (l *Limiter) InflightEstimate(userID int64) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if u := l.users[userID]; u != nil {
		return u.inflightEst
	}
	return 0
}
