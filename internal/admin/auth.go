package admin

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/randx"
)

// LoginResult 是第一步（密码）登录的结果。NeedTOTPSetup 为 true 时必须先调用 ConfirmTOTPSetup 完成绑定。
type LoginResult struct {
	Token         string
	NeedTOTPSetup bool
	OTPAuthURI    string
}

var (
	dummyOnce sync.Once
	dummyHash string
)

// burnPasswordCheck 对不存在的用户名也做一次同等开销的哈希，避免通过耗时差异探测用户名。
func burnPasswordCheck(password string) {
	dummyOnce.Do(func() { dummyHash = HashPassword("spark-cloud-dummy-password") })
	VerifyPassword(dummyHash, password)
}

// Login 校验用户名与密码，成功后返回 5 分钟内有效的「密码已通过」会话。
func (s *Service) Login(ctx context.Context, username, password, ip string) (LoginResult, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	now := s.d.Clock.Now()
	var res LoginResult
	var bizErr error
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		a, err := scanAdminRow(tx.QueryRow(ctx, `SELECT `+adminRowCols+` FROM admins WHERE username = $1 FOR UPDATE`, username))
		if errors.Is(err, apperr.ErrNotFound) {
			burnPasswordCheck(password)
			bizErr = ErrInvalidCredentials
			return nil
		}
		if err != nil {
			return err
		}
		if a.locked(now) {
			bizErr = ErrLocked
			return nil
		}
		if !VerifyPassword(a.passwordHash, password) || a.Status != StatusActive {
			bizErr = ErrInvalidCredentials
			return recordFailure(ctx, tx, a.ID, now)
		}
		token := randx.Token()
		if _, err := tx.Exec(ctx, `INSERT INTO admin_sessions (token_hash, admin_id, stage, ip, created_at, last_seen_at, expires_at)
			VALUES ($1, $2, 'password_ok', $3, $4, $4, $5)`, hashSecret(token), a.ID, ip, now, now.Add(PendingTTL)); err != nil {
			return err
		}
		res.Token = token
		if !a.TOTPConfirmed {
			secret, err := s.ensureSecret(ctx, tx, a, now)
			if err != nil {
				return err
			}
			res.NeedTOTPSetup = true
			res.OTPAuthURI = OTPAuthURI(Issuer, a.Username, secret)
		}
		return nil
	})
	if err != nil {
		return LoginResult{}, err
	}
	if bizErr != nil {
		return LoginResult{}, bizErr
	}
	return res, nil
}

// pendingSession 找到「密码已通过」阶段且未过期的会话，并锁定其管理员行。
func (s *Service) pendingSession(ctx context.Context, tx pgx.Tx, token string, now time.Time) (adminRow, error) {
	var adminID int64
	err := tx.QueryRow(ctx, `SELECT admin_id FROM admin_sessions
		WHERE token_hash = $1 AND stage = 'password_ok' AND expires_at > $2`, hashSecret(token), now).Scan(&adminID)
	if db.IsNoRows(err) {
		return adminRow{}, apperr.ErrUnauthorized
	}
	if err != nil {
		return adminRow{}, err
	}
	a, err := lockAdmin(ctx, tx, adminID)
	if err != nil {
		return adminRow{}, err
	}
	if a.Status != StatusActive {
		return adminRow{}, apperr.ErrUnauthorized
	}
	return a, nil
}

// promote 把会话升为已登录，并清零失败计数、记录登录时间与 IP。
func promote(ctx context.Context, tx pgx.Tx, token string, a adminRow, ip string, now time.Time) error {
	if _, err := tx.Exec(ctx, `UPDATE admin_sessions SET stage = 'active', ip = $2, created_at = $3, last_seen_at = $3, expires_at = $4
		WHERE token_hash = $1`, hashSecret(token), ip, now, now.Add(SessionTTL)); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE admins SET failed_attempts = 0, locked_until = NULL, last_login_at = $2, last_login_ip = $3, updated_at = $2
		WHERE id = $1`, a.ID, now, ip)
	return err
}

// ConfirmTOTPSetup 首次登录绑定 TOTP：校验动态码，生成恢复码（只返回这一次），会话升为已登录。
func (s *Service) ConfirmTOTPSetup(ctx context.Context, token, code, ip string) (Admin, []string, error) {
	now := s.d.Clock.Now()
	var a adminRow
	var codes []string
	var bizErr error
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		var err error
		if a, err = s.pendingSession(ctx, tx, token, now); err != nil {
			return err
		}
		if a.locked(now) {
			bizErr = ErrLocked
			return nil
		}
		if a.TOTPConfirmed {
			return apperr.ErrInvalidState
		}
		if a.totpSecretEnc == nil {
			return ErrTOTPSetupRequired
		}
		if codes, bizErr, err = s.confirmTOTP(ctx, tx, a, code, now); err != nil || bizErr != nil {
			return err
		}
		if err := promote(ctx, tx, token, a, ip, now); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, audit.Actor{AdminID: a.ID, IP: ip}.Entry("admin.totp_bind", "admin", strconv.FormatInt(a.ID, 10), nil))
	})
	if err != nil {
		return Admin{}, nil, err
	}
	if bizErr != nil {
		return Admin{}, nil, bizErr
	}
	a.TOTPConfirmed, a.LastLoginAt, a.LastLoginIP = true, &now, ip
	return a.Admin, codes, nil
}

// VerifyTOTP 第二步：用动态码完成登录。
func (s *Service) VerifyTOTP(ctx context.Context, token, code, ip string) (Admin, error) {
	return s.secondFactor(ctx, token, ip, "admin.login", func(tx pgx.Tx, a adminRow, now time.Time) (error, error) {
		return s.checkTOTP(ctx, tx, a, code, now)
	})
}

// VerifyRecoveryCode 第二步：用一次性恢复码完成登录（手机丢失时使用）。
func (s *Service) VerifyRecoveryCode(ctx context.Context, token, recoveryCode, ip string) (Admin, error) {
	return s.secondFactor(ctx, token, ip, "admin.login_recovery_code", func(tx pgx.Tx, a adminRow, now time.Time) (error, error) {
		tag, err := tx.Exec(ctx, `UPDATE admin_recovery_codes SET used_at = $3
			WHERE admin_id = $1 AND code_hash = $2 AND used_at IS NULL`, a.ID, hashRecoveryCode(recoveryCode), now)
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() == 0 {
			return ErrInvalidTOTP, recordFailure(ctx, tx, a.ID, now)
		}
		return nil, nil
	})
}

func (s *Service) secondFactor(ctx context.Context, token, ip, action string,
	check func(tx pgx.Tx, a adminRow, now time.Time) (bizErr, err error)) (Admin, error) {
	now := s.d.Clock.Now()
	var a adminRow
	var bizErr error
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		var err error
		if a, err = s.pendingSession(ctx, tx, token, now); err != nil {
			return err
		}
		if a.locked(now) {
			bizErr = ErrLocked
			return nil
		}
		if !a.TOTPConfirmed {
			bizErr = ErrTOTPSetupRequired
			return nil
		}
		if bizErr, err = check(tx, a, now); err != nil || bizErr != nil {
			return err
		}
		if err := promote(ctx, tx, token, a, ip, now); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, audit.Actor{AdminID: a.ID, IP: ip}.Entry(action, "admin", strconv.FormatInt(a.ID, 10), nil))
	})
	if err != nil {
		return Admin{}, err
	}
	if bizErr != nil {
		return Admin{}, bizErr
	}
	a.LastLoginAt, a.LastLoginIP = &now, ip
	return a.Admin, nil
}

const adminColsA = `a.id, a.username, a.role, a.status, a.totp_confirmed, a.last_login_at, a.last_login_ip, a.created_at`

// Authenticate 校验已登录会话（绝对 12 小时、空闲 2 小时），并刷新最近访问时间。
func (s *Service) Authenticate(ctx context.Context, token string) (Session, error) {
	if token == "" {
		return Session{}, apperr.ErrUnauthorized
	}
	now := s.d.Clock.Now()
	h := hashSecret(token)
	var sess Session
	var stage string
	var lastSeen, expires time.Time
	a := &sess.Admin
	err := s.d.Pool.QueryRow(ctx, `SELECT s.stage, s.stepup_at, s.last_seen_at, s.expires_at, `+adminColsA+`
		FROM admin_sessions s JOIN admins a ON a.id = s.admin_id WHERE s.token_hash = $1`, h).
		Scan(&stage, &sess.StepUpAt, &lastSeen, &expires,
			&a.ID, &a.Username, &a.Role, &a.Status, &a.TOTPConfirmed, &a.LastLoginAt, &a.LastLoginIP, &a.CreatedAt)
	if db.IsNoRows(err) {
		return Session{}, apperr.ErrUnauthorized
	}
	if err != nil {
		return Session{}, err
	}
	if stage != "active" || !now.Before(expires) || !now.Before(lastSeen.Add(IdleTimeout)) || a.Status != StatusActive {
		return Session{}, apperr.ErrUnauthorized
	}
	if _, err := s.d.Pool.Exec(ctx, `UPDATE admin_sessions SET last_seen_at = $2 WHERE token_hash = $1`, h, now); err != nil {
		return Session{}, err
	}
	sess.TokenHash = h
	return sess, nil
}

// StepUp 高风险操作的二次验证：code 非空时只校验动态码（不记录验证时间）；为空时要求 5 分钟内验证过。
// 验证窗口由 RecordStepUp 在操作成功之后打开：参数错误、业务失败的请求不能借一个正确的动态码打开窗口。
func (s *Service) StepUp(ctx context.Context, sess Session, code string) error {
	now := s.d.Clock.Now()
	if NormalizeCode(code) == "" {
		if sess.StepUpAt != nil && now.Sub(*sess.StepUpAt) < StepUpWindow {
			return nil
		}
		return ErrTOTPRequired
	}
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
		if bizErr, err = s.checkTOTP(ctx, tx, a, code, now); err != nil || bizErr != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE admins SET failed_attempts = 0, updated_at = $2 WHERE id = $1`, a.ID, now)
		return err
	})
	if err != nil {
		return err
	}
	return bizErr
}

// RecordStepUp 打开会话的二次验证窗口（StepUpWindow）；调用方在带动态码的高风险操作成功之后调用。
func (s *Service) RecordStepUp(ctx context.Context, sess Session) error {
	_, err := s.d.Pool.Exec(ctx, `UPDATE admin_sessions SET stepup_at = $2 WHERE token_hash = $1`, sess.TokenHash, s.d.Clock.Now())
	return err
}

func (s *Service) Logout(ctx context.Context, token string) error {
	_, err := s.d.Pool.Exec(ctx, `DELETE FROM admin_sessions WHERE token_hash = $1`, hashSecret(token))
	return err
}
