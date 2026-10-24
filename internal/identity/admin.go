package identity

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/paging"
)

type UserFilter struct {
	Q         string // 完整手机号、手机尾号或用户 ID
	KYCStatus string
	Status    string
	// IncludeSystem 为 false（默认）时不列出平台系统账号（users.system，如 Token 商城的 system:llm，计划 9 规格 §2）
	IncludeSystem bool
}

var (
	kycStatuses  = []string{"none", "pending", "verified", "rejected"}
	userStatuses = []string{"active", "banned"}
)

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func (s *Service) AdminListUsers(ctx context.Context, f UserFilter, p paging.Page) ([]User, int, error) {
	var w db.Where
	if !f.IncludeSystem {
		w.Add("NOT system")
	}
	switch q := strings.TrimSpace(f.Q); {
	case q == "":
	case isDigits(q) && len(q) == 11: // 完整手机号
		w.Add("phone = ?", q)
	case isDigits(q): // 用户 ID 或手机尾号
		w.Add("(id::text = ? OR phone LIKE ?)", q, "%"+q)
	default:
		return nil, 0, apperr.BadRequest("q 只能是手机号、手机尾号或用户 ID")
	}
	if f.KYCStatus != "" {
		if !slices.Contains(kycStatuses, f.KYCStatus) {
			return nil, 0, apperr.BadRequest("实名状态不正确")
		}
		w.Add("kyc_status = ?", f.KYCStatus)
	}
	if f.Status != "" {
		if !slices.Contains(userStatuses, f.Status) {
			return nil, 0, apperr.BadRequest("账号状态不正确")
		}
		w.Add("status = ?", f.Status)
	}
	var total int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE `+w.SQL(), w.Args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	n := len(w.Args)
	rows, err := s.d.Pool.Query(ctx, `SELECT `+userCols+` FROM users WHERE `+w.SQL()+
		fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT $%d OFFSET $%d`, n+1, n+2), append(w.Args, p.Limit(), p.Offset())...)
	if err != nil {
		return nil, 0, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (User, error) { return scanUser(r) })
	return list, total, err
}

type KYCRecord struct {
	ID             int64     `json:"id"`
	UserID         int64     `json:"user_id"`
	Kind           string    `json:"kind"`
	RealName       string    `json:"real_name"`
	IDNumberMasked string    `json:"id_number_masked,omitempty"`
	LicenseNo      string    `json:"license_no,omitempty"`
	LicenseFile    string    `json:"license_file,omitempty"`
	Result         string    `json:"result"`
	Reason         string    `json:"reason"`
	ReviewerID     *int64    `json:"reviewer_id"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

const kycCols = `id, user_id, kind, real_name, id_number_enc, license_no, license_file, result, reason, reviewer_id, created_at, updated_at`

// maskIDNumber 只保留身份证号前 3 位与后 4 位。
func maskIDNumber(id string) string {
	if len(id) <= 7 {
		return strings.Repeat("*", len(id))
	}
	return id[:3] + strings.Repeat("*", len(id)-7) + id[len(id)-4:]
}

func (s *Service) scanKYC(row pgx.Row) (KYCRecord, error) {
	var k KYCRecord
	var enc []byte
	err := row.Scan(&k.ID, &k.UserID, &k.Kind, &k.RealName, &enc, &k.LicenseNo, &k.LicenseFile, &k.Result, &k.Reason,
		&k.ReviewerID, &k.CreatedAt, &k.UpdatedAt)
	if db.IsNoRows(err) {
		return KYCRecord{}, apperr.ErrNotFound
	}
	if err != nil {
		return KYCRecord{}, err
	}
	if enc != nil {
		plain, err := s.d.Box.Open(enc)
		if err != nil {
			return KYCRecord{}, fmt.Errorf("decrypt id number: %w", err)
		}
		k.IDNumberMasked = maskIDNumber(string(plain))
	}
	return k, nil
}

func (s *Service) collectKYC(rows pgx.Rows, err error) ([]KYCRecord, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (KYCRecord, error) { return s.scanKYC(r) })
}

func (s *Service) UserKYCRecords(ctx context.Context, userID int64) ([]KYCRecord, error) {
	return s.collectKYC(s.d.Pool.Query(ctx, `SELECT `+kycCols+` FROM kyc_records WHERE user_id = $1 ORDER BY created_at DESC, id DESC`, userID))
}

// PendingEnterpriseKYC 返回待人工审核的企业认证（先提交的在前）。
func (s *Service) PendingEnterpriseKYC(ctx context.Context, p paging.Page) ([]KYCRecord, int, error) {
	var total int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM kyc_records WHERE kind = 'enterprise' AND result = 'pending'`).Scan(&total); err != nil {
		return nil, 0, err
	}
	list, err := s.collectKYC(s.d.Pool.Query(ctx, `SELECT `+kycCols+` FROM kyc_records WHERE kind = 'enterprise' AND result = 'pending'
		ORDER BY created_at, id LIMIT $1 OFFSET $2`, p.Limit(), p.Offset()))
	return list, total, err
}

func (s *Service) GetKYCRecord(ctx context.Context, id int64) (KYCRecord, error) {
	return s.scanKYC(s.d.Pool.QueryRow(ctx, `SELECT `+kycCols+` FROM kyc_records WHERE id = $1`, id))
}

// CheckNotSystem 后台对用户的资金 / 状态操作前调用：系统账号（users.system）→ apperr.ErrInvalidState；用户不存在 → apperr.ErrNotFound。
func (s *Service) CheckNotSystem(ctx context.Context, userID int64) error {
	var system bool
	err := s.d.Pool.QueryRow(ctx, `SELECT system FROM users WHERE id = $1`, userID).Scan(&system)
	if db.IsNoRows(err) {
		return apperr.ErrNotFound
	}
	if err != nil {
		return err
	}
	if system {
		return apperr.ErrInvalidState
	}
	return nil
}

// Ban 封禁用户并删除其全部会话；运行中的实例不受影响。系统账号 → apperr.ErrInvalidState。
func (s *Service) Ban(ctx context.Context, actor audit.Actor, userID int64, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return apperr.BadRequest("请填写封禁原因")
	}
	return s.setStatus(ctx, actor, userID, "active", "banned", "user.ban", map[string]any{"reason": reason})
}

func (s *Service) Unban(ctx context.Context, actor audit.Actor, userID int64) error {
	return s.setStatus(ctx, actor, userID, "banned", "active", "user.unban", nil)
}

func (s *Service) setStatus(ctx context.Context, actor audit.Actor, userID int64, from, to, action string, diff any) error {
	now := s.d.Clock.Now()
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		var status string
		var system bool
		err := tx.QueryRow(ctx, `SELECT status, system FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&status, &system)
		if db.IsNoRows(err) {
			return apperr.ErrNotFound
		}
		if err != nil {
			return err
		}
		if system || status != from { // 系统账号不能封禁 / 解封（计划 9 规格 §2）
			return apperr.ErrInvalidState
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET status = $2, updated_at = $3 WHERE id = $1`, userID, to, now); err != nil {
			return err
		}
		if to == "banned" {
			if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, userID); err != nil {
				return err
			}
		}
		return audit.Record(ctx, tx, now, actor.Entry(action, "user", strconv.FormatInt(userID, 10), diff))
	})
}
