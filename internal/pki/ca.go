// Package pki 提供平台内置 CA：根证书存库（私钥 secretbox 加密）、签发网关服务端证书与机器客户端证书。
package pki

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/secretbox"
)

const (
	caValidity      = 20 * 365 * 24 * time.Hour
	serverValidity  = 365 * 24 * time.Hour
	machineValidity = 5 * 365 * 24 * time.Hour
)

type CA struct {
	cert    *x509.Certificate
	certDER []byte
	key     *ecdsa.PrivateKey
	pool    *x509.CertPool
}

// EnsureCA 读取或首次生成根 CA；多副本并发首次启动时靠 ON CONFLICT DO NOTHING 保证只有一套。
func EnsureCA(ctx context.Context, pool *pgxpool.Pool, box *secretbox.Box, clk clock.Clock) (*CA, error) {
	ca, err := loadCA(ctx, pool, box)
	if err == nil {
		return ca, nil
	}
	if !db.IsNoRows(err) {
		return nil, err
	}
	now := clk.Now()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Spark Cloud Agent CA"},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(caValidity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	if _, err := pool.Exec(ctx, `INSERT INTO platform_pki (id, ca_cert, ca_key_enc, created_at) VALUES (1, $1, $2, $3) ON CONFLICT (id) DO NOTHING`,
		der, box.Seal(keyDER), now); err != nil {
		return nil, err
	}
	return loadCA(ctx, pool, box)
}

func loadCA(ctx context.Context, pool *pgxpool.Pool, box *secretbox.Box) (*CA, error) {
	var certDER, keyEnc []byte
	if err := pool.QueryRow(ctx, `SELECT ca_cert, ca_key_enc FROM platform_pki WHERE id = 1`).Scan(&certDER, &keyEnc); err != nil {
		return nil, err
	}
	keyDER, err := box.Open(keyEnc)
	if err != nil {
		return nil, fmt.Errorf("解密 CA 私钥失败（SPARK_SECRET_KEY 是否被更换？）: %w", err)
	}
	k, err := x509.ParsePKCS8PrivateKey(keyDER)
	if err != nil {
		return nil, err
	}
	key, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("pki: CA key is not ECDSA")
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, err
	}
	p := x509.NewCertPool()
	p.AddCert(cert)
	return &CA{cert: cert, certDER: certDER, key: key, pool: p}, nil
}

func randomSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

func (c *CA) CertDER() []byte { return c.certDER }
func (c *CA) CertPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.certDER})
}
func (c *CA) Fingerprint() string  { return Fingerprint(c.certDER) }
func (c *CA) Pool() *x509.CertPool { return c.pool }

// Fingerprint 返回证书 DER 的 SHA-256 小写十六进制。
func Fingerprint(der []byte) string {
	h := sha256.Sum256(der)
	return hex.EncodeToString(h[:])
}

// IssueServer 为网关签发服务端证书，SAN 按 hosts 自动区分 IP 与域名，有效期 1 年。
func (c *CA) IssueServer(hosts []string, now time.Time) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := randomSerial()
	if err != nil {
		return tls.Certificate{}, err
	}
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "Spark Cloud Agent Gateway"},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.Add(serverValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tpl.IPAddresses = append(tpl.IPAddresses, ip)
		} else {
			tpl.DNSNames = append(tpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der, c.certDER}, PrivateKey: key, Leaf: leaf}, nil
}

// IssueMachine 用 CSR 里的公钥签发机器证书：Subject 只有 CN = 序列号，忽略 CSR 的其他内容，有效期 5 年。
func (c *CA) IssueMachine(pub *ecdsa.PublicKey, serial string, now time.Time) ([]byte, error) {
	sn, err := randomSerial()
	if err != nil {
		return nil, err
	}
	tpl := &x509.Certificate{
		SerialNumber: sn,
		Subject:      pkix.Name{CommonName: serial},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.Add(machineValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	return x509.CreateCertificate(rand.Reader, tpl, c.cert, pub, c.key)
}
