package agentgw_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"spark-cloud/internal/agentgw"
	"spark-cloud/internal/agentpb"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/identity"
	"spark-cloud/internal/pki"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/secretbox"
	"spark-cloud/internal/plugins/kyc"
	"spark-cloud/internal/plugins/sms"
	"spark-cloud/internal/rental"
	"spark-cloud/internal/wallet"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

type noopNotifier struct{}

func (noopNotifier) NotifyExpiry(context.Context, int64, string, time.Duration) error    { return nil }
func (noopNotifier) NotifyAutoRenewFailed(context.Context, int64, string, string) error  { return nil }
func (noopNotifier) NotifyBookingCancelled(context.Context, int64, string, string) error { return nil }

type env struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	clk    *clock.Fake
	fleet  *fleet.Service
	rental *rental.Service
	wallet *wallet.Service
	ca     *pki.CA
	srv    *agentgw.Server
	addr   string
}

const tick = 20 * time.Millisecond

func newEnvWith(t *testing.T, tweak func(*agentgw.Config)) *env { return newEnvFull(t, tweak, nil) }

// newEnvFull 额外允许改 Deps（替换 HostKeys、注入日志等）。
func newEnvFull(t *testing.T, tweak func(*agentgw.Config), tweakDeps func(*agentgw.Deps)) *env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	box, _ := secretbox.New(dbtest.Key)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ids := identity.NewService(identity.Deps{Pool: pool, Clock: clk, SMS: &sms.Mock{}, KYC: kyc.Mock{}, Box: box})
	w := wallet.NewService(pool, clk)
	f := fleet.NewService(pool, clk)
	ca, err := pki.EnsureCA(ctx, pool, box, clk)
	if err != nil {
		t.Fatal(err)
	}
	cfg := agentgw.Config{AdvertiseAddr: "agent.test:9443", Hosts: []string{"127.0.0.1"}, DNSServers: []string{"223.5.5.5"}, AllowCIDRs: []string{"10.0.9.0/24"},
		BandwidthMbps: 200, DataSizeGiB: 2048, UpstreamPubKey: "ssh-ed25519 AAAAtest", Tick: tick}
	if tweak != nil {
		tweak(&cfg)
	}
	// 与 internal/app 一致：平台入队 Create 的基准端口与网关的 SSHPort 同取 SPARK_UPSTREAM_SSH_PORT
	r := rental.NewService(rental.Deps{Pool: pool, Clock: clk, Box: box, Log: log, Users: ids, Wallet: w, Catalog: catalog.NewService(pool), Fleet: f, Notifier: noopNotifier{},
		SSHBasePort: cfg.SSHPort})
	deps := agentgw.Deps{Pool: pool, Fleet: f, Rental: r, HostKeys: r, CA: ca, Clock: clk, Log: log}
	if tweakDeps != nil {
		tweakDeps(&deps)
	}
	srv, err := agentgw.New(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ctx, lis) }()
	t.Cleanup(func() { cancel(); <-done })
	return &env{t: t, ctx: ctx, pool: pool, clk: clk, fleet: f, rental: r, wallet: w, ca: ca, srv: srv, addr: lis.Addr().String()}
}

func newEnv(t *testing.T) *env { return newEnvWith(t, nil) }

type agentCreds struct {
	key     *ecdsa.PrivateKey
	certDER []byte
	caDER   []byte
}

func (c *agentCreds) tlsCert() tls.Certificate {
	return tls.Certificate{Certificate: [][]byte{c.certDER}, PrivateKey: c.key}
}

func (e *env) machine(serial string) int64 { return dbtest.CreateMachine(e.t, e.pool, serial) }

// dial 建立到网关的 gRPC 连接；creds 为 nil 时不带客户端证书（只能调 Enroll）。
func (e *env) dial(creds *agentCreds) agentpb.AgentGatewayClient {
	e.t.Helper()
	tc := &tls.Config{RootCAs: e.ca.Pool(), ServerName: "127.0.0.1", MinVersion: tls.VersionTLS13}
	if creds != nil {
		tc.Certificates = []tls.Certificate{creds.tlsCert()}
	}
	cc, err := grpc.NewClient(e.addr, grpc.WithTransportCredentials(credentials.NewTLS(tc)))
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = cc.Close() })
	return agentpb.NewAgentGatewayClient(cc)
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func csrFor(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "evil"}, DNSNames: []string{"evil"}}, key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// enroll 走完整接入：签发令牌 → Enroll → 返回可用于 Connect 的凭据。
func (e *env) enroll(machineID int64) *agentCreds {
	e.t.Helper()
	tok, err := e.fleet.IssueEnrollToken(e.ctx, audit.Actor{AdminID: 1}, machineID)
	if err != nil {
		e.t.Fatal(err)
	}
	key := newKey(e.t)
	resp, err := e.dial(nil).Enroll(e.ctx, &agentpb.EnrollRequest{Token: tok.Plain, CsrDer: csrFor(e.t, key)})
	if err != nil {
		e.t.Fatalf("enroll: %v", err)
	}
	return &agentCreds{key: key, certDER: resp.CertDer, caDER: resp.CaDer}
}

// waitFor 轮询直到 cond 为真（最多 wait），否则失败。
func (e *env) waitFor(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.t.Fatalf("timeout waiting for %s", what)
}

type agentStream struct {
	t      *testing.T
	stream agentpb.AgentGateway_ConnectClient
	msgs   chan *agentpb.PlatformMessage
	errs   chan error
}

// connect 打开双向流并在后台收消息；不发 Hello。
func (e *env) connect(creds *agentCreds) *agentStream {
	e.t.Helper()
	stream, err := e.dial(creds).Connect(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	a := &agentStream{t: e.t, stream: stream, msgs: make(chan *agentpb.PlatformMessage, 64), errs: make(chan error, 1)}
	go func() {
		for {
			m, err := stream.Recv()
			if err != nil {
				a.errs <- err
				return
			}
			a.msgs <- m
		}
	}()
	return a
}

func (a *agentStream) send(m *agentpb.AgentMessage) {
	a.t.Helper()
	// io.EOF 表示服务端已结束流（例如认证失败）；真正的状态由 Recv 得到，见 waitErr。
	if err := a.stream.Send(m); err != nil && !errors.Is(err, io.EOF) {
		a.t.Fatalf("send: %v", err)
	}
}
func (a *agentStream) hello(ips ...string) {
	a.send(&agentpb.AgentMessage{Msg: &agentpb.AgentMessage_Hello{Hello: &agentpb.Hello{AgentVersion: "test-0.1", Hostname: "spark", Ips: ips}}})
}
func (a *agentStream) report(states ...*agentpb.InstanceState) {
	a.send(&agentpb.AgentMessage{Msg: &agentpb.AgentMessage_State{State: &agentpb.StateReport{Instances: states}}})
}
func (a *agentStream) heartbeat(m *agentpb.Metrics, inst ...*agentpb.InstanceMetrics) {
	a.send(&agentpb.AgentMessage{Msg: &agentpb.AgentMessage_Heartbeat{Heartbeat: &agentpb.Heartbeat{Metrics: m, Instances: inst}}})
}
func (a *agentStream) result(id string, ok bool, msg string) {
	a.send(&agentpb.AgentMessage{Msg: &agentpb.AgentMessage_Result{Result: &agentpb.CommandResult{CommandId: id, Ok: ok, Message: msg}}})
}
func (a *agentStream) next(timeout time.Duration) *agentpb.PlatformMessage {
	select {
	case m := <-a.msgs:
		return m
	case <-time.After(timeout):
		return nil
	}
}
func (a *agentStream) waitErr(timeout time.Duration) error {
	select {
	case err := <-a.errs:
		return err
	case <-time.After(timeout):
		return nil
	}
}
func running(id string) *agentpb.InstanceState {
	return &agentpb.InstanceState{InstanceId: id, Phase: "running"}
}
func stopped(id string) *agentpb.InstanceState {
	return &agentpb.InstanceState{InstanceId: id, Phase: "stopped"}
}

// up 建流 + Hello + 空 StateReport，并等到平台处理完首个报告（可投递）。
func (e *env) up(creds *agentCreds, machineID int64, states ...*agentpb.InstanceState) *agentStream {
	e.t.Helper()
	a := e.connect(creds)
	a.hello("10.0.0.10")
	if cfg := a.next(2 * time.Second); cfg == nil || cfg.GetConfig() == nil {
		e.t.Fatalf("expected AgentConfig first, got %v", cfg)
	}
	before := e.srv.ReportCount(machineID)
	a.report(states...)
	e.waitFor("first report", func() bool { return e.srv.ReportCount(machineID) > before })
	return a
}

func (e *env) alertCount(kind string) int {
	e.t.Helper()
	var n int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM alerts WHERE kind = $1`, kind).Scan(&n)
	return n
}

// containsJSON 忽略 jsonb 重新格式化造成的空格差异。
func containsJSON(doc, fragment string) bool {
	norm := func(s string) string { return strings.ReplaceAll(s, " ", "") }
	return strings.Contains(norm(doc), norm(fragment))
}

func mustReport(e *env, machineID int64) []byte {
	e.t.Helper()
	var raw []byte
	if err := e.pool.QueryRow(e.ctx, `SELECT last_state_report FROM machines WHERE id = $1`, machineID).Scan(&raw); err != nil {
		e.t.Fatal(err)
	}
	return raw
}
