package referral

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/db"
)

// Discover 扫描 wallet_ledger 中新的 order 流水（规格 §5.1）：时间水位 + 1 小时回看 + source_ledger_id 幂等；
// 每行独立事务。返回成功处理（含占位）的行数；有行失败时错误里汇总失败行数。水位规则见 discoverRound。
func (s *Service) Discover(ctx context.Context) (int, error) {
	n, failed, err := s.discoverCurrent(ctx)
	return n, errors.Join(err, failedRows(failed))
}

// discoverCurrent 以当前水位跑一轮 discoverRound。
func (s *Service) discoverCurrent(ctx context.Context) (processed, failed int, err error) {
	var since time.Time
	if err := s.d.Pool.QueryRow(ctx, `SELECT watermark FROM referral_state WHERE id = 1`).Scan(&since); err != nil {
		return 0, 0, err
	}
	return s.discoverRound(ctx, since)
}

// DiscoverMaxPages：每轮最多翻 DiscoverMaxPages 页（每页 ScanBatch 行）。测试可经 export_test.go 改小。
const DiscoverMaxPages = 10

var discoverMaxPages = DiscoverMaxPages

// StuckAfter：水位被钉在比 now − StuckAfter 更早的流水上时告警 referral_scan_stuck。暂时跳过的行过了 SkipGrace 会写占位，
// 再多 1 小时仍钉着只可能是处理一直失败（或在等一直失败的扣款）的行，需要人工处理。
const StuckAfter = SkipGrace + time.Hour

// AlertScanStuck 是水位卡住时的告警种类；消息固定，RaiseAlertOnce 按 kind + 消息去重，未处理前只有一条。
const (
	AlertScanStuck    = "referral_scan_stuck"
	alertScanStuckMsg = "邀请返利扫描有流水超过 25 小时仍未处理成功，扫描水位被钉住：请按日志「referral: process ledger failed」排查修复，" +
		"修复后在后台「邀请返利」执行重扫（重扫天数覆盖卡住的流水）"
)

// discoverRound 扫描 created_at ≥ since − ScanLookback 的未处理 order 流水（扣款先于退款，同类按 id）。
// 按 ((kind = 'refund'), id) 游标翻页，每页 ScanBatch 行、每轮最多 discoverMaxPages 页：被跳过 / 失败的行留在原处，
// 不会让后面的新流水饿死。
//
// 水位（裁定 Q5 / Q10）：
//   - 翻满 discoverMaxPages 页仍有下一页（有积压）时不动，下轮继续。
//   - 否则推进到 LEAST(now − ScanLag, 本轮被跳过 / 失败行中最早的 created_at)：跳过与失败的行留在之后每轮的窗口里，
//     不会在 1 小时回看之后被丢掉（规格 §5.1「下轮再试」）。永久跳过的行由 accrue / applyRefund 在 SkipGrace 后写占位，不会一直钉住水位。
//   - 只在水位仍等于本轮读到的 since 时更新（比较后写）：后台 Rescan 在本轮进行中把水位拨回时，本轮不会把它覆盖掉。
//   - 被钉在 now − StuckAfter 之前时告警一次（referral_scan_stuck）。
//
// ctx 到期（Settle 给发现单独的超时）时立即停止，不推进水位。
// 单行失败已由 processRow 逐行记日志，这里只计数（failed）；err 只含行处理以外的错误（查询、ctx 到期、推进水位、告警），
// 由调用方决定如何汇总（Discover / Settle 并入错误，Rescan 作为结果返回）。
func (s *Service) discoverRound(ctx context.Context, since time.Time) (processed, failed int, err error) {
	now := s.d.Clock.Now()
	var errs []error
	next := now.Add(-ScanLag)
	pinned := false
	drained := false
	afterRefund, afterID := false, int64(0)
	for page := 0; page < discoverMaxPages && !drained; page++ {
		rows, err := s.d.Pool.Query(ctx, `SELECT l.id, l.user_id, l.kind, l.amount, l.ref_id, l.created_at
			FROM wallet_ledger l JOIN referral_bindings b ON b.invitee_id = l.user_id
			WHERE l.ref_type = 'order' AND l.kind IN ('order_pay', 'refund')
			  AND l.created_at >= $1::timestamptz - make_interval(secs => $2)
			  AND NOT EXISTS (SELECT 1 FROM referral_events e WHERE e.source_ledger_id = l.id)
			  AND ((l.kind = 'refund'), l.id) > ($3::bool, $4::bigint)
			ORDER BY (l.kind = 'refund'), l.id LIMIT $5`, since, ScanLookback.Seconds(), afterRefund, afterID, ScanBatch)
		if err != nil {
			return processed, failed, errors.Join(append(errs, err)...)
		}
		list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ledgerRow, error) {
			var l ledgerRow
			err := r.Scan(&l.ID, &l.UserID, &l.Kind, &l.Amount, &l.RefID, &l.CreatedAt)
			return l, err
		})
		if err != nil {
			return processed, failed, errors.Join(append(errs, err)...)
		}
		for _, l := range list {
			if err := ctx.Err(); err != nil {
				return processed, failed, errors.Join(append(errs, err)...)
			}
			err := s.processRow(ctx, l)
			if err == nil {
				processed++
				continue
			}
			if !errors.Is(err, errSkip) {
				failed++
			}
			if l.CreatedAt.Before(next) {
				next, pinned = l.CreatedAt, true // 跳过 / 失败的行钉住水位
			}
		}
		drained = len(list) < ScanBatch
		if n := len(list); n > 0 {
			afterRefund, afterID = list[n-1].Kind == "refund", list[n-1].ID
		}
	}
	if drained {
		if _, err := s.d.Pool.Exec(ctx, `UPDATE referral_state SET watermark = $1 WHERE id = 1 AND watermark = $2`, next, since); err != nil {
			errs = append(errs, fmt.Errorf("advance watermark: %w", err))
		}
	}
	if pinned && next.Before(now.Add(-StuckAfter)) && s.d.Alerts != nil {
		s.d.Log.Error("referral: scan watermark stuck behind an unprocessed ledger row", "oldest", next)
		if err := s.d.Alerts.RaiseAlertOnce(ctx, s.d.Pool, fleet.Alert{Level: "warning", Kind: AlertScanStuck, Message: alertScanStuckMsg}); err != nil {
			errs = append(errs, fmt.Errorf("raise %s alert: %w", AlertScanStuck, err))
		}
	}
	return processed, failed, errors.Join(errs...)
}

// failedRows 汇总本轮失败行数（逐行详情已在 processRow 的日志里）；0 返回 nil。
func failedRows(n int) error {
	if n == 0 {
		return nil
	}
	return fmt.Errorf("referral discover: %d ledger rows failed (see per-row log)", n)
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

// Settle 是清扫附加任务：先发现（计提 / 退款处理）再发放。两步各有自己的超时、互相独立：
// 发现慢（或失败）不会把发放饿死，任一失败不阻止另一步。
func (s *Service) Settle(ctx context.Context) error {
	r := s.settle(ctx, s.discoverCurrent)
	return errors.Join(r.discoverErr, failedRows(r.failed), r.releaseErr)
}

// 发现 / 发放各自的超时；两者之和小于清扫器给附加任务的总超时（rental extraTaskTimeout，2 分钟）。
var (
	discoverTimeout = time.Minute
	releaseTimeout  = 50 * time.Second
)

// settleResult 是一轮发现 + 发放的结果：失败行数单独计（discoverErr 不含行处理失败）。
type settleResult struct {
	processed, failed, released int
	discoverErr, releaseErr     error
}

func (s *Service) settle(ctx context.Context, discover func(context.Context) (processed, failed int, err error)) settleResult {
	var r settleResult
	dctx, cancel := context.WithTimeout(ctx, discoverTimeout)
	r.processed, r.failed, r.discoverErr = discover(dctx)
	cancel()
	rctx, cancel := context.WithTimeout(ctx, releaseTimeout)
	defer cancel()
	r.released, r.releaseErr = s.Release(rctx)
	return r
}

// RescanResult 是一次重扫的结果。水位拨回（及审计）提交之后，发现 / 发放中的失败不再作为错误返回——拨回已生效、
// 重扫已执行——而是记在结果里：Failed 为处理失败的流水行数（逐行详情见日志「referral: process ledger failed」，
// 这些行钉住水位，之后每轮清扫重试）；Incomplete 表示发现或发放本身出错 / 超时（详情见日志），之后的清扫轮会继续。
type RescanResult struct {
	Watermark  time.Time `json:"watermark"` // 拨回后的水位（本次发现的下界）
	Processed  int       `json:"processed"` // 本次发现处理成功（含占位）的流水行数
	Failed     int       `json:"failed"`
	Released   int       `json:"released"` // 本次发放的返利笔数
	Incomplete bool      `json:"incomplete"`
}

// Rescan 把水位拨回 days 天并立刻用拨回后的水位跑一轮发现 + 发放；已处理的流水因 source_ledger_id 唯一不会重复计提。
// 发现的下界显式传入（裁定 Q10），不重读水位：并发的清扫轮无法让本次重扫落空；一轮处理不完（积压 ≥ ScanBatch）时
// 水位停在拨回处，之后的清扫轮继续补（清扫轮按「读到的水位」比较后才更新，不会覆盖拨回）。
//
// 只有参数错误与拨回水位的事务失败返回错误（此时什么都没发生）；之后的失败见 RescanResult。
func (s *Service) Rescan(ctx context.Context, actor audit.Actor, days int) (RescanResult, error) {
	if err := CheckRescanDays(days); err != nil {
		return RescanResult{}, err
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
		return RescanResult{}, err
	}
	r := s.settle(ctx, func(ctx context.Context) (int, int, error) { return s.discoverRound(ctx, since) })
	res := RescanResult{Watermark: since, Processed: r.processed, Failed: r.failed, Released: r.released}
	if err := errors.Join(r.discoverErr, r.releaseErr); err != nil {
		res.Incomplete = true
		s.d.Log.Error("referral: rescan incomplete", "days", days, "err", err)
	}
	if r.failed > 0 {
		s.d.Log.Warn("referral: rescan left failed ledger rows", "days", days, "failed", r.failed)
	}
	return res, nil
}
