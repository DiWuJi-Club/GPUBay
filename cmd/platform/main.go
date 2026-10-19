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
	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
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
	if len(args) > 0 && args[0] == "gen-upstream-key" { // 不需要数据库
		return runAdmin(context.Background(), nil, nil, args, os.Stdout, nil)
	}
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
	if len(args) > 0 && args[0] == "ssh-host-key" {
		return adminSSHHostKey(ctx, pool, box, os.Stdout)
	}
	svc := admin.NewService(admin.Deps{Pool: pool, Clock: clock.Real{}, Box: box})
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cat := appdeploy.NewCatalog(pool, clock.Real{}, fleet.NewService(pool, clock.Real{}), catalog.NewService(pool), log)
	return runAdmin(ctx, svc, cat, args, os.Stdout, terminalPrompter(os.Stdin, os.Stdout))
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
	sshLis, err := net.Listen("tcp", cfg.SSHAddr) // 先于启动任何服务：端口占用时干净退出
	if err != nil {
		return fmt.Errorf("ssh gateway listen: %w", err)
	}
	sweeperDone := make(chan struct{})
	go func() {
		defer close(sweeperDone)
		a.Rental.RunSweeper(ctx, cfg.SweepInterval, a.Billing.Sweep)
	}()

	srv, adminSrv := a.Servers(cfg)
	if cfg.TrustProxy && cfg.TLSEnabled() {
		log.Warn("SPARK_TRUST_PROXY=true 但 TLS 由 platform 自己终止：客户端 IP 取自 X-Forwarded-For，只有前面确有会追加该头的四层 / 七层代理时才可信，否则客户端可伪造 IP",
			"trusted_proxy_hops", cfg.TrustedProxyHops)
	}
	if listensOnAllInterfaces(cfg.AdminAddr) {
		log.Warn("后台 API 监听在所有网卡上，请确认防火墙只对内网开放该端口", "addr", cfg.AdminAddr)
	}
	// http.Server 的 ListenAndServe(TLS) 用 net.Listen：零值 ListenConfig 对接受的连接开启 TCP keepalive（15 秒探测），
	// 半开的 WebSocket / SSE 对端会被内核探测出来并断开。
	serverErr := make(chan error, 3)
	go func() {
		log.Info("platform listening", "addr", cfg.HTTPAddr, "env", cfg.Env, "tls", cfg.TLSEnabled(), "ingress_domain", cfg.IngressDomain)
		if cfg.TLSEnabled() {
			serverErr <- srv.ListenAndServeTLS("", "") // 证书来自 TLSConfig.GetCertificate（文件热加载）
			return
		}
		serverErr <- srv.ListenAndServe()
	}()
	redirect := a.RedirectServer(cfg)
	if redirect != nil {
		go func() {
			log.Info("http redirect listening", "addr", cfg.HTTPRedirectAddr)
			serverErr <- redirect.ListenAndServe()
		}()
	}
	go func() {
		log.Info("admin api listening", "addr", cfg.AdminAddr)
		serverErr <- adminSrv.ListenAndServe()
	}()
	gatewayErr := make(chan error, 1)
	go func() { // Serve 内含投递循环与每日服务端证书轮换检查，随 ctx 结束
		log.Info("agent gateway listening", "addr", cfg.AgentAddr, "hosts", cfg.AgentHosts)
		gatewayErr <- a.Gateway.ListenAndServe(ctx, cfg.AgentAddr)
	}()
	accessDone := make(chan struct{})
	go func() { // 路由表刷新、活动流撤销、上游连接丢弃、TLS 到期检查
		defer close(accessDone)
		a.RunAccessLoops(ctx)
	}()
	sshErr := make(chan error, 1)
	go func() {
		log.Info("ssh gateway listening", "addr", cfg.SSHAddr, "public_port", cfg.SSHPublicPort)
		sshErr <- a.SSH.Serve(ctx, sshLis) // ctx 结束后停止 accept，按 SPARK_SSH_DRAIN_TIMEOUT drain
	}()

	var runErr error
	gatewayExited := false
	select {
	case <-ctx.Done():
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			runErr = err
		}
	case err := <-gatewayErr:
		gatewayExited = true
		if err != nil {
			runErr = err
		}
	}
	// 停机顺序：1) 停止 accept（ctx → SSH 网关 / Agent 网关；Shutdown → 各 HTTP 监听器）；2) SSH drain（最长 SPARK_SSH_DRAIN_TIMEOUT，
	// 与 HTTP 关闭同时进行）；3) HTTP 与 ingress 并发关闭：ingress 立即撤销 WebSocket / SSE，普通请求给 5 秒宽限后撤销剩下的，
	// http.Server.Shutdown 因而不被长连接拖住；4) 关上游连接池；5) 关数据库（defer）。
	// 总耗时约 max(SSH drain 30s, HTTP 10s) + 几秒，compose / systemd 的停止超时应 ≥ 45s。
	stop() // 确保清扫器、网关、访问层循环退出，SSH 网关停止 accept 并开始 drain
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ingressDone := make(chan struct{})
	go func() {
		defer close(ingressDone)
		a.Ingress.Shutdown(shutdownCtx, ingressShutdownGrace)
	}()
	for _, s := range []*http.Server{srv, adminSrv, redirect} {
		if s == nil {
			continue
		}
		if err := s.Shutdown(shutdownCtx); err != nil && runErr == nil {
			runErr = err
		}
	}
	<-ingressDone
	<-sweeperDone
	<-accessDone
	// SSH 网关已随 ctx 停止 accept 并在 drain；Serve 总是返回 nil，等它 drain 完（会话记录已落库）再关闭连接池与数据库连接池（defer）
	<-sshErr
	if !gatewayExited { // 等网关的 Serve 随 ctx 返回（断开所有流、停止 gRPC）
		if err := <-gatewayErr; err != nil && runErr == nil {
			runErr = err
		}
	}
	a.Upstream.Close() // HTTP / ingress 已关闭，不再有经它转发的请求
	return runErr
}

// ingressShutdownGrace：停机时给进行中的普通租户请求的宽限。
const ingressShutdownGrace = 5 * time.Second

// listensOnAllInterfaces 报告监听地址是否绑定了所有网卡（:8081、0.0.0.0:8081、[::]:8081）。
func listensOnAllInterfaces(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	return err == nil && (host == "" || host == "0.0.0.0" || host == "::")
}
