package referral

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/wallet"
)

// ledgerRow 是扫描到的一条 wallet_ledger（ref_type='order'）。
type ledgerRow struct {
	ID        int64
	UserID    int64
	Kind      string
	Amount    int64
	RefID     string
	CreatedAt time.Time
}

// errSkip：本轮不处理这一行、不写事件；下轮因 NOT EXISTS 再试。
var errSkip = errors.New("referral: skip row")

type Reward struct {
	OrderID        string     `json:"order_id"`
	BindingID      int64      `json:"binding_id"`
	InviterID      int64      `json:"inviter_id"`
	InviteeID      int64      `json:"invitee_id"`
	RateVersionID  int64      `json:"rate_version_id"`
	RateBP         int        `json:"rate_bp"`
	OrderAmount    int64      `json:"order_amount"`
	RefundedAmount int64      `json:"refunded_amount"`
	RewardCap      int64      `json:"reward_cap"`
	RewardAmt      int64      `json:"reward"`
	Status         string     `json:"status"`
	SettleAfter    time.Time  `json:"settle_after"`
	SettledAmount  int64      `json:"settled_amount"`
	ClawedAmount   int64      `json:"clawed_amount"`
	SettledAt      *time.Time `json:"settled_at"`
	PaidAt         time.Time  `json:"paid_at"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

const rewardCols = `order_id, binding_id, inviter_id, invitee_id, rate_version_id, rate_bp, order_amount, refunded_amount, reward_cap, reward, status, settle_after, settled_amount, clawed_amount, settled_at, paid_at, created_at, updated_at`

func scanReward(row pgx.Row) (Reward, error) {
	var r Reward
	err := row.Scan(&r.OrderID, &r.BindingID, &r.InviterID, &r.InviteeID, &r.RateVersionID, &r.RateBP, &r.OrderAmount, &r.RefundedAmount, &r.RewardCap, &r.RewardAmt,
		&r.Status, &r.SettleAfter, &r.SettledAmount, &r.ClawedAmount, &r.SettledAt, &r.PaidAt, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

func lockReward(ctx context.Context, tx pgx.Tx, orderID string) (Reward, error) {
	r, err := scanReward(tx.QueryRow(ctx, `SELECT `+rewardCols+` FROM referral_rewards WHERE order_id = $1 FOR UPDATE`, orderID))
	if db.IsNoRows(err) {
		return r, apperr.ErrNotFound
	}
	return r, err
}

// alreadyProcessed 报告该流水是否已有事件。必须在锁住邀请人账户之后调用：并发处理同一行的事务在账户锁上排队，
// 后者拿到锁时（READ COMMITTED 新快照）能看到前者提交的事件，从而不会去撞 referral_rewards_pkey / referral_events_source_uniq。
func alreadyProcessed(ctx context.Context, q db.Querier, ledgerID int64) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM referral_events WHERE source_ledger_id = $1)`, ledgerID).Scan(&ok)
	return ok, err
}

// inviteeMasked 返回被邀请人的脱敏手机号，供发放 / 扣回流水的备注（「邀请返利 138****5678 订单 ord-…」）。
func (s *Service) inviteeMasked(ctx context.Context, q db.Querier, inviteeID int64) (string, error) {
	u, err := s.d.Users.UserState(ctx, q, inviteeID)
	if err != nil {
		return "", err
	}
	return MaskPhone(u.Phone), nil
}

// lockUsersKeyShare 按加锁顺序第一步对邀请人与被邀请人的 users 行取 FOR KEY SHARE（与之后插 referral_rewards /
// wallet_ledger 时外键检查所取的锁相同，只是提前）：实名事务持 users(本人) FOR UPDATE 后才锁 referral_accounts，
// 若我们先锁账户、再由外键检查等 users，就成了反向等待。
func lockUsersKeyShare(ctx context.Context, tx pgx.Tx, ids ...int64) error {
	_, err := tx.Exec(ctx, `SELECT 1 FROM users WHERE id = ANY($1) ORDER BY id FOR KEY SHARE`, ids)
	return err
}

// accrue 处理一条 order_pay 流水（规格 §4.2）。调用方提供事务。
// 加锁：users(邀请人, 被邀请人) FOR KEY SHARE → referral_accounts(邀请人) FOR UPDATE；幂等复查与绑定状态都在账户锁之后读。
func (s *Service) accrue(ctx context.Context, tx pgx.Tx, now time.Time, l ledgerRow) error {
	if l.Kind != string(wallet.KindOrderPay) || l.Amount >= 0 {
		return fmt.Errorf("referral: accrue on ledger %d kind=%s amount=%d", l.ID, l.Kind, l.Amount)
	}
	var inviterID int64
	err := tx.QueryRow(ctx, `SELECT inviter_id FROM referral_bindings WHERE invitee_id = $1`, l.UserID).Scan(&inviterID)
	if db.IsNoRows(err) {
		return nil // 非绑定用户（Discover 的 JOIN 不会选到；防御）
	}
	if err != nil {
		return err
	}
	if err := lockUsersKeyShare(ctx, tx, inviterID, l.UserID); err != nil {
		return err
	}
	if _, err := lockAccount(ctx, tx, inviterID); err != nil {
		return err
	}
	if done, err := alreadyProcessed(ctx, tx, l.ID); err != nil || done {
		return err
	}
	// 绑定的状态 / 返利期只在持有邀请人账户锁时改（冻结、撤销、实名撤销都先锁账户），锁后再读才稳定。
	var bindingID int64
	var status string
	var rebateUntil time.Time
	if err := tx.QueryRow(ctx, `SELECT id, status, rebate_until FROM referral_bindings WHERE invitee_id = $1`, l.UserID).
		Scan(&bindingID, &status, &rebateUntil); err != nil {
		return err
	}
	if status == BindingRevoked {
		return addEvent(ctx, tx, now, l.RefID, bindingID, EventVoid, l.ID, 0, 0, "binding revoked")
	}
	if !l.CreatedAt.Before(rebateUntil) {
		return addEvent(ctx, tx, now, l.RefID, bindingID, EventCapped, l.ID, 0, 0, "period_over")
	}
	order := l.RefID
	userID, start, err := s.d.Orders.OrderForReferral(ctx, tx, order)
	if errors.Is(err, apperr.ErrNotFound) {
		s.d.Log.Error("referral: order not found for order_pay ledger", "ledger", l.ID, "order", order, "user", l.UserID)
		return s.skipOrMark(ctx, tx, now, l, bindingID, "order not found")
	}
	if err != nil {
		return err
	}
	if userID != l.UserID {
		s.d.Log.Error("referral: order_pay ledger user is not the order owner", "ledger", l.ID, "order", order, "user", l.UserID, "owner", userID)
		return s.skipOrMark(ctx, tx, now, l, bindingID, "order owner mismatch")
	}
	v, err := s.ConfigAt(ctx, tx, l.CreatedAt)
	if err != nil {
		return err
	}
	base := -l.Amount
	raw := base * int64(v.RateBP) / 10000
	var usedInvitee, settledNet30, pendingInviter int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(reward), 0) FROM referral_rewards WHERE binding_id = $1 AND status <> 'void'`, bindingID).Scan(&usedInvitee); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(settled_amount - clawed_amount), 0) FROM referral_rewards WHERE inviter_id = $1 AND status = 'settled' AND settled_at > $2`,
		inviterID, now.Add(-InviterWindow)).Scan(&settledNet30); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(reward), 0) FROM referral_rewards WHERE inviter_id = $1 AND status = 'pending'`, inviterID).Scan(&pendingInviter); err != nil {
		return err
	}
	roomInvitee := max(0, v.MaxPerInvitee-usedInvitee)
	roomInviter := max(0, v.MaxPerInviter30d-settledNet30-pendingInviter)
	rewardCap := min(raw, roomInvitee, roomInviter)
	settleAfter := l.CreatedAt.Add(time.Duration(v.HoldDays) * 24 * time.Hour)
	if start != nil && start.After(settleAfter) {
		settleAfter = *start
	}
	if _, err := tx.Exec(ctx, `INSERT INTO referral_rewards (order_id, binding_id, inviter_id, invitee_id, rate_version_id, rate_bp, order_amount, reward_cap, reward, status, settle_after, paid_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8, 'pending', $9, $10, $11, $11)`,
		order, bindingID, inviterID, l.UserID, v.ID, v.RateBP, base, rewardCap, settleAfter, l.CreatedAt, now); err != nil {
		return err
	}
	if err := addEvent(ctx, tx, now, order, bindingID, EventAccrue, l.ID, 0, rewardCap,
		fmt.Sprintf("raw=%d room_invitee=%d room_inviter=%d rate_bp=%d version=%d", raw, roomInvitee, roomInviter, v.RateBP, v.ID)); err != nil {
		return err
	}
	if rewardCap < raw {
		return addEvent(ctx, tx, now, order, bindingID, EventCapped, 0, 0, raw-rewardCap, "cap")
	}
	return nil
}

// skipOrMark：流水还在 SkipGrace 内 → errSkip（本轮不写事件，Discover 钉住水位、下轮重试）；
// 已过 SkipGrace → 视为永久跳过，写 void 占位（source_ledger_id = 该流水），之后该订单的退款走「无 reward」占位。
// 调用方须已持有邀请人账户锁并做过幂等复查。
func (s *Service) skipOrMark(ctx context.Context, tx pgx.Tx, now time.Time, l ledgerRow, bindingID int64, reason string) error {
	if now.Sub(l.CreatedAt) < SkipGrace {
		return errSkip
	}
	s.d.Log.Error("referral: giving up on order_pay ledger, writing void placeholder", "ledger", l.ID, "order", l.RefID, "reason", reason)
	return addEvent(ctx, tx, now, l.RefID, bindingID, EventVoid, l.ID, 0, 0, reason)
}
