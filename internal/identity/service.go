// Package identity 管理用户、短信登录、会话与实名认证。
package identity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/secretbox"
	"spark-cloud/internal/plugins/kyc"
	"spark-cloud/internal/plugins/sms"
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
	Pool      *pgxpool.Pool
	Clock     clock.Clock
	SMS       sms.Sender
	KYC       kyc.Verifier
	Box       *secretbox.Box
	Uploads   UploadChecker // 校验营业执照文件归属（storage 模块实现）
	FixedCode string        // 仅开发环境：非空时验证码固定为该值
}

type Service struct {
	d Deps
}

func NewService(d Deps) *Service { return &Service{d: d} }

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
