package referral

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

// DeriveFingerprintKey 从 SPARK_SECRET_KEY 派生实名指纹的 HMAC 密钥（HKDF-SHA256，info "referral-fingerprint"）。
func DeriveFingerprintKey(secret []byte) ([]byte, error) {
	if len(secret) == 0 {
		return nil, errors.New("referral: empty secret")
	}
	return hkdf.Key(sha256.New, secret, nil, "referral-fingerprint", 32)
}

// Fingerprint = hex(HMAC-SHA256(key, kind + ":" + number))；kind 为 "kyc"（身份证号）或 "lic"（统一社会信用代码）。
func Fingerprint(key []byte, kind, number string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(kind + ":" + number))
	return hex.EncodeToString(m.Sum(nil))
}
