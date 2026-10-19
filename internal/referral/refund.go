package referral

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/wallet"
)

var refundSuffixRe = regexp.MustCompile(`-r\d+$`)

// OrderIDOf 从退款流水的 ref_id 取订单号：refundAcross 写 <订单号>-r<n>，refundAll 写 <订单号>。
func OrderIDOf(refID string) string { return refundSuffixRe.ReplaceAllString(refID, "") }

// applyRefund 处理一条 refund 流水（规格 §4.3）：按 reward 行里存的 rate_bp / reward_cap 重算应得；未发放只改数字；
// 已发放多出的部分经 ApplyUpTo 从邀请人余额扣回，不足记欠账。调用方提供事务。
// 加锁与计提相同：users(邀请人, 被邀请人) FOR KEY SHARE → referral_accounts(邀请人) FOR UPDATE → [幂等复查] →
// referral_rewards(订单) FOR UPDATE → [wallet_accounts(邀请人)，仅扣回]。
// 幂等只靠 referral_events.source_ledger_id + 账户锁后的复查：余额为 0 时 ApplyUpTo 不写钱包流水，钱包唯一索引兜不住。
func (s *Service) applyRefund(ctx context.Context, tx pgx.Tx, now time.Time, l ledgerRow) error {
	if l.Kind != string(wallet.KindRefund) || l.Amount <= 0 {
		return fmt.Errorf("referral: applyRefund on ledger %d kind=%s amount=%d", l.ID, l.Kind, l.Amount)
	}
	if done, err := alreadyProcessed(ctx, tx, l.ID); err != nil || done {
		return err // 无锁快路径；权威复查在账户锁之后
	}
	order := OrderIDOf(l.RefID)
	var inviterID, inviteeID int64
	err := tx.QueryRow(ctx, `SELECT inviter_id, invitee_id FROM referral_rewards WHERE order_id = $1`, order).Scan(&inviterID, &inviteeID)
	if db.IsNoRows(err) {
		// 没有 reward 行：订单已有 capped(period_over) / void(binding revoked) 占位 → 写 refund(0) 占位；
		// 什么都没有 → 延后（计提可能还没落地，下轮再试）。
		var bindingID int64
		err := tx.QueryRow(ctx, `SELECT e.binding_id, b.inviter_id, b.invitee_id FROM referral_events e JOIN referral_bindings b ON b.id = e.binding_id
			WHERE e.order_id = $1 AND e.kind IN ('capped', 'void') AND e.source_ledger_id IS NOT NULL ORDER BY e.id LIMIT 1`, order).Scan(&bindingID, &inviterID, &inviteeID)
		if db.IsNoRows(err) {
			return errSkip
		}
		if err != nil {
			return err
		}
		if done, err := s.lockForRefund(ctx, tx, inviterID, inviteeID, l.ID); err != nil || done {
			return err
		}
		return addEvent(ctx, tx, now, order, bindingID, EventRefund, l.ID, 0, 0, "no reward")
	}
	if err != nil {
		return err
	}
	if done, err := s.lockForRefund(ctx, tx, inviterID, inviteeID, l.ID); err != nil || done {
		return err
	}
	r, err := lockReward(ctx, tx, order) // 锁后重读：发放 / 前一笔退款可能刚改过
	if err != nil {
		return err
	}
	refunded := r.RefundedAmount + l.Amount
	if refunded > r.OrderAmount {
		s.d.Log.Error("referral: refunds exceed order amount, clamping", "order", order, "ledger", l.ID, "refunded", refunded, "order_amount", r.OrderAmount)
		refunded = r.OrderAmount
	}
	target := min(r.RewardCap, (r.OrderAmount-refunded)*int64(r.RateBP)/10000)
	if err := addEvent(ctx, tx, now, order, r.BindingID, EventRefund, l.ID, 0, r.RewardAmt-target, fmt.Sprintf("refund=%d target=%d", l.Amount, target)); err != nil {
		return err
	}
	switch r.Status {
	case StatusPending:
		_, err = tx.Exec(ctx, `UPDATE referral_rewards SET reward = $2, refunded_amount = $3, updated_at = $4 WHERE order_id = $1`, order, target, refunded, now)
		return err
	case StatusVoid:
		_, err = tx.Exec(ctx, `UPDATE referral_rewards SET refunded_amount = $2, updated_at = $3 WHERE order_id = $1`, order, refunded, now)
		return err
	}
	// settled
	excess := (r.SettledAmount - r.ClawedAmount) - target
	clawed := r.ClawedAmount
	if excess > 0 {
		// n 在账户锁内读、本事务内写回（clawback 事件），同一订单的退款串行 → -c<n> 不重复。
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM referral_events WHERE order_id = $1 AND kind = 'clawback'`, order).Scan(&n); err != nil {
			return err
		}
		masked, err := s.inviteeMasked(ctx, tx, r.InviteeID)
		if err != nil {
			return err
		}
		refID := order + "-c" + strconv.Itoa(n+1)
		applied, _, err := s.d.Wallet.ApplyUpTo(ctx, tx, wallet.Entry{UserID: inviterID, Amount: -excess, Kind: wallet.KindReferralClawback,
			RefType: RefType, RefID: refID, Remark: "返利扣回 " + masked + " 订单 " + order})
		if err != nil {
			return err
		}
		var ledgerID int64
		if applied > 0 {
			if err := tx.QueryRow(ctx, `SELECT id FROM wallet_ledger WHERE kind = 'referral_clawback' AND ref_type = $1 AND ref_id = $2`, RefType, refID).Scan(&ledgerID); err != nil {
				return err
			}
		}
		if shortfall := excess - applied; shortfall > 0 {
			if _, err := tx.Exec(ctx, `UPDATE referral_accounts SET debt = debt + $2, updated_at = $3 WHERE user_id = $1`, inviterID, shortfall, now); err != nil {
				return err
			}
			if err := addEvent(ctx, tx, now, order, r.BindingID, EventDebtAdd, 0, 0, shortfall, "balance short on clawback"); err != nil {
				return err
			}
		}
		if err := addEvent(ctx, tx, now, order, r.BindingID, EventClawback, 0, ledgerID, excess, refID); err != nil {
			return err
		}
		clawed += excess
	}
	_, err = tx.Exec(ctx, `UPDATE referral_rewards SET reward = $2, refunded_amount = $3, clawed_amount = $4, updated_at = $5 WHERE order_id = $1`, order, target, refunded, clawed, now)
	return err
}

// lockForRefund 按计提同样的顺序加锁并在账户锁后复查幂等；done 表示该流水已被处理（并发的前一个事务已提交）。
func (s *Service) lockForRefund(ctx context.Context, tx pgx.Tx, inviterID, inviteeID, ledgerID int64) (done bool, err error) {
	if err := lockUsersKeyShare(ctx, tx, inviterID, inviteeID); err != nil {
		return false, err
	}
	if _, err := lockAccount(ctx, tx, inviterID); err != nil {
		return false, err
	}
	return alreadyProcessed(ctx, tx, ledgerID)
}
