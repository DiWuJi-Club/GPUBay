package referral

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"spark-cloud/internal/platform/db"
)

// CheckInvariants 核对规格 §14 的不变量；任一不成立返回汇总错误。要求系统处于静止态（Settle 已跑到没有待处理流水）。
func (s *Service) CheckInvariants(ctx context.Context, q db.Querier) error {
	var problems []string
	check := func(name, sql string, args ...any) {
		rows, err := q.Query(ctx, sql, args...)
		if err != nil {
			problems = append(problems, name+": "+err.Error())
			return
		}
		defer rows.Close()
		for rows.Next() {
			vals, err := rows.Values()
			if err != nil {
				problems = append(problems, name+": "+err.Error())
				return
			}
			problems = append(problems, fmt.Sprintf("%s: %v", name, vals))
		}
		if err := rows.Err(); err != nil {
			problems = append(problems, name+": "+err.Error())
		}
	}
	// 1. 每个邀请人：钱包净额 == Σ(settled − clawed) + debt
	check("inv1 wallet net != book net + debt", `
		SELECT a.user_id, COALESCE(w.net, 0) AS wallet_net, COALESCE(r.net, 0) + a.debt AS book_net
		FROM referral_accounts a
		LEFT JOIN (SELECT user_id, sum(amount) AS net FROM wallet_ledger WHERE kind IN ('referral_rebate', 'referral_clawback') GROUP BY user_id) w ON w.user_id = a.user_id
		LEFT JOIN (SELECT inviter_id, sum(settled_amount - clawed_amount) AS net FROM referral_rewards GROUP BY inviter_id) r ON r.inviter_id = a.user_id
		WHERE COALESCE(w.net, 0) <> COALESCE(r.net, 0) + a.debt`)
	// 2. 行内一致性
	check("inv2 reward row inconsistent", `
		SELECT order_id, status, reward, settled_amount, clawed_amount FROM referral_rewards
		WHERE (status = 'settled' AND reward <> settled_amount - clawed_amount)
		   OR (status = 'pending' AND (settled_amount <> 0 OR clawed_amount <> 0 OR settled_at IS NOT NULL))
		   OR (status = 'void' AND EXISTS (SELECT 1 FROM wallet_ledger l WHERE l.ref_type = 'referral' AND l.kind = 'referral_rebate' AND l.ref_id = referral_rewards.order_id))`)
	// 3. refunded_amount == Σ 已处理的 refund 流水（≤ order_amount 由 CHECK 保证）
	check("inv3 refunded_amount != processed refunds", `
		SELECT r.order_id, r.refunded_amount, COALESCE(sum(l.amount), 0) FROM referral_rewards r
		LEFT JOIN referral_events e ON e.order_id = r.order_id AND e.kind = 'refund' AND e.source_ledger_id IS NOT NULL
		LEFT JOIN wallet_ledger l ON l.id = e.source_ledger_id
		GROUP BY r.order_id, r.refunded_amount HAVING r.refunded_amount <> COALESCE(sum(l.amount), 0)`)
	// 4. 静止态：被绑定用户的每条 order 流水都恰有一条事件（唯一索引保证 ≤ 1；这里查 = 0 的）
	check("inv4 unprocessed order_pay ledger", `
		SELECT l.id, l.ref_id FROM wallet_ledger l JOIN referral_bindings b ON b.invitee_id = l.user_id
		WHERE l.ref_type = 'order' AND l.kind = 'order_pay' AND NOT EXISTS (SELECT 1 FROM referral_events e WHERE e.source_ledger_id = l.id)`)
	check("inv4 unprocessed refund ledger", `
		SELECT l.id, l.ref_id FROM wallet_ledger l JOIN referral_rewards r ON r.order_id = regexp_replace(l.ref_id, '-r\d+$', '')
		WHERE l.ref_type = 'order' AND l.kind = 'refund' AND NOT EXISTS (SELECT 1 FROM referral_events e WHERE e.source_ledger_id = l.id)`)
	check("inv4 event kind vs ledger kind", `
		SELECT e.id, e.kind, l.kind FROM referral_events e JOIN wallet_ledger l ON l.id = e.source_ledger_id
		WHERE (l.kind = 'order_pay' AND e.kind NOT IN ('accrue', 'capped', 'void')) OR (l.kind = 'refund' AND e.kind <> 'refund')`)
	// 5. 上限（按该绑定 / 邀请人用到的版本中最大的上限，保守）
	check("inv5 per-invitee cap exceeded", `
		SELECT r.binding_id, sum(r.reward), max(v.max_per_invitee) FROM referral_rewards r JOIN referral_rate_versions v ON v.id = r.rate_version_id
		WHERE r.status <> 'void' GROUP BY r.binding_id HAVING sum(r.reward) > max(v.max_per_invitee)`)
	check("inv5 per-inviter 30d cap exceeded", `
		SELECT r.inviter_id, sum(r.settled_amount - r.clawed_amount), max(v.max_per_inviter_30d) FROM referral_rewards r JOIN referral_rate_versions v ON v.id = r.rate_version_id
		WHERE r.status = 'settled' AND r.settled_at > $1 GROUP BY r.inviter_id HAVING sum(r.settled_amount - r.clawed_amount) > max(v.max_per_inviter_30d)`, s.d.Clock.Now().Add(-InviterWindow))
	// 6. 未作废的行：reward == min(reward_cap, floor((order_amount − refunded_amount) × rate_bp / 10000))（整数除法，金额非负即向下取整）
	check("inv6 reward formula", `
		SELECT order_id, status, reward, reward_cap, order_amount, refunded_amount, rate_bp FROM referral_rewards
		WHERE status IN ('pending', 'settled') AND reward <> least(reward_cap, (order_amount - refunded_amount) * rate_bp / 10000)`)
	check("balance negative", `SELECT user_id, balance FROM wallet_accounts WHERE balance < 0`)
	if len(problems) > 0 {
		return errors.New("referral invariants violated:\n" + strings.Join(problems, "\n"))
	}
	return nil
}
