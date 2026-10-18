package pki

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"strings"

	"golang.org/x/crypto/ssh"
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
