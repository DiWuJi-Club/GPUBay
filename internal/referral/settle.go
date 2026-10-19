package referral

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

// Discover 扫描 wallet_ledger 中新的 order 流水（规格 §5.1）：时间水位 + 1 小时回看 + source_ledger_id 幂等；
// 每行独立事务。返回成功处理（含占位）的行数。水位规则见 discoverFrom。
func (s *Service) Discover(ctx context.Context) (int, error) {
	var since time.Time
	if err := s.d.Pool.QueryRow(ctx, `SELECT watermark FROM referral_state WHERE id = 1`).Scan(&since); err != nil {
		return 0, err
	}
	return s.discoverFrom(ctx, since)
}

// discoverFrom 扫描 created_at ≥ since − ScanLookback 的未处理 order 流水（扣款先于退款，同类按 id）。
//
// 水位（裁定 Q5 / Q10）：
//   - 取到 ScanBatch 行（有积压）时不动，下轮继续。
//   - 否则推进到 LEAST(now − ScanLag, 本轮被跳过 / 失败行中最早的 created_at)：跳过与失败的行留在之后每轮的窗口里，
//     不会在 1 小时回看之后被丢掉（规格 §5.1「下轮再试」）。永久跳过的行由 accrue / applyRefund 在 SkipGrace 后写占位，不会一直钉住水位。
//   - 只在水位仍等于本轮读到的 since 时更新（比较后写）：后台 Rescan 在本轮进行中把水位拨回时，本轮不会把它覆盖掉。
func (s *Service) discoverFrom(ctx context.Context, since time.Time) (int, error) {
	now := s.d.Clock.Now()
	rows, err := s.d.Pool.Query(ctx, `SELECT l.id, l.user_id, l.kind, l.amount, l.ref_id, l.created_at
		FROM wallet_ledger l JOIN referral_bindings b ON b.invitee_id = l.user_id
		WHERE l.ref_type = 'order' AND l.kind IN ('order_pay', 'refund')
		  AND l.created_at >= $1::timestamptz - make_interval(secs => $2)
		  AND NOT EXISTS (SELECT 1 FROM referral_events e WHERE e.source_ledger_id = l.id)
		ORDER BY (l.kind = 'refund'), l.id LIMIT $3`, since, ScanLookback.Seconds(), ScanBatch)
	if err != nil {
		return 0, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ledgerRow, error) {
		var l ledgerRow
		err := r.Scan(&l.ID, &l.UserID, &l.Kind, &l.Amount, &l.RefID, &l.CreatedAt)
		return l, err
	})
	if err != nil {
		return 0, err
	}
	processed := 0
	var errs []error
	next := now.Add(-ScanLag)
	for _, l := range list {
		err := s.processRow(ctx, l)
		if err == nil {
			processed++
			continue
		}
		if !errors.Is(err, errSkip) {
			errs = append(errs, fmt.Errorf("ledger %d: %w", l.ID, err))
		}
		if l.CreatedAt.Before(next) {
			next = l.CreatedAt // 跳过 / 失败的行钉住水位
		}
	}
	if len(list) < ScanBatch {
		if _, err := s.d.Pool.Exec(ctx, `UPDATE referral_state SET watermark = $1 WHERE id = 1 AND watermark = $2`, next, since); err != nil {
			errs = append(errs, fmt.Errorf("advance watermark: %w", err))
		}
	}
	return processed, errors.Join(errs...)
}

// processRow 在独立事务里处理一条流水；死锁 / 序列化失败重试一次。
// 撞 referral_events_source_uniq / referral_rewards_pkey（23505）说明并发的另一事务已处理同一行（裁定 Q3 的兜底；
// 正常情况下账户锁后的复查已挡住）：唯一冲突已让事务失败，WithTx 回滚后视为已处理。
func (s *Service) processRow(ctx context.Context, l ledgerRow) error {
	now := s.d.Clock.Now()
	err := withRetry(func() error {
		return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
			if l.Kind == "refund" {
				return s.applyRefund(ctx, tx, now, l)
			}
			return s.accrue(ctx, tx, now, l)
		})
	})
	if err == nil || errors.Is(err, errSkip) {
		return err
	}
	if c := db.ConstraintName(err); pgCode(err) == "23505" && (c == "referral_events_source_uniq" || c == "referral_rewards_pkey") {
		s.d.Log.Info("referral: ledger processed concurrently", "ledger", l.ID, "constraint", c)
		return nil
	}
	s.d.Log.Error("referral: process ledger failed", "ledger", l.ID, "kind", l.Kind, "ref", l.RefID, "err", err)
	return err
}

// Settle 是清扫附加任务：先发现（计提 / 退款处理）再发放。两步独立，任一失败不阻止另一步。
func (s *Service) Settle(ctx context.Context) error {
	_, derr := s.Discover(ctx)
	_, rerr := s.Release(ctx)
	return errors.Join(derr, rerr)
}

// Rescan 把水位拨回 days 天并立刻用拨回后的水位跑一轮发现 + 发放；已处理的流水因 source_ledger_id 唯一不会重复计提。
// 发现的下界显式传入（裁定 Q10），不重读水位：并发的清扫轮无法让本次重扫落空；一轮处理不完（积压 ≥ ScanBatch）时
// 水位停在拨回处，之后的清扫轮继续补（清扫轮按「读到的水位」比较后才更新，不会覆盖拨回）。
func (s *Service) Rescan(ctx context.Context, actor audit.Actor, days int) error {
	if days < 1 || days > 30 {
		return apperr.BadRequest("重扫天数需在 1～30 之间")
	}
	now := s.d.Clock.Now()
	target := now.Add(-time.Duration(days) * 24 * time.Hour)
	var since time.Time
	if err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `UPDATE referral_state SET watermark = LEAST(watermark, $1) WHERE id = 1 RETURNING watermark`, target).Scan(&since); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("referral.rescan", "referral_state", "1", map[string]any{"days": days, "watermark": since}))
	}); err != nil {
		return err
	}
	_, derr := s.discoverFrom(ctx, since)
	_, rerr := s.Release(ctx)
	return errors.Join(derr, rerr)
}
