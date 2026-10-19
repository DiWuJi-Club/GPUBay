package referral

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/wallet"
)

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// withRetry 对死锁检测（40P01）与序列化失败（40001）把整个事务重试一次。
func withRetry(fn func() error) error {
	err := fn()
	if c := pgCode(err); c == "40P01" || c == "40001" {
		err = fn()
	}
	return err
}

// Release 发放到期的待发放返利（规格 §4.4）：每轮最多 ReleaseBatch 行，逐行独立事务；返回本轮置为 settled / void 的行数。
// 单行失败记日志并继续，全部错误合并返回。
func (s *Service) Release(ctx context.Context) (int, error) {
	now := s.d.Clock.Now()
	rows, err := s.d.Pool.Query(ctx, `SELECT order_id, inviter_id, invitee_id FROM referral_rewards WHERE status = 'pending' AND settle_after <= $1
		ORDER BY settle_after, order_id LIMIT $2`, now, ReleaseBatch)
	if err != nil {
		return 0, err
	}
	type cand struct {
		order            string
		inviter, invitee int64
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (cand, error) {
		var c cand
		err := r.Scan(&c.order, &c.inviter, &c.invitee)
		return c, err
	})
	if err != nil {
		return 0, err
	}
	released := 0
	var errs []error
	for _, c := range list {
		done := false
		err := withRetry(func() error {
			done = false
			return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
				var err error
				done, err = s.releaseOne(ctx, tx, now, c.order, c.inviter, c.invitee)
				return err
			})
		})
		if err != nil {
			s.d.Log.Error("referral: release failed", "order", c.order, "err", err)
			errs = append(errs, fmt.Errorf("release %s: %w", c.order, err))
			continue
		}
		if done {
			released++
		}
	}
	return released, errors.Join(errs...)
}

// releaseOne 返回是否把该行推进到了终态（settled / void，含同身份撤销作废）。门槛不满足或搁置时返回 false, nil（下轮再看）。
// 加锁：users(邀请人, 被邀请人) FOR KEY SHARE → referral_accounts(邀请人) FOR UPDATE → referral_rewards(订单) FOR UPDATE SKIP LOCKED →
// [同身份撤销：referral_bindings → referral_rewards(该绑定其余 pending)] → wallet_accounts(邀请人)。
func (s *Service) releaseOne(ctx context.Context, tx pgx.Tx, now time.Time, order string, inviterID, inviteeID int64) (bool, error) {
	if err := lockUsersKeyShare(ctx, tx, inviterID, inviteeID); err != nil {
		return false, err
	}
	acc, err := lockAccount(ctx, tx, inviterID)
	if err != nil {
		return false, err
	}
	r, err := scanReward(tx.QueryRow(ctx, `SELECT `+rewardCols+` FROM referral_rewards WHERE order_id = $1 AND status = 'pending' AND settle_after <= $2 FOR UPDATE SKIP LOCKED`, order, now))
	if db.IsNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var bindingStatus string
	var reviewedAt *time.Time
	if err := tx.QueryRow(ctx, `SELECT status, reviewed_at FROM referral_bindings WHERE id = $1`, r.BindingID).Scan(&bindingStatus, &reviewedAt); err != nil {
		return false, err
	}
	if bindingStatus != BindingActive || acc.Status != AccountActive {
		return false, nil
	}
	u, err := s.d.Users.UserState(ctx, tx, inviterID)
	if err != nil {
		return false, err
	}
	if u.Status != "active" || u.KYCStatus != "verified" {
		return false, nil
	}
	// 纵深防御：实名钩子失败只留审计、不让实名失败，可能漏掉同身份撤销 → 付款前按全部 verified 指纹复查。
	same, err := s.sameIdentityAtRelease(ctx, tx, inviterID, inviteeID)
	if err != nil {
		return false, err
	}
	if same {
		s.d.Log.Warn("referral: same identity found at release, revoking binding", "order", order, "binding", r.BindingID)
		if err := s.autoRevoke(ctx, tx, now, r.BindingID); err != nil {
			return false, err
		}
		return true, nil // 本行已随绑定撤销作废
	}
	held, err := kycCheckUnresolved(ctx, tx, inviterID, inviteeID, reviewedAt)
	if err != nil {
		return false, err
	}
	if held {
		s.d.Log.Warn("referral: unresolved kyc_check_failed, holding reward", "order", order, "binding", r.BindingID)
		return false, nil
	}
	if r.RewardAmt == 0 {
		if _, err := tx.Exec(ctx, `UPDATE referral_rewards SET status = 'void', updated_at = $2 WHERE order_id = $1`, order, now); err != nil {
			return false, err
		}
		return true, addEvent(ctx, tx, now, order, r.BindingID, EventVoid, 0, 0, 0, "zero after refund")
	}
	offset := min(acc.Debt, r.RewardAmt)
	if offset > 0 {
		if _, err := tx.Exec(ctx, `UPDATE referral_accounts SET debt = debt - $2, updated_at = $3 WHERE user_id = $1`, inviterID, offset, now); err != nil {
			return false, err
		}
		if err := addEvent(ctx, tx, now, order, r.BindingID, EventDebtOffset, 0, 0, offset, "offset debt"); err != nil {
			return false, err
		}
	}
	var ledgerID int64
	if credit := r.RewardAmt - offset; credit > 0 {
		// 重放保护：发放流水已存在（行被异常改回 pending 等）则不再入账。
		// 不能靠捕获 Apply 的 ErrDuplicate：唯一冲突会让 PG 把事务置为失败态，之后的语句都会报 25P02。
		err := tx.QueryRow(ctx, `SELECT id FROM wallet_ledger WHERE kind = 'referral_rebate' AND ref_type = $1 AND ref_id = $2`, RefType, order).Scan(&ledgerID)
		switch {
		case err == nil:
			s.d.Log.Warn("referral: rebate ledger already exists, marking settled without crediting again", "order", order, "ledger", ledgerID)
		case db.IsNoRows(err):
			masked, err := s.inviteeMasked(ctx, tx, r.InviteeID)
			if err != nil {
				return false, err
			}
			if _, err := s.d.Wallet.Apply(ctx, tx, wallet.Entry{UserID: inviterID, Amount: credit, Kind: wallet.KindReferralRebate, RefType: RefType, RefID: order,
				Remark: "邀请返利 " + masked + " 订单 " + order}); err != nil {
				return false, err
			}
			if err := tx.QueryRow(ctx, `SELECT id FROM wallet_ledger WHERE kind = 'referral_rebate' AND ref_type = $1 AND ref_id = $2`, RefType, order).Scan(&ledgerID); err != nil {
				return false, err
			}
		default:
			return false, err
		}
	}
	if err := addEvent(ctx, tx, now, order, r.BindingID, EventSettle, 0, ledgerID, r.RewardAmt, fmt.Sprintf("offset=%d", offset)); err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, `UPDATE referral_rewards SET status = 'settled', settled_amount = reward, settled_at = $2, updated_at = $2 WHERE order_id = $1`, order, now)
	return true, err
}

// sameIdentityAtRelease：被邀请人的任一 verified 指纹等于邀请人或同邀请人其它未撤销被邀请人的任一 verified 指纹（与 OnKYCVerified 同一判定）。
// 调用方须已持有 referral_accounts(邀请人) 锁。
func (s *Service) sameIdentityAtRelease(ctx context.Context, tx pgx.Tx, inviterID, inviteeID int64) (bool, error) {
	self, err := s.d.Users.VerifiedFingerprints(ctx, tx, []int64{inviteeID})
	if err != nil || len(self[inviteeID]) == 0 {
		return false, err
	}
	mine := make(map[string]bool, len(self[inviteeID]))
	for _, fp := range self[inviteeID] {
		mine[fp] = true
	}
	return s.inviterSideMatches(ctx, tx, inviterID, inviteeID, func(fps []string) bool {
		for _, fp := range fps {
			if mine[fp] {
				return true
			}
		}
		return false
	})
}

// kycCheckUnresolved 报告邀请人或被邀请人是否有晚于绑定最近一次后台复核（reviewed_at；NULL 视为从未复核）的
// referral.kyc_check_failed 审计——实名同身份判定失败、尚待人工核查。后台对该绑定的裁决（通过 / 撤销）即视为已解决。
func kycCheckUnresolved(ctx context.Context, q db.Querier, inviterID, inviteeID int64, reviewedAt *time.Time) (bool, error) {
	var held bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM audit_logs WHERE target_type = 'user' AND target_id IN ($1, $2)
		AND action = 'referral.kyc_check_failed' AND ($3::timestamptz IS NULL OR created_at > $3))`,
		strconv.FormatInt(inviterID, 10), strconv.FormatInt(inviteeID, 10), reviewedAt).Scan(&held)
	return held, err
}
