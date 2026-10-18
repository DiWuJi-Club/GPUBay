package executor

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

// initScript 是容器入口：写最小 sshd 配置并 exec sshd；镜像自身的 ENTRYPOINT / CMD 不会执行。
// sftp 用 sshd 内置实现（镜像不需要 openssh-sftp-server）；X11 关闭；转发与 agent 转发放开（网关侧另有白名单）。
const initScript = `#!/bin/sh
set -e
mkdir -p /run/sshd
cat > /run/spark-sshd.conf <<'EOF'
Port 22
HostKey /.spark/hostkeys/ssh_host_ed25519_key
AuthorizedKeysFile /.spark/authorized_keys
StrictModes no
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin prohibit-password
PidFile /run/spark-sshd.pid
Subsystem sftp internal-sftp
AllowTcpForwarding yes
GatewayPorts no
AllowAgentForwarding yes
X11Forwarding no
PermitTunnel no
ClientAliveInterval 60
ClientAliveCountMax 3
MaxStartups 50:30:200
MaxSessions 32
EOF
exec /usr/sbin/sshd -D -e -f /run/spark-sshd.conf
`

// writeSSHDir 准备 ssh/<id>/{authorized_keys,hostkeys/ssh_host_ed25519_key,init.sh}。
// 主机密钥：已存在则保留；否则有平台下发的 PEM 就校验后写入（无效 → 返回错误，不回落本地生成），没有就本地生成（兼容旧平台）。
func (e *Executor) writeSSHDir(id, upstreamPubKey, hostKeyPEM string) error {
	dir := e.sshDir(id)
	if err := os.MkdirAll(filepath.Join(dir, "hostkeys"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "authorized_keys"), []byte(upstreamPubKey+"\n"), 0o644); err != nil {
		return err
	}
	if err := e.writeInitScript(id); err != nil {
		return err
	}
	keyPath := filepath.Join(dir, "hostkeys", "ssh_host_ed25519_key")
	if _, err := os.Stat(keyPath); err == nil {
		return nil
	}
	if strings.TrimSpace(hostKeyPEM) == "" {
		return e.d.HostKeyGen(filepath.Join(dir, "hostkeys"))
	}
	return writeHostKey(filepath.Join(dir, "hostkeys"), []byte(hostKeyPEM))
}

// writeInitScript 只重写 init.sh（create / start / 重启恢复都调用，存量实例下次启动即用新配置）。
func (e *Executor) writeInitScript(id string) error {
	path := filepath.Join(e.sshDir(id), "init.sh")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// WriteFile 不会改已存在文件的权限，显式 chmod 保证可执行。
	if err := os.WriteFile(path, []byte(initScript), 0o755); err != nil {
		return err
	}
	return os.Chmod(path, 0o755)
}

// parseHostKey 校验平台下发的主机私钥：必须是未加密的 OpenSSH 格式 ed25519 私钥。错误信息不含密钥内容。
func parseHostKey(pemBytes []byte) (ssh.PublicKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "OPENSSH PRIVATE KEY" {
		return nil, errors.New("平台下发的主机密钥不是 OpenSSH 私钥 PEM")
	}
	raw, err := ssh.ParseRawPrivateKey(pemBytes)
	if err != nil {
		return nil, errors.New("平台下发的主机密钥无法解析: " + err.Error())
	}
	var priv ed25519.PrivateKey
	switch k := raw.(type) {
	case *ed25519.PrivateKey:
		priv = *k
	case ed25519.PrivateKey:
		priv = k
	default:
		return nil, errors.New("平台下发的主机密钥不是 ed25519")
	}
	return ssh.NewPublicKey(priv.Public())
}

// writeHostKey 校验并写入平台下发的主机私钥（0600，先写临时文件再改名，避免半截文件被当成「已存在」保留）并派生 .pub。
func writeHostKey(dir string, pemBytes []byte) error {
	pub, err := parseHostKey(pemBytes)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "ssh_host_ed25519_key.pub"), ssh.MarshalAuthorizedKey(pub), 0o644); err != nil {
		return err
	}
	keyPath := filepath.Join(dir, "ssh_host_ed25519_key")
	tmp := keyPath + ".tmp"
	_ = os.Remove(tmp)
	if err := os.WriteFile(tmp, pemBytes, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, keyPath); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func generateHostKey(dir string) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "ssh_host_ed25519_key"), pem.EncodeToMemory(block), 0o600); err != nil {
		return err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "ssh_host_ed25519_key.pub"), ssh.MarshalAuthorizedKey(sshPub), 0o644)
}
