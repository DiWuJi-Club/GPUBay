package admin

import (
	"context"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

var (
	ErrLastSuper     = apperr.New("last_super", "不能停用或降级最后一个启用中的超级管理员", 409)
	errWrongPassword = apperr.New("wrong_password", "原密码不正确", 400)
)

func (s *Service) List(ctx context.Context) ([]Admin, error) {
	rows, err := s.d.Pool.Query(ctx, `SELECT `+adminCols+` FROM admins ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Admin, error) { return scanAdmin(r) })
}

func (s *Service) FindByUsername(ctx context.Context, username string) (Admin, error) {
	return scanAdmin(s.d.Pool.QueryRow(ctx, `SELECT `+adminCols+` FROM admins WHERE username = $1`,
		strings.ToLower(strings.TrimSpace(username))))
}

// Update 修改角色和 / 或状态；不能让系统失去最后一个启用中的超级管理员。停用时立即删除其全部会话。
func (s *Service) Update(ctx context.Context, actor audit.Actor, id int64, role *Role, status *string) (Admin, error) {
	if role != nil && !role.Valid() {
		return Admin{}, apperr.BadRequest("角色只能是 super、ops 或 finance")
	}
	if status != nil && *status != StatusActive && *status != StatusDisabled {
		return Admin{}, apperr.BadRequest("状态只能是 active 或 disabled")
	}
	now := s.d.Clock.Now()
	var out Admin
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		// 串行化所有角色与状态变更，保证「最后一个超级管理员」检查不会被并发绕过
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('admin:roles'))`); err != nil {
			return err
		}
		a, err := lockAdmin(ctx, tx, id)
		if err != nil {
			return err
		}
		newRole, newStatus := a.Role, a.Status
		if role != nil {
			newRole = *role
		}
		if status != nil {
			newStatus = *status
		}
		if a.Role == RoleSuper && a.Status == StatusActive && (newRole != RoleSuper || newStatus != StatusActive) {
			var others int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM admins WHERE role = 'super' AND status = 'active' AND id <> $1`, id).
				Scan(&others); err != nil {
				return err
			}
			if others == 0 {
				return ErrLastSuper
			}
		}
		if out, err = scanAdmin(tx.QueryRow(ctx, `UPDATE admins SET role = $2, status = $3, updated_at = $4 WHERE id = $1
			RETURNING `+adminCols, id, string(newRole), newStatus, now)); err != nil {
			return err
		}
		if newStatus == StatusDisabled {
			if _, err := tx.Exec(ctx, `DELETE FROM admin_sessions WHERE admin_id = $1`, id); err != nil {
				return err
			}
		}
		return audit.Record(ctx, tx, now, actor.Entry("admin.update", "admin", strconv.FormatInt(id, 10), map[string]any{
			"before": map[string]any{"role": a.Role, "status": a.Status},
			"after":  map[string]any{"role": newRole, "status": newStatus},
		}))
	})
	return out, err
}

// ResetTOTP 清除管理员的 TOTP 密钥、恢复码与全部会话；该管理员下次登录时重新绑定。
func (s *Service) ResetTOTP(ctx context.Context, actor audit.Actor, id int64) error {
	now := s.d.Clock.Now()
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if _, err := lockAdmin(ctx, tx, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE admins SET totp_secret_enc = NULL, totp_confirmed = false, updated_at = $2 WHERE id = $1`,
			id, now); err != nil {
			return err
		}
		for _, table := range []string{"admin_recovery_codes", "admin_sessions", "admin_totp_used"} {
			if _, err := tx.Exec(ctx, `DELETE FROM `+table+` WHERE admin_id = $1`, id); err != nil {
				return err
			}
		}
		return audit.Record(ctx, tx, now, actor.Entry("admin.reset_totp", "admin", strconv.FormatInt(id, 10), nil))
	})
}

// ChangePassword 修改自己的密码；原密码错误计入失败次数。成功后注销该管理员的其他会话。
func (s *Service) ChangePassword(ctx context.Context, sess Session, oldPassword, newPassword, ip string) error {
	if err := ValidatePassword(newPassword); err != nil {
		return err
	}
	hash := HashPassword(newPassword)
	now := s.d.Clock.Now()
	var bizErr error
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		a, err := lockAdmin(ctx, tx, sess.Admin.ID)
		if err != nil {
			return err
		}
		if a.locked(now) {
			bizErr = ErrLocked
			return nil
		}
		if !VerifyPassword(a.passwordHash, oldPassword) {
			bizErr = errWrongPassword
			return recordFailure(ctx, tx, a.ID, now)
		}
		if _, err := tx.Exec(ctx, `UPDATE admins SET password_hash = $2, failed_attempts = 0, updated_at = $3 WHERE id = $1`,
			a.ID, hash, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM admin_sessions WHERE admin_id = $1 AND token_hash <> $2`, a.ID, sess.TokenHash); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, audit.Actor{AdminID: a.ID, IP: ip}.Entry("admin.password_change", "admin", strconv.FormatInt(a.ID, 10), nil))
	})
	if err != nil {
		return err
	}
	return bizErr
}

// EnrollTOTP 供命令行在没有会话的情况下开始绑定：生成或复用待确认的密钥，返回 otpauth URI。
func (s *Service) EnrollTOTP(ctx context.Context, id int64) (string, error) {
	now := s.d.Clock.Now()
	var uri string
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		a, err := lockAdmin(ctx, tx, id)
		if err != nil {
			return err
		}
		if a.TOTPConfirmed {
			return apperr.ErrInvalidState
		}
		secret, err := s.ensureSecret(ctx, tx, a, now)
		if err != nil {
			return err
		}
		uri = OTPAuthURI(Issuer, a.Username, secret)
		return nil
	})
	return uri, err
}

// ConfirmEnrollment 供命令行确认绑定，返回恢复码（只显示这一次）。
func (s *Service) ConfirmEnrollment(ctx context.Context, id int64, code string) ([]string, error) {
	now := s.d.Clock.Now()
	var codes []string
	var bizErr error
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		a, err := lockAdmin(ctx, tx, id)
		if err != nil {
			return err
		}
		if a.TOTPConfirmed {
			return apperr.ErrInvalidState
		}
		if a.locked(now) {
			bizErr = ErrLocked
			return nil
		}
		if a.totpSecretEnc == nil {
			return ErrTOTPSetupRequired
		}
		if codes, bizErr, err = s.confirmTOTP(ctx, tx, a, code, now); err != nil || bizErr != nil {
			return err
		}
		return audit.Record(ctx, tx, now, audit.Actor{}.Entry("admin.totp_bind", "admin", strconv.FormatInt(id, 10), nil))
	})
	if err != nil {
		return nil, err
	}
	if bizErr != nil {
		return nil, bizErr
	}
	return codes, nil
}
