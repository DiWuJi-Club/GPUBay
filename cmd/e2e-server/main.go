// e2e-server 是端到端测试专用后端：嵌入式 PostgreSQL + mock 插件 + 假时钟，复用与生产相同的 app.Build 与 App.Servers，
// 另在 127.0.0.1:18090 提供测试钩子（internal/e2ehooks）。不随 make build 发布；cmd/platform 不包含任何钩子代码。
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"

	"spark-cloud/internal/app"
	"spark-cloud/internal/e2ehooks"
	"spark-cloud/internal/pki"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/config"
	"spark-cloud/internal/platform/db"
)

const (
	publicAddr = "127.0.0.1:18080"
	adminAddr  = "127.0.0.1:18081"
	hooksAddr  = "127.0.0.1:18090"
	agentAddr  = "127.0.0.1:18443" // 真实 Agent 网关（gRPC + mTLS）；进程内的步进式 fake Agent 连到这里
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "e2e-server:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	dir, err := os.MkdirTemp("", "spark-e2e-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	port := freePort()
	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Port(uint32(port)).
		RuntimePath(filepath.Join(dir, "runtime")).
		DataPath(filepath.Join(dir, "data")).
		Logger(io.Discard))
	if err := pg.Start(); err != nil {
		return fmt.Errorf("start embedded postgres: %w", err)
	}
	defer pg.Stop() //nolint:errcheck
	dbURL := fmt.Sprintf("postgres://postgres:postgres@localhost:%d/postgres?sslmode=disable", port)
	if err := db.Migrate(ctx, dbURL); err != nil {
		return err
	}
	pool, err := db.Open(ctx, dbURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	// 上游 SSH 密钥放在本进程临时目录里（退出时随目录删除），不让 app.Build 每次运行都在 os.TempDir() 留一把
	keyFile := filepath.Join(dir, "upstream.key")
	if err := pki.GenerateUpstreamKey(keyFile); err != nil {
		return err
	}
	cfg := config.Config{
		Env: "dev", HTTPAddr: publicAddr, AdminAddr: adminAddr, DatabaseURL: dbURL, Domain: "spark.test",
		SecretKey: bytes.Repeat([]byte("e"), 32), SMSDriver: "mock", KYCDriver: "mock", PayDriver: "mock", PDUDriver: "mock",
		UploadDir: filepath.Join(dir, "uploads"), SweepInterval: time.Minute,
		AgentAddr: agentAddr, AgentHosts: []string{"127.0.0.1"}, AgentDataSizeGiB: 2048, AgentBandwidthMbps: 200,
		AgentDNSServers: []string{"223.5.5.5", "119.29.29.29"}, UpstreamSSHKeyFile: keyFile,
	}
	clk := clock.NewFake(time.Now())
	kycSwitch := &e2ehooks.SwitchableKYC{}
	a, err := app.Build(cfg, pool, log, app.Options{Clock: clk, KYC: kycSwitch, DisableRateLimits: true, AgentTick: 50 * time.Millisecond})
	if err != nil {
		return err
	}
	// 先同步 Listen：端口被占用时立即失败，而不是让 fake Agent 连到别的进程上
	gwLis, err := net.Listen("tcp", agentAddr)
	if err != nil {
		return fmt.Errorf("agent gateway: %w", err)
	}
	// 网关不随 SIGTERM 的 ctx 退出，只在 defer 里、fake Agent 全部停掉之后关闭：否则几百个 fake Agent 会在退出前刷屏重连
	gwCtx, stopGW := context.WithCancel(context.Background())
	gwErr := make(chan error, 1)
	go func() { gwErr <- a.Gateway.Serve(gwCtx, gwLis) }()
	defer func() { stopGW(); <-gwErr }() // 在 fake Agent 停掉之后再关网关（defer 逆序）
	hooksSrv := &e2ehooks.Server{App: a, Pool: pool, Clock: clk, KYC: kycSwitch, Log: log, AgentAddr: agentAddr}
	defer hooksSrv.Close()

	public, adminSrv := a.Servers(cfg) // 与 cmd/platform 完全相同的监听与路由；不启动定时清扫器，清扫由钩子触发
	hooks := &http.Server{
		Addr:              hooksAddr,
		Handler:           hooksSrv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	servers := []*http.Server{public, adminSrv, hooks}
	errc := make(chan error, len(servers))
	for _, s := range servers {
		go func(s *http.Server) { errc <- s.ListenAndServe() }(s)
	}
	for _, s := range servers {
		if err := waitListening(ctx, s.Addr); err != nil {
			return err
		}
	}
	fmt.Println("E2E READY") // Playwright 以 GET /healthz 为就绪探测；这一行便于人工查看
	log.Info("e2e-server ready", "console", "http://"+publicAddr, "admin", "http://"+adminAddr+"/admin/", "hooks", "http://"+hooksAddr, "agent", agentAddr)

	var runErr error
	select {
	case <-ctx.Done():
	case err := <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			runErr = err
		}
	case err := <-gwErr:
		gwErr <- err // 留给 defer 读取
		runErr = fmt.Errorf("agent gateway stopped: %v", err)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range servers {
		if err := s.Shutdown(shutdownCtx); err != nil && runErr == nil {
			runErr = err
		}
	}
	return runErr
}

func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// waitListening 等待端口可连接（最多 10 秒）。
func waitListening(ctx context.Context, addr string) error {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
			c.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return fmt.Errorf("%s 未在 10 秒内开始监听", addr)
}
