// Package app 按配置组装所有服务，供 cmd/platform 与端到端测试共用。
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wechatpay-apiv3/wechatpay-go/utils"
	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/access/upstream"
	"spark-cloud/internal/admin"
	"spark-cloud/internal/adminapi"
	"spark-cloud/internal/agentgw"
	"spark-cloud/internal/api"
	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/billing"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/identity"
	"spark-cloud/internal/ingress"
	"spark-cloud/internal/pki"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/config"
	"spark-cloud/internal/platform/randx"
	"spark-cloud/internal/platform/secretbox"
	"spark-cloud/internal/platform/tlsfile"
	"spark-cloud/internal/plugins/aliyun"
	"spark-cloud/internal/plugins/kyc"
	"spark-cloud/internal/plugins/pay"
	"spark-cloud/internal/plugins/pdu"
	"spark-cloud/internal/plugins/sms"
	"spark-cloud/internal/rental"
	"spark-cloud/internal/sshgw"
	"spark-cloud/internal/storage"
	"spark-cloud/internal/wallet"
)

// Options 用于测试时替换时钟与插件；零值表示按配置创建。
type Options struct {
	Clock clock.Clock
	SMS   sms.Sender
	KYC   kyc.Verifier
	Pay   map[string]pay.Provider // 测试注入的支付渠道
	PDU   pdu.Driver              // 测试注入：所有 PDU 都使用该驱动
	// AgentTick 是 Agent 网关投递 tick 间隔；零值用网关默认（1 秒）。测试与 e2e-server 调小。
	AgentTick time.Duration
	// DisableRateLimits 把验证码按 IP 限流、后台登录限流与 Agent 接入（Enroll）按 IP 限流调到 100 万次。只供 cmd/e2e-server 使用：
	// 两个限流器都按注入的时钟计窗口，假时钟下窗口永不滑过。没有任何配置项或环境变量能打开它，
	// 并且生产环境设置为 true 时 Build 直接报错。
	DisableRateLimits bool
	// HostKeyGen 是测试注入的实例主机密钥生成；nil → pki.GenerateInstanceHostKey。Go 集成测试与 e2e-server 让所有实例共用假上游的主机密钥。
	HostKeyGen func() (pub string, pem []byte, err error)
	// AppHealthAddr 是测试注入的应用健康检查地址（run.sh 在容器里探测的 host:port）；nil = 127.0.0.1:<主端口>（生产）。
	// e2e-server 与集成测试的假容器共用一个回环地址时用它把探测指向假应用；生产环境设置时 Build 直接报错。
	AppHealthAddr func(port int) string
	// AppRoot / AppRootFor 是测试注入的容器内应用根目录（透传到 appdeploy.Deps 与 RunnerConfig）；零值 = /root/data（生产）。
	// 集成测试与 e2e-server 的所有实例共用一个假容器 sshd，用 AppRootFor 让每个实例各用 <Root>/<实例 ID>/root/data；生产环境设置时 Build 直接报错。
	AppRoot    string
	AppRootFor func(instanceID string) string
}

const noRateLimit = 1_000_000

type App struct {
	Identity *identity.Service
	Wallet   *wallet.Service
	Catalog  *catalog.Service
	Fleet    *fleet.Service
	Rental   *rental.Service
	Billing  *billing.Service
	Uploads  *storage.Service
	Handler  http.Handler

	Admins       *admin.Service
	AdminHandler http.Handler // 后台 API，只应监听内网地址

	Gateway *agentgw.Server // Agent 网关（gRPC，mTLS）；由 cmd/platform 在 SPARK_AGENT_ADDR 上监听
	CA      *pki.CA

	IngressAuth *ingress.Auth   // Web 入口鉴权（票据 / Cookie）
	Ingress     *ingress.Server // 租户 Web 端口反向代理；Handler 按 Host 把实例域名分给它
	Routes      *ingress.Table  // 入口路由表（RunAccessLoops 定时刷新）
	Upstream    *upstream.Pool  // 到容器 sshd 的共享连接池；cmd/platform 在 HTTP 关闭之后 Close
	TLS         *tlsfile.Loader // 由 platform 终止 TLS 时非 nil

	SSH            *sshgw.Server // SSH 网关；由 cmd/platform 在 SPARK_SSH_ADDR 上监听
	SSHHostKey     ssh.Signer
	UpstreamSigner ssh.Signer

	Apps       *appdeploy.Service // 应用一键部署（计划 7）
	AppCatalog *appdeploy.Catalog
	AppRunner  *appdeploy.Runner // 部署后台执行者；由 cmd/platform 按 SPARK_APP_RUNNER 启动
}

func Build(cfg config.Config, pool *pgxpool.Pool, log *slog.Logger, opt Options) (*App, error) {
	cfg = cfg.FillAccessDefaults().FillAppDefaults() // 测试里的 Config 字面量没有接入层 / 应用部署字段
	if opt.DisableRateLimits && cfg.Env == "prod" {
		return nil, errors.New("生产环境禁止关闭速率限制")
	}
	if opt.HostKeyGen != nil && cfg.Env == "prod" {
		return nil, errors.New("生产环境禁止注入实例主机密钥生成")
	}
	if opt.AppHealthAddr != nil && cfg.Env == "prod" {
		return nil, errors.New("生产环境禁止注入应用健康检查地址")
	}
	if (opt.AppRoot != "" || opt.AppRootFor != nil) && cfg.Env == "prod" {
		return nil, errors.New("生产环境禁止注入应用根目录")
	}
	ipHourLimit, loginLimit := 0, 0
	if opt.DisableRateLimits {
		ipHourLimit, loginLimit = noRateLimit, noRateLimit
	}
	if cfg.Env == "prod" && ((opt.SMS == nil && cfg.SMSDriver == "mock") || (opt.KYC == nil && cfg.KYCDriver == "mock") ||
		(opt.Pay == nil && cfg.PayDriver != "real") || (opt.PDU == nil && cfg.PDUDriver != "snmp")) {
		return nil, errors.New("生产环境禁止使用 mock 插件")
	}
	if cfg.Env != "dev" && cfg.AdminPublicBaseURL == "" {
		log.Warn("SPARK_ADMIN_PUBLIC_BASE_URL 未设置：后台写请求若带 Origin 而不带 Sec-Fetch-Site（旧版 Safari、部分内嵌 WebView 等）将一律返回 403；" +
			"经反向代理或 ssh -L 隧道访问后台时请把它设为浏览器地址栏里的后台地址")
	}
	clk := opt.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	box, err := secretbox.New(cfg.SecretKey)
	if err != nil {
		return nil, err
	}
	smsSender := opt.SMS
	if smsSender == nil {
		switch cfg.SMSDriver {
		case "mock":
			smsSender = &sms.Mock{}
		case "aliyun":
			smsSender = &sms.Aliyun{
				Client:   aliyunClient(cfg, cfg.Aliyun.SMSEndpoint),
				SignName: cfg.Aliyun.SMSSign,
				Templates: map[string]string{
					sms.TemplateLoginCode:        cfg.Aliyun.SMSTplLogin,
					sms.TemplateExpiryReminder:   cfg.Aliyun.SMSTplExpiry,
					sms.TemplateAutoRenewFailed:  cfg.Aliyun.SMSTplAutoRenewFailed,
					sms.TemplateBookingCancelled: cfg.Aliyun.SMSTplBookingCancelled,
				},
			}
		default:
			return nil, fmt.Errorf("未知的短信驱动 %q", cfg.SMSDriver)
		}
	}
	verifier := opt.KYC
	if verifier == nil {
		switch cfg.KYCDriver {
		case "mock":
			verifier = kyc.Mock{}
		case "aliyun":
			verifier = &kyc.Aliyun{Client: aliyunClient(cfg, cfg.Aliyun.KYCEndpoint), AuthCode: cfg.Aliyun.KYCAuthCode}
		default:
			return nil, fmt.Errorf("未知的实名驱动 %q", cfg.KYCDriver)
		}
	}
	fixedCode := ""
	if cfg.SMSDriver == "mock" && cfg.Env != "prod" {
		fixedCode = "000000"
	}
	disk, err := storage.NewLocalDisk(cfg.UploadDir)
	if err != nil {
		return nil, err
	}
	uploads := storage.NewService(pool, clk, disk)

	ids := identity.NewService(identity.Deps{Pool: pool, Clock: clk, SMS: smsSender, KYC: verifier, Box: box, Uploads: uploads, FixedCode: fixedCode, CodeIPHourLimit: ipHourLimit})
	w := wallet.NewService(pool, clk)
	c := catalog.NewService(pool).WithClock(clk)
	f := fleet.NewService(pool, clk)
	f.ConfigurePDU(fleet.PDUConfig{Box: box, Open: pduOpener(cfg, opt.PDU), AllowMock: cfg.Env != "prod"})
	r := rental.NewService(rental.Deps{
		Pool: pool, Clock: clk, Box: box, Log: log, Users: ids, Wallet: w, Catalog: c, Fleet: f, Keys: ids,
		Notifier: expiryNotifier{ids: ids, sms: smsSender}, HostKeyGen: opt.HostKeyGen,
	})
	appCat := appdeploy.NewCatalog(pool, clk, f, c, log)
	providers := opt.Pay
	devPay := false
	if providers == nil {
		switch cfg.PayDriver {
		case "", "mock":
			// mock 回调密钥随机生成，外部无法伪造；开发环境通过 /api/v1/dev/recharges/{id}/pay 模拟支付
			m := pay.NewMock(randx.Token())
			providers = map[string]pay.Provider{pay.ChannelWechat: m, pay.ChannelAlipay: m}
			devPay = cfg.Env != "prod"
		case "real":
			var err error
			if providers, err = realProviders(cfg); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("未知的支付驱动 %q", cfg.PayDriver)
		}
	}
	bill := billing.NewService(billing.Deps{
		Pool: pool, Clock: clk, Log: log, Wallet: w, Users: ids, Alerts: f,
		Providers: providers, NotifyBaseURL: cfg.PublicBaseURL,
	})
	ingressAuth, err := ingress.NewAuth(cfg.SecretKey, cfg.PublicBaseURL, cfg.IngressDomain, cfg.Env != "dev", clk)
	if err != nil {
		return nil, err
	}
	authorizeLimit := 0
	if opt.DisableRateLimits {
		authorizeLimit = noRateLimit
	}
	srv := &api.Server{
		Identity: ids, Wallet: w, Catalog: c, Fleet: f, Rental: r, Uploads: uploads, Billing: bill, DevPay: devPay, Clock: clk, Log: log,
		Domain: cfg.Domain, SSHHost: cfg.SSHHost(), SSHPort: cfg.SSHPublicPort, SecureCookie: cfg.Env != "dev", TrustProxy: cfg.TrustProxy, TrustedProxyHops: cfg.TrustedProxyHops, PublicBaseURL: cfg.PublicBaseURL,
		Ingress: ingressAuth, AuthorizeLimit: authorizeLimit, CalendarLimit: authorizeLimit, // 两者零值各取默认（60 / 120）；DisableRateLimits 时同为 noRateLimit
	}
	keyPath, err := upstreamKeyPath(cfg, log) // 先于 EnsureCA：配置错误不应留下任何数据库写入
	if err != nil {
		return nil, err
	}
	upstreamPub, err := pki.LoadUpstreamPublicKey(keyPath)
	if err != nil {
		return nil, fmt.Errorf("读取上游 SSH 密钥: %w", err)
	}
	upstreamSigner, err := pki.LoadUpstreamSigner(keyPath)
	if err != nil {
		return nil, fmt.Errorf("读取上游 SSH 密钥: %w", err)
	}
	// 证书有效期按墙上时钟（对端按真实时间校验）；注入的假时钟若领先真实时间，会签出尚未生效的 CA。
	ca, err := pki.EnsureCA(context.Background(), pool, box, clock.Real{})
	if err != nil {
		return nil, fmt.Errorf("初始化内置 CA: %w", err)
	}
	sshHostKey, err := pki.EnsureSSHHostKey(context.Background(), pool, box)
	if err != nil {
		return nil, fmt.Errorf("初始化 SSH 网关主机密钥: %w", err)
	}
	sshSrv, err := sshgw.New(sshgw.Config{HostKey: sshHostKey, Upstream: upstreamSigner, UpstreamPort: cfg.UpstreamSSHPort,
		MaxSessionsPerInstance: cfg.SSHMaxSessions, DrainTimeout: cfg.SSHDrainTimeout, DisableRateLimits: opt.DisableRateLimits},
		sshgw.Deps{Store: r, Keys: ids, Clock: clk, Log: log})
	if err != nil {
		return nil, err
	}
	enrollLimit := 0
	if opt.DisableRateLimits {
		enrollLimit = noRateLimit
	}
	gw, err := agentgw.New(agentgw.Config{
		AdvertiseAddr: cfg.AgentAdvertiseAddr(), Hosts: cfg.AgentHosts, AllowCIDRs: cfg.AgentAllowCIDRs, DNSServers: cfg.AgentDNSServers,
		BandwidthMbps: cfg.AgentBandwidthMbps, DataSizeGiB: cfg.AgentDataSizeGiB, UpstreamPubKey: upstreamPub,
		DownloadURL: cfg.AgentDownloadURL, BinarySHA256: cfg.AgentBinarySHA256, Tick: opt.AgentTick, EnrollLimit: enrollLimit,
		SSHPort: cfg.UpstreamSSHPort,
	}, agentgw.Deps{Pool: pool, Fleet: f, Rental: r, HostKeys: r, CA: ca, Clock: clk, Log: log})
	if err != nil {
		return nil, err
	}
	admins := admin.NewService(admin.Deps{Pool: pool, Clock: clk, Box: box})
	adminSrv := &adminapi.Server{
		Admins: admins, Identity: ids, Wallet: w, Catalog: c, Fleet: f, Rental: r, Billing: bill, Uploads: uploads, Gateway: gw,
		Clock: clk, Log: log, SecureCookie: cfg.Env != "dev", LoginLimit: loginLimit, PublicBaseURL: cfg.AdminPublicBaseURL,
		PortURL: ingressAuth.PortURL,
	}
	var loader *tlsfile.Loader
	if cfg.TLSEnabled() {
		loader = &tlsfile.Loader{CertFile: cfg.TLSCertFile, KeyFile: cfg.TLSKeyFile, Now: clk.Now, Log: log,
			OnReloadError: func(err error) {
				log.Error("tls certificate reload failed repeatedly, keeping previous certificate", "err", err)
				go alertOnce(f, log, "critical", "tls_cert_reload_failed", "TLS 证书文件重新加载失败，详情见日志") // 在 TLS 握手路径上：不等数据库
			},
			OnExpiring: func(na time.Time) {
				log.Warn("tls certificate expiring", "not_after", na)
				alertOnce(f, log, "warning", "tls_cert_expiring", "TLS 证书将在 14 天内到期")
			}}
		if err := loader.Load(); err != nil {
			return nil, fmt.Errorf("加载 TLS 证书（SPARK_TLS_CERT_FILE=%s, SPARK_TLS_KEY_FILE=%s）: %w", cfg.TLSCertFile, cfg.TLSKeyFile, err)
		}
	}
	upstreamPool := upstream.New(upstream.Config{Signer: upstreamSigner, Port: cfg.UpstreamSSHPort, Log: log}, r)
	// 应用一键部署：服务需要上游连接池（实时日志）与入口地址；接上 rental 钩子之后带 app_id 的下单才可用（noop 钩子报 app_not_found）。
	// 应用根目录生产用默认的容器路径 /root/data（appdeploy.DefaultAppRoot）；测试 / e2e 经 Options 注入，Runner 与「查看日志」用同一个。
	apps := appdeploy.NewService(appdeploy.Deps{Pool: pool, Clock: clk, Box: box, Log: log, Catalog: appCat, Images: c, Rental: r, Fleet: f,
		Upstream: upstreamPool, PortURL: ingressAuth.PortURL, HFEndpoint: cfg.AppHFEndpoint, PipIndexURL: cfg.AppPipIndexURL,
		AppRoot: opt.AppRoot, AppRootFor: opt.AppRootFor})
	r.SetAppHooks(apps)
	appRunner := appdeploy.NewRunner(apps, upstreamPool, appdeploy.RunnerConfig{HealthAddr: opt.AppHealthAddr, AppRoot: opt.AppRoot, AppRootFor: opt.AppRootFor})
	srv.Apps, srv.AppsLimit = apps, authorizeLimit // 零值取默认（每用户每小时 60 次写）；DisableRateLimits 时为 noRateLimit
	adminSrv.Apps, adminSrv.AppCatalog = apps, appCat
	routes := ingress.NewTable(r, log)
	ingSrv := ingress.NewServer(ingressAuth, routes, ids, upstreamPool, r, clk, log)
	proxyHops := 0 // 不信任代理：入口不看任何转发头
	if cfg.TrustProxy {
		proxyHops = cfg.TrustedProxyHops
	}
	handler := ingress.Front(cfg.IngressDomain, proxyHops, ingSrv, srv.Router())
	return &App{Admins: admins, AdminHandler: adminSrv.Router(), Identity: ids, Wallet: w, Catalog: c, Fleet: f, Rental: r, Billing: bill, Uploads: uploads, Handler: handler,
		Gateway: gw, CA: ca, IngressAuth: ingressAuth, Ingress: ingSrv, Routes: routes, Upstream: upstreamPool, TLS: loader,
		SSH: sshSrv, SSHHostKey: sshHostKey, UpstreamSigner: upstreamSigner, Apps: apps, AppCatalog: appCat, AppRunner: appRunner}, nil
}

// upstreamKeyPath 返回平台登录容器用的上游私钥路径；非生产环境未配置时自动在临时目录生成并打印路径。
func upstreamKeyPath(cfg config.Config, log *slog.Logger) (string, error) {
	path := cfg.UpstreamSSHKeyFile
	if path == "" {
		if cfg.Env == "prod" {
			return "", errors.New("生产环境必须设置 SPARK_UPSTREAM_SSH_KEY_FILE")
		}
		path = filepath.Join(os.TempDir(), "spark-upstream-"+randx.ID("", 8)+".key")
		if err := pki.GenerateUpstreamKey(path); err != nil {
			return "", err
		}
		log.Warn("SPARK_UPSTREAM_SSH_KEY_FILE 未设置，已生成临时上游 SSH 密钥（仅开发环境）", "path", path)
	}
	return path, nil
}

func aliyunClient(cfg config.Config, endpoint string) *aliyun.Client {
	return &aliyun.Client{
		Endpoint: endpoint, AccessKeyID: cfg.Aliyun.AccessKeyID, AccessKeySecret: cfg.Aliyun.AccessKeySecret,
		HTTP: &http.Client{Timeout: 10 * time.Second},
	}
}

// expiryNotifier 通过短信发送到期提醒。
type expiryNotifier struct {
	ids *identity.Service
	sms sms.Sender
}

func (n expiryNotifier) NotifyExpiry(ctx context.Context, userID int64, instanceID string, remaining time.Duration) error {
	u, err := n.ids.GetUser(ctx, userID)
	if err != nil {
		return err
	}
	hours := int(remaining.Round(time.Hour) / time.Hour)
	if hours < 1 {
		hours = 1
	}
	return n.sms.Send(ctx, u.Phone, sms.TemplateExpiryReminder, map[string]string{"instance": instanceID, "hours": strconv.Itoa(hours)})
}

func (n expiryNotifier) NotifyAutoRenewFailed(ctx context.Context, userID int64, instanceID, reason string) error {
	u, err := n.ids.GetUser(ctx, userID)
	if err != nil {
		return err
	}
	return n.sms.Send(ctx, u.Phone, sms.TemplateAutoRenewFailed, map[string]string{"instance": instanceID, "reason": reason})
}

func (n expiryNotifier) NotifyBookingCancelled(ctx context.Context, userID int64, instanceID, reason string) error {
	u, err := n.ids.GetUser(ctx, userID)
	if err != nil {
		return err
	}
	return n.sms.Send(ctx, u.Phone, sms.TemplateBookingCancelled, map[string]string{"instance": instanceID, "reason": reason})
}

// realProviders 按配置读取密钥文件，创建已启用的真实支付渠道。
func realProviders(cfg config.Config) (map[string]pay.Provider, error) {
	out := map[string]pay.Provider{}
	if w := cfg.Wechat; w.Enabled() {
		priv, err := utils.LoadPrivateKeyWithPath(w.PrivateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("读取微信支付商户私钥: %w", err)
		}
		pub, err := utils.LoadPublicKeyWithPath(w.PublicKeyPath)
		if err != nil {
			return nil, fmt.Errorf("读取微信支付公钥: %w", err)
		}
		p, err := pay.NewWechat(context.Background(), pay.WechatConfig{MchID: w.MchID, AppID: w.AppID, APIv3Key: w.APIv3Key,
			CertSerial: w.CertSerial, PrivateKey: priv, PublicKeyID: w.PublicKeyID, PublicKey: pub})
		if err != nil {
			return nil, err
		}
		out[pay.ChannelWechat] = p
	}
	if a := cfg.Alipay; a.Enabled() {
		priv, err := os.ReadFile(a.PrivateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("读取支付宝应用私钥: %w", err)
		}
		pub, err := os.ReadFile(a.PublicKeyPath)
		if err != nil {
			return nil, fmt.Errorf("读取支付宝公钥: %w", err)
		}
		p, err := pay.NewAlipay(pay.AlipayConfig{AppID: a.AppID, PrivateKeyPEM: string(priv), AlipayPublicKeyPEM: string(pub), Production: !a.Sandbox})
		if err != nil {
			return nil, err
		}
		out[pay.ChannelAlipay] = p
	}
	return out, nil
}

// pduOpener 按配置选择 PDU 驱动：mock 模式下所有 PDU 共用一个 mock；snmp 模式下按每台 PDU 记录的驱动选择。
func pduOpener(cfg config.Config, override pdu.Driver) fleet.PDUOpener {
	if override != nil {
		return func(fleet.PDUConn) (pdu.Driver, error) { return override, nil }
	}
	shared := &pdu.Mock{}
	if cfg.PDUDriver == "" || cfg.PDUDriver == "mock" {
		return func(fleet.PDUConn) (pdu.Driver, error) { return shared, nil }
	}
	return func(c fleet.PDUConn) (pdu.Driver, error) {
		switch c.Driver {
		case "snmp":
			return pdu.NewSNMP(pdu.SNMPConfig{Address: c.Address, Community: c.Community, OIDPrefix: c.OIDPrefix,
				CycleValue: c.CycleValue, OnValue: c.OnValue}), nil
		case "mock":
			return shared, nil
		}
		return nil, fmt.Errorf("未知的 PDU 驱动 %q", c.Driver)
	}
}

// Servers 按配置创建用户端与后台两个 HTTP 服务器（地址与超时），cmd/platform 与 cmd/e2e-server 共用，
// 保证端到端测试跑的是与生产相同的监听与路由。
func (a *App) Servers(cfg config.Config) (public, admin *http.Server) {
	public = &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           a.Handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	if a.TLS != nil {
		public.TLSConfig = a.TLS.TLSConfig()
	}
	admin = &http.Server{
		Addr:              cfg.AdminAddr,
		Handler:           a.AdminHandler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second, // CSV 导出最多 10 万行
		IdleTimeout:       120 * time.Second,
	}
	return public, admin
}

func alertOnce(f *fleet.Service, log *slog.Logger, level, kind, message string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.RaiseAlertOnce(ctx, f.Pool(), fleet.Alert{Level: level, Kind: kind, Message: message}); err != nil {
		log.Error("raise alert", "kind", kind, "err", err)
	}
}

// RedirectServer 返回把明文 HTTP 301 到 https 的服务器；未配置 SPARK_HTTP_REDIRECT_ADDR（或未启用 TLS）时为 nil。
// Location 只由配置决定：控制台域名 → 对外地址（SPARK_PUBLIC_BASE_URL）的 host；合法实例域名 → 规范化的实例地址；
// 其它 Host（IP、未知域名、其它父域）一律 404，绝不按请求里的 Host 拼出跳转目标。
func (a *App) RedirectServer(cfg config.Config) *http.Server {
	if cfg.HTTPRedirectAddr == "" || a.TLS == nil {
		return nil
	}
	cfg = cfg.FillAccessDefaults()
	base := a.IngressAuth.BaseURL // 已校验的对外地址（scheme + host[:port]）
	consoleHosts := map[string]bool{strings.ToLower(base.Hostname()): true, strings.ToLower(cfg.Domain): true}
	return &http.Server{
		Addr: cfg.HTTPRedirectAddr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var target string
			if id, port, ok := ingress.ParseHost(r.Host, cfg.IngressDomain); ok {
				target = strings.TrimSuffix(a.IngressAuth.PortURL(id, port), "/")
			} else {
				host := strings.ToLower(r.Host)
				if h, _, err := net.SplitHostPort(host); err == nil {
					host = h
				}
				if !consoleHosts[strings.TrimSuffix(host, ".")] {
					http.NotFound(w, r)
					return
				}
				target = "https://" + base.Host
			}
			http.Redirect(w, r, target+r.URL.RequestURI(), http.StatusMovedPermanently)
		}),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second,
	}
}

// accessSweepInterval：活动流复核间隔。加上路由表 2 秒刷新，访问权限变化后最迟约 7 秒断开（≤ ingress.MaxRevokeInterval）。
const accessSweepInterval = 5 * time.Second

// poolKeep：上游连接池应保留的实例。暂停（机器失联）仍算 running，不丢池（裁定 5）；上游真断了由池的 keepalive 自行丢弃。
func poolKeep(routes []rental.IngressRoute) map[string]bool {
	keep := map[string]bool{}
	for _, r := range routes {
		if r.State == rental.StateRunning {
			keep[r.InstanceID] = true
		}
	}
	return keep
}

// RunAccessLoops：路由表每 2 秒刷新、活动流（WebSocket / SSE）按访问权限变化撤销、离开 running 的实例从上游连接池丢弃、
// TLS 到期每日检查；ctx 结束后等所有子循环退出再返回。连接池不在这里关闭：停机时 HTTP / ingress 关闭之后由 cmd/platform 关。
func (a *App) RunAccessLoops(ctx context.Context) {
	var wg sync.WaitGroup
	defer wg.Wait()
	if a.TLS != nil {
		wg.Go(func() { a.TLS.RunExpiryCheck(ctx, 24*time.Hour, 14*24*time.Hour) })
	}
	wg.Go(func() { a.Routes.Run(ctx, 2*time.Second) })
	wg.Go(func() { a.Ingress.Run(ctx, accessSweepInterval) })
	running := map[string]bool{}
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := poolKeep(a.Routes.Routes())
			for id := range running {
				if !now[id] {
					a.Upstream.Drop(id)
				}
			}
			running = now
		}
	}
}
