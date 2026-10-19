package referral

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

// OnKYCVerified 在 identity 的实名结果事务内（已持有 users(本人) 行锁）做同身份判定：
// 被邀请人的指纹等于邀请人或同邀请人其它被邀请人 → 该绑定撤销；本人作为邀请人时对其所有未撤销绑定做同样比较。
// 加锁顺序见包注释：先本人的邀请人的 referral_accounts，再本人自己的。
// 只接收 userID 与 identity 算出的十六进制指纹，没有用户可控的字符串进入 SQL；并发撤销等竞态按「已处理」跳过。
func (s *Service) OnKYCVerified(ctx context.Context, tx pgx.Tx, userID int64, fingerprint string) error {
	if fingerprint == "" {
		return nil
	}
	now := s.d.Clock.Now()
	// (a) 作为被邀请人
	var bindingID, inviterID int64
	err := tx.QueryRow(ctx, `SELECT id, inviter_id FROM referral_bindings WHERE invitee_id = $1 AND status <> 'revoked'`, userID).Scan(&bindingID, &inviterID)
	if err != nil && !db.IsNoRows(err) {
		return err
	}
	if err == nil {
		if err := s.checkInvitee(ctx, tx, now, bindingID, inviterID, userID, fingerprint); err != nil {
			return err
		}
	}
	// (b) 作为邀请人（被邀请人先实名、邀请人后实名）
	if _, err := lockAccount(ctx, tx, userID); err != nil {
		if errors.Is(err, apperr.ErrNotFound) {
			return nil // 从未生成过邀请码，不可能有绑定
		}
		return err
	}
	rows, err := tx.Query(ctx, `SELECT id, invitee_id FROM referral_bindings WHERE inviter_id = $1 AND status <> 'revoked' ORDER BY id`, userID)
	if err != nil {
		return err
	}
	type row struct{ id, invitee int64 }
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.id, &x.invitee)
		return x, err
	})
	if err != nil || len(list) == 0 {
		return err
	}
	ids := make([]int64, len(list))
	for i, r := range list {
		ids[i] = r.invitee
	}
	fps, err := s.d.Users.VerifiedFingerprints(ctx, tx, ids)
	if err != nil {
		return err
	}
	for _, r := range list {
		if fps[r.invitee] == fingerprint {
			if err := s.autoRevoke(ctx, tx, now, r.id); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkInvitee：本人是被邀请人时，锁邀请人账户后与邀请人及同邀请人其它未撤销被邀请人的指纹比较。
func (s *Service) checkInvitee(ctx context.Context, tx pgx.Tx, now time.Time, bindingID, inviterID, self int64, fingerprint string) error {
	if _, err := lockAccount(ctx, tx, inviterID); err != nil {
		if errors.Is(err, apperr.ErrNotFound) {
			return nil // 有绑定必有码主账户；防御性跳过
		}
		return err
	}
	others, err := s.siblingInvitees(ctx, tx, inviterID, self)
	if err != nil {
		return err
	}
	fps, err := s.d.Users.VerifiedFingerprints(ctx, tx, append(others, inviterID))
	if err != nil {
		return err
	}
	for _, fp := range fps {
		if fp == fingerprint {
			return s.autoRevoke(ctx, tx, now, bindingID)
		}
	}
	return nil
}

// autoRevoke 以系统身份撤销；绑定在读取后、加锁前已被（后台）撤销时按已处理跳过。
func (s *Service) autoRevoke(ctx context.Context, tx pgx.Tx, now time.Time, bindingID int64) error {
	_, err := s.revokeBinding(ctx, tx, now, bindingID, FlagSameIdentity, audit.Actor{}, FlagSameIdentity, "referral.auto_revoke")
	if errors.Is(err, apperr.ErrReferralBindingState) {
		return nil
	}
	return err
}

// siblingInvitees 返回同邀请人下除 self 外的未撤销被邀请人。
func (s *Service) siblingInvitees(ctx context.Context, q db.Querier, inviterID, self int64) ([]int64, error) {
	rows, err := q.Query(ctx, `SELECT invitee_id FROM referral_bindings WHERE inviter_id = $1 AND invitee_id <> $2 AND status <> 'revoked'`, inviterID, self)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// revokeBinding 撤销绑定并作废其全部待发放返利（已发放的不动，由财务用调账处理），返回作废金额。
// 调用方必须已持有 referral_accounts(邀请人) 锁。action：referral.auto_revoke（系统）或 referral.revoke（后台）。
// 绑定已是 revoked 时返回 apperr.ErrReferralBindingState。
func (s *Service) revokeBinding(ctx context.Context, tx pgx.Tx, now time.Time, bindingID int64, reason string, actor audit.Actor, flag, action string) (int64, error) {
	var by *int64
	if actor.AdminID != 0 {
		by = &actor.AdminID
	}
	tag, err := tx.Exec(ctx, `UPDATE referral_bindings SET status = 'revoked', hold_reason = $2, review_note = $2,
		risk_flags = CASE WHEN $3::text = '' OR $3::text = ANY(risk_flags) THEN risk_flags ELSE array_append(risk_flags, $3::text) END,
		reviewed_by = $4, reviewed_at = $5, updated_at = $5 WHERE id = $1 AND status <> 'revoked'`, bindingID, reason, flag, by, now)
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() == 0 {
		return 0, apperr.ErrReferralBindingState
	}
	rows, err := tx.Query(ctx, `UPDATE referral_rewards SET status = 'void', updated_at = $2 WHERE binding_id = $1 AND status = 'pending' RETURNING order_id, reward`, bindingID, now)
	if err != nil {
		return 0, err
	}
	type voided struct {
		order  string
		reward int64
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (voided, error) {
		var v voided
		err := r.Scan(&v.order, &v.reward)
		return v, err
	})
	if err != nil {
		return 0, err
	}
	var total int64
	for _, v := range list {
		total += v.reward
		if err := addEvent(ctx, tx, now, v.order, bindingID, EventVoid, 0, 0, v.reward, reason); err != nil {
			return 0, err
		}
	}
	return total, audit.Record(ctx, tx, now, actor.Entry(action, "referral_binding", strconv.FormatInt(bindingID, 10),
		map[string]any{"reason": reason, "voided": total, "flag": flag}))
}
