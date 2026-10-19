package referral

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

type RateVersion struct {
	ID               int64     `json:"id"`
	RateBP           int       `json:"rate_bp"`
	MaxPerInvitee    int64     `json:"max_per_invitee"`
	MaxPerInviter30d int64     `json:"max_per_inviter_30d"`
	HoldDays         int       `json:"hold_days"`
	PeriodDays       int       `json:"period_days"`
	EffectiveFrom    time.Time `json:"effective_from"`
	Note             string    `json:"note"`
	CreatedBy        *int64    `json:"created_by"`
	CreatedAt        time.Time `json:"created_at"`
}

type RateInput struct {
	RateBP           int
	MaxPerInvitee    int64
	MaxPerInviter30d int64
	HoldDays         int
	PeriodDays       int
	EffectiveFrom    *time.Time // nil = now
	Note             string
}

const rateCols = `id, rate_bp, max_per_invitee, max_per_inviter_30d, hold_days, period_days, effective_from, note, created_by, created_at`

func scanRate(row pgx.Row) (RateVersion, error) {
	var v RateVersion
	err := row.Scan(&v.ID, &v.RateBP, &v.MaxPerInvitee, &v.MaxPerInviter30d, &v.HoldDays, &v.PeriodDays, &v.EffectiveFrom, &v.Note, &v.CreatedBy, &v.CreatedAt)
	return v, err
}

// ConfigAt 返回在 t 时刻生效的版本（effective_from <= t 的最新一条；表由迁移保证非空）。
func (s *Service) ConfigAt(ctx context.Context, q db.Querier, t time.Time) (RateVersion, error) {
	return scanRate(q.QueryRow(ctx, `SELECT `+rateCols+` FROM referral_rate_versions WHERE effective_from <= $1 ORDER BY effective_from DESC, id DESC LIMIT 1`, t))
}

func (s *Service) CurrentConfig(ctx context.Context) (RateVersion, error) {
	return s.ConfigAt(ctx, s.d.Pool, s.d.Clock.Now())
}

func (s *Service) ListRateVersions(ctx context.Context) ([]RateVersion, error) {
	rows, err := s.d.Pool.Query(ctx, `SELECT `+rateCols+` FROM referral_rate_versions ORDER BY effective_from DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (RateVersion, error) { return scanRate(r) })
}

func configInvalid(format string, a ...any) error {
	return apperr.New("referral_config_invalid", fmt.Sprintf(format, a...), 400)
}

// AddRateVersion 新增一个费率版本（R14：不能追溯生效）；写审计 referral.config（含旧版本与新版本全部字段）。
func (s *Service) AddRateVersion(ctx context.Context, actor audit.Actor, in RateInput) (RateVersion, error) {
	now := s.d.Clock.Now()
	switch {
	case in.RateBP < 0 || in.RateBP > 5000:
		return RateVersion{}, configInvalid("返利比例需在 0%%～50%% 之间")
	case in.MaxPerInvitee < 0:
		return RateVersion{}, configInvalid("单个被邀请人上限不能为负")
	case in.MaxPerInviter30d < 0:
		return RateVersion{}, configInvalid("单个邀请人 30 天上限不能为负")
	case in.HoldDays < 0 || in.HoldDays > 90:
		return RateVersion{}, configInvalid("发放延迟需在 0～90 天之间")
	case in.PeriodDays < 1 || in.PeriodDays > 3650:
		return RateVersion{}, configInvalid("返利期需在 1～3650 天之间")
	}
	from := now
	if in.EffectiveFrom != nil {
		from = in.EffectiveFrom.UTC()
		if from.Before(now.Add(-EffectiveTolerance)) {
			return RateVersion{}, configInvalid("生效时间不能早于当前时间")
		}
		// 容差只是输入宽限：落库时钳到 now，任何版本都不会早于提交时刻生效（R14）
		if from.Before(now) {
			from = now
		}
	}
	var v RateVersion
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		old, err := s.ConfigAt(ctx, tx, now)
		if err != nil {
			return err
		}
		var by *int64
		if actor.AdminID != 0 {
			by = &actor.AdminID
		}
		v, err = scanRate(tx.QueryRow(ctx, `INSERT INTO referral_rate_versions (rate_bp, max_per_invitee, max_per_inviter_30d, hold_days, period_days, effective_from, note, created_by, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING `+rateCols,
			in.RateBP, in.MaxPerInvitee, in.MaxPerInviter30d, in.HoldDays, in.PeriodDays, from, in.Note, by, now))
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("referral.config", "referral_rate_version", fmt.Sprint(v.ID), map[string]any{"old": old, "new": v}))
	})
	return v, err
}
