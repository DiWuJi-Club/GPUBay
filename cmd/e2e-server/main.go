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
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"

	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/access/sshtest"
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
	sshAddr    = "127.0.0.1:18022" // SSH 网关；/hooks/ssh 用进程内 ssh 客户端经它登录实例
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
	log := slog.New(quietExpected{slog.NewTextHandler(os.Stderr, nil)})

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
	// 进程内假「容器 sshd」：e2e 机器的 lan_ip 都是 127.0.0.1，网关与 Web 入口按 UpstreamSSHPort 拨到它。
	// 所有 e2e 实例共用这把主机密钥（HostKeyGen 注入），网关拨上游的 FixedHostKey 校验照常生效。
	upSigner, err := pki.LoadUpstreamSigner(keyFile)
	if err != nil {
		return err
	}
	hostPub, hostPEM, err := pki.GenerateInstanceHostKey()
	if err != nil {
		return err
	}
	hostSigner, err := ssh.ParsePrivateKey(hostPEM)
	if err != nil {
		return err
	}
	// shell 模式：exec 在临时目录（充当容器的 /）里用本机 /bin/sh 真执行，应用部署的 run.sh 在这里跑；echo 等命令的输出与回显模式相同
	upstream, err := sshtest.New(sshtest.Options{AuthorizedKey: upSigner.PublicKey(), HostKey: hostSigner, Shell: true})
	if err != nil {
		return err
	}
	defer upstream.Close()
	// fake 应用的「服务」：run.sh 的健康检查（AppHealthAddr）与 /hooks/apps/seed 登记的各应用端口（ingress / Runner 健康检查）都打到它
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "spark-app-ok|path=%s", r.URL.Path)
	}))
	defer health.Close()
	healthAddr := strings.TrimPrefix(health.URL, "http://")
	// 所有实例共用一个假容器：应用根目录按实例分开（<Root>/<实例 ID>/root/data）。run.sh 用 setsid 把 fake 服务移出了
	// sshtest 的进程组，Close 杀不到：退出前（defer 逆序，先于 upstream.Close 删目录）逐个 run.sh stop
	appRoot := upstream.Root()
	defer e2ehooks.StopFakeApps(appRoot)
	cfg := config.Config{
		Env: "dev", HTTPAddr: publicAddr, AdminAddr: adminAddr, DatabaseURL: dbURL, Domain: "spark.test",
		SecretKey: bytes.Repeat([]byte("e"), 32), SMSDriver: "mock", KYCDriver: "mock", PayDriver: "mock", PDUDriver: "mock",
		UploadDir: filepath.Join(dir, "uploads"), SweepInterval: time.Minute,
		AgentAddr: agentAddr, AgentHosts: []string{"127.0.0.1"}, AgentDataSizeGiB: 2048, AgentBandwidthMbps: 200,
		AgentDNSServers: []string{"223.5.5.5", "119.29.29.29"}, UpstreamSSHKeyFile: keyFile,
		IngressDomain: "spark.test", PublicBaseURL: "http://spark.test:18080", SSHAddr: sshAddr, SSHPublicPort: 22, UpstreamSSHPort: upstream.Port(),
		SSHDrainTimeout: time.Second, // 停机时不为残留会话等 30 秒（Playwright 的 gracefulShutdown 只给 20 秒）
	}
	clk := clock.NewFake(time.Now())
	kycSwitch := &e2ehooks.SwitchableKYC{}
	// 不启动应用部署 Runner（SPARK_APP_RUNNER 不适用于此进程）：由 /hooks/apps/tick 按假时钟逐轮驱动
	a, err := app.Build(cfg, pool, log, app.Options{Clock: clk, KYC: kycSwitch, DisableRateLimits: true, AgentTick: 50 * time.Millisecond,
		HostKeyGen:    func() (string, []byte, error) { return hostPub, hostPEM, nil },
		AppHealthAddr: func(int) string { return healthAddr },
		AppRootFor:    func(id string) string { return filepath.Join(appRoot, id, "root", "data") }})
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
	sshLis, err := net.Listen("tcp", sshAddr)
	if err != nil {
		stopGW()
		<-gwErr
		return fmt.Errorf("ssh gateway: %w", err)
	}
	sshErr := make(chan error, 1)
	go func() { sshErr <- a.SSH.Serve(gwCtx, sshLis) }()
	loopsDone := make(chan struct{})
	go func() { defer close(loopsDone); a.RunAccessLoops(gwCtx) }() // 路由表 2 秒刷新、活动流撤销
	// 在 fake Agent 停掉之后（defer 逆序）先取消 gwCtx，再等 Agent 网关、SSH 网关与接入层循环全部退出
	defer func() { stopGW(); <-gwErr; <-sshErr; <-loopsDone; a.Upstream.Close() }()
	hooksSrv := &e2ehooks.Server{App: a, Pool: pool, Clock: clk, KYC: kycSwitch, Log: log, AgentAddr: agentAddr, SSHAddr: sshAddr, Upstream: upstream,
		HealthAddr: healthAddr}
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
	case err := <-sshErr:
		sshErr <- err // 留给 defer 读取
		runErr = fmt.Errorf("ssh gateway stopped: %v", err)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ingressDone := make(chan struct{})
	go func() { defer close(ingressDone); a.Ingress.Shutdown(shutdownCtx, time.Second) }() // 先撤销租户长连接，HTTP Shutdown 才不被拖住
	for _, s := range servers {
		if err := s.Shutdown(shutdownCtx); err != nil && runErr == nil {
			runErr = err
		}
	}
	<-ingressDone
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

// quietExpected 丢掉端到端用例里预期会大量出现的日志（用错误密码 / 已删除的公钥登录是断言的一部分），
// 让 Playwright 输出里只剩真正的问题。只在 e2e-server 使用；生产日志不受影响。
type quietExpected struct{ slog.Handler }

func (q quietExpected) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == "ssh auth failed" {
		return nil
	}
	return q.Handler.Handle(ctx, r)
}

func (q quietExpected) WithAttrs(attrs []slog.Attr) slog.Handler {
	return quietExpected{q.Handler.WithAttrs(attrs)}
}

func (q quietExpected) WithGroup(name string) slog.Handler {
	return quietExpected{q.Handler.WithGroup(name)}
}
