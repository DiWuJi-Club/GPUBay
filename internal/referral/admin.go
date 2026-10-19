package referral

import (
	"context"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

// 后台裁决。加锁顺序与发放一致（见包注释）：users FOR KEY SHARE → referral_accounts(邀请人) FOR UPDATE →
// referral_bindings → referral_rewards。冻结与同一轮发放的互斥由账户锁保证：releaseOne 在账户锁内复核账户状态。

// adminRef 把系统身份（AdminID 为 0）记为 NULL。
func adminRef(actor audit.Actor) *int64 {
	if actor.AdminID == 0 {
		return nil
	}
	return &actor.AdminID
}

// Freeze 冻结邀请人：码不再可绑定、返利停止发放（继续计提）。
func (s *Service) Freeze(ctx context.Context, actor audit.Actor, userID int64, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return apperr.BadRequest("请填写冻结原因")
	}
	now := s.d.Clock.Now()
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if err := lockUsersKeyShare(ctx, tx, userID); err != nil {
			return err
		}
		a, err := lockAccount(ctx, tx, userID)
		if err != nil {
			return err
		}
		if a.Status == AccountFrozen {
			return apperr.ErrReferralBindingState
		}
		if _, err := tx.Exec(ctx, `UPDATE referral_accounts SET status = 'frozen', frozen_reason = $2, frozen_by = $3, frozen_at = $4, updated_at = $4 WHERE user_id = $1`,
			userID, reason, adminRef(actor), now); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("referral.freeze", "referral_account", strconv.FormatInt(userID, 10), map[string]any{"reason": reason}))
	})
}

// Unfreeze 解冻：冻结期间累计的到期返利在下一轮发放。
func (s *Service) Unfreeze(ctx context.Context, actor audit.Actor, userID int64) error {
	now := s.d.Clock.Now()
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if err := lockUsersKeyShare(ctx, tx, userID); err != nil {
			return err
		}
		a, err := lockAccount(ctx, tx, userID)
		if err != nil {
			return err
		}
		if a.Status != AccountFrozen {
			return apperr.ErrReferralBindingState
		}
		if _, err := tx.Exec(ctx, `UPDATE referral_accounts SET status = 'active', frozen_reason = '', frozen_by = NULL, frozen_at = NULL, updated_at = $2 WHERE user_id = $1`, userID, now); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("referral.unfreeze", "referral_account", strconv.FormatInt(userID, 10), map[string]any{"previous_reason": a.FrozenReason}))
	})
}

// lockBindingOwner 读绑定的邀请人与被邀请人（二者不变，加锁前读安全；状态在锁后由各操作的条件更新判定），
// 再按加锁顺序锁 users(邀请人, 被邀请人) → referral_accounts(邀请人)。绑定不存在 → apperr.ErrNotFound。
func lockBindingOwner(ctx context.Context, tx pgx.Tx, bindingID int64) error {
	var inviterID, inviteeID int64
	err := tx.QueryRow(ctx, `SELECT inviter_id, invitee_id FROM referral_bindings WHERE id = $1`, bindingID).Scan(&inviterID, &inviteeID)
	if db.IsNoRows(err) {
		return apperr.ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := lockUsersKeyShare(ctx, tx, inviterID, inviteeID); err != nil {
		return err
	}
	_, err = lockAccount(ctx, tx, inviterID)
	return err
}

// Approve 把搁置的绑定（same_device / kyc_check_failed）转为有效；待发放返利在下一轮正常发放。
// 写 reviewed_at：发放据此把此前的 referral.kyc_check_failed 审计视为已解决（kycCheckUnresolved）。
func (s *Service) Approve(ctx context.Context, actor audit.Actor, bindingID int64, note string) error {
	now := s.d.Clock.Now()
	note = strings.TrimSpace(note)
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if err := lockBindingOwner(ctx, tx, bindingID); err != nil {
			return err
		}
		var status, prevHold string
		if err := tx.QueryRow(ctx, `SELECT status, hold_reason FROM referral_bindings WHERE id = $1 FOR UPDATE`, bindingID).Scan(&status, &prevHold); err != nil {
			return err
		}
		if status != BindingHeld {
			return apperr.ErrReferralBindingState
		}
		if _, err := tx.Exec(ctx, `UPDATE referral_bindings SET status = 'active', hold_reason = '', reviewed_by = $2, reviewed_at = $3, review_note = $4, updated_at = $3 WHERE id = $1`,
			bindingID, adminRef(actor), now, note); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("referral.approve", "referral_binding", strconv.FormatInt(bindingID, 10),
			map[string]any{"note": note, "hold_reason": prevHold}))
	})
}

// Revoke 撤销绑定（任意非 revoked 状态）：待发放全部作废，已发放不自动扣回（财务用调账）；返回作废金额。
// 与实名同身份自动撤销共用 revokeBinding（写 reviewed_by / reviewed_at、void 事件与审计）。
func (s *Service) Revoke(ctx context.Context, actor audit.Actor, bindingID int64, reason string) (int64, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return 0, apperr.BadRequest("请填写撤销原因")
	}
	now := s.d.Clock.Now()
	var voided int64
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if err := lockBindingOwner(ctx, tx, bindingID); err != nil {
			return err
		}
		var err error
		voided, err = s.revokeBinding(ctx, tx, now, bindingID, reason, actor, "", "referral.revoke")
		return err
	})
	return voided, err
}
