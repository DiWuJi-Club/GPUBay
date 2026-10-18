package client

import (
	"math/rand/v2"
	"time"
)

// Backoff 指数退避：Min·2^attempt，封顶 Max，±20% 抖动。零值为 1s / 60s。
type Backoff struct {
	Min, Max time.Duration
}

func (b Backoff) Next(attempt int) time.Duration {
	if b.Min <= 0 {
		b.Min = time.Second
	}
	if b.Max <= 0 {
		b.Max = 60 * time.Second
	}
	d := b.Min
	for i := 0; i < attempt && d < b.Max; i++ {
		d *= 2
	}
	if d > b.Max {
		d = b.Max
	}
	jitter := 1 + (rand.Float64()*0.4 - 0.2)
	return time.Duration(float64(d) * jitter)
}
