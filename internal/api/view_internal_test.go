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
