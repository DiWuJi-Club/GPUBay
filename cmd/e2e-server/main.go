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
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/config"
	"spark-cloud/internal/platform/db"
)

const (
	publicAddr = "127.0.0.1:18080"
	adminAddr  = "127.0.0.1:18081"
	hooksAddr  = "127.0.0.1:18090"
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

	cfg := config.Config{
		Env: "dev", HTTPAddr: publicAddr, AdminAddr: adminAddr, DatabaseURL: dbURL, Domain: "spark.test",
		SecretKey: bytes.Repeat([]byte("e"), 32), SMSDriver: "mock", KYCDriver: "mock", PayDriver: "mock", PDUDriver: "mock",
		UploadDir: filepath.Join(dir, "uploads"), SweepInterval: time.Minute,
	}
	clk := clock.NewFake(time.Now())
	kycSwitch := &e2ehooks.SwitchableKYC{}
	a, err := app.Build(cfg, pool, log, app.Options{Clock: clk, KYC: kycSwitch, DisableRateLimits: true})
	if err != nil {
		return err
	}
	public, adminSrv := a.Servers(cfg) // 与 cmd/platform 完全相同的监听与路由；不启动定时清扫器，清扫由钩子触发
	hooks := &http.Server{
		Addr:              hooksAddr,
		Handler:           (&e2ehooks.Server{App: a, Pool: pool, Clock: clk, KYC: kycSwitch, Log: log}).Handler(),
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
	log.Info("e2e-server ready", "console", "http://"+publicAddr, "admin", "http://"+adminAddr+"/admin/", "hooks", "http://"+hooksAddr)

	var runErr error
	select {
	case <-ctx.Done():
	case err := <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			runErr = err
		}
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
