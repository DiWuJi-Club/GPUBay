package llm

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/wallet"
)

// settleCandidateLimit 每轮最多处理的账户数（规格 §7）。
const settleCandidateLimit = 500

// Settle 清扫附加任务（规格 §7）：把每个到期账户的未结算用量汇成一条 llm_settlements + 至多一条 llm_pay 流水，
// 再清理一批 90 天前的已结算明细。
//
// 候选（按 llm_accounts.updated_at，每轮 ≤ 500）直接按触发条件筛：unsettled_micro ≥ ¥1、或有早于 now − 10m 的未结算用量
// （含 0 费用行——它们也要被标记，才能进 llm_usage_daily、日后被清理）、或 debt > 0 且钱包余额 > 0。
// 规格写的是 unsettled_micro > 0 OR debt > 0 再在事务里判断；那样 500 个「有零头但没到期」的账户会一直占住候选名额
// （跳过的账户不更新 updated_at），后面到期的账户永远轮不到。事务里仍按同一条件复核。
//
// 每个账户独立事务（40P01 / 40001 重试一次）；单账户失败（含重放撞 ErrDuplicate）只记 Error、继续其它账户。
// 只有候选查询 / 清理失败或 ctx 结束才返回错误。
func (s *Service) Settle(ctx context.Context) error {
	now := s.d.Clock.Now()
	rows, err := s.d.Pool.Query(ctx, `SELECT a.user_id FROM llm_accounts a
		WHERE a.unsettled_micro >= $1
		   OR (a.debt > 0 AND EXISTS (SELECT 1 FROM wallet_accounts w WHERE w.user_id = a.user_id AND w.balance > 0))
		   OR EXISTS (SELECT 1 FROM llm_usage g WHERE g.user_id = a.user_id AND g.settlement_id IS NULL AND g.created_at <= $2)
		ORDER BY a.updated_at, a.user_id
		LIMIT $3`, SettleMinMicro, now.Add(-SettleMaxAge), settleCandidateLimit)
	if err != nil {
		return fmt.Errorf("llm settle candidates: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return fmt.Errorf("llm settle candidates: %w", err)
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := s.settleAccount(ctx, id, now); err != nil {
			s.d.Log.Error("llm: settle account failed", "user_id", id, "err", err)
		}
	}
	if _, err := s.purge(ctx, now); err != nil {
		return fmt.Errorf("llm purge usage: %w", err)
	}
	return nil
}

// settleOutcome 一次单账户结算提交后的结果（事务外用于日志与缓存失效）。
type settleOutcome struct {
	settled          bool
	healed           bool // unsettled_micro 按明细校正过（不一定有结算）
	debtBefore, debt int64
	settlementID     int64
}

// settleAccount 单账户结算（规格 §7 伪代码逐行实现）。加锁顺序（§14）：llm_accounts(user) FOR UPDATE → llm_usage（UPDATE 标记）
// → llm_settlements → llm_usage_daily → wallet_accounts(user)（ApplyUpTo 里 FOR UPDATE）。
// 返回是否写了一条结算。提交后（有结算或校正过 unsettled_micro）失效该用户的 Authorize 缓存
// （欠费 / 未结算变化要尽快反映到网关预检与 402）。
func (s *Service) settleAccount(ctx context.Context, userID int64, now time.Time) (bool, error) {
	var out settleOutcome
	err := withRetry(func() error {
		out = settleOutcome{}
		return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
			var err error
			out, err = s.settleTx(ctx, tx, userID, now)
			return err
		})
	})
	if err != nil {
		return false, err
	}
	if out.settled || out.healed {
		s.InvalidateUser(userID)
	}
	if !out.settled {
		return false, nil
	}
	switch {
	case out.debtBefore == 0 && out.debt > 0:
		s.d.Log.Warn("llm: user in debt", "user_id", userID, "settlement_id", out.settlementID, "debt", out.debt)
	case out.debtBefore > 0 && out.debt == 0:
		s.d.Log.Info("llm: user debt repaid", "user_id", userID, "settlement_id", out.settlementID, "repaid", out.debtBefore)
	}
	return true, nil
}

// dailyGroup 本次标记的用量按 (北京时间自然日, 模型) 的聚合。
type dailyGroup struct {
	day                time.Time
	modelID            int64
	requests, p, c, mc int64
	from, to           time.Time
}

func (s *Service) settleTx(ctx context.Context, tx pgx.Tx, userID int64, now time.Time) (settleOutcome, error) {
	var out settleOutcome
	a, err := scanAccount(tx.QueryRow(ctx, `SELECT `+accountCols+` FROM llm_accounts WHERE user_id = $1 FOR UPDATE`, userID))
	if db.IsNoRows(err) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	// 锁后读 max_id：之后才可见的行（不持账户锁的写入方在本事务进行中提交的）留到下一次
	var oldest *time.Time
	var maxID *int64
	if err := tx.QueryRow(ctx, `SELECT min(created_at), max(id) FROM llm_usage WHERE user_id = $1 AND settlement_id IS NULL`,
		userID).Scan(&oldest, &maxID); err != nil {
		return out, err
	}
	if maxID == nil && a.UnsettledMicro > 0 {
		// 账上有未结算、明细里一行都没有：计数漂移，以明细为准归零（否则 ≥ ¥1 的漂移会让账户每轮都是候选）
		s.d.Log.Error("llm: settle unsettled_micro drift", "user_id", userID, "unsettled_micro", a.UnsettledMicro, "actual", 0)
		if _, err := tx.Exec(ctx, `UPDATE llm_accounts SET unsettled_micro = 0, updated_at = $2 WHERE user_id = $1`,
			userID, now); err != nil {
			return out, err
		}
		out.healed = true
	}
	dueUsage := maxID != nil && (a.UnsettledMicro >= SettleMinMicro || !oldest.After(now.Add(-SettleMaxAge)))
	if !dueUsage {
		if a.Debt == 0 {
			return out, nil
		}
		// 只读余额、不加锁：wallet_accounts 必须最后锁（ApplyUpTo 里）
		var bal int64
		err := tx.QueryRow(ctx, `SELECT balance FROM wallet_accounts WHERE user_id = $1`, userID).Scan(&bal)
		if err != nil && !db.IsNoRows(err) {
			return out, err
		}
		if bal <= 0 {
			return out, nil
		}
	}

	// 纯扣欠费在保存点里做：余额在上面的只读之后、ApplyUpTo 之前被扣光（applied = 0）时整段回滚，
	// 不留一条全零的结算行（之前的计数校正保留）。
	q := tx
	if !dueUsage {
		sp, err := tx.Begin(ctx)
		if err != nil {
			return out, err
		}
		defer sp.Rollback(ctx) //nolint:errcheck // 释放后回滚是空操作
		q = sp
	}

	var sid int64
	if err := q.QueryRow(ctx, `INSERT INTO llm_settlements (user_id, requests, prompt_tokens, completion_tokens, cost_micro,
			usage_amount, debt_amount, charged, created_at)
		VALUES ($1, 0, 0, 0, 0, 0, 0, 0, $2) RETURNING id`, userID, now).Scan(&sid); err != nil {
		return out, err
	}
	if s.settleBeforeMark != nil {
		s.settleBeforeMark(userID)
	}

	var requests, promptTokens, completionTokens, sum int64
	var periodFrom, periodTo *time.Time
	unsettled, carry := a.UnsettledMicro, a.CarryMicro
	if out.healed {
		unsettled = 0
	}
	var usageAmount int64
	if dueUsage {
		// 自然日口径 usageDaySQL：与不变量 5 / DailyUsage 相同
		rows, err := q.Query(ctx, `WITH m AS (
				UPDATE llm_usage SET settlement_id = $3
				WHERE user_id = $1 AND settlement_id IS NULL AND id <= $2
				RETURNING `+usageDaySQL+` AS day, model_id, prompt_tokens, completion_tokens,
					cost_micro, created_at)
			SELECT day, model_id, count(*), COALESCE(sum(prompt_tokens), 0)::bigint, COALESCE(sum(completion_tokens), 0)::bigint,
				COALESCE(sum(cost_micro), 0)::bigint, min(created_at), max(created_at)
			FROM m GROUP BY day, model_id ORDER BY day, model_id`, userID, *maxID, sid)
		if err != nil {
			return out, err
		}
		groups, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (dailyGroup, error) {
			var g dailyGroup
			err := r.Scan(&g.day, &g.modelID, &g.requests, &g.p, &g.c, &g.mc, &g.from, &g.to)
			return g, err
		})
		if err != nil {
			return out, err
		}
		for _, g := range groups {
			requests += g.requests
			promptTokens += g.p
			completionTokens += g.c
			sum += g.mc
			if periodFrom == nil || g.from.Before(*periodFrom) {
				periodFrom = &g.from
			}
			if periodTo == nil || g.to.After(*periodTo) {
				periodTo = &g.to
			}
			if _, err := q.Exec(ctx, `INSERT INTO llm_usage_daily (day, user_id, model_id, requests, prompt_tokens, completion_tokens, cost_micro)
				VALUES ($1, $2, $3, $4, $5, $6, $7)
				ON CONFLICT (day, user_id, model_id) DO UPDATE SET
					requests = llm_usage_daily.requests + EXCLUDED.requests,
					prompt_tokens = llm_usage_daily.prompt_tokens + EXCLUDED.prompt_tokens,
					completion_tokens = llm_usage_daily.completion_tokens + EXCLUDED.completion_tokens,
					cost_micro = llm_usage_daily.cost_micro + EXCLUDED.cost_micro`,
				g.day, userID, g.modelID, g.requests, g.p, g.c, g.mc); err != nil {
				return out, err
			}
		}
		// 自愈：unsettled_micro 一律重算为剩余未结算明细之和（max_id 之后的行仍计在账上）；与预期不符记 Error
		if err := q.QueryRow(ctx, `SELECT COALESCE(sum(cost_micro), 0)::bigint FROM llm_usage
			WHERE user_id = $1 AND settlement_id IS NULL`, userID).Scan(&unsettled); err != nil {
			return out, err
		}
		if want := a.UnsettledMicro - sum; unsettled != want {
			s.d.Log.Error("llm: settle unsettled_micro mismatch", "user_id", userID, "settlement_id", sid,
				"unsettled_micro", a.UnsettledMicro, "marked", sum, "expected_remaining", want, "actual_remaining", unsettled)
		}
		total := sum + a.CarryMicro
		usageAmount, carry = total/MicroPerFen, total%MicroPerFen
	}

	debtAmount := a.Debt
	due := usageAmount + debtAmount
	var applied int64
	var ledgerID *int64
	if due > 0 {
		remark := "Token 消费欠费补扣"
		if dueUsage {
			remark = fmt.Sprintf("Token 消费 %d 次请求 · 输入 %d · 输出 %d token", requests, promptTokens, completionTokens)
			if debtAmount > 0 {
				remark += fmt.Sprintf("（含欠费 ¥%d.%02d）", debtAmount/100, debtAmount%100)
			}
		}
		applied, _, err = s.d.Wallet.ApplyUpTo(ctx, q, wallet.Entry{UserID: userID, Amount: -due, Kind: wallet.KindLLMPay,
			RefType: LedgerRefTypeSettlement, RefID: strconv.FormatInt(sid, 10), Remark: remark})
		if err != nil {
			return out, fmt.Errorf("settlement %d: %w", sid, err)
		}
		if applied > 0 {
			var id int64
			if err := q.QueryRow(ctx, `SELECT id FROM wallet_ledger WHERE kind = $1 AND ref_type = $2 AND ref_id = $3`,
				string(wallet.KindLLMPay), LedgerRefTypeSettlement, strconv.FormatInt(sid, 10)).Scan(&id); err != nil {
				return out, err
			}
			ledgerID = &id
		}
	}
	if !dueUsage && applied == 0 {
		return out, nil // 纯扣欠费一分没扣到：回滚保存点（defer），不记结算
	}
	debt := due - applied

	if _, err := q.Exec(ctx, `UPDATE llm_settlements SET requests = $2, prompt_tokens = $3, completion_tokens = $4, cost_micro = $5,
			usage_amount = $6, debt_amount = $7, charged = $8, wallet_ledger_id = $9, period_from = $10, period_to = $11
		WHERE id = $1`,
		sid, requests, promptTokens, completionTokens, sum, usageAmount, debtAmount, applied, ledgerID, periodFrom, periodTo); err != nil {
		return out, err
	}
	if _, err := q.Exec(ctx, `UPDATE llm_accounts SET unsettled_micro = $2, carry_micro = $3, debt = $4, updated_at = $5
		WHERE user_id = $1`, userID, unsettled, carry, debt, now); err != nil {
		return out, err
	}
	if q != tx {
		if err := q.Commit(ctx); err != nil { // 释放保存点
			return out, err
		}
	}
	return settleOutcome{settled: true, healed: out.healed, debtBefore: a.Debt, debt: debt, settlementID: sid}, nil
}

// purge 删一批（≤ PurgeBatch）created_at 早于 now − UsageRetention 的已结算明细；llm_usage_daily 与 llm_settlements 保留。
func (s *Service) purge(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.d.Pool.Exec(ctx, `DELETE FROM llm_usage WHERE id IN (
			SELECT id FROM llm_usage WHERE created_at < $1 AND settlement_id IS NOT NULL ORDER BY id LIMIT $2)`,
		now.Add(-UsageRetention), PurgeBatch)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// withRetry 对死锁检测（40P01）与序列化失败（40001）把整个事务重试一次；其它错误原样返回（同 rental.withRetry）。
func withRetry(fn func() error) error {
	err := fn()
	if c := pgCode(err); c == "40P01" || c == "40001" {
		err = fn()
	}
	return err
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// SettlementFilter 后台结算记录筛选：UserID 0 = 全部用户；时间按 created_at，[From, To)。
type SettlementFilter struct {
	UserID int64
	From   *time.Time
	To     *time.Time
}

const settlementCols = `id, user_id, requests, prompt_tokens, completion_tokens, cost_micro, usage_amount, debt_amount, charged,
	wallet_ledger_id, period_from, period_to, created_at`

func scanSettlement(row pgx.Row) (Settlement, error) {
	var x Settlement
	err := row.Scan(&x.ID, &x.UserID, &x.Requests, &x.PromptTokens, &x.CompletionTokens, &x.CostMicro, &x.UsageAmount,
		&x.DebtAmount, &x.Charged, &x.WalletLedgerID, &x.PeriodFrom, &x.PeriodTo, &x.CreatedAt)
	return x, err
}

func (s *Service) listSettlements(ctx context.Context, w db.Where, p paging.Page) ([]Settlement, int, error) {
	var total int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM llm_settlements WHERE `+w.SQL(), w.Args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	n := len(w.Args)
	rows, err := s.d.Pool.Query(ctx, `SELECT `+settlementCols+` FROM llm_settlements WHERE `+w.SQL()+
		fmt.Sprintf(` ORDER BY id DESC LIMIT $%d OFFSET $%d`, n+1, n+2), append(w.Args, p.Limit(), p.Offset())...)
	if err != nil {
		return nil, 0, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Settlement, error) { return scanSettlement(r) })
	if list == nil && err == nil {
		list = []Settlement{}
	}
	return list, total, err
}

// ListSettlements 控制台：该用户的结算记录分页，新的在前。返回本页与总数。
func (s *Service) ListSettlements(ctx context.Context, userID int64, p paging.Page) ([]Settlement, int, error) {
	var w db.Where
	w.Add("user_id = ?", userID)
	return s.listSettlements(ctx, w, p)
}

// AdminListSettlements 后台：按用户 / 时间筛选的结算记录分页，新的在前。
func (s *Service) AdminListSettlements(ctx context.Context, f SettlementFilter, p paging.Page) ([]Settlement, int, error) {
	var w db.Where
	if f.UserID > 0 {
		w.Add("user_id = ?", f.UserID)
	}
	if f.From != nil {
		w.Add("created_at >= ?", *f.From)
	}
	if f.To != nil {
		w.Add("created_at < ?", *f.To)
	}
	return s.listSettlements(ctx, w, p)
}
