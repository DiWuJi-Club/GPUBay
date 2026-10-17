package adminapi

import (
	"net/http/httptest"
	"testing"
	"time"

	"spark-cloud/internal/platform/apperr"
)

// 后台列表的 from / to：日期按北京时间解释，to 为日期时包含当天；RFC3339 原样转 UTC；其他格式 bad_request。
func TestParseTimeParam(t *testing.T) {
	utc := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cases := []struct {
		name     string
		v        string
		endOfDay bool
		want     *time.Time
		badReq   bool
	}{
		{"空值不筛选", "", false, nil, false},
		{"日期作为 from：北京时间当天 0 点", "2026-10-17", false, ptr(utc("2026-10-16T16:00:00Z")), false},
		{"日期作为 to：含当天，即次日北京时间 0 点", "2026-10-17", true, ptr(utc("2026-10-17T16:00:00Z")), false},
		{"跨月的 to", "2026-10-31", true, ptr(utc("2026-10-31T16:00:00Z")), false},
		{"RFC3339 带时区转 UTC", "2026-10-17T08:30:00+08:00", false, ptr(utc("2026-10-17T00:30:00Z")), false},
		{"RFC3339 作为 to 不加一天", "2026-10-17T08:30:00Z", true, ptr(utc("2026-10-17T08:30:00Z")), false},
		{"斜杠日期", "2026/10/17", false, nil, true},
		{"不存在的日期", "2026-02-30", false, nil, true},
		{"乱写", "yesterday", true, nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseTimeParam("from", c.v, c.endOfDay)
			if c.badReq {
				if e := apperr.As(err); e == nil || e.Code != "bad_request" {
					t.Fatalf("err = %v, want bad_request", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if (got == nil) != (c.want == nil) || (got != nil && (!got.Equal(*c.want) || got.Location() != time.UTC)) {
				t.Fatalf("got %v, want %v (UTC)", got, c.want)
			}
		})
	}
}

func TestParseRange(t *testing.T) {
	parse := func(q string) (*time.Time, *time.Time, error) {
		return parseRange(httptest.NewRequest("GET", "/audit?"+q, nil))
	}
	from, to, err := parse("from=2026-10-17&to=2026-10-17")
	if err != nil || !from.Equal(time.Date(2026, 10, 16, 16, 0, 0, 0, time.UTC)) || !to.Equal(time.Date(2026, 10, 17, 16, 0, 0, 0, time.UTC)) {
		t.Fatalf("same-day range = %v, %v, %v", from, to, err)
	}
	if from, to, err := parse(""); err != nil || from != nil || to != nil {
		t.Fatalf("empty = %v, %v, %v", from, to, err)
	}
	for _, q := range []string{"from=2026-10-18&to=2026-10-17", "from=bad", "to=bad", "from=2026-10-17T10:00:00Z&to=2026-10-17T10:00:00Z"} {
		if _, _, err := parse(q); apperr.As(err) == nil || apperr.As(err).Code != "bad_request" {
			t.Fatalf("%s: err = %v, want bad_request", q, err)
		}
	}
}

func ptr[T any](v T) *T { return &v }
