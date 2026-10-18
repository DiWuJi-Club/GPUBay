// Package upstream 负责平台到容器 sshd 的连接：固定主机密钥校验（存量实例首连信任）与按实例共享的 SSH 连接池。
package upstream

import (
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

const clientVersion = "SSH-2.0-SparkCloud"

// HostKeyCallback 按 instances.ssh_host_pubkey 校验上游主机密钥：expected 非空时必须逐字相等；
// 为空（计划 2 创建的存量实例）时调用 tofu 写回并信任本次。
func HostKeyCallback(expected string, tofu func(pubLine string) error) ssh.HostKeyCallback {
	expected = strings.TrimSpace(expected)
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
		if expected == "" {
			if tofu == nil {
				return fmt.Errorf("上游主机密钥未登记且无法首连信任")
			}
			return tofu(line)
		}
		if line != expected {
			return fmt.Errorf("上游主机密钥不匹配：登记 %s，实际 %s", fingerprintOf(expected), ssh.FingerprintSHA256(key))
		}
		return nil
	}
}

func fingerprintOf(line string) string {
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return "(无法解析)"
	}
	return ssh.FingerprintSHA256(pub)
}

// ClientConfig 是拨容器 sshd 用的客户端配置（用户 root，平台上游私钥）。
func ClientConfig(signer ssh.Signer, hostKey ssh.HostKeyCallback, timeout time.Duration) *ssh.ClientConfig {
	return &ssh.ClientConfig{User: "root", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: hostKey, Timeout: timeout, ClientVersion: clientVersion}
}

// hostKeyAlgorithms 按登记公钥限定协商的主机密钥算法（让对端出示被固定的那把）；未登记或无法解析返回 nil（用默认）。
func hostKeyAlgorithms(line string) []string {
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return nil
	}
	if pub.Type() == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
	}
	return []string{pub.Type()}
}
