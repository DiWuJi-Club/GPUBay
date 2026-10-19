package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"spark-cloud/internal/platform/randx"
)

// RecoveryCodeCount 是每次绑定 TOTP 时生成的恢复码数量。
const RecoveryCodeCount = 8

// NewRecoveryCodes 生成形如 abcde-12345 的一次性恢复码。
func NewRecoveryCodes() []string {
	out := make([]string, RecoveryCodeCount)
	for i := range out {
		out[i] = randx.ID("", 5) + "-" + randx.ID("", 5)
	}
	return out
}

func hashSecret(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// hashRecoveryCode 忽略大小写、空白与连字符后再哈希，方便管理员照抄输入。
func hashRecoveryCode(code string) string {
	normalized := strings.ReplaceAll(strings.ToLower(NormalizeCode(code)), "-", "")
	return hashSecret("recovery:" + normalized)
}
