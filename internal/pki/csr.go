package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"errors"
)

// ParseCSR 校验 CSR 签名并只取出公钥；只接受 ECDSA P-256。
func ParseCSR(der []byte) (*ecdsa.PublicKey, error) {
	req, err := x509.ParseCertificateRequest(der)
	if err != nil {
		return nil, err
	}
	if err := req.CheckSignature(); err != nil {
		return nil, err
	}
	pub, ok := req.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return nil, errors.New("pki: CSR public key must be ECDSA P-256")
	}
	return pub, nil
}
