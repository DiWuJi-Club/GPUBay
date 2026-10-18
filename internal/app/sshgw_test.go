package app_test

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/app"
	"spark-cloud/internal/pki"
	"spark-cloud/internal/platform/config"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/sshgw"
)

// Build 组装 SSH 网关：主机密钥持久化在库里（重建不变），上游签名器来自上游私钥文件，网关可监听并按实例状态给 Banner。
func TestBuildWiresSSHGateway(t *testing.T) {
	keyFile := upstreamKeyFile(t)
	cfg := config.Config{UpstreamSSHKeyFile: keyFile, Env: "dev", Domain: "spark.test", SecretKey: dbtest.Key,
		SMSDriver: "mock", KYCDriver: "mock", UploadDir: t.TempDir()}
	pool := dbtest.New(t)
	a, err := app.Build(cfg, pool, discardLog(), app.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if a.SSH == nil || a.SSHHostKey == nil || a.UpstreamSigner == nil {
		t.Fatal("ssh gateway not wired")
	}
	b, err := app.Build(cfg, pool, discardLog(), app.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.SSHHostKey.PublicKey().Marshal(), b.SSHHostKey.PublicKey().Marshal()) {
		t.Fatal("ssh host key changed across Build")
	}
	pub, err := pki.LoadUpstreamPublicKey(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(ssh.MarshalAuthorizedKey(a.UpstreamSigner.PublicKey()))) != pub {
		t.Fatal("upstream signer does not match the upstream key file")
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.SSH.Serve(ctx, lis) }()
	var banner string
	_, err = ssh.Dial("tcp", lis.Addr().String(), &ssh.ClientConfig{User: "root", Auth: []ssh.AuthMethod{ssh.Password("x")},
		HostKeyCallback: ssh.FixedHostKey(a.SSHHostKey.PublicKey()), Timeout: 5 * time.Second,
		BannerCallback: func(m string) error { banner += m; return nil }})
	if err == nil || !strings.Contains(banner, sshgw.BannerUnknown) {
		t.Fatalf("dial err = %v, banner = %q", err, banner)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ssh gateway Serve did not return")
	}
}
