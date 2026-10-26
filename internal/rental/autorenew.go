package rental

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

// SetAutoRenew 用户开关自动续费并设档位；开关时清掉上次的失败标记。关闭时档位可留空（保留原档位）。
func (s *Service) SetAutoRenew(ctx context.Context, userID int64, id string, enabled bool, d catalog.Duration, ip string) (Instance, error) {
	if !d.Valid() && (enabled || d != "") {
		return Instance{}, apperr.BadRequest("自动续费档位不正确")
	}
	now := s.d.Clock.Now()
	var out Instance
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		inst, err := lockInstance(ctx, tx, id)
		if err != nil {
			return err
		}
		if inst.UserID != userID {
			return apperr.ErrNotFound
		}
		if inst.ClusterID != nil { // 集群成员整组设置（SetClusterAutoRenew）
			return apperr.ErrClusterMember
		}
		if inst.System { // 系统实例不到期，没有续费可言
			return apperr.ErrInvalidState
		}
		switch inst.State {
		case StateReserved, StateCreating, StateRunning, StateStopped:
		default:
			return apperr.ErrInvalidState
		}
		out, err = scanInstance(tx.QueryRow(ctx, `UPDATE instances SET auto_renew = $2, auto_renew_duration = COALESCE(NULLIF($3, ''), auto_renew_duration),
			auto_renew_failed_at = NULL, auto_renew_failed_reason = '', updated_at = $4 WHERE id = $1 RETURNING `+instanceCols, id, enabled, string(d), now))
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, audit.Actor{UserID: userID, IP: ip}.Entry("instance.auto_renew", "instance", id,
			map[string]any{"enabled": enabled, "duration": d}))
	})
	return out, err
}

// autoRenewPred：到期前 AutoRenewLead 内、运行中未暂停、开了自动续费、不是集群成员（成员由 AutoRenewClustersDue 整组续费）；
// 失败后按原因间隔重试（$1 = now；间隔来自常量）。
// $1 必须显式转 timestamptz：否则 `$1 + interval` 会把 $1 推断成 interval。
var autoRenewPred = fmt.Sprintf(`state = 'running' AND suspended_at IS NULL AND auto_renew AND cluster_id IS NULL AND expires_at <= $1::timestamptz + interval '%d seconds'
	AND (auto_renew_failed_at IS NULL OR auto_renew_failed_at <= $1::timestamptz
	     - CASE WHEN auto_renew_failed_reason = 'renew_blocked' THEN interval '%d seconds' ELSE interval '%d seconds' END)`,
	int(AutoRenewLead/time.Second), int(AutoRenewRetryBlocked/time.Second), int(AutoRenewRetry/time.Second))

// AutoRenewDue 自动续费（规格 §7.2）：到期前 AutoRenewLead 扣款并延长；余额不足或被预订挡住时记失败标记，首次失败发短信。
// 每行一个事务（sweepEachAfter 已对 40P01 / 40001 重试），短信在该行事务提交后才发。
func (s *Service) AutoRenewDue(ctx context.Context) (int, error) {
	now := s.d.Clock.Now()
	return s.sweepEachAfter(ctx, autoRenewPred, now, "expires_at",
		func(tx pgx.Tx, inst Instance) (func(), error) { return s.autoRenewOne(ctx, tx, inst, now) }, nil)
}

// autoRenewOne：选档、扣款与加时在 savepoint 里走与手动续费相同的 applyRenew；业务失败只回滚 savepoint
// （不扣款也不加时），失败标记随行事务提交。
func (s *Service) autoRenewOne(ctx context.Context, tx pgx.Tx, inst Instance, now time.Time) (func(), error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	failReason := ""
	d, err := s.autoRenewDuration(ctx, sp, inst, now)
	if err == nil && d == "" {
		failReason = "renew_blocked"
	} else if err == nil {
		_, err = s.applyRenew(ctx, sp, inst, d, 1, true, "", now,
			func(o Order) string { return fmt.Sprintf("auto renew %s (%s)", o.ID, d) })
	}
	switch {
	case errors.Is(err, apperr.ErrInsufficientBalance):
		failReason = "insufficient_balance"
	case codeIs(err, "renew_blocked"): // renewFits 之后同机预订才提交（addTime / verifyLeaseFits 报出）
		failReason = "renew_blocked"
	case err != nil:
		return nil, errors.Join(err, sp.Rollback(ctx))
	}
	if failReason != "" {
		if err := sp.Rollback(ctx); err != nil {
			return nil, err
		}
		return s.markAutoRenewFailed(ctx, tx, inst, failReason, now)
	}
	return nil, sp.Commit(ctx) // 失败标记已由 applyRenew 清掉
}

// autoRenewTiers 从配置档位起按 1m → 1w → 1d 降档的序列；未知档位只试最便宜的 1d（绝不默认成 1m 多扣款）。
func autoRenewTiers(d catalog.Duration) []catalog.Duration {
	order := []catalog.Duration{catalog.D1m, catalog.D1w, catalog.D1d}
	start := slices.Index(order, d)
	if start < 0 {
		start = len(order) - 1
	}
	return order[start:]
}

// autoRenewDuration 从配置档位起按 1m → 1w → 1d 找第一个放得下的档位（只降不升）；都放不下返回空串。
func (s *Service) autoRenewDuration(ctx context.Context, q db.Querier, inst Instance, now time.Time) (catalog.Duration, error) {
	for _, d := range autoRenewTiers(inst.AutoRenewDuration) {
		err := s.renewFits(ctx, q, inst, seconds(d), now)
		if err == nil {
			return d, nil
		}
		if !codeIs(err, "renew_blocked") {
			return "", err
		}
	}
	return "", nil
}

var autoRenewFailText = map[string]string{"insufficient_balance": "余额不足", "renew_blocked": "机器已有后续预订"}

// markAutoRenewFailed 写失败标记（每次失败都刷新 failed_at，作为下次重试的计时起点）；
// 首次失败（行锁下读到的标记为空）写事件，并返回提交后发短信的动作。
func (s *Service) markAutoRenewFailed(ctx context.Context, tx pgx.Tx, inst Instance, reason string, now time.Time) (func(), error) {
	if _, err := tx.Exec(ctx, `UPDATE instances SET auto_renew_failed_at = $2, auto_renew_failed_reason = $3, updated_at = $2 WHERE id = $1`,
		inst.ID, now, reason); err != nil {
		return nil, err
	}
	if inst.AutoRenewFailedAt != nil {
		return nil, nil
	}
	if err := recordEvent(ctx, tx, inst.ID, inst.State, inst.State, "auto renew failed: "+reason, now); err != nil {
		return nil, err
	}
	text := autoRenewFailText[reason]
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.d.Notifier.NotifyAutoRenewFailed(ctx, inst.UserID, inst.ID, text); err != nil {
			s.d.Log.Warn("auto renew failed sms", "instance", inst.ID, "err", err)
		}
	}, nil
}

// clearAutoRenewFailure 加时成功（手动续费、自动续费、后台延长）后清掉自动续费失败标记：下次再失败按首次失败重发短信。
func clearAutoRenewFailure(ctx context.Context, tx pgx.Tx, inst Instance, now time.Time) error {
	if inst.AutoRenewFailedAt == nil && inst.AutoRenewFailedReason == "" {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE instances SET auto_renew_failed_at = NULL, auto_renew_failed_reason = '', updated_at = $2 WHERE id = $1`, inst.ID, now)
	return err
}
