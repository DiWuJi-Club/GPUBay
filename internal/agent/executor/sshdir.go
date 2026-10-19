package executor

import (
	"bytes"
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
// sftp 用 sshd 内置实现（镜像不需要 openssh-sftp-server）；X11、Unix socket 转发关闭；TCP 转发与 agent 转发放开（网关侧另有白名单）。
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
AllowStreamLocalForwarding no
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
// 主机密钥以平台为准：
//   - 平台下发了 PEM：先校验（无效 → 返回错误，绝不回落本地生成）；现有密钥公钥不同、为空或无效 → 原子替换为平台密钥；
//   - 没有 PEM（旧平台）：现有密钥有效则保留，否则（不存在 / 为空 / 无效）本地重新生成。
//
// 每次都按最终私钥重写 .pub。
func (e *Executor) writeSSHDir(id, upstreamPubKey, hostKeyPEM string) error {
	dir := e.sshDir(id)
	keyDir := filepath.Join(dir, "hostkeys")
	if err := os.MkdirAll(keyDir, 0o755); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(dir, "authorized_keys"), []byte(upstreamPubKey+"\n"), 0o644); err != nil {
		return err
	}
	if err := e.writeInitScript(id); err != nil {
		return err
	}
	keyPath := filepath.Join(keyDir, "ssh_host_ed25519_key")
	var curPub ssh.PublicKey
	if b, err := os.ReadFile(keyPath); err == nil {
		curPub, _ = parseHostKey(b) // 为空或无效 → nil，按缺失处理
	} else if !os.IsNotExist(err) {
		return err
	}
	if strings.TrimSpace(hostKeyPEM) != "" {
		pub, err := parseHostKey([]byte(hostKeyPEM))
		if err != nil {
			return err
		}
		if curPub == nil || !bytes.Equal(curPub.Marshal(), pub.Marshal()) {
			if err := writeFileAtomic(keyPath, []byte(hostKeyPEM), 0o600); err != nil {
				return err
			}
		}
		return writeFileAtomic(keyPath+".pub", ssh.MarshalAuthorizedKey(pub), 0o644)
	}
	if curPub != nil {
		return writeFileAtomic(keyPath+".pub", ssh.MarshalAuthorizedKey(curPub), 0o644)
	}
	return e.d.HostKeyGen(keyDir)
}

// writeInitScript 只重写 init.sh（create / start / 重启恢复都调用，存量实例下次启动即用新配置）。
// 原子替换：容器挂载的是整个目录，改名后新启动的容器读到的总是完整脚本。
func (e *Executor) writeInitScript(id string) error {
	dir := e.sshDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, "init.sh"), []byte(initScript), 0o755)
}

// parseHostKey 校验主机私钥：必须是未加密的 OpenSSH 格式 ed25519 私钥。错误信息不含密钥内容。
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

// writeFileAtomic 写同目录临时文件 → chmod → fsync → 改名 → fsync 目录：崩溃或断电后要么是旧内容，要么是完整新内容。
func writeFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// generateHostKey 本地生成 ed25519 主机密钥（旧平台不下发 PEM 时），原子写入。
func generateHostKey(dir string) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(dir, "ssh_host_ed25519_key"), pem.EncodeToMemory(block), 0o600); err != nil {
		return err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, "ssh_host_ed25519_key.pub"), ssh.MarshalAuthorizedKey(sshPub), 0o644)
}
