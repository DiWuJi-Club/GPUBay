// Package identity 管理用户、短信登录、会话与实名认证。
package identity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/secretbox"
	"spark-cloud/internal/plugins/kyc"
	"spark-cloud/internal/plugins/sms"
	"spark-cloud/internal/referral"
)

type User struct {
	ID        int64     `json:"id"`
	Phone     string    `json:"phone"`
	Kind      string    `json:"kind"`
	KYCStatus string    `json:"kyc_status"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

// UploadChecker 校验上传文件确实属于该用户且用途正确。
type UploadChecker interface {
	CheckOwned(ctx context.Context, key string, userID int64, purpose string) error
}

type Deps struct {
	// CodeIPHourLimit 同一 IP 每小时验证码上限；零值表示默认 30。只有端到端测试服务会调大它。
	CodeIPHourLimit int
	Pool            *pgxpool.Pool
	Clock           clock.Clock
	SMS             sms.Sender
	KYC             kyc.Verifier
	Box             *secretbox.Box
	Uploads         UploadChecker // 校验营业执照文件归属（storage 模块实现）
	FixedCode       string        // 仅开发环境：非空时验证码固定为该值
	Referral        ReferralHook  // 计划 8；装配时 referral 依赖 identity，故用 SetReferral 后置注入
	FingerprintKey  []byte        // 计划 8：实名指纹 HMAC 密钥（referral.DeriveFingerprintKey）；空时不写指纹
	Log             *slog.Logger  // nil 时用 slog.Default()
}

// ReferralHook 由 referral 模块实现（计划 8）：注册时绑定邀请关系、实名通过时同身份判定。nil 时两者都跳过。
type ReferralHook interface {
	Bind(ctx context.Context, tx pgx.Tx, req referral.BindRequest) (referral.BindResult, error)
	OnKYCVerified(ctx context.Context, tx pgx.Tx, userID int64, fingerprint string) error
}

type Service struct {
	d Deps
}

func NewService(d Deps) *Service {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	return &Service{d: d}
}

// SetReferral 后置注入邀请返利钩子（referral.Service 的构造需要 identity 先存在）。必须在开始服务前调用。
func (s *Service) SetReferral(h ReferralHook) { s.d.Referral = h }

const userCols = `id, phone, kind, kyc_status, status, created_at`

func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Phone, &u.Kind, &u.KYCStatus, &u.Status, &u.CreatedAt)
	if db.IsNoRows(err) {
		return User{}, apperr.ErrNotFound
	}
	return u, err
}

func hashSecret(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func (s *Service) GetUser(ctx context.Context, id int64) (User, error) {
	return scanUser(s.d.Pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id = $1`, id))
}

// CanRent 供 rental 模块在下单前校验：账号正常且已实名。
func (s *Service) CanRent(ctx context.Context, userID int64) error {
	u, err := s.GetUser(ctx, userID)
	if err != nil {
		return err
	}
	if u.Status == "banned" {
		return apperr.ErrBanned
	}
	if u.KYCStatus != "verified" {
		return apperr.ErrKYCRequired
	}
	return nil
}
