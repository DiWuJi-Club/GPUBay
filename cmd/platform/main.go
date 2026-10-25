// platform 是 Spark Cloud 控制面服务。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/app"
	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/butler"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/comfywf"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/config"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/secretbox"
	"spark-cloud/internal/storage"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "admin" {
		if err := adminMain(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "referral" {
		if err := referralMain(os.Args[2:]); err != nil {
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
		return runAdmin(context.Background(), nil, nil, nil, args, os.Stdout, nil)
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
	var wf *comfywf.Catalog
	if len(args) > 0 && args[0] == "workflow" { // 工作流导入要存封面：与服务同一个上传目录（只在需要时创建）
		disk, err := storage.NewLocalDisk(cfg.UploadDir)
		if err != nil {
			return err
		}
		wf = comfywf.NewCatalog(pool, clock.Real{}, storage.NewService(pool, clock.Real{}, disk), log)
	}
	return runAdmin(ctx, svc, cat, wf, args, os.Stdout, terminalPrompter(os.Stdin, os.Stdout))
}

func run(log *slog.Logger) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	if cfg.ButlerOffReason != "" {
		log.Warn("AI 管家未开启", "reason", cfg.ButlerOffReason)
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
	switch {
	case a.LLM != nil: // 只记提供方、模型名与端点主机，绝不记密钥（BaseURL 已校验不含用户名密码，这里也只取 host）
		endpoint := "（SDK 默认）"
		if u, err := url.Parse(cfg.LLM.BaseURL); err == nil && u.Host != "" {
			endpoint = u.Host
		}
		log.Info("AI 管家已开启", "provider", a.LLM.Name(), "model", a.LLM.Model(), "endpoint", endpoint, "tools", cfg.LLM.Tools)
	case cfg.ButlerOffReason == "":
		log.Info("AI 管家已关闭（SPARK_BUTLER=off）")
	}
	sshLis, err := net.Listen("tcp", cfg.SSHAddr) // 先于启动任何服务：端口占用时干净退出
	if err != nil {
		return fmt.Errorf("ssh gateway listen: %w", err)
	}
	sweeperDone := make(chan struct{})
	go func() {
		defer close(sweeperDone)
		extra := []func(context.Context) error{a.Billing.Sweep, a.Referral.Settle}
		if a.Butler != nil { // AI 管家：确认过期、崩溃轮次接管、90 天对话保留、用量保留（规格 §8）
			extra = append(extra, a.Butler.Sweep)
		}
		a.Rental.RunSweeper(ctx, cfg.SweepInterval, extra...)
	}()
	runnerDone := make(chan struct{})
	if cfg.AppRunner { // 多副本都开也安全：每轮先拿会话级咨询锁，同一时刻只有一个副本在推进部署
		go func() {
			defer close(runnerDone)
			a.AppRunner.Run(ctx)
		}()
	} else {
		close(runnerDone)
		log.Warn("SPARK_APP_RUNNER=off：应用部署不会推进")
	}

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
	// 总耗时约 max(SSH drain 30s, 管家 35s, HTTP 10s) + 几秒，compose / systemd 的停止超时应 ≥ 45s（compose 为 60s）。
	stop() // 确保清扫器、网关、访问层循环退出，SSH 网关停止 accept 并开始 drain
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ingressDone := make(chan struct{})
	go func() {
		defer close(ingressDone)
		a.Ingress.Shutdown(shutdownCtx, ingressShutdownGrace)
	}()
	// AI 管家：先取消运行中的轮次（落「服务正在重启」、回 idle），它们的 SSE handler 随即返回，http.Server.Shutdown 不被拖到超时；
	// 与 HTTP 关闭并发。已批准的写工具不随停机取消、最长跑 WriteToolTimeout，所以管家有自己的预算 WriteToolTimeout + 5s
	// （不受 HTTP 的 10 秒限制；SSE 连接即使被 HTTP 关闭强行断开，轮次仍在后台收尾），关连接池前等它。
	// 来不及收尾的轮次由其它副本的清扫器在 StaleRunAfter 后接管。
	butlerCtx, butlerCancel := context.WithTimeout(context.Background(), butlerShutdownBudget)
	defer butlerCancel()
	butlerDone := make(chan struct{})
	go func() {
		defer close(butlerDone)
		if a.Butler != nil {
			if err := a.Butler.Shutdown(butlerCtx); err != nil {
				log.Warn("AI 管家停机时仍有未结束的轮次", "err", err)
			}
		}
	}()
	for _, s := range []*http.Server{srv, adminSrv, redirect} {
		if s == nil {
			continue
		}
		if err := s.Shutdown(shutdownCtx); err != nil {
			if errors.Is(err, context.DeadlineExceeded) { // 宽限期内没关完的连接直接断开：停机本身不算失败
				log.Warn("http server shutdown timed out, closing remaining connections", "addr", s.Addr)
				_ = s.Close()
				continue
			}
			if runErr == nil {
				runErr = err
			}
		}
	}
	<-butlerDone // 轮次回写用数据库连接池：先等它们收尾再关池（defer）
	<-ingressDone
	<-sweeperDone
	<-runnerDone // Runner 的 exec 走上游连接池：先等它退出再关池
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

// butlerShutdownBudget：停机时等管家轮次收尾的上限——已批准的写工具最长 WriteToolTimeout，再留 5 秒回写结果与收尾文案。
// 须小于 compose 的 stop_grace_period（60s）。
const butlerShutdownBudget = butler.WriteToolTimeout + 5*time.Second

// listensOnAllInterfaces 报告监听地址是否绑定了所有网卡（:8081、0.0.0.0:8081、[::]:8081）。
func listensOnAllInterfaces(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	return err == nil && (host == "" || host == "0.0.0.0" || host == "::")
}
