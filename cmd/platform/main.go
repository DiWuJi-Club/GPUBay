// platform 是 Spark Cloud 控制面服务。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/app"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/config"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/secretbox"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "admin" {
		if err := adminMain(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("platform exited", "err", err)
		os.Exit(1)
	}
}

// adminMain 执行 `platform admin ...`：使用与服务相同的配置连接数据库（会先执行迁移）。
func adminMain(args []string) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if err := db.Migrate(ctx, cfg.DatabaseURL); err != nil {
		return err
	}
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	box, err := secretbox.New(cfg.SecretKey)
	if err != nil {
		return err
	}
	svc := admin.NewService(admin.Deps{Pool: pool, Clock: clock.Real{}, Box: box})
	return runAdmin(ctx, svc, args, os.Stdout, terminalPrompter(os.Stdin, os.Stdout))
}

func run(log *slog.Logger) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := db.Migrate(ctx, cfg.DatabaseURL); err != nil {
		return err
	}
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	a, err := app.Build(cfg, pool, log, app.Options{})
	if err != nil {
		return err
	}
	sweeperDone := make(chan struct{})
	go func() {
		defer close(sweeperDone)
		a.Rental.RunSweeper(ctx, cfg.SweepInterval, a.Billing.Sweep)
	}()

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           a.Handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	adminSrv := &http.Server{
		Addr:              cfg.AdminAddr,
		Handler:           a.AdminHandler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second, // CSV 导出最多 10 万行
		IdleTimeout:       120 * time.Second,
	}
	if listensOnAllInterfaces(cfg.AdminAddr) {
		log.Warn("后台 API 监听在所有网卡上，请确认防火墙只对内网开放该端口", "addr", cfg.AdminAddr)
	}
	serverErr := make(chan error, 2)
	go func() {
		log.Info("platform listening", "addr", cfg.HTTPAddr, "env", cfg.Env)
		serverErr <- srv.ListenAndServe()
	}()
	go func() {
		log.Info("admin api listening", "addr", cfg.AdminAddr)
		serverErr <- adminSrv.ListenAndServe()
	}()

	var runErr error
	select {
	case <-ctx.Done():
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			runErr = err
		}
	}
	stop() // 确保清扫器退出
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range []*http.Server{srv, adminSrv} {
		if err := s.Shutdown(shutdownCtx); err != nil && runErr == nil {
			runErr = err
		}
	}
	<-sweeperDone
	return runErr
}

// listensOnAllInterfaces 报告监听地址是否绑定了所有网卡（:8081、0.0.0.0:8081、[::]:8081）。
func listensOnAllInterfaces(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	return err == nil && (host == "" || host == "0.0.0.0" || host == "::")
}
