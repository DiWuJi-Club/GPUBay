// Package agentgw 是平台侧 Agent 网关：mTLS gRPC 服务，负责接入（Enroll）、双向流（Connect）、指令投递、心跳与对账。
package agentgw

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/peer"

	"spark-cloud/internal/agentpb"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/pki"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/rental"
)

type Config struct {
	AdvertiseAddr     string   // 安装命令里的平台地址 host:port
	Hosts             []string // 服务端证书 SAN
	AllowCIDRs        []string
	DNSServers        []string
	BandwidthMbps     int
	DataSizeGiB       int
	UpstreamPubKey    string
	DownloadURL       string
	BinarySHA256      string
	Tick              time.Duration // 投递 tick，零值 1 秒
	EnrollLimit       int           // Enroll 每 IP 每分钟次数，零值 10
	HeartbeatInterval time.Duration // 下发给 Agent 的心跳间隔，零值 10 秒
}

type Deps struct {
	Pool      *pgxpool.Pool
	Fleet     *fleet.Service
	Rental    *rental.Service
	CA        *pki.CA
	Clock     clock.Clock // 业务时间戳（可注入假时钟）
	CertClock clock.Clock // 证书签发与轮换：必须是墙上时钟，nil → clock.Real{}（对端按真实时间校验证书）
	Log       *slog.Logger
}

type Server struct {
	agentpb.UnimplementedAgentGatewayServer
	cfg     Config
	d       Deps
	certs   *pki.ServerCerts
	limiter *ipLimiter
	conns   *registry
	ticks   atomic.Int64
	tickMu  sync.Mutex // 串行化 tick：后台循环与手动 TickNow 重叠会重复补推、打乱顺序
	// deliveries 记录指令最近一次被本副本推送时的 tick 号（集成测试断言「≤2 个 tick 内投递」）。
	deliveries sync.Map
}

func New(cfg Config, d Deps) (*Server, error) {
	if cfg.Tick <= 0 {
		cfg.Tick = time.Second
	}
	if cfg.EnrollLimit <= 0 {
		cfg.EnrollLimit = 10
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = 10 * time.Second
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.CA == nil || d.Fleet == nil || d.Rental == nil || d.Pool == nil || d.Clock == nil {
		return nil, errors.New("agentgw: missing deps")
	}
	if len(cfg.Hosts) == 0 {
		cfg.Hosts = []string{"localhost", "127.0.0.1"}
	}
	if d.CertClock == nil {
		d.CertClock = clock.Real{}
	}
	certs, err := pki.NewServerCerts(d.CA, cfg.Hosts, d.CertClock)
	if err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, d: d, certs: certs, limiter: newIPLimiter(cfg.EnrollLimit, time.Minute, d.Clock), conns: newRegistry()}, nil
}

// TLSConfig：客户端证书可选（Enroll 不带、Connect 必须带且由 CA 签发），最低 TLS 1.3，服务端证书热替换。
func (s *Server) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS13,
		ClientAuth:     tls.VerifyClientCertIfGiven,
		ClientCAs:      s.d.CA.Pool(),
		GetCertificate: s.certs.GetCertificate,
	}
}

func (s *Server) CAFingerprint() string { return s.d.CA.Fingerprint() }
func (s *Server) CAPEM() []byte         { return s.d.CA.CertPEM() }

// InstallCommand 生成后台展示的一次性安装命令。
func (s *Server) InstallCommand(token string) string {
	return InstallCommand(s.cfg.AdvertiseAddr, token, s.d.CA.Fingerprint(), s.cfg.DownloadURL, s.cfg.BinarySHA256)
}

func InstallCommand(addr, token, caSHA256, downloadURL, binarySHA256 string) string {
	if downloadURL == "" {
		return fmt.Sprintf("sudo spark-agent enroll --platform %s --token %s --ca-sha256 %s", addr, token, caSHA256)
	}
	// 通过管道 + sudo 执行时脚本拿不到任何环境变量，也不知道自己来自哪个 URL，所以二进制地址必须作为参数带上
	base := strings.TrimRight(downloadURL, "/")
	return fmt.Sprintf("curl -fsSL %s/install.sh | sudo sh -s -- --platform %s --token %s --ca-sha256 %s --binary-sha256 %s --download-url %s",
		base, addr, token, caSHA256, binarySHA256, base)
}

func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.Serve(ctx, lis)
}

// Serve 运行 gRPC 服务、投递循环与每日证书检查，直到 ctx 结束。
func (s *Server) Serve(ctx context.Context, lis net.Listener) error {
	gs := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(s.TLSConfig())),
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 30 * time.Second, Timeout: 10 * time.Second}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}),
	)
	agentpb.RegisterAgentGatewayServer(gs, s)
	errc := make(chan error, 1)
	go func() { errc <- gs.Serve(lis) }()
	go s.runTicks(ctx)
	go s.runCertRotation(ctx)
	select {
	case <-ctx.Done():
		s.conns.abortAll(errors.New("gateway shutting down"))
		gs.Stop()
		<-errc
		return nil
	case err := <-errc:
		return err
	}
}

func (s *Server) runCertRotation(ctx context.Context) {
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if rotated, err := s.certs.Rotate(); err != nil {
				s.d.Log.Error("agent gateway cert rotation failed", "err", err)
			} else if rotated {
				s.d.Log.Info("agent gateway server certificate renewed")
			}
		}
	}
}

// RotateServerCert 供测试与运维手动触发证书检查。
func (s *Server) RotateServerCert() (bool, error) { return s.certs.Rotate() }

func peerIP(ctx context.Context) string {
	p, ok := peer.FromContext(ctx)
	if !ok || p.Addr == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(p.Addr.String())
	if err != nil {
		return p.Addr.String()
	}
	return host
}
