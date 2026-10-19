package admin

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"

	"spark-cloud/internal/platform/apperr"
)

const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 2
	argonKeyLen  = 32
	argonSaltLen = 16

	MinPasswordLen = 12
	maxPasswordLen = 128

	maxVerifyMemory = 1 << 20 // KiB（1 GiB），校验时允许的内存参数上限
)

// HashPassword 用 argon2id 生成 PHC 格式字符串：$argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash>（base64 无填充）。
func HashPassword(password string) string {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		panic(err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads, enc.EncodeToString(salt), enc.EncodeToString(key))
}

// VerifyPassword 校验密码；哈希格式不正确一律视为不匹配。
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	var memory, iterations uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &threads); err != nil {
		return false
	}
	// argon2.IDKey 对非法参数会 panic，且内存参数来自不可信输入，需先做范围校验。
	if iterations < 1 || threads < 1 || memory < 8*uint32(threads) || memory > maxVerifyMemory {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, iterations, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// ValidatePassword 校验管理员密码长度（按字符计）。
func ValidatePassword(password string) error {
	if n := utf8.RuneCountInString(password); n < MinPasswordLen || n > maxPasswordLen {
		return apperr.BadRequest("密码长度需为 12～128 个字符")
	}
	return nil
}
