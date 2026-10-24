package api

import (
	"testing"
	"time"
)

func TestUserLimiterWindowAndEviction(t *testing.T) {
	t0 := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	l := &userLimiter{limit: 2, window: time.Hour}
	if !l.allow(1, t0) || !l.allow(1, t0) || l.allow(1, t0.Add(59*time.Minute)) {
		t.Fatal("limit 2 per hour not enforced")
	}
	if !l.allow(1, t0.Add(time.Hour)) {
		t.Fatal("window did not slide")
	}
	for uid := int64(2); uid <= 10; uid++ {
		l.allow(uid, t0)
	}
	if l.size() != 10 {
		t.Fatalf("tracked users = %d, want 10", l.size())
	}
	// 窗口过后，活跃用户的每次调用顺手清掉一个过期用户
	later := t0.Add(2 * time.Hour)
	for i := 0; i < 20; i++ {
		l.allow(1, later.Add(time.Duration(i)*time.Second))
	}
	if l.size() != 1 {
		t.Fatalf("tracked users after window = %d, want 1 (expired users evicted)", l.size())
	}
	// window 零值 = 一分钟
	m := &userLimiter{limit: 1}
	if !m.allow(1, t0) || m.allow(1, t0.Add(59*time.Second)) || !m.allow(1, t0.Add(time.Minute)) {
		t.Fatal("default window is not one minute")
	}
}
