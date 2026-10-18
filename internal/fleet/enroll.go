package fleet

import (
	"context"
	"crypto/sha256"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/randx"
)

const (
	EnrollTokenPrefix = "spk1_"
	EnrollTokenTTL    = 24 * time.Hour
)

// ErrEnrollToken 覆盖令牌不存在、已用、已过期、被新令牌取代、格式不对的全部情况（统一文案，不泄露原因）。
var ErrEnrollToken = apperr.New("invalid_token", "令牌无效", 400)

type EnrollToken struct {
	ID        int64
	Plain     string // 只在签发时返回一次
	ExpiresAt time.Time
}

type EnrollTokenRow struct {
	ID        int64
	MachineID int64
	IssuedBy  int64
	Serial    string
}

func hashEnrollToken(plain string) []byte {
	h := sha256.Sum256([]byte(plain))
	return h[:]
}

// IssueEnrollToken 为机器签发一次性接入令牌；同机旧的未用令牌立即失效。审计只记录令牌记录 id。
func (s *Service) IssueEnrollToken(ctx context.Context, actor audit.Actor, machineID int64) (EnrollToken, error) {
	now := s.clock.Now()
	plain := EnrollTokenPrefix + randx.Token()
	var out EnrollToken
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := s.Get(ctx, tx, machineID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE machine_enroll_tokens SET expires_at = $2
			WHERE machine_id = $1 AND used_at IS NULL AND expires_at > $2`, machineID, now); err != nil {
			return err
		}
		exp := now.Add(EnrollTokenTTL)
		var id int64
		if err := tx.QueryRow(ctx, `INSERT INTO machine_enroll_tokens (machine_id, token_hash, expires_at, issued_by, created_at)
			VALUES ($1, $2, $3, $4, $5) RETURNING id`, machineID, hashEnrollToken(plain), exp, actor.AdminID, now).Scan(&id); err != nil {
			return err
		}
		out = EnrollToken{ID: id, Plain: plain, ExpiresAt: exp}
		return audit.Record(ctx, tx, now, actor.Entry("machine.enroll_token", "machine", strconv.FormatInt(machineID, 10),
			map[string]any{"token_id": id, "expires_at": exp}))
	})
	return out, err
}

// ConsumeEnrollToken 在调用方事务内校验并标记令牌已用。首尾空白被忽略。
func (s *Service) ConsumeEnrollToken(ctx context.Context, tx pgx.Tx, plain string) (EnrollTokenRow, error) {
	plain = strings.TrimSpace(plain)
	if !strings.HasPrefix(plain, EnrollTokenPrefix) {
		return EnrollTokenRow{}, ErrEnrollToken
	}
	now := s.clock.Now()
	var r EnrollTokenRow
	err := tx.QueryRow(ctx, `SELECT t.id, t.machine_id, t.issued_by, m.serial
		FROM machine_enroll_tokens t JOIN machines m ON m.id = t.machine_id
		WHERE t.token_hash = $1 AND t.used_at IS NULL AND t.expires_at > $2 FOR UPDATE OF t`,
		hashEnrollToken(plain), now).Scan(&r.ID, &r.MachineID, &r.IssuedBy, &r.Serial)
	if db.IsNoRows(err) {
		return EnrollTokenRow{}, ErrEnrollToken
	}
	if err != nil {
		return EnrollTokenRow{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE machine_enroll_tokens SET used_at = $2 WHERE id = $1`, r.ID, now)
	return r, err
}

// SetCertFingerprint 写入新证书指纹并清空连接归属（旧 Agent 的流在下一个 tick 被断开）。
func (s *Service) SetCertFingerprint(ctx context.Context, q db.Querier, machineID int64, fp string) error {
	_, err := q.Exec(ctx, `UPDATE machines SET cert_fingerprint = $2, agent_conn_id = NULL, updated_at = $3 WHERE id = $1`,
		machineID, fp, s.clock.Now())
	return err
}

// RevokeEnrollment 撤销接入：清空指纹与连接归属，写审计。未接入的机器返回 invalid_state。
func (s *Service) RevokeEnrollment(ctx context.Context, actor audit.Actor, machineID int64) error {
	now := s.clock.Now()
	return db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var fp string
		err := tx.QueryRow(ctx, `SELECT cert_fingerprint FROM machines WHERE id = $1 FOR UPDATE`, machineID).Scan(&fp)
		if db.IsNoRows(err) {
			return apperr.ErrNotFound
		}
		if err != nil {
			return err
		}
		if fp == "" {
			return apperr.ErrInvalidState
		}
		if err := s.SetCertFingerprint(ctx, tx, machineID, ""); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("machine.revoke_enrollment", "machine", strconv.FormatInt(machineID, 10),
			map[string]any{"fingerprint": fp}))
	})
}
