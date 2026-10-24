package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/randx"
)

const (
	MaxUploadSize  = 10 << 20 // 10MB
	PurposeLicense = "license"
	// PurposeWorkflowCover 工作流封面（计划 11 D13）：后台上传，所有者 userID = 0（uploads.user_id 为 NULL），
	// 只接受 PNG / JPG / WebP，≤ MaxCoverSize，不计入每用户每日上传次数。
	PurposeWorkflowCover = "workflow_cover"
	// MaxCoverSize 与 comfywf.MaxCoverBytes 一致（storage 不能 import comfywf，测试核对两者相等）。
	MaxCoverSize = 2 << 20 // 2MB

	maxUploadsPerDay = 20 // 每个用户每 24 小时最多上传次数，防止占满磁盘
)

var (
	ErrFileTooLarge    = apperr.New("file_too_large", "文件不能超过 10MB", 413)
	ErrUnsupportedType = apperr.New("unsupported_file_type", "仅支持 JPG、PNG、PDF 文件", 415)
	// ErrCoverTooLarge / ErrUnsupportedCoverType 是封面用途的同码错误（文案按封面的限制写）。
	ErrCoverTooLarge        = apperr.New("file_too_large", "封面图片不能超过 2MB", 413)
	ErrUnsupportedCoverType = apperr.New("unsupported_file_type", "封面仅支持 PNG、JPG、WebP 图片", 415)
	errInvalidUpload        = apperr.BadRequest("营业执照文件无效，请重新上传")
)

// purposeRule 是每种用途允许的文件类型（按文件内容判断，不看扩展名）、大小上限与对应错误。
type purposeRule struct {
	extByType   map[string]string
	limit       int
	tooLarge    *apperr.Error
	unsupported *apperr.Error
	anonymous   bool // true = 所有者必须是 0（平台自己的文件），不计每日次数
}

var purposeRules = map[string]purposeRule{
	PurposeLicense: {extByType: map[string]string{"image/jpeg": "jpg", "image/png": "png", "application/pdf": "pdf"},
		limit: MaxUploadSize, tooLarge: ErrFileTooLarge, unsupported: ErrUnsupportedType},
	PurposeWorkflowCover: {extByType: map[string]string{"image/jpeg": "jpg", "image/png": "png", "image/webp": "webp"},
		limit: MaxCoverSize, tooLarge: ErrCoverTooLarge, unsupported: ErrUnsupportedCoverType, anonymous: true},
}

type Upload struct {
	Key         string    `json:"key"`
	UserID      int64     `json:"-"`
	Size        int64     `json:"size"`
	ContentType string    `json:"content_type"`
	Purpose     string    `json:"purpose"`
	CreatedAt   time.Time `json:"created_at"`
}

type Service struct {
	pool  *pgxpool.Pool
	clock clock.Clock
	store Store
}

func NewService(pool *pgxpool.Pool, clk clock.Clock, store Store) *Service {
	return &Service{pool: pool, clock: clk, store: store}
}

// Save 读取文件内容（不超过该用途的上限），按内容判断类型后保存，并登记归属。
// workflow_cover 用途的 userID 必须为 0（登记为 NULL），且不计每用户每日次数；其它用途 userID 必须是真实用户。
func (s *Service) Save(ctx context.Context, userID int64, purpose string, r io.Reader) (Upload, error) {
	rule, ok := purposeRules[purpose]
	if !ok || rule.anonymous != (userID == 0) {
		return Upload{}, apperr.BadRequest("不支持的上传用途")
	}
	now := s.clock.Now()
	data, err := io.ReadAll(io.LimitReader(r, int64(rule.limit)+1))
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) || len(data) > rule.limit {
		return Upload{}, rule.tooLarge
	}
	if err != nil {
		return Upload{}, apperr.BadRequest("读取上传文件失败")
	}
	if len(data) == 0 {
		return Upload{}, apperr.BadRequest("文件为空")
	}
	contentType := http.DetectContentType(data)
	ext, ok := rule.extByType[contentType]
	if !ok {
		return Upload{}, rule.unsupported
	}
	up := Upload{
		Key:    fmt.Sprintf("%04d/%02d/%s.%s", now.Year(), int(now.Month()), randx.ID("", 32), ext),
		UserID: userID, Size: int64(len(data)), ContentType: contentType, Purpose: purpose, CreatedAt: now,
	}
	stored := false
	var owner *int64 // 0 → NULL（uploads.user_id 引用 users）
	if userID != 0 {
		owner = &userID
	}
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		if owner != nil {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "upload:"+strconv.FormatInt(userID, 10)); err != nil {
				return err
			}
			var recent int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM uploads WHERE user_id = $1 AND created_at > $2`,
				userID, now.Add(-24*time.Hour)).Scan(&recent); err != nil {
				return err
			}
			if recent >= maxUploadsPerDay {
				return apperr.ErrRateLimited
			}
		}
		if _, err := s.store.Put(ctx, up.Key, bytes.NewReader(data)); err != nil {
			return fmt.Errorf("store upload: %w", err)
		}
		stored = true
		_, err := tx.Exec(ctx, `INSERT INTO uploads (key, user_id, size, content_type, purpose, created_at)
			VALUES ($1, $2, $3, $4, $5, $6)`, up.Key, owner, up.Size, contentType, purpose, now)
		return err
	})
	if err != nil {
		if stored {
			_ = s.store.Delete(context.WithoutCancel(ctx), up.Key)
		}
		return Upload{}, err
	}
	return up, nil
}

// CheckOwned 校验 key 是该用户上传的、指定用途的文件。
func (s *Service) CheckOwned(ctx context.Context, key string, userID int64, purpose string) error {
	if !ValidKey(key) {
		return errInvalidUpload
	}
	var ok bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM uploads WHERE key = $1 AND user_id = $2 AND purpose = $3)`,
		key, userID, purpose).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return errInvalidUpload
	}
	return nil
}

// Open 返回文件登记信息与内容（后台查看营业执照用），调用方负责关闭。
func (s *Service) Open(ctx context.Context, key string) (Upload, io.ReadCloser, error) {
	if !ValidKey(key) {
		return Upload{}, nil, apperr.ErrNotFound
	}
	var up Upload
	err := s.pool.QueryRow(ctx, `SELECT key, COALESCE(user_id, 0), size, content_type, purpose, created_at FROM uploads WHERE key = $1`, key).
		Scan(&up.Key, &up.UserID, &up.Size, &up.ContentType, &up.Purpose, &up.CreatedAt)
	if db.IsNoRows(err) {
		return Upload{}, nil, apperr.ErrNotFound
	}
	if err != nil {
		return Upload{}, nil, err
	}
	rc, err := s.store.Open(ctx, key)
	if err != nil {
		return Upload{}, nil, err
	}
	return up, rc, nil
}

// Delete 删除文件登记与文件内容（工作流换封面时删旧文件用）；登记不存在视为成功。
// 先删登记再删文件：文件删除失败只会留下无人引用的孤儿文件，不会留下指向不存在文件的登记。
func (s *Service) Delete(ctx context.Context, key string) error {
	if !ValidKey(key) {
		return errBadKey
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM uploads WHERE key = $1`, key); err != nil {
		return err
	}
	return s.store.Delete(ctx, key)
}
