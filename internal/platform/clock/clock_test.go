package clock_test

import (
	"testing"
	"time"

	"spark-cloud/internal/platform/clock"
)

func TestFakeAdvance(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := clock.NewFake(start)
	c.Advance(90 * time.Minute)
	if got, want := c.Now(), start.Add(90*time.Minute); !got.Equal(want) {
		t.Fatalf("Now() = %v, want %v", got, want)
	}
}

func TestRealIsUTCMicrosecond(t *testing.T) {
	now := clock.Real{}.Now()
	if now.Location() != time.UTC {
		t.Fatalf("location = %v, want UTC", now.Location())
	}
	if now.Nanosecond()%1000 != 0 {
		t.Fatalf("Real().Now() must be truncated to microseconds, got %d ns", now.Nanosecond())
	}
}
