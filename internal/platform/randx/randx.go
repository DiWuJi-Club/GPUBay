// Package randx 生成验证码、会话 token、资源 ID 和密码，全部基于 crypto/rand。
package randx

import (
	"crypto/rand"
	"encoding/base64"
	"math/big"
)

const (
	digits      = "0123456789"
	idAlphabet  = "abcdefghijklmnopqrstuvwxyz0123456789"
	pwdAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789" // 去掉易混淆字符
)

func pick(alphabet string, n int) string {
	out := make([]byte, n)
	max := big.NewInt(int64(len(alphabet)))
	for i := range out {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(err)
		}
		out[i] = alphabet[v.Int64()]
	}
	return string(out)
}

func Digits(n int) string { return pick(digits, n) }

func ID(prefix string, n int) string { return prefix + pick(idAlphabet, n) }

func Password(n int) string { return pick(pwdAlphabet, n) }

// Token 返回 32 字节随机数的 base64url（无填充，43 个字符）。
func Token() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
