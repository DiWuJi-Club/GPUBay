package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"spark-cloud/internal/agent/features"
	"spark-cloud/internal/agentpb"
	"spark-cloud/internal/pki"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/secretbox"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

// gateway 签发证书并在 Connect 时下发 AgentConfig，记录收到的 Hello。
type gateway struct {
	agentpb.UnimplementedAgentGatewayServer
	ca     *pki.CA
	mu     sync.Mutex
	hellos []*agentpb.Hello
}

func (g *gateway) Enroll(_ context.Context, req *agentpb.EnrollRequest) (*agentpb.EnrollResponse, error) {
	pub, err := pki.ParseCSR(req.CsrDer)
	if err != nil {
		return nil, err
	}
	der, err := g.ca.IssueMachine(pub, "SN-CMD", time.Now())
	if err != nil {
		return nil, err
	}
	return &agentpb.EnrollResponse{CertDer: der, CaDer: g.ca.CertDER()}, nil
}

func (g *gateway) Connect(stream agentpb.AgentGateway_ConnectServer) error {
	if err := stream.Send(&agentpb.PlatformMessage{Msg: &agentpb.PlatformMessage_Config{Config: &agentpb.AgentConfig{LanIp: "10.0.0.10", HeartbeatIntervalS: 10}}}); err != nil {
		return err
	}
	for {
		m, err := stream.Recv()
		if err != nil {
			return nil
		}
		if h := m.GetHello(); h != nil {
			g.mu.Lock()
			g.hellos = append(g.hellos, h)
			g.mu.Unlock()
		}
	}
}

func (g *gateway) helloCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.hellos)
}

// startGateway 起一个 TLS 网关（签发证书、记录 Hello），返回网关、地址与 CA 指纹。
func startGateway(t *testing.T) (*gateway, string, string) {
	t.Helper()
	pool := dbtest.New(t)
	box, _ := secretbox.New(dbtest.Key)
	ca, err := pki.EnsureCA(context.Background(), pool, box, clock.Real{})
	if err != nil {
		t.Fatal(err)
	}
	certs, err := pki.NewServerCerts(ca, []string{"127.0.0.1"}, clock.Real{})
	if err != nil {
		t.Fatal(err)
	}
	g := &gateway{ca: ca}
	gs := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: ca.Pool(), GetCertificate: certs.GetCertificate})))
	agentpb.RegisterAgentGatewayServer(gs, g)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go gs.Serve(lis) //nolint:errcheck
	t.Cleanup(gs.Stop)
	return g, lis.Addr().String(), ca.Fingerprint()
}

func TestEnrollThenRunFake(t *testing.T) {
	g, addr, fp := startGateway(t)
	root := t.TempDir()
	confDir := filepath.Join(root, "etc", "spark-agent") // 首次运行时目录不存在
	stateDir := filepath.Join(root, "var", "lib", "spark-agent")

	var out bytes.Buffer
	if err := runAgent(context.Background(), []string{"--dir", confDir, "--state-dir", stateDir, "--fake"}, &out); err == nil || !strings.Contains(err.Error(), "enroll") {
		t.Fatalf("run before enroll = %v", err)
	}
	// 指纹带空白和大写、令牌带换行（从终端粘贴）也能接入。
	if err := run([]string{"enroll", "--platform", addr, "--token", " spk1_x\n", "--ca-sha256", " " + strings.ToUpper(fp) + " ", "--dir", confDir}, &out); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if fi, err := os.Stat(confDir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("conf dir mode = %v %v", fi, err)
	}
	for _, name := range []string{"agent.key", "agent.crt", "ca.crt", "config.yaml"} {
		if fi, err := os.Stat(filepath.Join(confDir, name)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v %v", name, fi, err)
		}
	}
	cfg, err := loadAgentConfig(confDir)
	if err != nil || cfg.Platform != addr || cfg.ServerName != "127.0.0.1" {
		t.Fatalf("config = %+v %v", cfg, err)
	}
	if err := run([]string{"enroll", "--platform", addr, "--token", "spk1_x", "--ca-sha256", strings.Repeat("0", 64), "--dir", confDir}, &out); err == nil {
		t.Fatal("wrong CA fingerprint accepted")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runAgent(ctx, []string{"--dir", confDir, "--state-dir", stateDir, "--fake"}, &bytes.Buffer{})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for g.helloCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if g.helloCount() == 0 {
		cancel()
		t.Fatal("agent never connected")
	}
	g.mu.Lock()
	hv := g.hellos[0].GetAgentVersion()
	g.mu.Unlock()
	if hv != version+"+fake" {
		t.Fatalf("hello version = %q", hv)
	}
	if fi, err := os.Stat(stateDir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("state dir mode = %v %v", fi, err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop on cancel")
	}
}

// --fake 下执行器接 clusternet fake：Hello 带 features = SharedSlots|Cluster 与（空的）网卡探测结果。
func TestRunAgentWiresCluster(t *testing.T) {
	g, addr, fp := startGateway(t)
	root := t.TempDir()
	confDir, stateDir := filepath.Join(root, "etc"), filepath.Join(root, "var")
	if err := run([]string{"enroll", "--platform", addr, "--token", "spk1_x", "--ca-sha256", fp, "--dir", confDir}, &bytes.Buffer{}); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runAgent(ctx, []string{"--dir", confDir, "--state-dir", stateDir, "--fake"}, &bytes.Buffer{})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for g.helloCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run returned %v", err)
	}
	if g.helloCount() == 0 {
		t.Fatal("agent never connected")
	}
	g.mu.Lock()
	h := g.hellos[0]
	g.mu.Unlock()
	if want := features.SharedSlots | features.Cluster; h.GetFeatures() != want || want != 3 {
		t.Fatalf("hello features = %d, want %d", h.GetFeatures(), want)
	}
	if h.GetNet() == nil {
		t.Fatal("hello without net info")
	}
}

func TestRunFakeRequiresExplicitStateDir(t *testing.T) {
	err := runAgent(context.Background(), []string{"--dir", t.TempDir(), "--fake"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "--state-dir") {
		t.Fatalf("fake with default state dir = %v", err)
	}
}

func TestEnsurePrivateDirDoesNotLoosenOrChmodExisting(t *testing.T) {
	root := t.TempDir()
	fresh := filepath.Join(root, "a", "b")
	if err := ensurePrivateDir(fresh); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(fresh); fi.Mode().Perm() != 0o700 {
		t.Fatalf("created mode = %v", fi.Mode())
	}
	if err := ensurePrivateDir(fresh); err != nil { // 已存在且 0700：通过
		t.Fatal(err)
	}
	shared := filepath.Join(root, "shared")
	_ = os.Mkdir(shared, 0o755)
	_ = os.Chmod(shared, 0o755)
	if err := ensurePrivateDir(shared); err == nil {
		t.Fatal("pre-existing 0755 dir accepted")
	}
	if fi, _ := os.Stat(shared); fi.Mode().Perm() != 0o755 {
		t.Fatalf("existing dir was chmodded to %v", fi.Mode())
	}
}

func TestWritePrivateFileReplacesAtomically(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "agent.crt")
	_ = os.WriteFile(p, []byte("old"), 0o644)
	if err := writePrivateFile(p, []byte("new")); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	fi, _ := os.Stat(p)
	entries, _ := os.ReadDir(dir)
	if string(b) != "new" || fi.Mode().Perm() != 0o600 || len(entries) != 1 {
		t.Fatalf("content=%q mode=%v entries=%d", b, fi.Mode(), len(entries))
	}
}
