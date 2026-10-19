package identity

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/randx"
	"spark-cloud/internal/plugins/sms"
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
		if recent > 0 || daily >= codeDailyLimit || ipHour >= codeIPHourLimit {
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

// Login 校验验证码，不存在则创建用户，返回新的会话 token。
func (s *Service) Login(ctx context.Context, phone, code string) (string, User, error) {
	now := s.d.Clock.Now()
	var (
		bizErr error // 需要提交事务（记录试错次数）后再返回的业务错误
		user   User
		token  string
	)
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		var id int64
		var hash string
		var attempts int
		err := tx.QueryRow(ctx, `SELECT id, code_hash, attempts FROM sms_codes
			WHERE phone = $1 AND used_at IS NULL AND expires_at > $2
			ORDER BY id DESC LIMIT 1 FOR UPDATE`, phone, now).Scan(&id, &hash, &attempts)
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
		if subtle.ConstantTimeCompare([]byte(hash), []byte(hashSecret(phone+":"+code))) != 1 {
			bizErr = apperr.ErrInvalidCode
			_, err := tx.Exec(ctx, `UPDATE sms_codes SET attempts = attempts + 1 WHERE id = $1`, id)
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE sms_codes SET used_at = $2 WHERE id = $1`, id, now); err != nil {
			return err
		}
		user, err = scanUser(tx.QueryRow(ctx, `INSERT INTO users (phone, created_at, updated_at) VALUES ($1, $2, $2)
			ON CONFLICT (phone) DO UPDATE SET updated_at = EXCLUDED.updated_at
			RETURNING `+userCols, phone, now))
		if err != nil {
			return err
		}
		if user.Status == "banned" {
			bizErr = apperr.ErrBanned
			return nil
		}
		token = randx.Token()
		_, err = tx.Exec(ctx, `INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES ($1, $2, $3, $4)`,
			hashSecret(token), user.ID, now, now.Add(sessionTTL))
		return err
	})
	if err != nil {
		return "", User{}, err
	}
	if bizErr != nil {
		return "", User{}, bizErr
	}
	return token, user, nil
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
