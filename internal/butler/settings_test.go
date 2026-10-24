package butler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/dbtest"
)

func newTestStore(t *testing.T) (*Store, *clock.Fake) {
	t.Helper()
	clk := clock.NewFake(dbtest.T0)
	return NewStore(dbtest.New(t), clk), clk
}

func TestSettingsDefaultRow(t *testing.T) {
	s, _ := newTestStore(t)
	v, err := s.Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	d := DefaultSettings()
	if v.Enabled != d.Enabled || v.DailyTokenLimit != d.DailyTokenLimit || v.MaxTurns != d.MaxTurns || v.Notice != "" || v.UpdatedBy != nil {
		t.Fatalf("default settings = %+v", v)
	}
}

func TestUpdateSettingsValidation(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	ok := Settings{Enabled: true, DailyTokenLimit: 300000, MaxTurns: 60}
	bad := []func(*Settings){
		func(v *Settings) { v.DailyTokenLimit = MinDailyTokenLimit - 1 },
		func(v *Settings) { v.DailyTokenLimit = MaxDailyTokenLimit + 1 },
		func(v *Settings) { v.MaxTurns = MinMaxTurns - 1 },
		func(v *Settings) { v.MaxTurns = MaxMaxTurns + 1 },
		func(v *Settings) { v.Notice = strings.Repeat("停", MaxNoticeRunes+1) },
		func(v *Settings) { v.Notice = "a\x00b" },
	}
	for i, mut := range bad {
		in := ok
		mut(&in)
		_, err := s.UpdateSettings(ctx, audit.Actor{AdminID: 1}, in)
		if e := apperr.As(err); e == nil || e.Code != "bad_request" {
			t.Errorf("case %d: err = %v, want bad_request", i, err)
		}
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action = $1`, AuditSettings).Scan(&n); err != nil || n != 0 {
		t.Fatalf("audit rows after invalid updates = %d, %v", n, err)
	}
	// 边界值合法；notice 去首尾空白、恰好 200 字可以
	in := Settings{Enabled: false, DailyTokenLimit: MinDailyTokenLimit, MaxTurns: MaxMaxTurns, Notice: "  " + strings.Repeat("停", MaxNoticeRunes) + "\n"}
	v, err := s.UpdateSettings(ctx, audit.Actor{AdminID: 1}, in)
	if err != nil {
		t.Fatal(err)
	}
	if v.Notice != strings.Repeat("停", MaxNoticeRunes) || v.MaxTurns != MaxMaxTurns || v.DailyTokenLimit != MinDailyTokenLimit {
		t.Fatalf("boundary update = %+v", v)
	}
}

func TestUpdateSettingsAudit(t *testing.T) {
	s, clk := newTestStore(t)
	ctx := context.Background()
	clk.Advance(time.Hour)
	v, err := s.UpdateSettings(ctx, audit.Actor{AdminID: 7, IP: "10.0.0.1"},
		Settings{Enabled: false, DailyTokenLimit: 500000, MaxTurns: 30, Notice: "维护中"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Enabled || v.DailyTokenLimit != 500000 || v.MaxTurns != 30 || v.Notice != "维护中" ||
		v.UpdatedBy == nil || *v.UpdatedBy != 7 || !v.UpdatedAt.Equal(clk.Now()) {
		t.Fatalf("updated = %+v", v)
	}
	// 本副本缓存立即刷新
	got, err := s.Settings(ctx)
	if err != nil || got.Enabled || got.DailyTokenLimit != 500000 {
		t.Fatalf("Settings after update = %+v, %v", got, err)
	}
	logs, total, err := audit.List(ctx, s.pool, audit.Filter{Action: AuditSettings}, pagingAll())
	if err != nil || total != 1 {
		t.Fatalf("audit = %d, %v", total, err)
	}
	l := logs[0]
	if l.ActorType != "admin" || l.ActorID != 7 || l.TargetType != "butler_settings" || l.TargetID != "1" || l.IP != "10.0.0.1" {
		t.Fatalf("audit log = %+v", l)
	}
	var diff struct {
		Old, New struct {
			Enabled         bool   `json:"enabled"`
			DailyTokenLimit int64  `json:"daily_token_limit"`
			MaxTurns        int    `json:"max_turns"`
			Notice          string `json:"notice"`
		}
	}
	if err := json.Unmarshal(l.Diff, &diff); err != nil {
		t.Fatal(err)
	}
	if !diff.Old.Enabled || diff.Old.DailyTokenLimit != 300000 || diff.Old.MaxTurns != 60 || diff.Old.Notice != "" ||
		diff.New.Enabled || diff.New.DailyTokenLimit != 500000 || diff.New.MaxTurns != 30 || diff.New.Notice != "维护中" {
		t.Fatalf("diff = %s", l.Diff)
	}
}

func TestSettingsCache(t *testing.T) {
	s, clk := newTestStore(t)
	ctx := context.Background()
	if v, err := s.Settings(ctx); err != nil || !v.Enabled {
		t.Fatalf("initial = %+v, %v", v, err)
	}
	// 绕过 UpdateSettings 直接改库（模拟其它副本的后台修改）
	if _, err := s.pool.Exec(ctx, `UPDATE butler_settings SET enabled = false, daily_token_limit = 20000`); err != nil {
		t.Fatal(err)
	}
	clk.Advance(9 * time.Second)
	if v, _ := s.Settings(ctx); !v.Enabled {
		t.Fatal("within 10s the cached value must be served")
	}
	clk.Advance(2 * time.Second) // 共 11 秒
	if v, _ := s.Settings(ctx); v.Enabled || v.DailyTokenLimit != 20000 {
		t.Fatalf("after 11s = %+v, want DB value", v)
	}
	// InvalidateSettings 立即生效
	if _, err := s.pool.Exec(ctx, `UPDATE butler_settings SET enabled = true`); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Settings(ctx); v.Enabled {
		t.Fatal("still cached before invalidate")
	}
	s.InvalidateSettings()
	if v, _ := s.Settings(ctx); !v.Enabled {
		t.Fatal("InvalidateSettings must force a DB read")
	}
}
