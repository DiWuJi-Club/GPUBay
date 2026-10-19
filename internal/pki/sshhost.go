package pki

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/secretbox"
)

// GenerateInstanceHostKey 生成一把容器 sshd 主机密钥（ed25519）：返回 authorized_keys 一行的公钥与 OpenSSH PEM 私钥。
func GenerateInstanceHostKey() (pubLine string, privPEM []byte, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", nil, err
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return "", nil, err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", nil, err
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))), pem.EncodeToMemory(block), nil
}

// EnsureSSHHostKey 读取或首次生成 SSH 网关的主机密钥（ed25519），secretbox 加密存 platform_pki.ssh_host_key_enc；
// 多副本并发首次启动时靠「只在 NULL 时写入」保证只有一把。platform_pki 行由 EnsureCA 创建，必须先行。
func EnsureSSHHostKey(ctx context.Context, pool *pgxpool.Pool, box *secretbox.Box) (ssh.Signer, error) {
	load := func() (ssh.Signer, bool, error) {
		var enc []byte
		err := pool.QueryRow(ctx, `SELECT ssh_host_key_enc FROM platform_pki WHERE id = 1`).Scan(&enc)
		if db.IsNoRows(err) {
			return nil, false, errors.New("pki: 内置 CA 尚未初始化（platform_pki 为空）")
		}
		if err != nil {
			return nil, false, err
		}
		if enc == nil {
			return nil, false, nil
		}
		raw, err := box.Open(enc)
		if err != nil {
			// 不静默重新生成：网关主机密钥一变，所有用户的 known_hosts 都会报警。由运维二选一
			return nil, false, fmt.Errorf("解密 SSH 网关主机密钥（platform_pki.ssh_host_key_enc）失败，SPARK_SECRET_KEY 是否被更换？"+
				"恢复：换回原来的 SPARK_SECRET_KEY；或确认接受网关主机密钥变更（所有用户会看到主机密钥变更警告，需更新帮助页指纹）后执行 "+
				"`UPDATE platform_pki SET ssh_host_key_enc = NULL WHERE id = 1;` 再启动以重新生成: %w", err)
		}
		s, err := ssh.ParsePrivateKey(raw)
		return s, true, err
	}
	if s, ok, err := load(); err != nil || ok {
		return s, err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(priv, "spark-cloud ssh gateway")
	if err != nil {
		return nil, err
	}
	if _, err := pool.Exec(ctx, `UPDATE platform_pki SET ssh_host_key_enc = $1 WHERE id = 1 AND ssh_host_key_enc IS NULL`, box.Seal(pem.EncodeToMemory(block))); err != nil {
		return nil, err
	}
	s, ok, err := load()
	if err == nil && !ok {
		return nil, errors.New("pki: ssh host key not persisted")
	}
	return s, err
}

// SSHFingerprint 返回 OpenSSH 风格的 SHA256 指纹。
func SSHFingerprint(pub ssh.PublicKey) string { return ssh.FingerprintSHA256(pub) }
