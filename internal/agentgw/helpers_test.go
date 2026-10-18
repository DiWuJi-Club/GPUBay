package agentgw_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log/slog"
	"net"
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

func (noopNotifier) NotifyExpiry(context.Context, int64, string, time.Duration) error { return nil }

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

func newEnvWith(t *testing.T, tweak func(*agentgw.Config)) *env {
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
	r := rental.NewService(rental.Deps{Pool: pool, Clock: clk, Box: box, Log: log, Users: ids, Wallet: w, Catalog: catalog.NewService(pool), Fleet: f, Notifier: noopNotifier{}})
	ca, err := pki.EnsureCA(ctx, pool, box, clk)
	if err != nil {
		t.Fatal(err)
	}
	cfg := agentgw.Config{AdvertiseAddr: "agent.test:9443", Hosts: []string{"127.0.0.1"}, DNSServers: []string{"223.5.5.5"}, AllowCIDRs: []string{"10.0.9.0/24"},
		BandwidthMbps: 200, DataSizeGiB: 2048, UpstreamPubKey: "ssh-ed25519 AAAAtest", Tick: tick}
	if tweak != nil {
		tweak(&cfg)
	}
	srv, err := agentgw.New(cfg, agentgw.Deps{Pool: pool, Fleet: f, Rental: r, CA: ca, Clock: clk, Log: log})
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
