package executor

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
)

// initScript 是容器入口：写最小 sshd 配置并 exec sshd；镜像自身的 ENTRYPOINT / CMD 不会执行。
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
EOF
exec /usr/sbin/sshd -D -e -f /run/spark-sshd.conf
`

// writeSSHDir 准备 ssh/<id>/{authorized_keys,hostkeys/ssh_host_ed25519_key,init.sh}；主机密钥已存在则保留。
func (e *Executor) writeSSHDir(id, upstreamPubKey string) error {
	dir := e.sshDir(id)
	if err := os.MkdirAll(filepath.Join(dir, "hostkeys"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "authorized_keys"), []byte(upstreamPubKey+"\n"), 0o644); err != nil {
		return err
	}
	// WriteFile 不会改已存在文件的权限，显式 chmod 保证可执行。
	if err := os.WriteFile(filepath.Join(dir, "init.sh"), []byte(initScript), 0o755); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Join(dir, "init.sh"), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "hostkeys", "ssh_host_ed25519_key")); err == nil {
		return nil
	}
	return e.d.HostKeyGen(filepath.Join(dir, "hostkeys"))
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
