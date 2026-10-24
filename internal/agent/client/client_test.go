package client_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"spark-cloud/internal/agent/client"
	diskfake "spark-cloud/internal/agent/disk/fake"
	"spark-cloud/internal/agent/executor"
	"spark-cloud/internal/agent/metrics"
	netfake "spark-cloud/internal/agent/netfilter/fake"
	rtfake "spark-cloud/internal/agent/runtime/fake"
	"spark-cloud/internal/agent/state"
	"spark-cloud/internal/agentpb"
	"spark-cloud/internal/pki"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/secretbox"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

// fakeGateway 记录收到的消息，并允许测试向流推送指令。
type fakeGateway struct {
	agentpb.UnimplementedAgentGatewayServer
	mu       sync.Mutex
	received []*agentpb.AgentMessage
	streams  []agentpb.AgentGateway_ConnectServer
	tokens   []string
	csrs     [][]byte
	ca       *pki.CA
	drop     chan struct{} // 关闭当前流
	refuse   bool          // 为真时拒绝新流（模拟平台不可达）
	reject   bool          // 为真时立即返回 Unauthenticated（模拟证书被撤销）
	rejected chan struct{} // 非 nil 时，拒绝流的 Connect 返回时关闭（「网关已返回」信号）
}

func (g *fakeGateway) Enroll(_ context.Context, req *agentpb.EnrollRequest) (*agentpb.EnrollResponse, error) {
	g.mu.Lock()
	g.tokens = append(g.tokens, req.Token)
	g.csrs = append(g.csrs, req.CsrDer)
	g.mu.Unlock()
	pub, err := pki.ParseCSR(req.CsrDer)
	if err != nil {
		return nil, err
	}
	der, err := g.ca.IssueMachine(pub, "SN-TEST", time.Now())
	if err != nil {
		return nil, err
	}
	return &agentpb.EnrollResponse{CertDer: der, CaDer: g.ca.CertDER()}, nil
}

func (g *fakeGateway) Connect(stream agentpb.AgentGateway_ConnectServer) error {
	g.mu.Lock()
	if g.refuse {
		g.mu.Unlock()
		return status.Error(codes.Unavailable, "refused")
	}
	if g.reject {
		if g.rejected != nil {
			defer close(g.rejected)
			g.rejected = nil // 只关闭一次
		}
		g.mu.Unlock()
		return status.Error(codes.Unauthenticated, "certificate not recognised")
	}
	g.streams = append(g.streams, stream)
	g.drop = make(chan struct{})
	drop := g.drop
	g.mu.Unlock()
	_ = stream.Send(&agentpb.PlatformMessage{Msg: &agentpb.PlatformMessage_Config{Config: &agentpb.AgentConfig{LanIp: "10.0.0.10", HeartbeatIntervalS: 1, BandwidthMbps: 200, DataSizeGib: 1}}})
	errc := make(chan error, 1)
	go func() {
		for {
			m, err := stream.Recv()
			if err != nil {
				errc <- err
				return
			}
			g.mu.Lock()
			g.received = append(g.received, m)
			g.mu.Unlock()
		}
	}()
	select {
	case <-drop:
		return nil
	case err := <-errc:
		if err == io.EOF {
			return nil
		}
		return err
	}
}

func (g *fakeGateway) msgs() []*agentpb.AgentMessage {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]*agentpb.AgentMessage(nil), g.received...)
}

func (g *fakeGateway) push(cmd *agentpb.Command) {
	g.mu.Lock()
	s := g.streams[len(g.streams)-1]
	g.mu.Unlock()
	_ = s.Send(&agentpb.PlatformMessage{Msg: &agentpb.PlatformMessage_Command{Command: cmd}})
}

func (g *fakeGateway) dropStream() {
	g.mu.Lock()
	close(g.drop)
	g.mu.Unlock()
}

func (g *fakeGateway) setRefuse(v bool) {
	g.mu.Lock()
	g.refuse = v
	g.mu.Unlock()
}

type harness struct {
	t         *testing.T
	gw        *fakeGateway
	addr      string
	ca        *pki.CA
	key       *ecdsa.PrivateKey
	dir       string // 最近一个 agent 的状态目录
	collect   func(context.Context) metrics.Metrics
	helloGate chan struct{} // 非 nil 时，构造 Hello 前等它关闭（LocalIPs 在建流之后、发送 Hello 之前被调用）
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	pool := dbtest.New(t)
	box, _ := secretbox.New(dbtest.Key)
	ca, err := pki.EnsureCA(context.Background(), pool, box, clock.Real{})
	if err != nil {
		t.Fatal(err)
	}
	certs, _ := pki.NewServerCerts(ca, []string{"127.0.0.1"}, clock.Real{})
	gw := &fakeGateway{ca: ca}
	gs := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: ca.Pool(), GetCertificate: certs.GetCertificate})))
	agentpb.RegisterAgentGatewayServer(gs, gw)
	lis, _ := net.Listen("tcp", "127.0.0.1:0")
	go gs.Serve(lis) //nolint:errcheck
	t.Cleanup(gs.Stop)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	return &harness{t: t, gw: gw, addr: lis.Addr().String(), ca: ca, key: key,
		collect: func(context.Context) metrics.Metrics { return metrics.Metrics{Load1: 1} }}
}

func (h *harness) waitHelloGate() {
	if h.helloGate == nil {
		return
	}
	select {
	case <-h.helloGate:
	case <-time.After(5 * time.Second):
		h.t.Error("gateway never returned the rejection")
	}
}

func (h *harness) agent(stepped bool) (*client.Client, *executor.Executor, *rtfake.Runtime) {
	h.t.Helper()
	return h.agentWith(stepped, false)
}

func (h *harness) agentWith(stepped, noReconnect bool) (*client.Client, *executor.Executor, *rtfake.Runtime) {
	h.t.Helper()
	certDER, caDER, err := client.Enroll(context.Background(), h.addr, "spk1_x", h.ca.Fingerprint(), h.key)
	if err != nil {
		h.t.Fatal(err)
	}
	tc, err := client.TLSFor(h.key, certDER, caDER, "127.0.0.1")
	if err != nil {
		h.t.Fatal(err)
	}
	dir := h.t.TempDir()
	h.dir = dir
	st, _ := state.Open(filepath.Join(dir, "state.json"))
	rt := rtfake.New()
	ex := executor.New(executor.Deps{Runtime: rt, Disk: diskfake.New(4000 << 30), Net: netfake.New(), State: st, StateDir: dir,
		LocalIPs: func() []string { return []string{"10.0.0.10"} }, MemTotal: func() uint64 { return 128 << 30 },
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, stepped)
	c := client.New(client.Config{Addr: h.addr, TLS: tc, Version: "test", Hostname: "h", LocalIPs: func() []string { h.waitHelloGate(); return []string{"10.0.0.10"} },
		Stepped: stepped, HeartbeatInterval: 50 * time.Millisecond, ReportInterval: 80 * time.Millisecond, Backoff: client.Backoff{Min: 10 * time.Millisecond, Max: 50 * time.Millisecond},
		NoReconnect: noReconnect},
		ex, h.collect, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return c, ex, rt
}

// start 运行客户端（auto 时也运行执行器），测试结束时先停掉它们再清理临时目录。
func (h *harness) start(c *client.Client, ex *executor.Executor, auto bool) context.Context {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = c.Run(ctx) }()
	if auto {
		wg.Add(1)
		go func() { defer wg.Done(); ex.Run(ctx) }() // 自动模式的消费 goroutine 在 Executor.Run 里启动
	}
	h.t.Cleanup(func() { cancel(); wg.Wait() })
	return ctx
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

func kinds(ms []*agentpb.AgentMessage) []string {
	var out []string
	for _, m := range ms {
		switch m.Msg.(type) {
		case *agentpb.AgentMessage_Hello:
			out = append(out, "hello")
		case *agentpb.AgentMessage_State:
			out = append(out, "state")
		case *agentpb.AgentMessage_Heartbeat:
			out = append(out, "heartbeat")
		case *agentpb.AgentMessage_Result:
			out = append(out, "result")
		}
	}
	return out
}

func count(ms []*agentpb.AgentMessage, kind string) int {
	n := 0
	for _, k := range kinds(ms) {
		if k == kind {
			n++
		}
	}
	return n
}

func TestEnrollPinsCAFingerprint(t *testing.T) {
	h := newHarness(t)
	if _, _, err := client.Enroll(context.Background(), h.addr, "spk1_x", "deadbeef", h.key); err == nil {
		t.Fatal("wrong CA fingerprint accepted")
	}
	// 格式正确但不是该 CA 的指纹：TLS 握手阶段就拒绝，令牌不会被发出去
	other := pki.Fingerprint([]byte("some other CA"))
	if _, _, err := client.Enroll(context.Background(), h.addr, "spk1_x", other, h.key); err == nil {
		t.Fatal("foreign CA fingerprint accepted")
	}
	h.gw.mu.Lock()
	n := len(h.gw.tokens)
	h.gw.mu.Unlock()
	if n != 0 {
		t.Fatalf("token sent to an unpinned server %d times", n)
	}
	certDER, caDER, err := client.Enroll(context.Background(), h.addr, "spk1_x", h.ca.Fingerprint(), h.key)
	if err != nil || pki.Fingerprint(caDER) != h.ca.Fingerprint() {
		t.Fatalf("enroll = %v", err)
	}
	cert, _ := x509.ParseCertificate(certDER)
	if cert.Subject.CommonName != "SN-TEST" {
		t.Fatalf("cn = %s", cert.Subject.CommonName)
	}
}

func TestEnrollTrimsTokenAndSendsOnlyCSR(t *testing.T) {
	h := newHarness(t)
	if _, _, err := client.Enroll(context.Background(), h.addr, "  spk1_abc\r\n", h.ca.Fingerprint(), h.key); err != nil {
		t.Fatal(err)
	}
	h.gw.mu.Lock()
	defer h.gw.mu.Unlock()
	if h.gw.tokens[0] != "spk1_abc" {
		t.Fatalf("token = %q", h.gw.tokens[0])
	}
	req, err := x509.ParseCertificateRequest(h.gw.csrs[0])
	if err != nil {
		t.Fatal(err)
	}
	if !req.PublicKey.(*ecdsa.PublicKey).Equal(&h.key.PublicKey) {
		t.Fatal("CSR does not carry the local public key")
	}
}

func TestLoadOrCreateKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.key")
	k1, err := client.LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if k1.Curve != elliptic.P256() {
		t.Fatal("not P-256")
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v %v", fi.Mode().Perm(), err)
	}
	k2, err := client.LoadOrCreateKey(path)
	if err != nil || !k2.Equal(k1) {
		t.Fatalf("reload = %v", err)
	}
	if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.LoadOrCreateKey(path); err == nil {
		t.Fatal("garbage key accepted")
	}
}

func TestAutoModeHelloReportHeartbeatsAndResults(t *testing.T) {
	h := newHarness(t)
	c, ex, rt := h.agent(false)
	ctx := h.start(c, ex, true)
	if err := c.WaitConnected(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "hello+state+heartbeat", func() bool { k := kinds(h.gw.msgs()); return len(k) >= 3 && k[0] == "hello" && k[1] == "state" })
	waitFor(t, "periodic heartbeat", func() bool { return count(h.gw.msgs(), "heartbeat") >= 2 })
	_, hostKey, err := pki.GenerateInstanceHostKey()
	if err != nil {
		t.Fatal(err)
	}
	cmd := &agentpb.Command{Id: "cmd-1", InstanceId: "ins-aaaaaaaa", Body: &agentpb.Command_Create{Create: &agentpb.CreateInstance{ImageRef: "img:1", UpstreamPubkey: "k", BandwidthMbps: 200, SshPort: 2222, DataSizeGib: 1, HostKeyPem: string(hostKey)}}}
	h.gw.push(cmd)
	h.gw.push(cmd) // 网关可能重复推送同一指令：执行器去重
	waitFor(t, "result followed by a state report", func() bool {
		ms := h.gw.msgs()
		ok := false
		for i, m := range ms {
			if r := m.GetResult(); r != nil && r.CommandId == "cmd-1" {
				if i+1 >= len(ms) {
					return false // 每条结果（含重复推送的回放）后面都要等到下一条消息
				}
				ok = ok || r.Ok
			}
		}
		return ok
	})
	if !c.Received("cmd-1") || !rt.Containers()["ins-aaaaaaaa"].Running {
		t.Fatal("command not executed")
	}
	if got, _ := os.ReadFile(filepath.Join(h.dir, "ssh", "ins-aaaaaaaa", "hostkeys", "ssh_host_ed25519_key")); string(got) != string(hostKey) {
		t.Fatal("host_key_pem not carried through to the container ssh dir")
	}
	// 结果之后紧跟一份 StateReport，且含 running
	ms := h.gw.msgs()
	for i, m := range ms {
		if r := m.GetResult(); r != nil && r.CommandId == "cmd-1" {
			if !r.Ok {
				t.Fatalf("duplicate push produced a failure: %v", r)
			}
			if i+1 >= len(ms) || ms[i+1].GetState() == nil || ms[i+1].GetState().Instances[0].Phase != "running" {
				t.Fatalf("no state report after result: %v", kinds(ms))
			}
		}
	}
	waitFor(t, "result marked sent", func() bool { return len(ex.StateStore().UnsentResults()) == 0 })
	if n := count(h.gw.msgs(), "state"); n < 2 {
		t.Fatalf("periodic state reports = %d", n)
	}
}

// Results() 满时结果只留在 state.json（Sent=false）：心跳之后补发，不必等重连。
func TestHeartbeatResendsUnsentResults(t *testing.T) {
	h := newHarness(t)
	c, ex, _ := h.agent(false)
	ctx := h.start(c, ex, false)
	if err := c.WaitConnected(ctx); err != nil {
		t.Fatal(err)
	}
	if err := ex.StateStore().PutResult("cmd-dropped", state.Result{OK: true}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "resend after heartbeat", func() bool {
		for _, m := range h.gw.msgs() {
			if r := m.GetResult(); r != nil && r.CommandId == "cmd-dropped" {
				return true
			}
		}
		return false
	})
	waitFor(t, "marked sent", func() bool { r, _ := ex.StateStore().Result("cmd-dropped"); return r.Sent })
}

// nvidia-smi 卡死（采集函数不理会 ctx）也不能拖住心跳。
func TestHungCollectorDoesNotStallHeartbeat(t *testing.T) {
	h := newHarness(t)
	release := make(chan struct{})
	defer close(release)
	var calls atomic.Int32
	h.collect = func(context.Context) metrics.Metrics { calls.Add(1); <-release; return metrics.Metrics{} }
	c, ex, _ := h.agent(false)
	client.SetMetricsTimeoutForTest(c, 20*time.Millisecond)
	ctx := h.start(c, ex, false)
	if err := c.WaitConnected(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "heartbeats despite hung collector", func() bool { return count(h.gw.msgs(), "heartbeat") >= 3 })
	if n := calls.Load(); n != 1 { // 单飞：卡住的采集没返回前不再起新的
		t.Fatalf("collector started %d times while hung", n)
	}
}

// 结果已发出但「已发送」标记落盘失败（根分区写满）：只记日志，会话不断、心跳照常，不会反复重连。
func TestMarkSentFailureDoesNotEndSession(t *testing.T) {
	h := newHarness(t)
	c, ex, _ := h.agent(false)
	if err := ex.StateStore().PutResult("cmd-x", state.Result{OK: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(h.dir, 0o500); err != nil { // 状态文件从此写不进去
		t.Fatal(err)
	}
	dir := h.dir
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := ex.StateStore().PutResult("probe", state.Result{}); err == nil {
		t.Skip("state dir still writable (running as root?)")
	}
	ctx := h.start(c, ex, false)
	if err := c.WaitConnected(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "heartbeats keep flowing", func() bool { return count(h.gw.msgs(), "heartbeat") >= 4 })
	ms := h.gw.msgs()
	if n := count(ms, "hello"); n != 1 {
		t.Fatalf("session flapped: %d hellos %v", n, kinds(ms))
	}
	found := false
	for _, m := range ms {
		if r := m.GetResult(); r != nil && r.CommandId == "cmd-x" {
			found = true
		}
	}
	if !found {
		t.Fatal("unsent result not resent")
	}
}

// 并发的 SendHeartbeat 不能抢在 Hello 之前：网关要求第一条消息是 Hello。
func TestHelloIsAlwaysFirst(t *testing.T) {
	h := newHarness(t)
	c, ex, _ := h.agent(true)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				_ = c.SendHeartbeat(context.Background())
			}
		}
	}()
	ctx := h.start(c, ex, false)
	err := c.WaitConnected(ctx)
	close(stop)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	if k := kinds(h.gw.msgs()); len(k) < 2 || k[0] != "hello" || k[1] != "state" {
		t.Fatalf("first messages = %v", k)
	}
}

func TestReceivedIsBounded(t *testing.T) {
	h := newHarness(t)
	c, ex, _ := h.agent(true)
	ctx := h.start(c, ex, false)
	if err := c.WaitConnected(ctx); err != nil {
		t.Fatal(err)
	}
	const n = 1001
	for i := 0; i < n; i++ {
		h.gw.push(&agentpb.Command{Id: fmt.Sprintf("cmd-%d", i), Body: &agentpb.Command_Pull{Pull: &agentpb.PullImage{ImageRef: "img:1"}}})
	}
	waitFor(t, "last received", func() bool { return c.Received(fmt.Sprintf("cmd-%d", n-1)) })
	if c.Received("cmd-0") {
		t.Fatal("oldest command id not evicted")
	}
	if !c.Received("cmd-1") {
		t.Fatal("evicted too much")
	}
}

func TestReconnectResendsUnsentResultsAfterHello(t *testing.T) {
	h := newHarness(t)
	c, ex, _ := h.agent(true)
	ctx := h.start(c, ex, false)
	if err := c.WaitConnected(ctx); err != nil {
		t.Fatal(err)
	}
	// 断开网关侧的流（并暂时拒绝重连），再在断线期间执行指令：结果发送失败，重连后补发
	h.gw.push(&agentpb.Command{Id: "cmd-1", InstanceId: "ins-aaaaaaaa", Body: &agentpb.Command_Create{Create: &agentpb.CreateInstance{ImageRef: "img:1", UpstreamPubkey: "k", DataSizeGib: 1}}})
	waitFor(t, "received", func() bool { return c.Received("cmd-1") })
	h.gw.setRefuse(true)
	h.gw.dropStream()
	waitFor(t, "disconnected", func() bool { return !c.Connected() })
	if res := ex.RunPending(ctx, nil); len(res) != 1 || !res[0].OK { // 断线期间执行，结果留在 state.json（未发送）
		t.Fatalf("executed %+v", res)
	}
	if _, err := c.Step(ctx, nil); err == nil {
		t.Fatal("step while disconnected should fail")
	}
	h.gw.setRefuse(false)
	waitFor(t, "reconnected", func() bool { return c.Connected() })
	waitFor(t, "resend", func() bool {
		k := kinds(h.gw.msgs())
		// 第二条流：hello, state, result(补发), state
		idx := 0
		for i, x := range k {
			if x == "hello" {
				idx = i
			}
		}
		rest := k[idx:]
		return idx > 0 && len(rest) >= 4 && rest[0] == "hello" && rest[1] == "state" && rest[2] == "result" && rest[3] == "state"
	})
	waitFor(t, "marked sent", func() bool { return len(ex.StateStore().UnsentResults()) == 0 })
}

func TestSteppedModeSendsNothingAutomatically(t *testing.T) {
	h := newHarness(t)
	c, ex, _ := h.agent(true)
	ctx := h.start(c, ex, false)
	if err := c.WaitConnected(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "hello+state recorded", func() bool { return len(h.gw.msgs()) >= 2 })
	time.Sleep(300 * time.Millisecond) // 否定断言：远长于心跳与上报间隔，期间不应再有自动消息
	if k := kinds(h.gw.msgs()); len(k) != 2 {
		t.Fatalf("stepped agent sent %v", k)
	}
	if err := c.SendHeartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	h.gw.push(&agentpb.Command{Id: "cmd-1", InstanceId: "ins-aaaaaaaa", Body: &agentpb.Command_Create{Create: &agentpb.CreateInstance{ImageRef: "img:1", DataSizeGib: 1}}})
	waitFor(t, "received", func() bool { return c.Received("cmd-1") })
	n, err := c.Step(ctx, &state.Result{OK: false, Message: "simulated"})
	if err != nil || n != 1 {
		t.Fatalf("step = %d %v", n, err)
	}
	waitFor(t, "result+state", func() bool {
		k := kinds(h.gw.msgs())
		return len(k) == 5 && k[2] == "heartbeat" && k[3] == "result" && k[4] == "state"
	})
	if r := h.gw.msgs()[3].GetResult(); r.Ok || r.Message != "simulated" {
		t.Fatalf("result = %v", r)
	}
}

func TestNoReconnectReturnsAfterSession(t *testing.T) {
	h := newHarness(t)
	c, _, _ := h.agentWith(true, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	if err := c.WaitConnected(ctx); err != nil {
		t.Fatal(err)
	}
	h.gw.dropStream()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	if c.Connected() || c.ConfigReceived() {
		t.Fatal("still marked connected")
	}
}

// 网关立即以 Unauthenticated 终止流时，session 的错误要是真实状态，而不是初始发送返回的 EOF。
func TestSessionSurfacesGatewayRejection(t *testing.T) {
	h := newHarness(t)
	c, _, _ := h.agentWith(true, true)
	h.gw.mu.Lock()
	h.gw.reject = true
	h.helloGate = make(chan struct{})
	h.gw.rejected = h.helloGate
	h.gw.mu.Unlock()
	// 等网关的 Connect 带着 Unauthenticated 返回后再发 Hello，Send 走 EOF 路径（不再靠固定 sleep）
	err := c.Run(context.Background())
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("err = %v (code %v), want Unauthenticated", err, status.Code(err))
	}
}

func TestBackoff(t *testing.T) {
	b := client.Backoff{}
	for i := 0; i < 10; i++ {
		d := b.Next(i)
		if d < 800*time.Millisecond || d > 72*time.Second {
			t.Fatalf("attempt %d = %v", i, d)
		}
		if i >= 7 && d < 48*time.Second { // 达到上限 60s ± 20%
			t.Fatalf("attempt %d = %v, want near max", i, d)
		}
	}
	if d := b.Next(0); d > 1200*time.Millisecond {
		t.Fatalf("first = %v", d)
	}
	if d := b.Next(1000); d < 48*time.Second || d > 72*time.Second { // 大 attempt 不溢出
		t.Fatalf("huge attempt = %v", d)
	}
}

// StateReport 带上运行时报告的已缓存镜像；拿不到列表（nil）时不带该字段，同旧 Agent。
func TestStateReportCarriesImages(t *testing.T) {
	h := newHarness(t)
	c, ex, rt := h.agent(true)
	rt.SetImages([]string{"spark/llm-serving:25.09"})
	ctx := h.start(c, ex, false)
	if err := c.WaitConnected(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "hello+state", func() bool { return len(h.gw.msgs()) >= 2 })
	if st := h.gw.msgs()[1].GetState(); st == nil || !slices.Equal(st.GetImages(), []string{"spark/llm-serving:25.09"}) {
		t.Fatalf("first state = %v", h.gw.msgs()[1])
	}
	rt.FailNext("images", errors.New("docker down"))
	if err := c.SendStateReport(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "second state", func() bool { return len(h.gw.msgs()) >= 3 })
	if st := h.gw.msgs()[2].GetState(); st == nil || st.GetImages() != nil {
		t.Fatalf("state after images error = %v", h.gw.msgs()[2])
	}
}
