package butler

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

// Sweep 是清扫器的附加任务（每轮一次，规格 §8）：
//
//	(a) 过期的 pending 调用 → expired（清 secret_env_enc）+ tool 消息「操作已过期未执行」，对话 awaiting_confirm → idle；
//	(b) running 且 run_started_at < now − StaleRunAfter（副本崩溃）→ idle + assistant 消息「本轮处理中断，请重试」+ run_token 清空；
//	(c) 删除 updated_at 早于 RetentionDays 天的对话（每轮 ≤ SweepDeleteBatch 个，级联消息与工具调用；运行中的不删）；
//	(d) 删除 UsageRetentionDays 天前的用量。
//
// 只做数据库处理（不调用模型）；(a)(b) 每轮各处理 ≤ SweepDeleteBatch 个。各步骤互不影响，返回所有数据库错误的合并。
func (s *Service) Sweep(ctx context.Context) error {
	now := s.d.Clock.Now()
	var errs []error

	// (a)
	ids, err := collectPairs(s.pool.Query(ctx, `SELECT id, conversation_id FROM butler_tool_calls
		WHERE status = 'pending' AND expires_at <= $1 ORDER BY expires_at LIMIT $2`, now, SweepDeleteBatch))
	errs = append(errs, err)
	for _, p := range ids {
		errs = append(errs, s.withTx(ctx, func(tx pgx.Tx) error {
			conv, err := s.lockConversation(ctx, tx, 0, p[1])
			if err != nil {
				return ignoreNotFound(err)
			}
			tc, err := scanCall(tx.QueryRow(ctx, `SELECT `+callCols+` FROM butler_tool_calls WHERE id = $1 FOR UPDATE`, p[0]))
			if err != nil {
				if db.IsNoRows(err) {
					return nil
				}
				return err
			}
			if tc.Status != CallPending || tc.ExpiresAt == nil || tc.ExpiresAt.After(now) {
				return nil
			}
			if err := s.expireCall(ctx, tx, tc, msgExpired, now); err != nil {
				return err
			}
			if conv.Status == ConvAwaitingConfirm {
				return setStatus(ctx, tx, conv.ID, ConvIdle, now)
			}
			return nil
		}))
	}

	// (b)
	stale, err := collectPairs(s.pool.Query(ctx, `SELECT id, '' FROM butler_conversations
		WHERE status = 'running' AND run_started_at < $1 ORDER BY run_started_at LIMIT $2`, now.Add(-StaleRunAfter), SweepDeleteBatch))
	errs = append(errs, err)
	for _, p := range stale {
		errs = append(errs, s.withTx(ctx, func(tx pgx.Tx) error {
			conv, err := s.lockConversation(ctx, tx, 0, p[0])
			if err != nil {
				return ignoreNotFound(err)
			}
			if conv.Status != ConvRunning || conv.RunStartedAt == nil || !conv.RunStartedAt.Before(now.Add(-StaleRunAfter)) {
				return nil
			}
			if _, err := s.appendMessage(ctx, tx, conv.ID, RoleAssistant, []Block{{Type: BlockText, Text: TextInterrupted}}, 0, now); err != nil {
				return err
			}
			s.log.Warn("butler: stale run interrupted", "conversation", conv.ID)
			return setStatus(ctx, tx, conv.ID, ConvIdle, now)
		}))
	}

	// (c)
	_, err = s.pool.Exec(ctx, `DELETE FROM butler_conversations WHERE id IN (
		SELECT id FROM butler_conversations WHERE updated_at < $1 AND status <> 'running' ORDER BY updated_at LIMIT $2)`,
		now.AddDate(0, 0, -RetentionDays), SweepDeleteBatch)
	errs = append(errs, err)

	// (d)
	_, err = s.PruneUsage(ctx, now.AddDate(0, 0, -UsageRetentionDays))
	errs = append(errs, err)
	return errors.Join(errs...)
}

func ignoreNotFound(err error) error {
	if err == apperr.ErrNotFound {
		return nil
	}
	return err
}

func collectPairs(rows pgx.Rows, err error) ([][2]string, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) ([2]string, error) {
		var p [2]string
		err := r.Scan(&p[0], &p[1])
		return p, err
	})
}
