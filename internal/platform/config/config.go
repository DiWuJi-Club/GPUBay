// Package config 从环境变量读取 platform 配置。
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Env           string // dev / staging / prod
	HTTPAddr      string // 用户 API（公网）
	AdminAddr     string // 后台 API，只应绑定内网地址
	DatabaseURL   string
	Domain        string
	PublicBaseURL string // 支付回调地址前缀，例如 https://spark.example.com（无结尾斜杠）
	SecretKey     []byte // 32 字节，用于加密身份证号、实例密码、TOTP 密钥与 SNMP community
	SMSDriver     string // mock / aliyun
	KYCDriver     string // mock / aliyun
	PayDriver     string // mock / real
	PDUDriver     string // mock / snmp
	UploadDir     string
	SweepInterval time.Duration
	TrustProxy    bool // 仅当部署在会改写 X-Forwarded-For 的反向代理之后才设为 true
	// TrustedProxyHops：TrustProxy 时我们自己的反向代理层数；客户端 IP 取 X-Forwarded-For 自右数第 N 项。默认 1。
	TrustedProxyHops int
	Aliyun           AliyunConfig
	Wechat           WechatConfig
	Alipay           AlipayConfig

	// 计划 2：Agent 网关
	AgentAddr          string   // gRPC 监听地址，只对机房内网开放
	AgentHosts         []string // 网关服务端证书 SAN（域名或 IP），第一项用于安装命令
	AgentDownloadURL   string   // install.sh 与 agent 二进制所在目录 URL；为空时后台只显示 enroll 命令
	AgentBinarySHA256  string   // agent 二进制 SHA-256，填进安装命令
	UpstreamSSHKeyFile string   // 上游 ed25519 私钥（OpenSSH 格式，0600）
	AgentDataSizeGiB   int
	AgentAllowCIDRs    []string
	AgentDNSServers    []string
	AgentBandwidthMbps int

	// 计划 3：接入层
	SSHAddr            string        // SSH 网关监听地址
	SSHPublicPort      int           // 写进 ssh_command 的公网端口
	SSHMaxSessions     int           // 每实例并发 SSH 会话上限
	SSHDrainTimeout    time.Duration // 停机时等待会话结束
	UpstreamSSHPort    int           // 容器 sshd 在机器上的端口（Create 的 ssh_port 与上游拨号共用）
	IngressDomain      string        // 租户子域的父域：ins-<id>-<port>.<IngressDomain>
	AdminPublicBaseURL string        // 后台对外地址（经反向代理暴露时），用于写请求的 Origin 校验；空表示不比对
	TLSCertFile        string
	TLSKeyFile         string
	HTTPRedirectAddr   string // 纯 HTTP 301 到 https 的监听地址；仅 TLS 开启时有效

	// 计划 7：应用一键部署
	AppHFEndpoint  string // 注入应用容器的 HF_ENDPOINT（HuggingFace 镜像站）
	AppPipIndexURL string // 注入应用容器的 PIP_INDEX_URL
	AppGitMirror   string // 计划 11：注入应用容器的 SPARK_GIT_MIRROR（GitHub 加速前缀，如 https://gh-proxy.com/）；空 = 直连
	AppRunner      bool   // 是否在本副本启动应用部署 Runner（多副本都开时由咨询锁保证只有一个在工作）

	// 计划 9：Token 商城。经 Load 默认都是 on；Config 字面量（测试、e2e-server）零值为 off，需要时显式设置。
	LLMGateway    bool // SPARK_LLM_GATEWAY：是否在用户 API 上挂 /v1（OpenAI 兼容网关）；限额按进程各算
	LLMController bool // SPARK_LLM_CONTROLLER：是否在本副本启动节点 Controller（多副本都开时由咨询锁保证只有一个在工作）
	// 计划 12：AI 管家
	ButlerLLM ButlerLLMConfig
	Butler    bool // 部署级开关（SPARK_BUTLER）；false 时不注册管家路由、不显示入口
	// ButlerOffReason：开发环境未显式开启管家、openai 又没有可用端点（没配 SPARK_LLM_BASE_URL 且网关关闭，或走本进程网关却没有 SPARK_LLM_API_KEY）时自动关闭管家的原因（启动时记一条日志）；其它情况为空。
	ButlerOffReason string
}

// ButlerLLMConfig AI 管家使用的模型后端（计划 12；与 Token 商城的 LLMGateway / LLMController 无关）。
type ButlerLLMConfig struct {
	Provider          string // openai / anthropic / mock
	BaseURL           string // openai 为空 = 本进程的 Token 商城网关（进程内调用，需 SPARK_LLM_GATEWAY=on）；非空 = 外部 OpenAI 兼容端点（写到 /v1）；anthropic 可空（SDK 默认）；已去掉结尾斜杠
	APIKey            string // 生产环境（非 mock）必填；默认端点时是一把 Token 商城 API Key（sk-spark-…，平台管家账号的）；只放环境变量，不写日志
	Model             string // 为空时按 provider 取默认值；接 Token 商城时是模型的 slug（llm_models.slug，如 qwen3-32b）
	MaxOutputTokens   int
	PriceInCentsPerM  int  // 用量页估算费用：输入每百万 token 的价格（分）
	PriceOutCentsPerM int  // 输出每百万 token 的价格（分）
	Tools             bool // SPARK_LLM_TOOLS=off 时不给模型工具（端点不支持工具调用时的应急开关）
	// Stream：SPARK_LLM_STREAM，模型请求是否用流式（只对 openai 生效；anthropic 适配器总是流式，mock 不走网络）。
	// openai 默认 off：vLLM 0.10.1（spark/llm-serving:25.09）的 hermes 解析器流式输出工具调用时会丢参数片段，
	// 非流式没有这个问题；节点的 vLLM 升级并验证后可改回 on。anthropic / mock 默认 on。
	Stream bool
}

// LLM 提供方与各自的默认模型。
const (
	LLMOpenAI    = "openai"
	LLMAnthropic = "anthropic"
	LLMMock      = "mock"

	DefaultLLMMaxOutputTokens = 1024

	// LLMGatewayKeyPrefix 是 Token 商城 API Key 的前缀（与 llm.KeyPrefix 相同；config 不导入 internal/llm，测试断言两者一致）。
	LLMGatewayKeyPrefix = "sk-spark-"
)

// DefaultLLMModel 返回 provider 的默认模型名；未知 provider 返回空串。
func DefaultLLMModel(provider string) string {
	switch provider {
	case LLMOpenAI:
		return "qwen3-32b"
	case LLMAnthropic:
		return "claude-opus-5-5"
	case LLMMock:
		return "mock"
	}
	return ""
}

// AliyunConfig 阿里云短信与号码百科（三要素核验）共用一组 AccessKey。
type AliyunConfig struct {
	AccessKeyID            string
	AccessKeySecret        string
	SMSSign                string
	SMSTplLogin            string
	SMSTplExpiry           string
	SMSTplAutoRenewFailed  string
	SMSTplBookingCancelled string
	KYCAuthCode            string
	SMSEndpoint            string
	KYCEndpoint            string
}

// WechatConfig 微信支付 APIv3 Native 扫码，使用「微信支付公钥」验签。MchID 非空即视为启用。
type WechatConfig struct {
	MchID          string
	AppID          string
	APIv3Key       string
	CertSerial     string
	PrivateKeyPath string
	PublicKeyID    string
	PublicKeyPath  string
}

func (w WechatConfig) Enabled() bool { return w.MchID != "" }

// AlipayConfig 支付宝当面付。AppID 非空即视为启用。
type AlipayConfig struct {
	AppID          string
	PrivateKeyPath string // 应用私钥
	PublicKeyPath  string // 支付宝公钥
	Sandbox        bool
}

func (a AlipayConfig) Enabled() bool { return a.AppID != "" }

func Load(getenv func(string) string) (Config, error) {
	or := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}
	c := Config{
		Env:           or("SPARK_ENV", "dev"),
		HTTPAddr:      or("SPARK_HTTP_ADDR", ":8080"),
		AdminAddr:     or("SPARK_ADMIN_ADDR", "127.0.0.1:8081"),
		DatabaseURL:   getenv("SPARK_DATABASE_URL"),
		Domain:        or("SPARK_DOMAIN", "spark.local"),
		PublicBaseURL: strings.TrimRight(getenv("SPARK_PUBLIC_BASE_URL"), "/"),
		SMSDriver:     or("SPARK_SMS_DRIVER", "mock"),
		KYCDriver:     or("SPARK_KYC_DRIVER", "mock"),
		PayDriver:     or("SPARK_PAY_DRIVER", "mock"),
		PDUDriver:     or("SPARK_PDU_DRIVER", "mock"),
		UploadDir:     or("SPARK_UPLOAD_DIR", "./.devdata/uploads"),
		Aliyun: AliyunConfig{
			AccessKeyID:            getenv("SPARK_ALIYUN_ACCESS_KEY_ID"),
			AccessKeySecret:        getenv("SPARK_ALIYUN_ACCESS_KEY_SECRET"),
			SMSSign:                getenv("SPARK_SMS_SIGN"),
			SMSTplLogin:            getenv("SPARK_SMS_TPL_LOGIN"),
			SMSTplExpiry:           getenv("SPARK_SMS_TPL_EXPIRY"),
			SMSTplAutoRenewFailed:  getenv("SPARK_SMS_TPL_AUTO_RENEW_FAILED"),
			SMSTplBookingCancelled: getenv("SPARK_SMS_TPL_BOOKING_CANCELLED"),
			KYCAuthCode:            getenv("SPARK_KYC_AUTH_CODE"),
			SMSEndpoint:            or("SPARK_ALIYUN_SMS_ENDPOINT", "https://dysmsapi.aliyuncs.com"),
			KYCEndpoint:            or("SPARK_ALIYUN_KYC_ENDPOINT", "https://dytnsapi.aliyuncs.com"),
		},
		Wechat: WechatConfig{
			MchID:          getenv("SPARK_WECHAT_MCH_ID"),
			AppID:          getenv("SPARK_WECHAT_APP_ID"),
			APIv3Key:       getenv("SPARK_WECHAT_API_V3_KEY"),
			CertSerial:     getenv("SPARK_WECHAT_CERT_SERIAL"),
			PrivateKeyPath: getenv("SPARK_WECHAT_PRIVATE_KEY_PATH"),
			PublicKeyID:    getenv("SPARK_WECHAT_PUBLIC_KEY_ID"),
			PublicKeyPath:  getenv("SPARK_WECHAT_PUBLIC_KEY_PATH"),
		},
		Alipay: AlipayConfig{
			AppID:          getenv("SPARK_ALIPAY_APP_ID"),
			PrivateKeyPath: getenv("SPARK_ALIPAY_PRIVATE_KEY_PATH"),
			PublicKeyPath:  getenv("SPARK_ALIPAY_PUBLIC_KEY_PATH"),
		},
	}
	switch c.Env {
	case "dev", "staging", "prod":
	default:
		return Config{}, fmt.Errorf("SPARK_ENV 只能是 dev、staging 或 prod，当前为 %q", c.Env)
	}
	if c.DatabaseURL == "" {
		return Config{}, errors.New("SPARK_DATABASE_URL 未设置")
	}
	key, err := base64.StdEncoding.DecodeString(getenv("SPARK_SECRET_KEY"))
	if err != nil || len(key) != 32 {
		return Config{}, errors.New("SPARK_SECRET_KEY 必须是 32 字节密钥的 base64 编码（可用 openssl rand -base64 32 生成）")
	}
	c.SecretKey = key
	for _, d := range []struct{ name, value, a, b string }{
		{"SPARK_SMS_DRIVER", c.SMSDriver, "mock", "aliyun"},
		{"SPARK_KYC_DRIVER", c.KYCDriver, "mock", "aliyun"},
		{"SPARK_PAY_DRIVER", c.PayDriver, "mock", "real"},
		{"SPARK_PDU_DRIVER", c.PDUDriver, "mock", "snmp"},
	} {
		if d.value != d.a && d.value != d.b {
			return Config{}, fmt.Errorf("%s 只能是 %s 或 %s，当前为 %q", d.name, d.a, d.b, d.value)
		}
	}
	if c.Env == "prod" && (c.SMSDriver == "mock" || c.KYCDriver == "mock" || c.PayDriver == "mock" || c.PDUDriver == "mock") {
		return Config{}, errors.New("生产环境禁止使用 mock 插件（SPARK_SMS_DRIVER / SPARK_KYC_DRIVER / SPARK_PAY_DRIVER / SPARK_PDU_DRIVER）")
	}
	if c.TrustProxy, err = parseBool("SPARK_TRUST_PROXY", getenv("SPARK_TRUST_PROXY")); err != nil {
		return Config{}, err
	}
	if c.Alipay.Sandbox, err = parseBool("SPARK_ALIPAY_SANDBOX", getenv("SPARK_ALIPAY_SANDBOX")); err != nil {
		return Config{}, err
	}
	c.SweepInterval = time.Minute
	if v := getenv("SPARK_SWEEP_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("SPARK_SWEEP_INTERVAL 格式不正确：%q", v)
		}
		c.SweepInterval = d
	}
	if err := c.loadAgent(getenv, or); err != nil {
		return Config{}, err
	}
	if err := c.loadAccess(getenv, or); err != nil {
		return Config{}, err
	}
	if err := c.loadApps(or); err != nil {
		return Config{}, err
	}
	if err := c.loadLLM(or); err != nil {
		return Config{}, err
	}
	// 管家在 Token 商城开关之后读：默认端点取决于本进程是否挂了网关（SPARK_LLM_GATEWAY）
	if err := c.loadButler(getenv, or); err != nil {
		return Config{}, err
	}
	if err := c.checkPlugins(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func parseBool(name, v string) (bool, error) {
	switch v {
	case "", "false", "0":
		return false, nil
	case "true", "1":
		return true, nil
	}
	return false, fmt.Errorf("%s 只能是 true、false、1 或 0，当前为 %q", name, v)
}

// checkPlugins 校验启用的真实插件所需的变量是否齐全。
func (c Config) checkPlugins() error {
	a := c.Aliyun
	if c.SMSDriver == "aliyun" {
		if err := requireAll("SPARK_SMS_DRIVER=aliyun",
			"SPARK_ALIYUN_ACCESS_KEY_ID", a.AccessKeyID, "SPARK_ALIYUN_ACCESS_KEY_SECRET", a.AccessKeySecret,
			"SPARK_SMS_SIGN", a.SMSSign, "SPARK_SMS_TPL_LOGIN", a.SMSTplLogin, "SPARK_SMS_TPL_EXPIRY", a.SMSTplExpiry,
			"SPARK_SMS_TPL_AUTO_RENEW_FAILED", a.SMSTplAutoRenewFailed, "SPARK_SMS_TPL_BOOKING_CANCELLED", a.SMSTplBookingCancelled); err != nil {
			return err
		}
	}
	if c.KYCDriver == "aliyun" {
		if err := requireAll("SPARK_KYC_DRIVER=aliyun",
			"SPARK_ALIYUN_ACCESS_KEY_ID", a.AccessKeyID, "SPARK_ALIYUN_ACCESS_KEY_SECRET", a.AccessKeySecret,
			"SPARK_KYC_AUTH_CODE", a.KYCAuthCode); err != nil {
			return err
		}
	}
	if c.PayDriver == "real" {
		if !strings.HasPrefix(c.PublicBaseURL, "https://") {
			return errors.New("SPARK_PAY_DRIVER=real 需要设置以 https:// 开头的 SPARK_PUBLIC_BASE_URL（支付回调地址）")
		}
		if !c.Wechat.Enabled() && !c.Alipay.Enabled() {
			return errors.New("SPARK_PAY_DRIVER=real 需要至少配置微信支付（SPARK_WECHAT_MCH_ID）或支付宝（SPARK_ALIPAY_APP_ID）")
		}
		if w := c.Wechat; w.Enabled() {
			if err := requireAll("微信支付",
				"SPARK_WECHAT_APP_ID", w.AppID, "SPARK_WECHAT_API_V3_KEY", w.APIv3Key, "SPARK_WECHAT_CERT_SERIAL", w.CertSerial,
				"SPARK_WECHAT_PRIVATE_KEY_PATH", w.PrivateKeyPath, "SPARK_WECHAT_PUBLIC_KEY_ID", w.PublicKeyID,
				"SPARK_WECHAT_PUBLIC_KEY_PATH", w.PublicKeyPath); err != nil {
				return err
			}
			if len(w.APIv3Key) != 32 {
				return errors.New("SPARK_WECHAT_API_V3_KEY 必须是 32 个字符")
			}
		}
		if p := c.Alipay; p.Enabled() {
			if err := requireAll("支付宝",
				"SPARK_ALIPAY_PRIVATE_KEY_PATH", p.PrivateKeyPath, "SPARK_ALIPAY_PUBLIC_KEY_PATH", p.PublicKeyPath); err != nil {
				return err
			}
			if p.Sandbox && c.Env == "prod" {
				return errors.New("生产环境不能使用支付宝沙箱，请移除 SPARK_ALIPAY_SANDBOX")
			}
		}
	}
	return nil
}

// requireAll 接收「变量名, 值」成对参数，列出所有为空的变量名。
func requireAll(what string, pairs ...string) error {
	var missing []string
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] == "" {
			missing = append(missing, pairs[i])
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s 需要设置 %s", what, strings.Join(missing, "、"))
	}
	return nil
}

// loadAgent 读取并校验 Agent 网关相关配置。
func (c *Config) loadAgent(getenv func(string) string, or func(key, def string) string) error {
	var err error
	c.AgentAddr = or("SPARK_AGENT_ADDR", ":9443")
	if _, _, err := net.SplitHostPort(c.AgentAddr); err != nil {
		return fmt.Errorf("SPARK_AGENT_ADDR 必须是 host:port 形式，当前为 %q", c.AgentAddr)
	}
	hosts := getenv("SPARK_AGENT_HOSTS")
	if hosts == "" && c.Env != "prod" {
		hosts = "localhost,127.0.0.1"
	}
	c.AgentHosts = splitList(hosts)
	for _, h := range c.AgentHosts {
		if !validHost(h) {
			return fmt.Errorf("SPARK_AGENT_HOSTS 含无效的域名或 IP %q（不要带端口、协议或空格）", h)
		}
	}
	c.AgentDownloadURL = strings.TrimRight(getenv("SPARK_AGENT_DOWNLOAD_URL"), "/")
	c.AgentBinarySHA256 = strings.TrimSpace(getenv("SPARK_AGENT_BINARY_SHA256"))
	c.UpstreamSSHKeyFile = getenv("SPARK_UPSTREAM_SSH_KEY_FILE")
	if c.AgentDataSizeGiB, err = positiveInt("SPARK_AGENT_DATA_SIZE_GIB", or("SPARK_AGENT_DATA_SIZE_GIB", "2048")); err != nil {
		return err
	}
	if c.AgentBandwidthMbps, err = positiveInt("SPARK_AGENT_BANDWIDTH_MBPS", or("SPARK_AGENT_BANDWIDTH_MBPS", "200")); err != nil {
		return err
	}
	c.AgentAllowCIDRs = splitList(getenv("SPARK_AGENT_ALLOW_CIDRS"))
	for _, cidr := range c.AgentAllowCIDRs {
		// 必须是规范的 IPv4 网段（无主机位）：iptables -S 只会打印规范形式，且 IPv6 无法加入 iptables 链。
		p, err := netip.ParsePrefix(cidr)
		if err != nil || !p.Addr().Is4() || p.Bits() == 0 || p.Masked() != p {
			return fmt.Errorf("SPARK_AGENT_ALLOW_CIDRS 含无效网段 %q（需规范的 IPv4 CIDR，如 10.0.9.0/24，不含主机位）", cidr)
		}
	}
	c.AgentDNSServers = splitList(or("SPARK_AGENT_DNS_SERVERS", "223.5.5.5,119.29.29.29"))
	for _, ip := range c.AgentDNSServers {
		parsed := net.ParseIP(ip)
		if parsed == nil {
			return fmt.Errorf("SPARK_AGENT_DNS_SERVERS 含无效地址 %q", ip)
		}
		// 容器的 DNS 查询由 dockerd 在宿主机网络命名空间代发、不经过 SPARK-FWD：私网 DNS 必须显式在白名单里
		if parsed.IsPrivate() && !inAnyCIDR(parsed, c.AgentAllowCIDRs) {
			return fmt.Errorf("SPARK_AGENT_DNS_SERVERS 的 %s 是私网地址，必须包含在 SPARK_AGENT_ALLOW_CIDRS 内", ip)
		}
	}
	if c.Env == "prod" {
		if len(c.AgentHosts) == 0 {
			return errors.New("生产环境必须设置 SPARK_AGENT_HOSTS（网关证书的域名或 IP）")
		}
		if c.UpstreamSSHKeyFile == "" {
			return errors.New("生产环境必须设置 SPARK_UPSTREAM_SSH_KEY_FILE（用 platform admin gen-upstream-key 生成）")
		}
	}
	return nil
}

var hostLabel = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?$`)

// validHost 判断 s 是 IP 或合法主机名（不含端口、协议、空白）。
func validHost(s string) bool {
	if net.ParseIP(s) != nil {
		return true
	}
	if len(s) > 253 {
		return false
	}
	for _, l := range strings.Split(s, ".") {
		if len(l) > 63 || !hostLabel.MatchString(l) {
			return false
		}
	}
	return true
}

// splitList 按逗号切分、去空白、去空项。
func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func inAnyCIDR(ip net.IP, cidrs []string) bool {
	for _, c := range cidrs {
		if _, n, err := net.ParseCIDR(c); err == nil && n.Contains(ip) {
			return true
		}
	}
	return false
}

func positiveInt(name, v string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s 必须是正整数，当前为 %q", name, v)
	}
	return n, nil
}

// AgentAdvertiseAddr 是写进安装命令的平台地址：第一个主机名 + 网关端口。
func (c Config) AgentAdvertiseAddr() string {
	_, port, _ := net.SplitHostPort(c.AgentAddr)
	host := "localhost"
	if len(c.AgentHosts) > 0 {
		host = c.AgentHosts[0]
	}
	return net.JoinHostPort(host, port)
}

// loadAccess 读取并校验接入层（SSH 网关 / Web 入口 / TLS）配置。
func (c *Config) loadAccess(getenv func(string) string, or func(key, def string) string) error {
	var err error
	c.SSHAddr = or("SPARK_SSH_ADDR", ":2222")
	if _, _, err := net.SplitHostPort(c.SSHAddr); err != nil {
		return fmt.Errorf("SPARK_SSH_ADDR 必须是 host:port 形式，当前为 %q", c.SSHAddr)
	}
	for _, other := range []string{c.HTTPAddr, c.AdminAddr, c.AgentAddr} {
		if samePort(c.SSHAddr, other) {
			return fmt.Errorf("SPARK_SSH_ADDR %q 与其它监听地址冲突", c.SSHAddr)
		}
	}
	if c.SSHPublicPort, err = portValue("SPARK_SSH_PUBLIC_PORT", or("SPARK_SSH_PUBLIC_PORT", "22")); err != nil {
		return err
	}
	if c.SSHMaxSessions, err = positiveInt("SPARK_SSH_MAX_SESSIONS_PER_INSTANCE", or("SPARK_SSH_MAX_SESSIONS_PER_INSTANCE", "16")); err != nil {
		return err
	}
	c.SSHDrainTimeout = 30 * time.Second
	if v := getenv("SPARK_SSH_DRAIN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return fmt.Errorf("SPARK_SSH_DRAIN_TIMEOUT 格式不正确：%q", v)
		}
		c.SSHDrainTimeout = d
	}
	if c.UpstreamSSHPort, err = portValue("SPARK_UPSTREAM_SSH_PORT", or("SPARK_UPSTREAM_SSH_PORT", "2222")); err != nil {
		return err
	}
	c.IngressDomain = strings.ToLower(or("SPARK_INGRESS_DOMAIN", c.Domain))
	if net.ParseIP(c.IngressDomain) != nil || !validHost(c.IngressDomain) {
		return fmt.Errorf("SPARK_INGRESS_DOMAIN 不是合法域名：%q", c.IngressDomain)
	}
	if c.IngressDomain != strings.ToLower(c.Domain) && strings.HasSuffix(c.IngressDomain, "."+strings.ToLower(c.Domain)) {
		return fmt.Errorf("SPARK_INGRESS_DOMAIN %q 不能是 SPARK_DOMAIN 的子域（请用不同的注册域，例如 sparkapp.example）", c.IngressDomain)
	}
	c.TLSCertFile, c.TLSKeyFile = strings.TrimSpace(getenv("SPARK_TLS_CERT_FILE")), strings.TrimSpace(getenv("SPARK_TLS_KEY_FILE"))
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return errors.New("SPARK_TLS_CERT_FILE 与 SPARK_TLS_KEY_FILE 必须同时设置")
	}
	if c.PublicBaseURL == "" {
		scheme := "http"
		if c.TLSEnabled() {
			scheme = "https"
		}
		_, port, _ := net.SplitHostPort(c.HTTPAddr)
		c.PublicBaseURL = scheme + "://" + net.JoinHostPort(c.Domain, port)
	}
	u, err := url.Parse(c.PublicBaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Path != "" || u.RawQuery != "" || u.User != nil {
		return fmt.Errorf("SPARK_PUBLIC_BASE_URL 必须形如 https://域名[:端口]，当前为 %q", c.PublicBaseURL)
	}
	if !strings.EqualFold(u.Hostname(), c.Domain) {
		return fmt.Errorf("SPARK_PUBLIC_BASE_URL 的域名 %q 必须等于 SPARK_DOMAIN %q", u.Hostname(), c.Domain)
	}
	if c.Env == "prod" && u.Scheme != "https" {
		return errors.New("生产环境的 SPARK_PUBLIC_BASE_URL 必须以 https:// 开头")
	}
	if c.TLSEnabled() && u.Scheme != "https" {
		return errors.New("配置了 TLS 证书时 SPARK_PUBLIC_BASE_URL 必须以 https:// 开头")
	}
	if c.TrustedProxyHops, err = positiveInt("SPARK_TRUSTED_PROXY_HOPS", or("SPARK_TRUSTED_PROXY_HOPS", "1")); err != nil {
		return err
	}
	c.AdminPublicBaseURL = strings.TrimRight(strings.TrimSpace(getenv("SPARK_ADMIN_PUBLIC_BASE_URL")), "/")
	if c.AdminPublicBaseURL != "" {
		au, err := url.Parse(c.AdminPublicBaseURL)
		if err != nil || (au.Scheme != "http" && au.Scheme != "https") || au.Hostname() == "" || au.Path != "" || au.RawQuery != "" || au.User != nil {
			return fmt.Errorf("SPARK_ADMIN_PUBLIC_BASE_URL 必须形如 https://域名[:端口]，当前为 %q", c.AdminPublicBaseURL)
		}
	}
	c.HTTPRedirectAddr = getenv("SPARK_HTTP_REDIRECT_ADDR")
	if c.HTTPRedirectAddr != "" {
		if !c.TLSEnabled() {
			return errors.New("SPARK_HTTP_REDIRECT_ADDR 只在配置了 TLS 证书时有效")
		}
		if _, _, err := net.SplitHostPort(c.HTTPRedirectAddr); err != nil {
			return fmt.Errorf("SPARK_HTTP_REDIRECT_ADDR 必须是 host:port 形式，当前为 %q", c.HTTPRedirectAddr)
		}
		for _, other := range []string{c.HTTPAddr, c.SSHAddr, c.AdminAddr, c.AgentAddr} {
			if samePort(c.HTTPRedirectAddr, other) {
				return fmt.Errorf("SPARK_HTTP_REDIRECT_ADDR %q 与其它监听地址冲突", c.HTTPRedirectAddr)
			}
		}
	}
	if c.Env == "prod" && !c.TLSEnabled() && !c.TrustProxy {
		return errors.New("生产环境必须配置 TLS 证书或在反向代理后设置 SPARK_TRUST_PROXY=true")
	}
	return nil
}

// 应用一键部署的默认镜像站（国内可达）。
const (
	DefaultAppHFEndpoint  = "https://hf-mirror.com"
	DefaultAppPipIndexURL = "https://pypi.tuna.tsinghua.edu.cn/simple"
	DefaultAppGitMirror   = "https://gh-proxy.com/"
	// AppGitMirrorOff 是 SPARK_APP_GIT_MIRROR 的「直连」取值：环境变量设成空串与未设置无法区分（都取默认值）。
	AppGitMirrorOff = "off"
)

// loadApps 读取并校验应用一键部署（计划 7）配置。
func (c *Config) loadApps(or func(key, def string) string) error {
	c.AppHFEndpoint = strings.TrimSpace(or("SPARK_APP_HF_ENDPOINT", DefaultAppHFEndpoint))
	c.AppPipIndexURL = strings.TrimSpace(or("SPARK_APP_PIP_INDEX_URL", DefaultAppPipIndexURL))
	for _, v := range []struct{ name, value string }{{"SPARK_APP_HF_ENDPOINT", c.AppHFEndpoint}, {"SPARK_APP_PIP_INDEX_URL", c.AppPipIndexURL}} {
		u, err := url.Parse(v.value)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("%s 必须以 http:// 或 https:// 开头，当前为 %q", v.name, v.value)
		}
	}
	// 自定义节点 clone 用的 GitHub 加速前缀：容器内拼成 <mirror>https://github.com/…，所以必须 https:// 开头、/ 结尾；off = 直连
	switch v := strings.TrimSpace(or("SPARK_APP_GIT_MIRROR", DefaultAppGitMirror)); {
	case v == AppGitMirrorOff:
		c.AppGitMirror = ""
	case strings.HasPrefix(v, "https://") && strings.HasSuffix(v, "/") && len(v) > len("https:///") && !strings.ContainsAny(v, " '\"\\\n\t"):
		c.AppGitMirror = v
	default:
		return fmt.Errorf("SPARK_APP_GIT_MIRROR 必须以 https:// 开头、以 / 结尾（或 off 表示直连），当前为 %q", v)
	}
	switch v := or("SPARK_APP_RUNNER", "on"); v {
	case "on":
		c.AppRunner = true
	case "off":
		c.AppRunner = false
	default:
		return fmt.Errorf("SPARK_APP_RUNNER 只能是 on 或 off，当前为 %q", v)
	}
	return nil
}

// loadButler 读取并校验 AI 管家（计划 12）的 LLM 配置。
func (c *Config) loadButler(getenv func(string) string, or func(key, def string) string) error {
	var err error
	l := ButlerLLMConfig{
		Provider: strings.TrimSpace(or("SPARK_LLM_PROVIDER", LLMOpenAI)),
		BaseURL:  strings.TrimRight(strings.TrimSpace(getenv("SPARK_LLM_BASE_URL")), "/"),
		APIKey:   strings.TrimSpace(getenv("SPARK_LLM_API_KEY")),
		Model:    strings.TrimSpace(getenv("SPARK_LLM_MODEL")),
	}
	switch l.Provider {
	case LLMOpenAI, LLMAnthropic, LLMMock:
	default:
		return fmt.Errorf("SPARK_LLM_PROVIDER 只能是 openai、anthropic 或 mock，当前为 %q", l.Provider)
	}
	if c.Env == "prod" && l.Provider == LLMMock {
		return errors.New("生产环境禁止使用 mock LLM 提供方（SPARK_LLM_PROVIDER）")
	}
	if l.Model == "" {
		l.Model = DefaultLLMModel(l.Provider)
	}
	if l.BaseURL != "" {
		// 错误信息不回显原值：URL 里可能带着凭据
		u, err := url.Parse(l.BaseURL)
		if err != nil {
			return errors.New("SPARK_LLM_BASE_URL 不是合法的 URL")
		}
		if u.User != nil {
			return errors.New("SPARK_LLM_BASE_URL 不能包含用户名或密码（密钥请放 SPARK_LLM_API_KEY）")
		}
		if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("SPARK_LLM_BASE_URL 必须以 http:// 或 https:// 开头，当前为 %q", u.Redacted())
		}
	}
	if l.MaxOutputTokens, err = positiveInt("SPARK_LLM_MAX_OUTPUT_TOKENS", or("SPARK_LLM_MAX_OUTPUT_TOKENS", strconv.Itoa(DefaultLLMMaxOutputTokens))); err != nil {
		return err
	}
	if l.PriceInCentsPerM, err = nonNegativeInt("SPARK_LLM_PRICE_INPUT_CENTS_PER_M", or("SPARK_LLM_PRICE_INPUT_CENTS_PER_M", "0")); err != nil {
		return err
	}
	if l.PriceOutCentsPerM, err = nonNegativeInt("SPARK_LLM_PRICE_OUTPUT_CENTS_PER_M", or("SPARK_LLM_PRICE_OUTPUT_CENTS_PER_M", "0")); err != nil {
		return err
	}
	if l.Tools, err = onOff("SPARK_LLM_TOOLS", or("SPARK_LLM_TOOLS", "on")); err != nil {
		return err
	}
	streamDef := "on"
	if l.Provider == LLMOpenAI {
		streamDef = "off"
	}
	if l.Stream, err = onOff("SPARK_LLM_STREAM", or("SPARK_LLM_STREAM", streamDef)); err != nil {
		return err
	}
	explicit := getenv("SPARK_BUTLER") != ""
	if c.Butler, err = onOff("SPARK_BUTLER", or("SPARK_BUTLER", "on")); err != nil {
		return err
	}
	c.ButlerLLM = l
	c.ButlerOffReason = ""
	if !c.Butler {
		return nil
	}
	if l.Provider == LLMOpenAI && l.BaseURL == "" {
		if c.LLMGateway {
			// 默认端点：本进程挂载的 Token 商城网关（计划 9 的 /v1），进程内调用；SPARK_LLM_API_KEY 是一把 Token 商城 API Key（sk-spark-…）。
			// 开发环境没显式开启、又没有 Key：自动关闭（否则每轮都 401），与没有端点时同理
			if c.Env == "dev" && !explicit && l.APIKey == "" {
				c.Butler = false
				c.ButlerOffReason = "未设置 SPARK_LLM_API_KEY（Token 商城 API Key），开发环境已自动关闭 AI 管家（设置 SPARK_LLM_PROVIDER=mock、或在 Token 商城为平台管家账号创建 API Key 填入 SPARK_LLM_API_KEY 即可开启）"
				return nil
			}
			// 任何环境：设了 Key 就必须是 Token 商城 Key（常见误配：填了外部端点的密钥却忘了 SPARK_LLM_BASE_URL，每轮都 401）。错误信息不回显 Key
			if l.APIKey != "" && !strings.HasPrefix(l.APIKey, LLMGatewayKeyPrefix) {
				return errors.New("AI 管家走本进程的 Token 商城网关（SPARK_LLM_BASE_URL 为空）时，SPARK_LLM_API_KEY 必须是 Token 商城 API Key（" +
					LLMGatewayKeyPrefix + " 开头）；外部端点的密钥请同时设置 SPARK_LLM_BASE_URL")
			}
			// 开发环境之外（staging / prod）必须有 Key：没有 Key 每轮都 401
			if c.Env != "dev" && l.APIKey == "" {
				return errors.New("AI 管家走本进程的 Token 商城网关时需要设置 SPARK_LLM_API_KEY（平台管家账号的 Token 商城 API Key，" +
					LLMGatewayKeyPrefix + "…）；暂不开放可设 SPARK_BUTLER=off")
			}
		} else {
			// 开发环境没显式要求开启管家：自动关闭而不是拒绝启动，免得 make run / platform admin 开箱即失败
			if c.Env == "dev" && !explicit {
				c.Butler = false
				c.ButlerOffReason = "未设置 SPARK_LLM_BASE_URL 且未开启 Token 商城网关（SPARK_LLM_GATEWAY=off），开发环境已自动关闭 AI 管家（设置 SPARK_LLM_PROVIDER=mock 或 SPARK_LLM_BASE_URL 即可开启）"
				return nil
			}
			return errors.New("SPARK_LLM_PROVIDER=openai 且 SPARK_LLM_BASE_URL 为空时，AI 管家走本进程的 Token 商城网关，需要 SPARK_LLM_GATEWAY=on；" +
				"或设置 SPARK_LLM_BASE_URL（外部 OpenAI 兼容端点，写到 /v1 为止）；暂不开放 AI 管家可设 SPARK_BUTLER=off")
		}
	}
	if c.Env == "prod" && l.Provider != LLMMock && l.APIKey == "" {
		return errors.New("生产环境开启 AI 管家需要设置 SPARK_LLM_API_KEY；暂不开放可设 SPARK_BUTLER=off")
	}
	return nil
}

// loadLLM 读取 Token 商城（计划 9）的两个开关：on / off，默认 on，其它值报错（与 SPARK_APP_RUNNER 同模式）。
func (c *Config) loadLLM(or func(key, def string) string) error {
	for _, v := range []struct {
		name string
		dst  *bool
	}{{"SPARK_LLM_GATEWAY", &c.LLMGateway}, {"SPARK_LLM_CONTROLLER", &c.LLMController}} {
		switch s := or(v.name, "on"); s {
		case "on":
			*v.dst = true
		case "off":
			*v.dst = false
		default:
			return fmt.Errorf("%s 只能是 on 或 off，当前为 %q", v.name, s)
		}
	}
	return nil
}

func onOff(name, v string) (bool, error) {
	switch v {
	case "on":
		return true, nil
	case "off":
		return false, nil
	}
	return false, fmt.Errorf("%s 只能是 on 或 off，当前为 %q", name, v)
}

func nonNegativeInt(name, v string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s 必须是非负整数，当前为 %q", name, v)
	}
	return n, nil
}

// FillAppDefaults 给没有经过 Load 的 Config 字面量（测试、e2e-server）补上应用部署镜像站的默认值；Load 之后调用无副作用。
// AppGitMirror 不补：空串是合法取值（直连，Load 读到 SPARK_APP_GIT_MIRROR=off），app.go 在 Load 之后也会调用本函数。
// AppRunner 不在这里补：它只由 cmd/platform 读取（经 Load，默认 on）。
func (c Config) FillAppDefaults() Config {
	if c.AppHFEndpoint == "" {
		c.AppHFEndpoint = DefaultAppHFEndpoint
	}
	if c.AppPipIndexURL == "" {
		c.AppPipIndexURL = DefaultAppPipIndexURL
	}
	return c
}

// TLSEnabled 报告公网监听器是否由 platform 自己终止 TLS。
func (c Config) TLSEnabled() bool { return c.TLSCertFile != "" }

// FillAccessDefaults 给没有经过 Load 的 Config 字面量（测试、e2e-server）补上接入层零值字段的默认值；Load 之后调用无副作用。
func (c Config) FillAccessDefaults() Config {
	if c.SSHAddr == "" {
		c.SSHAddr = ":2222"
	}
	if c.SSHPublicPort == 0 {
		c.SSHPublicPort = 22
	}
	if c.SSHMaxSessions == 0 {
		c.SSHMaxSessions = 16
	}
	if c.SSHDrainTimeout == 0 {
		c.SSHDrainTimeout = 30 * time.Second
	}
	if c.TrustedProxyHops == 0 {
		c.TrustedProxyHops = 1
	}
	if c.UpstreamSSHPort == 0 {
		c.UpstreamSSHPort = 2222
	}
	if c.IngressDomain == "" {
		c.IngressDomain = strings.ToLower(c.Domain)
	}
	if c.PublicBaseURL == "" {
		_, port, _ := net.SplitHostPort(c.HTTPAddr)
		if port == "" {
			port = "8080"
		}
		scheme := "http"
		if c.TLSEnabled() {
			scheme = "https"
		}
		c.PublicBaseURL = scheme + "://" + net.JoinHostPort(c.Domain, port)
	}
	return c
}

// PublicURL 返回已校验的对外地址（Load 之后一定可解析）。
func (c Config) PublicURL() *url.URL {
	u, _ := url.Parse(c.PublicBaseURL)
	return u
}

// SSHHost 是用户 SSH 命令里的主机名。
func (c Config) SSHHost() string { return "ssh." + c.Domain }

func portValue(name, v string) (int, error) {
	n, err := positiveInt(name, v)
	if err != nil || n > 65535 {
		return 0, fmt.Errorf("%s 必须是 1～65535 的端口号，当前为 %q", name, v)
	}
	return n, nil
}

// samePort 判断两个监听地址是否会抢同一个端口（host 为空、0.0.0.0 或 :: 视为所有地址）。
func samePort(a, b string) bool {
	ha, pa, err1 := net.SplitHostPort(a)
	hb, pb, err2 := net.SplitHostPort(b)
	if err1 != nil || err2 != nil || pa != pb {
		return false
	}
	any := func(h string) bool { return h == "" || h == "0.0.0.0" || h == "::" }
	return any(ha) || any(hb) || ha == hb
}
