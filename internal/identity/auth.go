package identity

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/randx"
	"spark-cloud/internal/plugins/sms"
	"spark-cloud/internal/referral"
)

const (
	codeTTL         = 5 * time.Minute
	codeInterval    = 60 * time.Second
	codeDailyLimit  = 10
	codeIPHourLimit = 30
	codeMaxAttempts = 5
	sessionTTL      = 30 * 24 * time.Hour
)

var phoneRe = regexp.MustCompile(`^1[3-9]\d{9}$`)

func (s *Service) SendLoginCode(ctx context.Context, phone, ip string) error {
	if !phoneRe.MatchString(phone) {
		return apperr.BadRequest("手机号格式不正确")
	}
	now := s.d.Clock.Now()
	ipLimit := s.d.CodeIPHourLimit
	if ipLimit <= 0 {
		ipLimit = codeIPHourLimit
	}
	code := s.d.FixedCode
	if code == "" {
		code = randx.Digits(6)
	}
	// 计数与插入在同一事务内完成，并用事务级咨询锁串行化同一手机号与同一 IP 的发送，
	// 避免并发请求都读到旧计数而绕过限流。锁的获取顺序固定（先手机号后 IP）以防死锁。
	var codeID int64
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "sms:phone:"+phone); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "sms:ip:"+ip); err != nil {
			return err
		}
		var recent, daily, ipHour int
		if err := tx.QueryRow(ctx, `SELECT
				count(*) FILTER (WHERE phone = $1 AND created_at > $2),
				count(*) FILTER (WHERE phone = $1),
				count(*) FILTER (WHERE ip = $4 AND created_at > $5)
			FROM sms_codes WHERE created_at > $3`,
			phone, now.Add(-codeInterval), now.Add(-24*time.Hour), ip, now.Add(-time.Hour)).
			Scan(&recent, &daily, &ipHour); err != nil {
			return err
		}
		if recent > 0 || daily >= codeDailyLimit || ipHour >= ipLimit {
			return apperr.ErrRateLimited
		}
		return tx.QueryRow(ctx, `INSERT INTO sms_codes (phone, code_hash, ip, created_at, expires_at)
			VALUES ($1, $2, $3, $4, $5) RETURNING id`, phone, hashSecret(phone+":"+code), ip, now, now.Add(codeTTL)).Scan(&codeID)
	})
	if err != nil {
		return err
	}
	// 事务提交后再发短信，回滚的请求不会发出验证码。
	if err := s.d.SMS.Send(ctx, phone, sms.TemplateLoginCode, map[string]string{"code": code}); err != nil {
		// 发送失败则删除记录（尽力而为），用户不会因为没收到的验证码被限流。
		if _, derr := s.d.Pool.Exec(context.WithoutCancel(ctx), `DELETE FROM sms_codes WHERE id = $1`, codeID); derr != nil {
			return errors.Join(err, fmt.Errorf("delete unsent sms code: %w", derr))
		}
		return err
	}
	return nil
}

type LoginRequest struct {
	Phone, Code string
	InviteCode  string // 计划 8：仅新用户生效
	IP          string
	DeviceID    string // 请求头 X-Device-Id；非 UUID 形态视同未提供（不记设备、不转给 Bind）
	UserAgent   string
}

// LoginResult 是登录响应：用户字段平铺，invite 仅在本次带了 invite_code 时出现。
type LoginResult struct {
	User
	IsNew  bool                 `json:"-"`
	Invite *referral.BindResult `json:"invite,omitempty"`
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Login 校验验证码，不存在则创建用户，返回新的会话 token（计划 8 之前的签名，等价于不带邀请码 / 设备的 LoginWith）。
func (s *Service) Login(ctx context.Context, phone, code string) (string, User, error) {
	token, r, err := s.LoginWith(ctx, LoginRequest{Phone: phone, Code: code})
	return token, r.User, err
}

// LoginWith 校验验证码；首次登录即注册（is_new 由 INSERT … ON CONFLICT 的 xmax = 0 判断），新用户带邀请码时在同一事务里绑定。
// Bind 的业务失败（码无效 / 码主冻结）只体现在响应里，不影响注册；数据库错误回滚整个登录。
func (s *Service) LoginWith(ctx context.Context, req LoginRequest) (string, LoginResult, error) {
	now := s.d.Clock.Now()
	deviceID := normalizeDeviceID(req.DeviceID)
	var (
		bizErr error // 需要提交事务（记录试错次数）后再返回的业务错误
		res    LoginResult
		token  string
	)
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		var id int64
		var hash string
		var attempts int
		err := tx.QueryRow(ctx, `SELECT id, code_hash, attempts FROM sms_codes
			WHERE phone = $1 AND used_at IS NULL AND expires_at > $2
			ORDER BY id DESC LIMIT 1 FOR UPDATE`, req.Phone, now).Scan(&id, &hash, &attempts)
		if db.IsNoRows(err) {
			bizErr = apperr.ErrInvalidCode
			return nil
		}
		if err != nil {
			return err
		}
		if attempts >= codeMaxAttempts {
			bizErr = apperr.ErrInvalidCode
			return nil
		}
		if subtle.ConstantTimeCompare([]byte(hash), []byte(hashSecret(req.Phone+":"+req.Code))) != 1 {
			bizErr = apperr.ErrInvalidCode
			_, err := tx.Exec(ctx, `UPDATE sms_codes SET attempts = attempts + 1 WHERE id = $1`, id)
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE sms_codes SET used_at = $2 WHERE id = $1`, id, now); err != nil {
			return err
		}
		// is_new：INSERT 新写入的行版本 xmax = 0；走 ON CONFLICT DO UPDATE 时新版本的 xmax 是本事务的 ID（行被锁过），
		// 因此 xmax = 0 当且仅当本次是插入（PostgreSQL 的实现细节，是 upsert 判断新旧的常用做法）。
		// 封禁用户登录会被拒绝，不更新其 last_login_*（新插入的行不可能是封禁状态）。
		row := tx.QueryRow(ctx, `INSERT INTO users (phone, created_at, updated_at, last_login_ip, last_login_at) VALUES ($1, $2, $2, $3, $2)
			ON CONFLICT (phone) DO UPDATE SET updated_at = EXCLUDED.updated_at,
				last_login_ip = CASE WHEN users.status = 'banned' THEN users.last_login_ip ELSE EXCLUDED.last_login_ip END,
				last_login_at = CASE WHEN users.status = 'banned' THEN users.last_login_at ELSE EXCLUDED.last_login_at END
			RETURNING `+userCols+`, (xmax = 0) AS is_new`, req.Phone, now, req.IP)
		var u User
		if err := row.Scan(&u.ID, &u.Phone, &u.Kind, &u.KYCStatus, &u.Status, &u.CreatedAt, &res.IsNew); err != nil {
			return err
		}
		res.User = u
		if u.Status == "banned" {
			bizErr = apperr.ErrBanned
			return nil
		}
		if deviceID != "" {
			if err := s.touchDevice(ctx, tx, u.ID, deviceID, now); err != nil {
				return err
			}
		}
		if req.InviteCode != "" {
			switch {
			case !res.IsNew:
				res.Invite = &referral.BindResult{Bound: false, Reason: referral.ReasonExistingUser}
			case s.d.Referral == nil:
				res.Invite = &referral.BindResult{Bound: false, Reason: referral.ReasonInvalidCode}
			default:
				// UA 原样传入，由 Bind 负责清洗（非法 UTF-8、按字符截断）。
				r, err := s.d.Referral.Bind(ctx, tx, referral.BindRequest{Code: req.InviteCode, InviteeID: u.ID, IP: req.IP, DeviceID: deviceID, UserAgent: req.UserAgent, Now: now})
				if err != nil {
					return err
				}
				res.Invite = &r
			}
		}
		token = randx.Token()
		_, err = tx.Exec(ctx, `INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES ($1, $2, $3, $4)`,
			hashSecret(token), u.ID, now, now.Add(sessionTTL))
		return err
	})
	if err != nil {
		return "", LoginResult{}, err
	}
	if bizErr != nil {
		return "", LoginResult{}, bizErr
	}
	return token, res, nil
}

// normalizeDeviceID 实现裁定 Q15：只有 UUID 形态才算设备 ID（按小写存），其余视同未提供（返回 ""）。
func normalizeDeviceID(raw string) string {
	if uuidRe.MatchString(raw) {
		return strings.ToLower(raw)
	}
	return ""
}

// touchSessionEvery：会话启动时补记设备 / 最近登录的最小间隔（同 IP、同设备一小时内最多写一次）。
const touchSessionEvery = time.Hour

// TouchSession 在控制台会话启动（GET /me）时补记设备与最近登录（计划 8 终审 I1）：只在登录时记录的话，
// 上线前就已登录（会话 30 天不滑动）或清过本地存储的邀请人没有设备 / 最近登录，同设备、同 IP 判定会漏。
//   - 设备 ID 规则与 10 台上限同登录（normalizeDeviceID / touchDevice）；
//   - last_login_ip / last_login_at：IP 变了立即刷新，否则一小时内不重复写；空 IP 不覆盖；
//   - 两者都新鲜时只有一次只读查询；需要写时先锁 users 行（与登录同序：users → user_devices），封禁用户不记。
//
// 调用方（/me）只记日志，错误不影响响应。
func (s *Service) TouchSession(ctx context.Context, userID int64, ip, rawDeviceID string) error {
	now := s.d.Clock.Now()
	cutoff := now.Add(-touchSessionEvery)
	deviceID := normalizeDeviceID(rawDeviceID)
	// fresh 判定：IP 为空视为无需刷新；设备为空视为无需记录。
	const freshSQL = `SELECT u.status,
		($2::text = '' OR (u.last_login_ip = $2::text AND COALESCE(u.last_login_at > $3::timestamptz, false))),
		($4::text = '' OR EXISTS (SELECT 1 FROM user_devices d WHERE d.user_id = u.id AND d.device_id = $4::text AND d.last_seen_at > $3::timestamptz))
		FROM users u WHERE u.id = $1`
	var status string
	var loginFresh, deviceFresh bool
	err := s.d.Pool.QueryRow(ctx, freshSQL, userID, ip, cutoff, deviceID).Scan(&status, &loginFresh, &deviceFresh)
	if db.IsNoRows(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if status == "banned" || (loginFresh && deviceFresh) {
		return nil
	}
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		// 锁 users 行后复查（并发的 /me 或登录已写过就不再写），也让 touchDevice 的上限删除与登录串行。
		err := tx.QueryRow(ctx, freshSQL+` FOR NO KEY UPDATE OF u`, userID, ip, cutoff, deviceID).
			Scan(&status, &loginFresh, &deviceFresh)
		if db.IsNoRows(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if status == "banned" {
			return nil
		}
		if !loginFresh {
			if _, err := tx.Exec(ctx, `UPDATE users SET last_login_ip = $2, last_login_at = $3 WHERE id = $1`, userID, ip, now); err != nil {
				return err
			}
		}
		if !deviceFresh {
			return s.touchDevice(ctx, tx, userID, deviceID, now)
		}
		return nil
	})
}

// touchDevice 记录设备；每用户最多 MaxDevicesPerUser 台，超出删最旧。
func (s *Service) touchDevice(ctx context.Context, tx pgx.Tx, userID int64, deviceID string, now time.Time) error {
	if _, err := tx.Exec(ctx, `INSERT INTO user_devices (user_id, device_id, first_seen_at, last_seen_at) VALUES ($1, $2, $3, $3)
		ON CONFLICT (user_id, device_id) DO UPDATE SET last_seen_at = EXCLUDED.last_seen_at`, userID, deviceID, now); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `DELETE FROM user_devices WHERE user_id = $1 AND device_id NOT IN
		(SELECT device_id FROM user_devices WHERE user_id = $1 ORDER BY last_seen_at DESC, device_id LIMIT $2)`, userID, referral.MaxDevicesPerUser)
	return err
}

func (s *Service) Authenticate(ctx context.Context, token string) (User, error) {
	if token == "" {
		return User{}, apperr.ErrUnauthorized
	}
	u, err := scanUser(s.d.Pool.QueryRow(ctx, `SELECT u.id, u.phone, u.kind, u.kyc_status, u.status, u.created_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.expires_at > $2`, hashSecret(token), s.d.Clock.Now()))
	if errors.Is(err, apperr.ErrNotFound) {
		return User{}, apperr.ErrUnauthorized
	}
	if err != nil {
		return User{}, err
	}
	if u.Status == "banned" {
		return User{}, apperr.ErrBanned
	}
	return u, nil
}

func (s *Service) Logout(ctx context.Context, token string) error {
	_, err := s.d.Pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, hashSecret(token))
	return err
}

// SessionHash 返回会话 token 的存储哈希（与 sessions.token_hash 一致），供 Web 入口把 Cookie 绑定到控制台会话。
func SessionHash(token string) string { return hashSecret(token) }

// SessionUserID 按哈希校验会话仍有效并返回用户 ID；不存在 / 过期 → ErrUnauthorized，用户封禁 → ErrBanned。
func (s *Service) SessionUserID(ctx context.Context, hash string) (int64, error) {
	var userID int64
	var status string
	err := s.d.Pool.QueryRow(ctx, `SELECT u.id, u.status FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.expires_at > $2`, hash, s.d.Clock.Now()).Scan(&userID, &status)
	if db.IsNoRows(err) {
		return 0, apperr.ErrUnauthorized
	}
	if err != nil {
		return 0, err
	}
	if status == "banned" {
		return 0, apperr.ErrBanned
	}
	return userID, nil
}
