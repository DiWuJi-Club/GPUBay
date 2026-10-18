package identity

import (
	"context"
	"crypto/rsa"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

// MaxSSHKeys 每个用户最多保存的公钥数。
const MaxSSHKeys = 10

type SSHKey struct {
	ID          int64      `json:"id"`
	UserID      int64      `json:"-"`
	Name        string     `json:"name"`
	PublicKey   string     `json:"-"` // "<type> <base64>"，无注释
	Fingerprint string     `json:"fingerprint"`
	LastUsedAt  *time.Time `json:"last_used_at"`
	CreatedAt   time.Time  `json:"created_at"`
}

var allowedKeyTypes = map[string]bool{
	ssh.KeyAlgoED25519: true, ssh.KeyAlgoECDSA256: true, ssh.KeyAlgoECDSA384: true, ssh.KeyAlgoECDSA521: true, ssh.KeyAlgoRSA: true,
}

// ParseSSHPublicKey 解析一行 authorized_keys 格式的公钥：去首尾空白与 CRLF，拒绝多行；只接受 ed25519 / ecdsa / rsa（≥ 2048 位）；
// 返回归一化的 "<type> <base64>"、SHA256 指纹与注释（可能为空）。
func ParseSSHPublicKey(raw string) (normalized, fingerprint, comment string, err error) {
	line := strings.TrimSpace(raw)
	if line == "" || strings.ContainsAny(line, "\r\n") {
		return "", "", "", apperr.ErrSSHKeyInvalid
	}
	pub, comment, _, _, perr := ssh.ParseAuthorizedKey([]byte(line))
	if perr != nil || !allowedKeyTypes[pub.Type()] {
		return "", "", "", apperr.ErrSSHKeyInvalid
	}
	if pub.Type() == ssh.KeyAlgoRSA {
		ck, ok := pub.(ssh.CryptoPublicKey)
		if !ok {
			return "", "", "", apperr.ErrSSHKeyInvalid
		}
		rk, ok := ck.CryptoPublicKey().(*rsa.PublicKey)
		if !ok || rk.N.BitLen() < 2048 {
			return "", "", "", apperr.ErrSSHKeyInvalid
		}
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))), ssh.FingerprintSHA256(pub), strings.TrimSpace(comment), nil
}

const sshKeyCols = `id, user_id, name, public_key, fingerprint, last_used_at, created_at`

func scanSSHKey(row pgx.Row) (SSHKey, error) {
	var k SSHKey
	err := row.Scan(&k.ID, &k.UserID, &k.Name, &k.PublicKey, &k.Fingerprint, &k.LastUsedAt, &k.CreatedAt)
	if db.IsNoRows(err) {
		return SSHKey{}, apperr.ErrNotFound
	}
	return k, err
}

// AddSSHKey 为用户添加公钥；name 为空取注释，仍为空取「密钥 N」；同指纹重复 → ErrSSHKeyExists；超过 MaxSSHKeys → ErrSSHKeyLimit。
func (s *Service) AddSSHKey(ctx context.Context, userID int64, name, raw, ip string) (SSHKey, error) {
	normalized, fp, comment, err := ParseSSHPublicKey(raw)
	if err != nil {
		return SSHKey{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = comment
	}
	if utf8.RuneCountInString(name) > 64 {
		return SSHKey{}, apperr.BadRequest("名称不能超过 64 个字符")
	}
	now := s.d.Clock.Now()
	var key SSHKey
	err = db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		// 锁用户行：并发添加不会越过上限
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ssh_keys WHERE user_id = (SELECT id FROM users WHERE id = $1 FOR UPDATE)`, userID).Scan(&n); err != nil {
			return err
		}
		if n >= MaxSSHKeys {
			return apperr.ErrSSHKeyLimit
		}
		if name == "" {
			name = fmt.Sprintf("密钥 %d", n+1)
		}
		k, err := scanSSHKey(tx.QueryRow(ctx, `INSERT INTO ssh_keys (user_id, name, public_key, fingerprint, created_at)
			VALUES ($1, $2, $3, $4, $5) RETURNING `+sshKeyCols, userID, name, normalized, fp, now))
		if db.IsUniqueViolation(err) {
			return apperr.ErrSSHKeyExists
		}
		if err != nil {
			return err
		}
		key = k
		return audit.Record(ctx, tx, now, audit.Actor{UserID: userID, IP: ip}.Entry("ssh_key.add", "ssh_key", fmt.Sprint(k.ID),
			map[string]any{"name": name, "fingerprint": fp}))
	})
	return key, err
}

func (s *Service) ListSSHKeys(ctx context.Context, userID int64) ([]SSHKey, error) {
	rows, err := s.d.Pool.Query(ctx, `SELECT `+sshKeyCols+` FROM ssh_keys WHERE user_id = $1 ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (SSHKey, error) { return scanSSHKey(r) })
}

// DeleteSSHKey 删除用户自己的公钥；不属于该用户的 ID 返回 ErrNotFound。已建立的会话不受影响。
func (s *Service) DeleteSSHKey(ctx context.Context, userID, id int64, ip string) error {
	now := s.d.Clock.Now()
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		k, err := scanSSHKey(tx.QueryRow(ctx, `DELETE FROM ssh_keys WHERE id = $1 AND user_id = $2 RETURNING `+sshKeyCols, id, userID))
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, audit.Actor{UserID: userID, IP: ip}.Entry("ssh_key.delete", "ssh_key", fmt.Sprint(k.ID),
			map[string]any{"name": k.Name, "fingerprint": k.Fingerprint}))
	})
}

// FindSSHKey 按用户与指纹查找（SSH 网关公钥认证用）。
func (s *Service) FindSSHKey(ctx context.Context, userID int64, fingerprint string) (SSHKey, error) {
	return scanSSHKey(s.d.Pool.QueryRow(ctx, `SELECT `+sshKeyCols+` FROM ssh_keys WHERE user_id = $1 AND fingerprint = $2`, userID, fingerprint))
}

// TouchSSHKey 记录最近使用时间。
func (s *Service) TouchSSHKey(ctx context.Context, id int64) error {
	_, err := s.d.Pool.Exec(ctx, `UPDATE ssh_keys SET last_used_at = $2 WHERE id = $1`, id, s.d.Clock.Now())
	return err
}

func (s *Service) CountSSHKeys(ctx context.Context, userID int64) (int, error) {
	var n int
	err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM ssh_keys WHERE user_id = $1`, userID).Scan(&n)
	return n, err
}
