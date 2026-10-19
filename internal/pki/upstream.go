package pki

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"
)

// GenerateUpstreamKey 生成平台登录容器用的 ed25519 私钥（OpenSSH 格式，0600）与 .pub。
func GenerateUpstreamKey(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("文件已存在：%s", path)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	block, err := ssh.MarshalPrivateKey(priv, "spark-cloud upstream")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return err
	}
	return os.WriteFile(path+".pub", ssh.MarshalAuthorizedKey(sshPub), 0o644)
}

// LoadUpstreamPublicKey 读取私钥并派生 authorized_keys 格式的公钥（不含注释与换行）。
func LoadUpstreamPublicKey(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("上游 SSH 私钥 %s 权限过宽（%o），请 chmod 600", path, st.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	key, err := ssh.ParseRawPrivateKey(raw)
	if err != nil {
		return "", fmt.Errorf("解析上游 SSH 私钥: %w", err)
	}
	var pk ed25519.PrivateKey
	switch k := key.(type) {
	case *ed25519.PrivateKey:
		pk = *k
	case ed25519.PrivateKey:
		pk = k
	default:
		return "", errors.New("上游 SSH 私钥必须是 ed25519")
	}
	pub, err := ssh.NewPublicKey(pk.Public())
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))), nil
}

// LoadUpstreamSigner 读取上游私钥供网关 / 入口登录容器 sshd（权限检查同 LoadUpstreamPublicKey）。
func LoadUpstreamSigner(path string) (ssh.Signer, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("上游 SSH 私钥 %s 权限过宽（%o），请 chmod 600", path, st.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("解析上游 SSH 私钥: %w", err)
	}
	if s.PublicKey().Type() != ssh.KeyAlgoED25519 {
		return nil, errors.New("上游 SSH 私钥必须是 ed25519")
	}
	return s, nil
}
