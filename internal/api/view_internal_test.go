package api

import (
	"testing"
	"time"

	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/rental"
)

// 预订实例的 scheduled_end_at：开机前 = 开始 + 待开始时长；running / stopped 以 expires_at 为准（续费、迟开通补偿会改它）。
func TestViewScheduledEnd(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := &Server{Clock: clock.NewFake(now)}
	start := now.Add(3 * time.Hour)
	reserved := s.view(rental.Instance{ID: "a", State: rental.StateReserved, ScheduledStartAt: &start, PendingSeconds: 86400})
	if !reserved.ScheduledEndAt.Equal(start.Add(24*time.Hour)) || !reserved.CancellableUntil.Equal(start.Add(-rental.PrepLead)) || reserved.RenewOptions == nil {
		t.Fatalf("reserved = %+v", reserved)
	}
	expires := start.Add(50 * time.Hour)
	for _, st := range []string{rental.StateRunning, rental.StateStopped} {
		v := s.view(rental.Instance{ID: "a", State: st, ScheduledStartAt: &start, ExpiresAt: &expires})
		if !v.ScheduledEndAt.Equal(expires) || v.CancellableUntil != nil {
			t.Fatalf("%s = %+v", st, v)
		}
	}
	if v := s.view(rental.Instance{ID: "a", State: rental.StateRunning, ExpiresAt: &expires}); v.ScheduledStartAt != nil || v.ScheduledEndAt != nil {
		t.Fatalf("immediate = %+v", v)
	}
}

// 终审 M3：cancellable_seconds 按服务器时钟算到取消截止（S − PrepLead）的剩余秒数，过了截止或非 reserved 为 0；
// 前端以拿到数据的本地时刻为基准递减（同 remaining_seconds），不再拿浏览器时钟比 cancellable_until。
func TestViewCancellableSeconds(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := &Server{Clock: clock.NewFake(now)}
	start := now.Add(3 * time.Hour)
	if v := s.view(rental.Instance{ID: "a", State: rental.StateReserved, ScheduledStartAt: &start, PendingSeconds: 86400}); v.CancellableSeconds != 2*3600 {
		t.Fatalf("reserved = %d", v.CancellableSeconds)
	}
	soon := now.Add(30 * time.Minute) // 已过截止（开始前 1 小时），仍是 reserved（清扫还没开通）
	if v := s.view(rental.Instance{ID: "a", State: rental.StateReserved, ScheduledStartAt: &soon, PendingSeconds: 86400}); v.CancellableSeconds != 0 || v.CancellableUntil == nil {
		t.Fatalf("past deadline = %+v", v)
	}
	if v := s.view(rental.Instance{ID: "a", State: rental.StateCreating, ScheduledStartAt: &start, PendingSeconds: 86400}); v.CancellableSeconds != 0 {
		t.Fatalf("creating = %d", v.CancellableSeconds)
	}
}
