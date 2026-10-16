// Package admin 管理后台管理员：账号、密码、TOTP 动态码、恢复码、会话与高风险操作二次验证。
package admin

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/secretbox"
)

type Role string

const (
	RoleSuper   Role = "super"
	RoleOps     Role = "ops"
	RoleFinance Role = "finance"
)

func (r Role) Valid() bool { return r == RoleSuper || r == RoleOps || r == RoleFinance }

const (
	StatusActive   = "active"
	StatusDisabled = "disabled"

	PendingTTL        = 5 * time.Minute  // 密码通过后等待动态码的时长
	SessionTTL        = 12 * time.Hour   // 会话绝对有效期
	IdleTimeout       = 2 * time.Hour    // 会话空闲失效
	StepUpWindow      = 5 * time.Minute  // 高风险操作二次验证的有效期
	MaxFailures       = 5                // 连续失败次数上限（密码与动态码合计）
	LockDuration      = 15 * time.Minute // 锁定时长
	totpUsedRetention = 10 * time.Minute

	Issuer = "Spark Cloud"
)

var (
	ErrInvalidCredentials = apperr.New("invalid_credentials", "用户名或密码错误", 401)
	ErrLocked             = apperr.New("account_locked", "登录失败次数过多，请 15 分钟后再试", 423)
	ErrInvalidTOTP        = apperr.New("invalid_totp", "动态码错误或已使用过", 400)
	ErrTOTPRequired       = apperr.New("totp_required", "该操作需要输入动态码", 403)
	ErrTOTPSetupRequired  = apperr.New("totp_setup_required", "请先绑定动态码", 403)
)

var usernameRe = regexp.MustCompile(`^[a-z][a-z0-9_.-]{2,31}$`)

type Admin struct {
	ID            int64      `json:"id"`
	Username      string     `json:"username"`
	Role          Role       `json:"role"`
	Status        string     `json:"status"`
	TOTPConfirmed bool       `json:"totp_confirmed"`
	LastLoginAt   *time.Time `json:"last_login_at"`
	LastLoginIP   string     `json:"last_login_ip"`
	CreatedAt     time.Time  `json:"created_at"`
}

// Session 是已登录的后台会话。
type Session struct {
	Admin     Admin
	TokenHash string
	StepUpAt  *time.Time
}

type Deps struct {
	Pool  *pgxpool.Pool
	Clock clock.Clock
	Box   *secretbox.Box
}

type Service struct {
	d Deps
}

func NewService(d Deps) *Service { return &Service{d: d} }

const adminCols = `id, username, role, status, totp_confirmed, last_login_at, last_login_ip, created_at`

func scanAdmin(row pgx.Row) (Admin, error) {
	var a Admin
	err := row.Scan(&a.ID, &a.Username, &a.Role, &a.Status, &a.TOTPConfirmed, &a.LastLoginAt, &a.LastLoginIP, &a.CreatedAt)
	if db.IsNoRows(err) {
		return Admin{}, apperr.ErrNotFound
	}
	return a, err
}

// adminRow 带有密码哈希、加密的 TOTP 密钥与失败计数，只在包内使用。
type adminRow struct {
	Admin
	passwordHash   string
	totpSecretEnc  []byte
	failedAttempts int
	lockedUntil    *time.Time
}

func (a adminRow) locked(now time.Time) bool {
	return a.lockedUntil != nil && now.Before(*a.lockedUntil)
}

const adminRowCols = adminCols + `, password_hash, totp_secret_enc, failed_attempts, locked_until`

func scanAdminRow(row pgx.Row) (adminRow, error) {
	var a adminRow
	err := row.Scan(&a.ID, &a.Username, &a.Role, &a.Status, &a.TOTPConfirmed, &a.LastLoginAt, &a.LastLoginIP, &a.CreatedAt,
		&a.passwordHash, &a.totpSecretEnc, &a.failedAttempts, &a.lockedUntil)
	if db.IsNoRows(err) {
		return adminRow{}, apperr.ErrNotFound
	}
	return a, err
}

func lockAdmin(ctx context.Context, tx pgx.Tx, id int64) (adminRow, error) {
	return scanAdminRow(tx.QueryRow(ctx, `SELECT `+adminRowCols+` FROM admins WHERE id = $1 FOR UPDATE`, id))
}

func (s *Service) Get(ctx context.Context, id int64) (Admin, error) {
	return scanAdmin(s.d.Pool.QueryRow(ctx, `SELECT `+adminCols+` FROM admins WHERE id = $1`, id))
}

// Create 新建管理员（状态 active、未绑定 TOTP，首次登录时绑定）。actor 为零值表示命令行创建。
func (s *Service) Create(ctx context.Context, actor audit.Actor, username, password string, role Role) (Admin, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if !usernameRe.MatchString(username) {
		return Admin{}, apperr.BadRequest("用户名需为 3～32 位小写字母、数字或 _ . -，并以字母开头")
	}
	if !role.Valid() {
		return Admin{}, apperr.BadRequest("角色只能是 super、ops 或 finance")
	}
	if err := ValidatePassword(password); err != nil {
		return Admin{}, err
	}
	hash := HashPassword(password)
	now := s.d.Clock.Now()
	var a Admin
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		var err error
		a, err = scanAdmin(tx.QueryRow(ctx, `INSERT INTO admins (username, password_hash, role, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $4) RETURNING `+adminCols, username, hash, string(role), now))
		if db.IsUniqueViolation(err) {
			return apperr.New("duplicate_username", "用户名已存在", 409)
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("admin.create", "admin", strconv.FormatInt(a.ID, 10),
			map[string]any{"username": username, "role": role}))
	})
	return a, err
}

// secret 解密管理员的 TOTP 密钥。
func (s *Service) secret(a adminRow) (string, error) {
	if a.totpSecretEnc == nil {
		return "", ErrTOTPSetupRequired
	}
	plain, err := s.d.Box.Open(a.totpSecretEnc)
	if err != nil {
		return "", fmt.Errorf("open totp secret: %w", err)
	}
	return string(plain), nil
}

// ensureSecret 返回管理员待确认的 TOTP 密钥；还没有则生成并加密保存。
func (s *Service) ensureSecret(ctx context.Context, tx pgx.Tx, a adminRow, now time.Time) (string, error) {
	if a.totpSecretEnc != nil {
		return s.secret(a)
	}
	secret := NewTOTPSecret()
	_, err := tx.Exec(ctx, `UPDATE admins SET totp_secret_enc = $2, updated_at = $3 WHERE id = $1`, a.ID, s.d.Box.Seal([]byte(secret)), now)
	return secret, err
}

// recordFailure 累计一次失败；达到上限时锁定账号并清零计数。
func recordFailure(ctx context.Context, tx pgx.Tx, id int64, now time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE admins SET
			failed_attempts = CASE WHEN failed_attempts + 1 >= $2 THEN 0 ELSE failed_attempts + 1 END,
			locked_until = CASE WHEN failed_attempts + 1 >= $2 THEN $3::timestamptz ELSE locked_until END,
			updated_at = $4
		WHERE id = $1`, id, MaxFailures, now.Add(LockDuration), now)
	return err
}

// useStep 记录已使用的动态码时间片；同一时间片第二次使用返回 false（防重放）。
func useStep(ctx context.Context, tx pgx.Tx, adminID, step int64, now time.Time) (bool, error) {
	if _, err := tx.Exec(ctx, `DELETE FROM admin_totp_used WHERE admin_id = $1 AND used_at < $2`,
		adminID, now.Add(-totpUsedRetention)); err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO admin_totp_used (admin_id, step, used_at) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
		adminID, step, now)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// checkTOTP 校验动态码并防重放；不通过时累计失败次数并返回 false（调用方应提交事务后返回 ErrInvalidTOTP）。
func (s *Service) checkTOTP(ctx context.Context, tx pgx.Tx, a adminRow, code string, now time.Time) (bool, error) {
	secret, err := s.secret(a)
	if err != nil {
		return false, err
	}
	step, ok := MatchTOTP(secret, code, now)
	if ok {
		if ok, err = useStep(ctx, tx, a.ID, step, now); err != nil {
			return false, err
		}
	}
	if !ok {
		return false, recordFailure(ctx, tx, a.ID, now)
	}
	return true, nil
}

// confirmTOTP 用动态码确认绑定，生成并保存新的恢复码。bizErr 非空时调用方应提交事务（保留失败计数）后返回它。
func (s *Service) confirmTOTP(ctx context.Context, tx pgx.Tx, a adminRow, code string, now time.Time) (codes []string, bizErr, err error) {
	ok, err := s.checkTOTP(ctx, tx, a, code, now)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, ErrInvalidTOTP, nil
	}
	codes = NewRecoveryCodes()
	if _, err := tx.Exec(ctx, `DELETE FROM admin_recovery_codes WHERE admin_id = $1`, a.ID); err != nil {
		return nil, nil, err
	}
	for _, c := range codes {
		if _, err := tx.Exec(ctx, `INSERT INTO admin_recovery_codes (admin_id, code_hash) VALUES ($1, $2)`, a.ID, hashRecoveryCode(c)); err != nil {
			return nil, nil, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE admins SET totp_confirmed = true, updated_at = $2 WHERE id = $1`, a.ID, now); err != nil {
		return nil, nil, err
	}
	return codes, nil, nil
}
