package tlsfile_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"spark-cloud/internal/platform/tlsfile"
)

func writeCert(t *testing.T, dir string, notAfter time.Time, serial int64) (certFile, keyFile string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "spark.test"}, DNSNames: []string{"spark.test", "*.spark.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	certFile, keyFile = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)
	return certFile, keyFile
}

func TestLoaderHotReloadKeepsOldOnError(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writeCert(t, dir, time.Now().Add(90*24*time.Hour), 1)
	var reloadErrs int
	now := time.Now()
	l := &tlsfile.Loader{CertFile: certFile, KeyFile: keyFile, Interval: time.Minute, Now: func() time.Time { return now }, OnReloadError: func(error) { reloadErrs++ }}
	if err := l.Load(); err != nil {
		t.Fatal(err)
	}
	c1, _ := l.GetCertificate(&tls.ClientHelloInfo{})
	if c1.Leaf == nil || c1.Leaf.SerialNumber.Int64() != 1 {
		t.Fatalf("initial cert = %+v", c1)
	}
	// 一分钟内不检查文件
	time.Sleep(20 * time.Millisecond) // 保证 mtime 不同
	writeCert(t, dir, time.Now().Add(90*24*time.Hour), 2)
	if c, _ := l.GetCertificate(&tls.ClientHelloInfo{}); c.Leaf.SerialNumber.Int64() != 1 {
		t.Fatal("reloaded before interval elapsed")
	}
	now = now.Add(2 * time.Minute)
	if c, _ := l.GetCertificate(&tls.ClientHelloInfo{}); c.Leaf.SerialNumber.Int64() != 2 {
		t.Fatalf("not reloaded after interval: serial %d", c.Leaf.SerialNumber.Int64())
	}
	// 坏文件：保留旧证书；第一次失败不回调，mtime 不变也在下次检查重试，连续第二次失败回调
	time.Sleep(20 * time.Millisecond)
	os.WriteFile(certFile, []byte("garbage"), 0o644)
	l.CheckNow()
	if c, _ := l.GetCertificate(&tls.ClientHelloInfo{}); c.Leaf.SerialNumber.Int64() != 2 || reloadErrs != 0 {
		t.Fatalf("first bad reload: serial %d errs %d", c.Leaf.SerialNumber.Int64(), reloadErrs)
	}
	l.CheckNow()
	if c, _ := l.GetCertificate(&tls.ClientHelloInfo{}); c.Leaf.SerialNumber.Int64() != 2 || reloadErrs != 1 {
		t.Fatalf("second bad reload: serial %d errs %d", c.Leaf.SerialNumber.Int64(), reloadErrs)
	}
	if l.TLSConfig().MinVersion != tls.VersionTLS12 || l.TLSConfig().GetCertificate == nil {
		t.Fatal("tls config")
	}
}

func TestLoaderExpiryCallback(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writeCert(t, dir, time.Now().Add(10*24*time.Hour), 3)
	var warned []time.Time
	l := &tlsfile.Loader{CertFile: certFile, KeyFile: keyFile, OnExpiring: func(na time.Time) { warned = append(warned, na) }}
	if err := l.Load(); err != nil {
		t.Fatal(err)
	}
	l.CheckExpiry(14 * 24 * time.Hour)
	if len(warned) != 1 || !warned[0].Equal(l.NotAfter()) {
		t.Fatalf("warned = %v", warned)
	}
	far, farKey := writeCert(t, t.TempDir(), time.Now().Add(60*24*time.Hour), 4)
	l2 := &tlsfile.Loader{CertFile: far, KeyFile: farKey, OnExpiring: func(time.Time) { t.Fatal("warned for far expiry") }}
	l2.Load()
	l2.CheckExpiry(14 * 24 * time.Hour)
	if _, err := (&tlsfile.Loader{CertFile: "/nonexistent.crt", KeyFile: keyFile}).GetCertificate(&tls.ClientHelloInfo{}); err == nil {
		t.Fatal("missing cert file accepted")
	}
}

func TestLoaderLoadFailsOnMissingOrMismatchedFiles(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writeCert(t, dir, time.Now().Add(90*24*time.Hour), 5)
	if err := (&tlsfile.Loader{CertFile: filepath.Join(dir, "missing.crt"), KeyFile: keyFile}).Load(); err == nil {
		t.Fatal("missing cert accepted")
	}
	_, otherKey := writeCert(t, t.TempDir(), time.Now().Add(90*24*time.Hour), 6)
	if err := (&tlsfile.Loader{CertFile: certFile, KeyFile: otherKey}).Load(); err == nil {
		t.Fatal("mismatched key accepted")
	}
}

// 续期工具先写证书、再写私钥：中间那次检查看到不配对的一对，不告警；私钥到位后的下一次检查加载新的一对。
func TestLoaderCertThenKeyUpdateNoAlert(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writeCert(t, dir, time.Now().Add(90*24*time.Hour), 1)
	var reloadErrs int
	now := time.Now()
	l := &tlsfile.Loader{CertFile: certFile, KeyFile: keyFile, Now: func() time.Time { return now }, OnReloadError: func(error) { reloadErrs++ }}
	if err := l.Load(); err != nil {
		t.Fatal(err)
	}
	newCert, newKey := writeCert(t, t.TempDir(), time.Now().Add(90*24*time.Hour), 7)
	copyFile := func(src, dst string) {
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(dst, b, 0o600)
	}
	time.Sleep(20 * time.Millisecond)
	copyFile(newCert, certFile) // 证书先到
	now = now.Add(2 * time.Minute)
	if c, _ := l.GetCertificate(&tls.ClientHelloInfo{}); c.Leaf.SerialNumber.Int64() != 1 || reloadErrs != 0 {
		t.Fatalf("mismatched pair: serial %d errs %d", c.Leaf.SerialNumber.Int64(), reloadErrs)
	}
	copyFile(newKey, keyFile) // 私钥随后到（mtime 可能与上次相同：失败后不看 mtime 也会重试）
	now = now.Add(2 * time.Minute)
	if c, _ := l.GetCertificate(&tls.ClientHelloInfo{}); c.Leaf.SerialNumber.Int64() != 7 || reloadErrs != 0 {
		t.Fatalf("after key update: serial %d errs %d", c.Leaf.SerialNumber.Int64(), reloadErrs)
	}
	// 恢复后计数清零：再坏一次也只记日志
	time.Sleep(20 * time.Millisecond)
	os.WriteFile(certFile, []byte("garbage"), 0o644)
	l.CheckNow()
	if reloadErrs != 0 {
		t.Fatalf("failure count not reset after successful reload: errs %d", reloadErrs)
	}
}
