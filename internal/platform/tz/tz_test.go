package tz_test

import (
	"testing"
	"time"

	"spark-cloud/internal/platform/tz"
)

func TestHourHelpers(t *testing.T) {
	// 2026-03-01 23:30 北京时间 = 15:30 UTC
	x := time.Date(2026, 3, 1, 15, 30, 0, 0, time.UTC)
	if tz.IsHour(x) {
		t.Fatal("15:30 is not an hour boundary")
	}
	if got := tz.FloorHour(x); !got.Equal(time.Date(2026, 3, 1, 15, 0, 0, 0, time.UTC)) || got.Location() != time.UTC {
		t.Fatalf("FloorHour = %v", got)
	}
	if !tz.IsHour(tz.FloorHour(x)) {
		t.Fatal("floor must be an hour boundary")
	}
	// 北京时间 3 月 1 日 00:00 = 2 月 28 日 16:00 UTC
	if got := tz.DayStart(x); !got.Equal(time.Date(2026, 2, 28, 16, 0, 0, 0, time.UTC)) {
		t.Fatalf("DayStart = %v", got)
	}
	if got := tz.Format(x); got != "2026-03-01 23:30" {
		t.Fatalf("Format = %q", got)
	}
}

func TestHourHelpersBoundaries(t *testing.T) {
	// 北京时间 00:00 整 = 前一天 16:00 UTC：DayStart 返回自身
	midnight := time.Date(2026, 3, 1, 16, 0, 0, 0, time.UTC)
	if got := tz.DayStart(midnight); !got.Equal(midnight) {
		t.Fatalf("DayStart(midnight) = %v", got)
	}
	// 差 1 纳秒到北京时间零点：仍属前一天
	before := midnight.Add(-time.Nanosecond)
	if got := tz.DayStart(before); !got.Equal(midnight.Add(-24 * time.Hour)) {
		t.Fatalf("DayStart(just before midnight) = %v", got)
	}
	if !tz.IsHour(midnight) {
		t.Fatal("midnight is an hour boundary")
	}
	if tz.IsHour(midnight.Add(time.Nanosecond)) {
		t.Fatal("non-zero nanoseconds is not an hour boundary")
	}
}
