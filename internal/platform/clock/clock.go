// Package clock 抽象当前时间。业务代码一律通过 Clock 取时间，测试时注入 Fake 快进。
package clock

import (
	"sync"
	"time"
)

type Clock interface {
	Now() time.Time
}

// Real 返回截断到微秒的 UTC 时间，与 PostgreSQL timestamptz 精度一致。
type Real struct{}

func (Real) Now() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

type Fake struct {
	mu sync.Mutex
	t  time.Time
}

func NewFake(t time.Time) *Fake { return &Fake{t: t.UTC()} }

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}
