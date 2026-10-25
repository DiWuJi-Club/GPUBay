package fakeagent_test

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"spark-cloud/internal/agent/fakeagent"
	"spark-cloud/internal/agent/features"
	"spark-cloud/internal/agent/metrics"
	"spark-cloud/internal/agentpb"
	"spark-cloud/internal/pki"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/secretbox"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

// gateway 是最小的网关：Enroll 签证书，Connect 先发 AgentConfig，随后可推指令。
type gateway struct {
	agentpb.UnimplementedAgentGatewayServer
	ca      *pki.CA
	mu      sync.Mutex
	enrolls int
	hellos  int
	feats   []uint32 // 每次 Hello 的 features
	beats   []*agentpb.Heartbeat
	results []string
	refuse  bool
	stream  agentpb.AgentGateway_ConnectServer
}

func (g *gateway) Enroll(_ context.Context, req *agentpb.EnrollRequest) (*agentpb.EnrollResponse, error) {
	pub, err := pki.ParseCSR(req.CsrDer)
	if err != nil {
		return nil, err
	}
	der, err := g.ca.IssueMachine(pub, "SN-FAKE", time.Now())
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	g.enrolls++
	g.mu.Unlock()
	return &agentpb.EnrollResponse{CertDer: der, CaDer: g.ca.CertDER()}, nil
}

func (g *gateway) Connect(stream agentpb.AgentGateway_ConnectServer) error {
	g.mu.Lock()
	if g.refuse {
		g.mu.Unlock()
		return status.Error(codes.PermissionDenied, "revoked")
	}
	g.stream = stream
	g.mu.Unlock()
	if err := stream.Send(&agentpb.PlatformMessage{Msg: &agentpb.PlatformMessage_Config{Config: &agentpb.AgentConfig{LanIp: "10.0.0.10", HeartbeatIntervalS: 10, DataSizeGib: 1}}}); err != nil {
		return err
	}
	for {
		m, err := stream.Recv()
		if err != nil {
			return nil
		}
		g.mu.Lock()
		switch {
		case m.GetHello() != nil:
			g.hellos++
			g.feats = append(g.feats, m.GetHello().GetFeatures())
		case m.GetHeartbeat() != nil:
			g.beats = append(g.beats, m.GetHeartbeat())
		case m.GetResult() != nil:
			g.results = append(g.results, m.GetResult().GetCommandId())
		}
		g.mu.Unlock()
	}
}

func (g *gateway) push(cmd *agentpb.Command) error {
	g.mu.Lock()
	s := g.stream
	g.mu.Unlock()
	return s.Send(&agentpb.PlatformMessage{Msg: &agentpb.PlatformMessage_Command{Command: cmd}})
}

func (g *gateway) count() (enrolls, hellos int, results []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.enrolls, g.hellos, append([]string(nil), g.results...)
}

func newGateway(t *testing.T) (*gateway, string) {
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
	return g, lis.Addr().String()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout: %s", what)
}

func TestStartEnrollsConnectsAndReconnectsWithSameCreds(t *testing.T) {
	g, addr := newGateway(t)
	ctx := context.Background()
	ag, err := fakeagent.Start(ctx, fakeagent.Options{Addr: addr, CAPEM: g.ca.CertPEM(), Token: "spk1_x", Dir: t.TempDir(), Stepped: true})
	if err != nil {
		t.Fatal(err)
	}
	if !ag.Client.ConfigReceived() || ag.Creds() == nil || ag.Exec.Config().LanIP != "10.0.0.10" {
		t.Fatalf("not configured: %v %+v", ag.Client.ConfigReceived(), ag.Exec.Config())
	}
	// 步进模式：指令只入队，Step 时才执行并回报。
	if err := g.push(&agentpb.Command{Id: "c1", InstanceId: "i1", Body: &agentpb.Command_Pull{Pull: &agentpb.PullImage{ImageRef: "img:1"}}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "command received", func() bool { return ag.Client.Received("c1") })
	if _, _, res := g.count(); len(res) != 0 {
		t.Fatalf("stepped agent executed without Step: %v", res)
	}
	if n, err := ag.Client.Step(ctx, nil); err != nil || n != 1 {
		t.Fatalf("step = %d %v", n, err)
	}
	waitFor(t, "result reported", func() bool { _, _, res := g.count(); return len(res) == 1 && res[0] == "c1" })
	ag.Stop()

	again, err := fakeagent.Start(ctx, fakeagent.Options{Addr: addr, Creds: ag.Creds(), Dir: ag.Dir, Stepped: true})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Stop()
	waitFor(t, "second hello", func() bool { _, hellos, _ := g.count(); return hellos == 2 })
	if enrolls, _, _ := g.count(); enrolls != 1 {
		t.Fatalf("enrolls=%d, want 1 (creds reused)", enrolls)
	}
	if r, ok := again.Exec.StateStore().Result("c1"); !ok || !r.OK { // 同一状态目录：已执行的结果保留
		t.Fatalf("state not reused: %+v %v", r, ok)
	}
}

func TestStartNoReconnectFailsFastWhenRefused(t *testing.T) {
	g, addr := newGateway(t)
	ag, err := fakeagent.Start(context.Background(), fakeagent.Options{Addr: addr, CAPEM: g.ca.CertPEM(), Token: "spk1_x", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ag.Stop()
	g.mu.Lock()
	g.refuse = true
	g.mu.Unlock()
	start := time.Now()
	_, err = fakeagent.Start(context.Background(), fakeagent.Options{Addr: addr, Creds: ag.Creds(), Dir: ag.Dir, NoReconnect: true})
	if err == nil || !strings.Contains(err.Error(), "revoked") || time.Since(start) > 5*time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
}

func TestStopRemovesOnlyOwnTempDir(t *testing.T) {
	g, addr := newGateway(t)
	ag, err := fakeagent.Start(context.Background(), fakeagent.Options{Addr: addr, CAPEM: g.ca.CertPEM(), Token: "spk1_x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ag.Dir); err != nil {
		t.Fatal(err)
	}
	ag.Stop()
	if _, err := os.Stat(ag.Dir); !os.IsNotExist(err) {
		t.Fatalf("temp dir not removed: %v", err)
	}
	own := t.TempDir()
	ag, err = fakeagent.Start(context.Background(), fakeagent.Options{Addr: addr, Creds: ag.Creds(), Dir: own})
	if err != nil {
		t.Fatal(err)
	}
	ag.Stop()
	if _, err := os.Stat(own); err != nil {
		t.Fatalf("caller dir must be kept: %v", err)
	}
}

func TestStartReturnsCallerCancellation(t *testing.T) {
	g, addr := newGateway(t)
	ag, err := fakeagent.Start(context.Background(), fakeagent.Options{Addr: addr, CAPEM: g.ca.CertPEM(), Token: "spk1_x", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ag.Stop()
	g.mu.Lock()
	g.refuse = true // 会话一直失败、一直重连，Start 只能靠 ctx 结束
	g.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = fakeagent.Start(ctx, fakeagent.Options{Addr: addr, Creds: ag.Creds(), Dir: ag.Dir})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
}

// Hello.features 默认 SharedSlots，可设为 0 模拟旧 Agent；心跳带每实例指标（本地实例 + 注入值）。
func TestFeaturesAndInstanceMetrics(t *testing.T) {
	g, addr := newGateway(t)
	ctx := context.Background()
	old, err := fakeagent.Start(ctx, fakeagent.Options{Addr: addr, CAPEM: g.ca.CertPEM(), Token: "spk1_x", Stepped: true, Features: new(uint32)})
	if err != nil {
		t.Fatal(err)
	}
	old.Stop()
	ag, err := fakeagent.Start(ctx, fakeagent.Options{Addr: addr, Creds: old.Creds(), Stepped: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Stop()
	waitFor(t, "two hellos", func() bool { _, h, _ := g.count(); return h == 2 })
	g.mu.Lock()
	feats := append([]uint32(nil), g.feats...)
	g.mu.Unlock()
	if feats[0] != 0 || feats[1] != features.SharedSlots {
		t.Fatalf("features = %v", feats)
	}
	if err := g.push(&agentpb.Command{Id: "c-create", InstanceId: "ins-aaaaaaaa", Body: &agentpb.Command_Create{Create: &agentpb.CreateInstance{
		ImageRef: "img:1", UpstreamPubkey: "ssh-ed25519 AAAA", SshPort: 2223, DataSizeGib: 8, ShareSlots: 2}}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "create received", func() bool { return ag.Client.Received("c-create") })
	if _, err := ag.Client.Step(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if sp, ok := ag.Runtime.Spec("ins-aaaaaaaa"); !ok || sp.CPUs != 10 || sp.PidsLimit != 16384 || sp.MemoryBytes != 60<<30 || ag.Disk.SizeGiB("ins-aaaaaaaa") != 4 {
		t.Fatalf("spec = %+v disk = %d", sp, ag.Disk.SizeGiB("ins-aaaaaaaa"))
	}
	ag.SetInstanceMetrics("ins-aaaaaaaa", metrics.InstanceMetrics{MemCurrent: 58 << 30, MemMax: 60 << 30, CPUPercent: 300})
	ag.SetInstanceMetrics("ins-zzzzzzzz", metrics.InstanceMetrics{MemCurrent: 1})
	if err := ag.Client.SendHeartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	ag.ClearInstanceMetrics("ins-zzzzzzzz")
	ag.ClearInstanceMetrics("ins-aaaaaaaa")
	if err := ag.Client.SendHeartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "two heartbeats", func() bool { g.mu.Lock(); defer g.mu.Unlock(); return len(g.beats) >= 2 })
	g.mu.Lock()
	b1, b2 := g.beats[0].GetInstances(), g.beats[1].GetInstances()
	g.mu.Unlock()
	if len(b1) != 2 || b1[0].GetInstanceId() != "ins-aaaaaaaa" || b1[0].GetMemCurrent() != 58<<30 || b1[0].GetMemMax() != 60<<30 || b1[0].GetCpuPercent() != 300 ||
		b1[1].GetInstanceId() != "ins-zzzzzzzz" || b1[1].GetMemCurrent() != 1 {
		t.Fatalf("first heartbeat instances = %v", b1)
	}
	if len(b2) != 1 || b2[0].GetInstanceId() != "ins-aaaaaaaa" || b2[0].GetMemCurrent() != 0 {
		t.Fatalf("second heartbeat instances = %v", b2)
	}
}
