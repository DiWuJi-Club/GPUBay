// Package tz 固定使用北京时间（Asia/Shanghai，UTC+8，无夏令时）解释整点与自然日。
package tz

import "time"

// CST 北京时间。
var CST = time.FixedZone("CST", 8*3600)

// FloorHour 按北京时间取整到小时，结果转回 UTC。
func FloorHour(t time.Time) time.Time {
	l := t.In(CST)
	return time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), 0, 0, 0, CST).UTC()
}

// IsHour 是否整点（分、秒、纳秒都为 0）。
func IsHour(t time.Time) bool {
	l := t.In(CST)
	return l.Minute() == 0 && l.Second() == 0 && l.Nanosecond() == 0
}

// DayStart 北京时间当天 00:00，结果转回 UTC。
func DayStart(t time.Time) time.Time {
	l := t.In(CST)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, CST).UTC()
}

// Format 北京时间 2006-01-02 15:04。
func Format(t time.Time) string { return t.In(CST).Format("2006-01-02 15:04") }
