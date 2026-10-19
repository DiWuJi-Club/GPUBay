package referral_test

import (
	"testing"
	"time"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/referral"
)

func TestConfigAtPicksLatestEffectiveVersion(t *testing.T) {
	e := newEnv(t)
	v0, err := e.svc.ConfigAt(e.ctx, e.pool, dbtest.T0)
	if err != nil || v0.RateBP != 1000 || v0.MaxPerInvitee != 50000 || v0.MaxPerInviter30d != 500000 || v0.HoldDays != 7 || v0.PeriodDays != 365 {
		t.Fatalf("seed = %+v %v", v0, err)
	}
	at := dbtest.T0.Add(2 * time.Hour)
	v1, err := e.svc.AddRateVersion(e.ctx, audit.Actor{AdminID: 7}, referral.RateInput{RateBP: 1500, MaxPerInvitee: 1000, MaxPerInviter30d: 20000, HoldDays: 0, PeriodDays: 30, EffectiveFrom: &at, Note: "活动"})
	if err != nil || v1.RateBP != 1500 || *v1.CreatedBy != 7 || !v1.EffectiveFrom.Equal(at) {
		t.Fatalf("add = %+v %v", v1, err)
	}
	// 边界：effective_from == t 取新版本；之前 1 微秒仍是旧版本
	if v, _ := e.svc.ConfigAt(e.ctx, e.pool, at); v.ID != v1.ID {
		t.Fatalf("at boundary = %+v", v)
	}
	if v, _ := e.svc.ConfigAt(e.ctx, e.pool, at.Add(-time.Microsecond)); v.ID != v0.ID {
		t.Fatalf("before boundary = %+v", v)
	}
	// 当前版本（now = T0）仍是首版；列表按 effective_from 倒序
	if cur, _ := e.svc.CurrentConfig(e.ctx); cur.ID != v0.ID {
		t.Fatalf("current = %+v", cur)
	}
	list, _ := e.svc.ListRateVersions(e.ctx)
	if len(list) != 2 || list[0].ID != v1.ID {
		t.Fatalf("list = %+v", list)
	}
	// 审计
	var action string
	if err := e.pool.QueryRow(e.ctx, `SELECT action FROM audit_logs ORDER BY id DESC LIMIT 1`).Scan(&action); err != nil || action != "referral.config" {
		t.Fatalf("audit = %q %v", action, err)
	}
}

func TestAddRateVersionValidation(t *testing.T) {
	e := newEnv(t)
	past := dbtest.T0.Add(-6 * time.Minute) // 超出 5 分钟容差
	bad := []referral.RateInput{
		{RateBP: 1000, MaxPerInvitee: 1, MaxPerInviter30d: 1, HoldDays: 7, PeriodDays: 365, EffectiveFrom: &past}, // 追溯
		{RateBP: 5001, MaxPerInvitee: 1, MaxPerInviter30d: 1, HoldDays: 7, PeriodDays: 365},
		{RateBP: -1, MaxPerInvitee: 1, MaxPerInviter30d: 1, HoldDays: 7, PeriodDays: 365},
		{RateBP: 1000, MaxPerInvitee: -1, MaxPerInviter30d: 1, HoldDays: 7, PeriodDays: 365},
		{RateBP: 1000, MaxPerInvitee: 1, MaxPerInviter30d: 1, HoldDays: 91, PeriodDays: 365},
		{RateBP: 1000, MaxPerInvitee: 1, MaxPerInviter30d: 1, HoldDays: 7, PeriodDays: 0},
		{RateBP: 1000, MaxPerInvitee: 1, MaxPerInviter30d: 1, HoldDays: 7, PeriodDays: 3651},
	}
	for i, in := range bad {
		_, err := e.svc.AddRateVersion(e.ctx, audit.Actor{AdminID: 1}, in)
		if ae := apperr.As(err); ae == nil || ae.Code != "referral_config_invalid" {
			t.Fatalf("case %d: want referral_config_invalid, got %v", i, err)
		}
	}
	// effective_from 省略 = now；5 分钟内的「过去」容差（表单提交延迟）也接受
	slightlyPast := dbtest.T0.Add(-4 * time.Minute)
	sp, err := e.svc.AddRateVersion(e.ctx, audit.Actor{AdminID: 1}, referral.RateInput{RateBP: 1000, MaxPerInvitee: 1, MaxPerInviter30d: 1, HoldDays: 7, PeriodDays: 365, EffectiveFrom: &slightlyPast})
	if err != nil {
		t.Fatalf("tolerance: %v", err)
	}
	// 容差只放宽输入：落库的 effective_from 钳到提交时刻，绝不追溯（R14）
	var stored time.Time
	if err := e.pool.QueryRow(e.ctx, `SELECT effective_from FROM referral_rate_versions WHERE id = $1`, sp.ID).Scan(&stored); err != nil || stored.Before(dbtest.T0) || !sp.EffectiveFrom.Equal(dbtest.T0) {
		t.Fatalf("slightly past must clamp to now: stored %v returned %v err %v", stored, sp.EffectiveFrom, err)
	}
	v, err := e.svc.AddRateVersion(e.ctx, audit.Actor{AdminID: 1}, referral.RateInput{RateBP: 0, MaxPerInvitee: 0, MaxPerInviter30d: 0, HoldDays: 0, PeriodDays: 1})
	if err != nil || !v.EffectiveFrom.Equal(dbtest.T0) {
		t.Fatalf("default effective_from = %+v %v", v, err)
	}
}
