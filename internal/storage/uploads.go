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

	maxUploadsPerDay = 20 // 每个用户每 24 小时最多上传次数，防止占满磁盘
)

var (
	ErrFileTooLarge    = apperr.New("file_too_large", "文件不能超过 10MB", 413)
	ErrUnsupportedType = apperr.New("unsupported_file_type", "仅支持 JPG、PNG、PDF 文件", 415)
	errInvalidUpload   = apperr.BadRequest("营业执照文件无效，请重新上传")
)

// extByType 是允许的文件类型（按文件内容判断，不看扩展名）。
var extByType = map[string]string{"image/jpeg": "jpg", "image/png": "png", "application/pdf": "pdf"}

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

// Save 读取文件内容（最多 10MB），按内容判断类型后保存，并登记归属。
func (s *Service) Save(ctx context.Context, userID int64, purpose string, r io.Reader) (Upload, error) {
	if purpose != PurposeLicense {
		return Upload{}, apperr.BadRequest("不支持的上传用途")
	}
	now := s.clock.Now()
	data, err := io.ReadAll(io.LimitReader(r, MaxUploadSize+1))
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) || len(data) > MaxUploadSize {
		return Upload{}, ErrFileTooLarge
	}
	if err != nil {
		return Upload{}, apperr.BadRequest("读取上传文件失败")
	}
	if len(data) == 0 {
		return Upload{}, apperr.BadRequest("文件为空")
	}
	contentType := http.DetectContentType(data)
	ext, ok := extByType[contentType]
	if !ok {
		return Upload{}, ErrUnsupportedType
	}
	up := Upload{
		Key:    fmt.Sprintf("%04d/%02d/%s.%s", now.Year(), int(now.Month()), randx.ID("", 32), ext),
		UserID: userID, Size: int64(len(data)), ContentType: contentType, Purpose: purpose, CreatedAt: now,
	}
	stored := false
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
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
		if _, err := s.store.Put(ctx, up.Key, bytes.NewReader(data)); err != nil {
			return fmt.Errorf("store upload: %w", err)
		}
		stored = true
		_, err := tx.Exec(ctx, `INSERT INTO uploads (key, user_id, size, content_type, purpose, created_at)
			VALUES ($1, $2, $3, $4, $5, $6)`, up.Key, userID, up.Size, contentType, purpose, now)
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
	err := s.pool.QueryRow(ctx, `SELECT key, user_id, size, content_type, purpose, created_at FROM uploads WHERE key = $1`, key).
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
