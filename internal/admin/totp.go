package admin

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	totpPeriod = 30 // 秒
	totpDigits = 6
	totpSkew   = 1 // 允许前后各 1 个时间片的时钟偏差
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret 生成 20 字节随机密钥的 base32 编码（32 个字符）。
func NewTOTPSecret() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b32.EncodeToString(b)
}

// TOTPStep 返回 t 所在的 30 秒时间片编号。
func TOTPStep(t time.Time) int64 { return t.Unix() / totpPeriod }

// TOTPCode 按 RFC 6238（HMAC-SHA1、6 位）计算指定时间片的动态码。
func TOTPCode(secret string, step int64) (string, error) {
	key, err := b32.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		return "", fmt.Errorf("totp secret: %w", err)
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", v%1_000_000), nil
}

// NormalizeCode 去掉动态码中的所有空白（验证器 App 常显示为「123 456」）。
func NormalizeCode(code string) string { return strings.Join(strings.Fields(code), "") }

// MatchTOTP 在 now 所在时间片前后各 1 个时间片内查找与 code 相同的动态码，返回匹配的时间片编号。
func MatchTOTP(secret, code string, now time.Time) (int64, bool) {
	code = NormalizeCode(code)
	if len(code) != totpDigits {
		return 0, false
	}
	cur := TOTPStep(now)
	for step := cur - totpSkew; step <= cur+totpSkew; step++ {
		want, err := TOTPCode(secret, step)
		if err != nil {
			return 0, false
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return step, true
		}
	}
	return 0, false
}

// OTPAuthURI 生成验证器 App 可扫描的 otpauth:// 链接。
func OTPAuthURI(issuer, account, secret string) string {
	q := url.Values{"secret": {secret}, "issuer": {issuer}, "algorithm": {"SHA1"}, "digits": {"6"}, "period": {"30"}}
	return "otpauth://totp/" + url.PathEscape(issuer+":"+account) + "?" + q.Encode()
}
