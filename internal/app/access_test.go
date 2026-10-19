package app_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/app"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/config"
	"spark-cloud/internal/platform/dbtest"
)

func writeSelfSigned(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "spark.test"}, DNSNames: []string{"spark.test", "*.spark.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(90 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
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

func TestHandlerSplitsIngressAndConsole(t *testing.T) {
	cfg := config.Config{UpstreamSSHKeyFile: upstreamKeyFile(t), Env: "dev", Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "mock", KYCDriver: "mock", UploadDir: t.TempDir()}
	a, err := app.Build(cfg, dbtest.New(t), discardLog(), app.Options{Clock: clock.NewFake(dbtest.T0)})
	if err != nil {
		t.Fatal(err)
	}
	if a.SSH == nil || a.Ingress == nil || a.Routes == nil || a.Upstream == nil || a.IngressAuth == nil || a.TLS != nil {
		t.Fatalf("access components missing: %+v", a)
	}
	ts := httptest.NewServer(a.Handler)
	defer ts.Close()
	get := func(host, path string) *http.Response {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if r := get("ins-a1b2c3d4-8888.spark.test", "/"); r.StatusCode != 404 || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("ingress 404 page = %d %s", r.StatusCode, r.Header.Get("Content-Type"))
	}
	if r := get("spark.test", "/api/v1/products"); r.StatusCode != 200 {
		t.Fatalf("console via apex = %d", r.StatusCode)
	}
	if r := get("127.0.0.1", "/api/v1/products"); r.StatusCode != 200 || r.Header.Get("Strict-Transport-Security") != "" {
		t.Fatalf("console via ip = %d hsts=%q", r.StatusCode, r.Header.Get("Strict-Transport-Security"))
	}
	if pub, _ := a.Servers(cfg.FillAccessDefaults()); pub.TLSConfig != nil || a.RedirectServer(cfg) != nil {
		t.Fatal("tls configured without cert files")
	}
	// 访问层循环随 ctx 退出（连接池由 cmd/platform 在 HTTP 关闭之后再关）
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.RunAccessLoops(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunAccessLoops did not return after ctx cancel")
	}
}

func TestBuildWithTLSFilesAndRedirect(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writeSelfSigned(t, dir)
	cfg := config.Config{UpstreamSSHKeyFile: upstreamKeyFile(t), Env: "staging", Domain: "spark.test", PublicBaseURL: "https://spark.test", SecretKey: dbtest.Key,
		SMSDriver: "mock", KYCDriver: "mock", UploadDir: t.TempDir(), TLSCertFile: certFile, TLSKeyFile: keyFile, HTTPRedirectAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0"}
	a, err := app.Build(cfg, dbtest.New(t), discardLog(), app.Options{Clock: clock.NewFake(dbtest.T0)})
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := a.Servers(cfg)
	if a.TLS == nil || pub.TLSConfig == nil || pub.TLSConfig.GetCertificate == nil {
		t.Fatal("tls not wired")
	}
	ts := httptest.NewUnstartedServer(a.Handler)
	ts.TLS = pub.TLSConfig
	ts.StartTLS()
	defer ts.Close()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/products", nil)
	req.Host = "spark.test"
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Strict-Transport-Security") != "max-age=31536000" {
		t.Fatalf("https = %d hsts=%q", resp.StatusCode, resp.Header.Get("Strict-Transport-Security"))
	}
	// httptest.StartTLS 会给 TLSConfig 补自己的证书；按 IP 连接无 SNI 时选不到我们的。带 SNI 握手，确认用的是文件里的证书（序列号 1）
	conn, err := tls.Dial("tcp", strings.TrimPrefix(ts.URL, "https://"), &tls.Config{ServerName: "spark.test", InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	if sn := conn.ConnectionState().PeerCertificates[0].SerialNumber.Int64(); sn != 1 {
		t.Fatalf("served certificate serial = %d, want 1 (file-based loader not used)", sn)
	}
	conn.Close()
	rs := a.RedirectServer(cfg)
	if rs == nil {
		t.Fatal("redirect server nil")
	}
	rt := httptest.NewServer(rs.Handler)
	defer rt.Close()
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	redirect := func(host, path string) (int, string) {
		req, _ := http.NewRequest("GET", rt.URL+path, nil)
		req.Host = host
		r, err := noFollow.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode, r.Header.Get("Location")
	}
	if code, loc := redirect("ins-a1b2c3d4-8888.spark.test:80", "/lab?x=1"); code != 301 || loc != "https://ins-a1b2c3d4-8888.spark.test/lab?x=1" {
		t.Fatalf("redirect = %d %s", code, loc)
	}
	if code, loc := redirect("INS-a1b2c3d4-8888.Spark.Test", "/"); code != 301 || loc != "https://ins-a1b2c3d4-8888.spark.test/" {
		t.Fatalf("redirect (mixed case) = %d %s", code, loc)
	}
	if code, loc := redirect("spark.test", "/login?next=%2Fx"); code != 301 || loc != "https://spark.test/login?next=%2Fx" {
		t.Fatalf("console redirect = %d %s", code, loc)
	}
	// Location 不能由攻击者控制的 Host 决定：未知 Host、IP、其它父域下的实例名一律 404，不重定向
	for _, host := range []string{"evil.test", "127.0.0.1", "ins-a1b2c3d4-8888.evil.test", "spark.test.evil.test", "ssh.spark.test"} {
		if code, loc := redirect(host, "/"); code != 404 || loc != "" {
			t.Fatalf("host %q → %d %q, want 404 without Location", host, code, loc)
		}
	}
	// 证书文件坏了：Build 失败而不是悄悄起一个没证书的服务
	os.WriteFile(certFile, []byte("x"), 0o644)
	if _, err := app.Build(cfg, dbtest.New(t), discardLog(), app.Options{Clock: clock.NewFake(dbtest.T0)}); err == nil || !strings.Contains(err.Error(), "TLS") {
		t.Fatalf("bad cert accepted: %v", err)
	}
	os.Remove(certFile)
	if _, err := app.Build(cfg, dbtest.New(t), discardLog(), app.Options{Clock: clock.NewFake(dbtest.T0)}); err == nil || !strings.Contains(err.Error(), "TLS") {
		t.Fatalf("missing cert accepted: %v", err)
	}
}

// M-5：staging / prod 未设置 SPARK_ADMIN_PUBLIC_BASE_URL 时启动告警（不带 Sec-Fetch-Site 的客户端后台写请求会 403）。
func TestBuildWarnsWithoutAdminPublicBaseURL(t *testing.T) {
	pool := dbtest.New(t)
	for _, tc := range []struct {
		env, adminURL string
		warn          bool
	}{
		{"staging", "", true},
		{"staging", "https://admin.spark.test", false},
		{"dev", "", false},
	} {
		var buf strings.Builder
		cfg := config.Config{UpstreamSSHKeyFile: upstreamKeyFile(t), Env: tc.env, Domain: "spark.test", PublicBaseURL: "https://spark.test", SecretKey: dbtest.Key,
			SMSDriver: "mock", KYCDriver: "mock", UploadDir: t.TempDir(), AdminPublicBaseURL: tc.adminURL}
		if _, err := app.Build(cfg, pool, slog.New(slog.NewTextHandler(&buf, nil)), app.Options{Clock: clock.NewFake(dbtest.T0)}); err != nil {
			t.Fatalf("%s/%q: %v", tc.env, tc.adminURL, err)
		}
		if got := strings.Contains(buf.String(), "SPARK_ADMIN_PUBLIC_BASE_URL 未设置"); got != tc.warn {
			t.Errorf("%s/%q: warned=%v, want %v", tc.env, tc.adminURL, got, tc.warn)
		}
	}
}
