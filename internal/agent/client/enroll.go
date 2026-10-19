package client

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"spark-cloud/internal/agentpb"
	"spark-cloud/internal/pki"
)

// fingerprintMatches 常量时间比较证书 DER 的 SHA-256 与期望指纹（小写十六进制）。
func fingerprintMatches(der []byte, want string) bool {
	return subtle.ConstantTimeCompare([]byte(pki.Fingerprint(der)), []byte(want)) == 1
}

// normalizeFingerprint 去空白、转小写，并要求是 64 位十六进制（SHA-256）。
func normalizeFingerprint(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if b, err := hex.DecodeString(s); err != nil || len(b) != 32 {
		return "", errors.New("--ca-sha256 必须是 64 位十六进制的 SHA-256 指纹")
	}
	return s, nil
}

// pinnedCA 返回只信任指纹为 caSHA256 的 CA 的 TLS 配置：服务端证书链里必须带该 CA，且叶子由它签发并匹配主机名。
func pinnedCA(caSHA256, serverName string) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		ServerName:         serverName,
		InsecureSkipVerify: true, //nolint:gosec // 由 VerifyPeerCertificate 按指纹校验整条链
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) < 2 {
				return errors.New("server did not present its CA certificate")
			}
			var leaf, ca *x509.Certificate
			for i, der := range raw {
				c, err := x509.ParseCertificate(der)
				if err != nil {
					return err
				}
				if i == 0 {
					leaf = c
				} else if fingerprintMatches(der, caSHA256) {
					ca = c
				}
			}
			if ca == nil {
				return errors.New("平台 CA 指纹不匹配 --ca-sha256")
			}
			pool := x509.NewCertPool()
			pool.AddCert(ca)
			_, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: serverName, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
			return err
		},
	}
}

// Enroll 用一次性令牌换取机器证书；私钥不出本机，只发送 CSR。
// TLS 只信任指纹等于 caSHA256 的 CA，返回的 CA 与机器证书也逐一核对。
func Enroll(ctx context.Context, addr, token, caSHA256 string, key *ecdsa.PrivateKey) (certDER, caDER []byte, err error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, nil, fmt.Errorf("--platform 必须是 host:port: %w", err)
	}
	want, err := normalizeFingerprint(caSHA256)
	if err != nil {
		return nil, nil, err
	}
	token = strings.TrimSpace(token)
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		return nil, nil, err
	}
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(pinnedCA(want, host))))
	if err != nil {
		return nil, nil, err
	}
	defer cc.Close()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := agentpb.NewAgentGatewayClient(cc).Enroll(ctx, &agentpb.EnrollRequest{Token: token, CsrDer: csr})
	if err != nil {
		return nil, nil, err
	}
	if !fingerprintMatches(resp.GetCaDer(), want) {
		return nil, nil, errors.New("平台返回的 CA 与 --ca-sha256 不符")
	}
	if err := checkMachineCert(key, resp.GetCertDer(), resp.GetCaDer()); err != nil {
		return nil, nil, err
	}
	return resp.GetCertDer(), resp.GetCaDer(), nil
}

// checkMachineCert：机器证书必须由该 CA 签发、可用于客户端认证，且公钥就是本机私钥对应的公钥。
func checkMachineCert(key *ecdsa.PrivateKey, certDER, caDER []byte) error {
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return fmt.Errorf("平台返回的机器证书无效: %w", err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return fmt.Errorf("平台返回的 CA 证书无效: %w", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return fmt.Errorf("平台返回的机器证书未由该 CA 签发: %w", err)
	}
	if pub, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok || !pub.Equal(&key.PublicKey) {
		return errors.New("平台返回的机器证书与本机私钥不匹配")
	}
	return nil
}

// TLSFor 构造 Connect 用的 mTLS 配置。
func TLSFor(key *ecdsa.PrivateKey, certDER, caDER []byte, serverName string) (*tls.Config, error) {
	if err := checkMachineCert(key, certDER, caDER); err != nil {
		return nil, err
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, ServerName: serverName,
		Certificates: []tls.Certificate{{Certificate: [][]byte{certDER}, PrivateKey: key}}}, nil
}

// LoadOrCreateKey 读取本机私钥（PKCS#8 PEM）；不存在时在本机生成 ECDSA P-256 私钥并以 0600 写入。私钥从不发送给平台。
func LoadOrCreateKey(path string) (*ecdsa.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return createKey(path)
	}
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("%s 不是 PKCS#8 PEM 私钥", path)
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := k.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("%s 不是 ECDSA P-256 私钥", path)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

func createKey(path string) (*ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if err := pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: der}); err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return nil, err
	}
	return key, nil
}
