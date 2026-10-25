package config_test

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/llm"
	"spark-cloud/internal/platform/config"
)

var validKey = base64.StdEncoding.EncodeToString([]byte("01234567890123456789012345678901"))

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	c, err := config.Load(env(map[string]string{"SPARK_DATABASE_URL": "postgres://x", "SPARK_SECRET_KEY": validKey}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Env != "dev" || c.HTTPAddr != ":8080" || c.Domain != "spark.local" || c.SMSDriver != "mock" ||
		c.KYCDriver != "mock" || c.SweepInterval != time.Minute || len(c.SecretKey) != 32 || c.TrustProxy {
		t.Fatalf("config = %+v", c)
	}
}

func TestLoadTrustProxy(t *testing.T) {
	base := func(v string) map[string]string {
		return map[string]string{"SPARK_DATABASE_URL": "x", "SPARK_SECRET_KEY": validKey, "SPARK_TRUST_PROXY": v}
	}
	for v, want := range map[string]bool{"true": true, "1": true, "false": false, "0": false, "": false} {
		c, err := config.Load(env(base(v)))
		if err != nil || c.TrustProxy != want {
			t.Fatalf("SPARK_TRUST_PROXY=%q: got %v, err %v, want %v", v, c.TrustProxy, err, want)
		}
	}
}

func TestLoadErrors(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"missing db", map[string]string{"SPARK_SECRET_KEY": validKey}, "SPARK_DATABASE_URL"},
		{"bad key", map[string]string{"SPARK_DATABASE_URL": "x", "SPARK_SECRET_KEY": "short"}, "SPARK_SECRET_KEY"},
		{"bad env", map[string]string{"SPARK_DATABASE_URL": "x", "SPARK_SECRET_KEY": validKey, "SPARK_ENV": "test"}, "SPARK_ENV"},
		{"prod mock", map[string]string{"SPARK_DATABASE_URL": "x", "SPARK_SECRET_KEY": validKey, "SPARK_ENV": "prod"}, "mock"},
		{"bad interval", map[string]string{"SPARK_DATABASE_URL": "x", "SPARK_SECRET_KEY": validKey, "SPARK_SWEEP_INTERVAL": "soon"}, "SPARK_SWEEP_INTERVAL"},
		{"bad trust proxy", map[string]string{"SPARK_DATABASE_URL": "x", "SPARK_SECRET_KEY": validKey, "SPARK_TRUST_PROXY": "yes"}, "SPARK_TRUST_PROXY"},
	}
	for _, c := range cases {
		_, err := config.Load(env(c.env))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: err = %v, want mention of %s", c.name, err, c.want)
		}
	}
}

func TestLoadPlan4Defaults(t *testing.T) {
	c, err := config.Load(env(map[string]string{"SPARK_DATABASE_URL": "postgres://x", "SPARK_SECRET_KEY": validKey}))
	if err != nil {
		t.Fatal(err)
	}
	if c.AdminAddr != "127.0.0.1:8081" || c.UploadDir != "./.devdata/uploads" || c.PayDriver != "mock" || c.PDUDriver != "mock" ||
		c.PublicBaseURL != "http://spark.local:8080" || c.Aliyun.SMSEndpoint != "https://dysmsapi.aliyuncs.com" ||
		c.Aliyun.KYCEndpoint != "https://dytnsapi.aliyuncs.com" || c.Wechat.Enabled() || c.Alipay.Enabled() {
		t.Fatalf("config = %+v", c)
	}
}

// prodEnv 返回一套完整、合法的生产配置；各测试在其上增删变量。
func prodEnv() map[string]string {
	return map[string]string{
		"SPARK_ENV": "prod", "SPARK_DATABASE_URL": "postgres://x", "SPARK_SECRET_KEY": validKey,
		"SPARK_SMS_DRIVER": "aliyun", "SPARK_KYC_DRIVER": "aliyun", "SPARK_PAY_DRIVER": "real", "SPARK_PDU_DRIVER": "snmp",
		"SPARK_ALIYUN_ACCESS_KEY_ID": "ak", "SPARK_ALIYUN_ACCESS_KEY_SECRET": "sk",
		"SPARK_SMS_SIGN": "Spark", "SPARK_SMS_TPL_LOGIN": "SMS_1", "SPARK_SMS_TPL_EXPIRY": "SMS_2", "SPARK_KYC_AUTH_CODE": "auth",
		"SPARK_SMS_TPL_AUTO_RENEW_FAILED": "SMS_3", "SPARK_SMS_TPL_BOOKING_CANCELLED": "SMS_4",
		"SPARK_PUBLIC_BASE_URL": "https://spark.example.com/", "SPARK_DOMAIN": "spark.example.com", "SPARK_TRUST_PROXY": "true",
		"SPARK_WECHAT_MCH_ID": "1900000001", "SPARK_WECHAT_APP_ID": "wx123", "SPARK_WECHAT_API_V3_KEY": "0123456789abcdef0123456789abcdef",
		"SPARK_WECHAT_CERT_SERIAL": "SERIAL", "SPARK_WECHAT_PRIVATE_KEY_PATH": "/etc/spark/wx.pem",
		"SPARK_WECHAT_PUBLIC_KEY_ID": "PUB_KEY_ID_1", "SPARK_WECHAT_PUBLIC_KEY_PATH": "/etc/spark/wxpub.pem",
		"SPARK_ALIPAY_APP_ID": "2021000000000001", "SPARK_ALIPAY_PRIVATE_KEY_PATH": "/etc/spark/ali.pem",
		"SPARK_ALIPAY_PUBLIC_KEY_PATH": "/etc/spark/alipub.pem", "SPARK_ALIPAY_SANDBOX": "false",
		"SPARK_AGENT_HOSTS": "agent.example", "SPARK_UPSTREAM_SSH_KEY_FILE": "/etc/spark/up.key",
		"SPARK_LLM_BASE_URL": "http://token-mall.spark.internal/v1", "SPARK_LLM_API_KEY": "test-key",
	}
}

func TestLoadProdFullyConfigured(t *testing.T) {
	c, err := config.Load(env(prodEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicBaseURL != "https://spark.example.com" || !c.Wechat.Enabled() || !c.Alipay.Enabled() || c.Alipay.Sandbox ||
		c.Wechat.PublicKeyID != "PUB_KEY_ID_1" || c.Aliyun.KYCAuthCode != "auth" {
		t.Fatalf("config = %+v", c)
	}
}

func TestLoadPluginErrors(t *testing.T) {
	cases := []struct {
		name   string
		change map[string]string // 空字符串表示删除该变量
		want   string
	}{
		{"prod pay mock", map[string]string{"SPARK_PAY_DRIVER": "mock"}, "mock"},
		{"prod pdu mock", map[string]string{"SPARK_PDU_DRIVER": "mock"}, "mock"},
		{"bad pay driver", map[string]string{"SPARK_PAY_DRIVER": "stripe"}, "SPARK_PAY_DRIVER"},
		{"bad pdu driver", map[string]string{"SPARK_PDU_DRIVER": "ipmi"}, "SPARK_PDU_DRIVER"},
		{"bad sms driver", map[string]string{"SPARK_SMS_DRIVER": "tencent"}, "SPARK_SMS_DRIVER"},
		{"sms missing sign", map[string]string{"SPARK_SMS_SIGN": ""}, "SPARK_SMS_SIGN"},
		{"kyc missing auth", map[string]string{"SPARK_KYC_AUTH_CODE": ""}, "SPARK_KYC_AUTH_CODE"},
		{"aliyun missing secret", map[string]string{"SPARK_ALIYUN_ACCESS_KEY_SECRET": ""}, "SPARK_ALIYUN_ACCESS_KEY_SECRET"},
		{"pay real http", map[string]string{"SPARK_PUBLIC_BASE_URL": "http://spark.example.com"}, "SPARK_PUBLIC_BASE_URL"},
		{"pay real no channel", map[string]string{"SPARK_WECHAT_MCH_ID": "", "SPARK_ALIPAY_APP_ID": ""}, "SPARK_WECHAT_MCH_ID"},
		{"wechat partial", map[string]string{"SPARK_WECHAT_PRIVATE_KEY_PATH": ""}, "SPARK_WECHAT_PRIVATE_KEY_PATH"},
		{"wechat bad v3 key", map[string]string{"SPARK_WECHAT_API_V3_KEY": "short"}, "SPARK_WECHAT_API_V3_KEY"},
		{"alipay partial", map[string]string{"SPARK_ALIPAY_PUBLIC_KEY_PATH": ""}, "SPARK_ALIPAY_PUBLIC_KEY_PATH"},
		{"alipay sandbox in prod", map[string]string{"SPARK_ALIPAY_SANDBOX": "true"}, "SPARK_ALIPAY_SANDBOX"},
		{"alipay bad sandbox", map[string]string{"SPARK_ALIPAY_SANDBOX": "maybe"}, "SPARK_ALIPAY_SANDBOX"},
	}
	for _, c := range cases {
		m := prodEnv()
		for k, v := range c.change {
			if v == "" {
				delete(m, k)
			} else {
				m[k] = v
			}
		}
		_, err := config.Load(env(m))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: err = %v, want mention of %s", c.name, err, c.want)
		}
	}
}

func TestLoadPayRealWithOnlyAlipay(t *testing.T) {
	m := prodEnv()
	for _, k := range []string{"SPARK_WECHAT_MCH_ID", "SPARK_WECHAT_APP_ID", "SPARK_WECHAT_API_V3_KEY", "SPARK_WECHAT_CERT_SERIAL",
		"SPARK_WECHAT_PRIVATE_KEY_PATH", "SPARK_WECHAT_PUBLIC_KEY_ID", "SPARK_WECHAT_PUBLIC_KEY_PATH"} {
		delete(m, k)
	}
	c, err := config.Load(env(m))
	if err != nil {
		t.Fatal(err)
	}
	if c.Wechat.Enabled() || !c.Alipay.Enabled() {
		t.Fatalf("wechat=%v alipay=%v", c.Wechat.Enabled(), c.Alipay.Enabled())
	}
}

func TestLoadAgentDefaults(t *testing.T) {
	c, err := config.Load(env(map[string]string{"SPARK_DATABASE_URL": "x", "SPARK_SECRET_KEY": validKey}))
	if err != nil {
		t.Fatal(err)
	}
	if c.AgentAddr != ":9443" || strings.Join(c.AgentHosts, ",") != "localhost,127.0.0.1" || c.AgentDataSizeGiB != 2048 ||
		c.AgentBandwidthMbps != 200 || strings.Join(c.AgentDNSServers, ",") != "223.5.5.5,119.29.29.29" || len(c.AgentAllowCIDRs) != 0 {
		t.Fatalf("agent defaults = %+v", c)
	}
	if c.AgentAdvertiseAddr() != "localhost:9443" {
		t.Fatalf("advertise = %s", c.AgentAdvertiseAddr())
	}
}

func TestLoadAgentValues(t *testing.T) {
	c, err := config.Load(env(map[string]string{"SPARK_DATABASE_URL": "x", "SPARK_SECRET_KEY": validKey,
		"SPARK_AGENT_ADDR": "10.0.0.5:19443", "SPARK_AGENT_HOSTS": " agent.example , 10.0.0.5,, ", "SPARK_AGENT_ALLOW_CIDRS": "10.0.9.0/24, 192.168.1.10/32",
		"SPARK_AGENT_DNS_SERVERS": "1.1.1.1", "SPARK_AGENT_DATA_SIZE_GIB": "512", "SPARK_AGENT_BANDWIDTH_MBPS": "50",
		"SPARK_AGENT_DOWNLOAD_URL": "http://files.local/agent", "SPARK_AGENT_BINARY_SHA256": "abc", "SPARK_UPSTREAM_SSH_KEY_FILE": "/etc/spark/up.key"}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.AgentHosts, ",") != "agent.example,10.0.0.5" || c.AgentAdvertiseAddr() != "agent.example:19443" ||
		strings.Join(c.AgentAllowCIDRs, ",") != "10.0.9.0/24,192.168.1.10/32" || c.AgentDNSServers[0] != "1.1.1.1" ||
		c.AgentDataSizeGiB != 512 || c.AgentBandwidthMbps != 50 || c.AgentDownloadURL != "http://files.local/agent" || c.UpstreamSSHKeyFile != "/etc/spark/up.key" {
		t.Fatalf("agent config = %+v", c)
	}
}

func TestLoadAgentErrors(t *testing.T) {
	with := func(k, v string) map[string]string {
		m := prodEnv()
		m[k] = v
		return m
	}
	if _, err := config.Load(env(prodEnv())); err != nil {
		t.Fatalf("valid prod: %v", err)
	}
	cases := []struct{ name, key, value, want string }{
		{"prod hosts", "SPARK_AGENT_HOSTS", "", "SPARK_AGENT_HOSTS"},
		{"prod hosts only separators", "SPARK_AGENT_HOSTS", " , ,", "SPARK_AGENT_HOSTS"},
		{"host with port", "SPARK_AGENT_HOSTS", "agent.example:9443", "SPARK_AGENT_HOSTS"},
		{"host with scheme", "SPARK_AGENT_HOSTS", "https://agent.example", "SPARK_AGENT_HOSTS"},
		{"host with space", "SPARK_AGENT_HOSTS", "agent example", "SPARK_AGENT_HOSTS"},
		{"host with underscore", "SPARK_AGENT_HOSTS", "bad_host.example", "SPARK_AGENT_HOSTS"},
		{"prod upstream", "SPARK_UPSTREAM_SSH_KEY_FILE", "", "SPARK_UPSTREAM_SSH_KEY_FILE"},
		{"bad cidr", "SPARK_AGENT_ALLOW_CIDRS", "10.0.0.0", "SPARK_AGENT_ALLOW_CIDRS"},
		{"cidr host bits", "SPARK_AGENT_ALLOW_CIDRS", "10.0.9.5/24", "SPARK_AGENT_ALLOW_CIDRS"},
		{"cidr ipv6", "SPARK_AGENT_ALLOW_CIDRS", "fd00::/8", "SPARK_AGENT_ALLOW_CIDRS"},
		{"cidr 4in6", "SPARK_AGENT_ALLOW_CIDRS", "::ffff:10.0.0.0/104", "SPARK_AGENT_ALLOW_CIDRS"},
		{"cidr zero", "SPARK_AGENT_ALLOW_CIDRS", "0.0.0.0/0", "SPARK_AGENT_ALLOW_CIDRS"},
		{"bad dns", "SPARK_AGENT_DNS_SERVERS", "dns.example", "SPARK_AGENT_DNS_SERVERS"},
		{"private dns outside allow list", "SPARK_AGENT_DNS_SERVERS", "10.0.0.53", "SPARK_AGENT_DNS_SERVERS"},
		{"bad size", "SPARK_AGENT_DATA_SIZE_GIB", "0", "SPARK_AGENT_DATA_SIZE_GIB"},
		{"bad bw", "SPARK_AGENT_BANDWIDTH_MBPS", "x", "SPARK_AGENT_BANDWIDTH_MBPS"},
		{"bad addr", "SPARK_AGENT_ADDR", "9443", "SPARK_AGENT_ADDR"},
	}
	for _, c := range cases {
		_, err := config.Load(env(with(c.key, c.value)))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: err = %v, want mention of %s", c.name, err, c.want)
		}
	}
	// 非生产环境同样校验条目格式
	if _, err := config.Load(env(map[string]string{"SPARK_DATABASE_URL": "x", "SPARK_SECRET_KEY": validKey, "SPARK_AGENT_HOSTS": "a b"})); err == nil {
		t.Fatal("dev accepted malformed host")
	}
	// 私网 DNS 落在白名单内则允许（规格 §4.2：DNS 须在白名单内或为公网地址）
	m := prodEnv()
	m["SPARK_AGENT_DNS_SERVERS"], m["SPARK_AGENT_ALLOW_CIDRS"] = "10.0.0.53", "10.0.0.0/24"
	if _, err := config.Load(env(m)); err != nil {
		t.Fatalf("private dns inside allow list: %v", err)
	}
}

func TestLoadAccessDefaults(t *testing.T) {
	c, err := config.Load(env(map[string]string{"SPARK_DATABASE_URL": "x", "SPARK_SECRET_KEY": validKey}))
	if err != nil {
		t.Fatal(err)
	}
	if c.SSHAddr != ":2222" || c.SSHPublicPort != 22 || c.SSHMaxSessions != 16 || c.SSHDrainTimeout != 30*time.Second || c.UpstreamSSHPort != 2222 ||
		c.IngressDomain != "spark.local" || c.AdminPublicBaseURL != "" || c.PublicBaseURL != "http://spark.local:8080" || c.TLSEnabled() || c.HTTPRedirectAddr != "" || c.TrustedProxyHops != 1 {
		t.Fatalf("access defaults = %+v", c)
	}
	if c.PublicURL().Host != "spark.local:8080" || c.SSHHost() != "ssh.spark.local" {
		t.Fatalf("derived = %s %s", c.PublicURL(), c.SSHHost())
	}
	// 字面量（测试 / e2e-server）补默认值
	lit := config.Config{Domain: "spark.test"}.FillAccessDefaults()
	if lit.UpstreamSSHPort != 2222 || lit.IngressDomain != "spark.test" || lit.PublicBaseURL != "http://spark.test:8080" || lit.SSHMaxSessions != 16 || lit.SSHDrainTimeout != 30*time.Second || lit.TrustedProxyHops != 1 {
		t.Fatalf("literal defaults = %+v", lit)
	}
}

func TestLoadAccessValues(t *testing.T) {
	c, err := config.Load(env(map[string]string{"SPARK_DATABASE_URL": "x", "SPARK_SECRET_KEY": validKey,
		"SPARK_DOMAIN": "spark.example", "SPARK_PUBLIC_BASE_URL": "https://spark.example/", "SPARK_INGRESS_DOMAIN": "sparkapp.example",
		"SPARK_SSH_ADDR": "0.0.0.0:2200", "SPARK_SSH_PUBLIC_PORT": "2200", "SPARK_SSH_MAX_SESSIONS_PER_INSTANCE": "4", "SPARK_SSH_DRAIN_TIMEOUT": "5s",
		"SPARK_UPSTREAM_SSH_PORT": "2223", "SPARK_ADMIN_PUBLIC_BASE_URL": "https://admin.spark.example/", "SPARK_TLS_CERT_FILE": "/etc/spark/tls.crt",
		"SPARK_TLS_KEY_FILE": "/etc/spark/tls.key", "SPARK_HTTP_REDIRECT_ADDR": ":8088", "SPARK_TRUSTED_PROXY_HOPS": "2"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.SSHAddr != "0.0.0.0:2200" || c.SSHPublicPort != 2200 || c.SSHMaxSessions != 4 || c.SSHDrainTimeout != 5*time.Second || c.UpstreamSSHPort != 2223 ||
		c.IngressDomain != "sparkapp.example" || c.AdminPublicBaseURL != "https://admin.spark.example" || c.PublicBaseURL != "https://spark.example" ||
		!c.TLSEnabled() || c.HTTPRedirectAddr != ":8088" || c.TrustedProxyHops != 2 {
		t.Fatalf("access values = %+v", c)
	}
}

func TestLoadAccessErrors(t *testing.T) {
	base := func() map[string]string {
		return map[string]string{"SPARK_DATABASE_URL": "x", "SPARK_SECRET_KEY": validKey}
	}
	cases := []struct {
		name string
		set  map[string]string
		want string
	}{
		{"ssh addr no port", map[string]string{"SPARK_SSH_ADDR": "2222"}, "SPARK_SSH_ADDR"},
		{"ssh addr equals http", map[string]string{"SPARK_SSH_ADDR": ":8080"}, "SPARK_SSH_ADDR"},
		{"public port 0", map[string]string{"SPARK_SSH_PUBLIC_PORT": "0"}, "SPARK_SSH_PUBLIC_PORT"},
		{"max sessions 0", map[string]string{"SPARK_SSH_MAX_SESSIONS_PER_INSTANCE": "0"}, "SPARK_SSH_MAX_SESSIONS_PER_INSTANCE"},
		{"drain bad", map[string]string{"SPARK_SSH_DRAIN_TIMEOUT": "soon"}, "SPARK_SSH_DRAIN_TIMEOUT"},
		{"upstream port", map[string]string{"SPARK_UPSTREAM_SSH_PORT": "99999"}, "SPARK_UPSTREAM_SSH_PORT"},
		{"ingress subdomain", map[string]string{"SPARK_INGRESS_DOMAIN": "app.spark.local"}, "SPARK_INGRESS_DOMAIN"},
		{"ingress invalid", map[string]string{"SPARK_INGRESS_DOMAIN": "bad domain"}, "SPARK_INGRESS_DOMAIN"},
		{"base url host mismatch", map[string]string{"SPARK_PUBLIC_BASE_URL": "http://other.local:8080"}, "SPARK_PUBLIC_BASE_URL"},
		{"base url no scheme", map[string]string{"SPARK_PUBLIC_BASE_URL": "spark.local"}, "SPARK_PUBLIC_BASE_URL"},
		{"admin url bad", map[string]string{"SPARK_ADMIN_PUBLIC_BASE_URL": "admin.local"}, "SPARK_ADMIN_PUBLIC_BASE_URL"},
		{"tls half", map[string]string{"SPARK_TLS_CERT_FILE": "/a.crt"}, "SPARK_TLS_KEY_FILE"},
		{"admin url path", map[string]string{"SPARK_ADMIN_PUBLIC_BASE_URL": "https://a.example/x"}, "SPARK_ADMIN_PUBLIC_BASE_URL"},
		{"admin url query", map[string]string{"SPARK_ADMIN_PUBLIC_BASE_URL": "https://a.example?x=1"}, "SPARK_ADMIN_PUBLIC_BASE_URL"},
		{"admin url userinfo", map[string]string{"SPARK_ADMIN_PUBLIC_BASE_URL": "https://u:p@a.example"}, "SPARK_ADMIN_PUBLIC_BASE_URL"},
		{"tls key only", map[string]string{"SPARK_TLS_KEY_FILE": "/a.key"}, "SPARK_TLS_CERT_FILE"},
		{"tls whitespace cert", map[string]string{"SPARK_TLS_CERT_FILE": "  ", "SPARK_TLS_KEY_FILE": "/a.key"}, "SPARK_TLS_CERT_FILE"},
		{"redirect equals http", map[string]string{"SPARK_TLS_CERT_FILE": "/a.crt", "SPARK_TLS_KEY_FILE": "/a.key", "SPARK_HTTP_REDIRECT_ADDR": ":8080"}, "SPARK_HTTP_REDIRECT_ADDR"},
		{"redirect equals ssh", map[string]string{"SPARK_TLS_CERT_FILE": "/a.crt", "SPARK_TLS_KEY_FILE": "/a.key", "SPARK_HTTP_REDIRECT_ADDR": ":2222"}, "SPARK_HTTP_REDIRECT_ADDR"},
		{"redirect without tls", map[string]string{"SPARK_HTTP_REDIRECT_ADDR": ":80"}, "SPARK_HTTP_REDIRECT_ADDR"},
		{"tls with http base url", map[string]string{"SPARK_TLS_CERT_FILE": "/a.crt", "SPARK_TLS_KEY_FILE": "/a.key", "SPARK_PUBLIC_BASE_URL": "http://spark.local:8080"}, "https://"},
		{"proxy hops 0", map[string]string{"SPARK_TRUSTED_PROXY_HOPS": "0"}, "SPARK_TRUSTED_PROXY_HOPS"},
		{"proxy hops bad", map[string]string{"SPARK_TRUSTED_PROXY_HOPS": "x"}, "SPARK_TRUSTED_PROXY_HOPS"},
	}
	for _, c := range cases {
		m := base()
		for k, v := range c.set {
			m[k] = v
		}
		if _, err := config.Load(env(m)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: err = %v, want mention of %s", c.name, err, c.want)
		}
	}
	// prod：无证书且不在代理后 → 拒绝；prodEnv 带 SPARK_TRUST_PROXY=true → 通过；base url 必须 https
	p := prodEnv()
	delete(p, "SPARK_TRUST_PROXY")
	if _, err := config.Load(env(p)); err == nil || !strings.Contains(err.Error(), "SPARK_TRUST_PROXY") {
		t.Fatalf("prod without tls = %v", err)
	}
	p = prodEnv()
	if _, err := config.Load(env(p)); err != nil {
		t.Fatalf("prod behind proxy = %v", err)
	}
	p["SPARK_PUBLIC_BASE_URL"] = "http://spark.example.com"
	if _, err := config.Load(env(p)); err == nil || !strings.Contains(err.Error(), "生产环境的 SPARK_PUBLIC_BASE_URL 必须以 https://") {
		t.Fatalf("prod http base = %v", err)
	}
	p = prodEnv()
	delete(p, "SPARK_PUBLIC_BASE_URL")
	if _, err := config.Load(env(p)); err == nil || !strings.Contains(err.Error(), "生产环境的 SPARK_PUBLIC_BASE_URL 必须以 https://") {
		t.Fatalf("prod without base url = %v", err)
	}
}

func TestLoadIngressDefaultsToDomain(t *testing.T) {
	c, err := config.Load(env(map[string]string{"SPARK_DATABASE_URL": "x", "SPARK_SECRET_KEY": validKey,
		"SPARK_DOMAIN": "spark.example", "SPARK_PUBLIC_BASE_URL": "https://spark.example"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.IngressDomain != "spark.example" {
		t.Fatalf("IngressDomain = %q", c.IngressDomain)
	}
}

func TestLoadTLSDefaultsToHTTPSBaseURL(t *testing.T) {
	c, err := config.Load(env(map[string]string{"SPARK_DATABASE_URL": "x", "SPARK_SECRET_KEY": validKey,
		"SPARK_TLS_CERT_FILE": " /a.crt ", "SPARK_TLS_KEY_FILE": "/a.key"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicBaseURL != "https://spark.local:8080" || c.TLSCertFile != "/a.crt" {
		t.Fatalf("config = %q cert %q", c.PublicBaseURL, c.TLSCertFile)
	}
	if lit := (config.Config{Domain: "spark.test", TLSCertFile: "/a", TLSKeyFile: "/b"}).FillAccessDefaults(); lit.PublicBaseURL != "https://spark.test:8080" {
		t.Fatalf("literal = %q", lit.PublicBaseURL)
	}
}

func TestLoadAdminBaseURLTrimmed(t *testing.T) {
	c, err := config.Load(env(map[string]string{"SPARK_DATABASE_URL": "x", "SPARK_SECRET_KEY": validKey,
		"SPARK_ADMIN_PUBLIC_BASE_URL": "  https://admin.spark.example/  "}))
	if err != nil || c.AdminPublicBaseURL != "https://admin.spark.example" {
		t.Fatalf("admin url = %q err %v", c.AdminPublicBaseURL, err)
	}
}

func TestAliyunRequiresBookingTemplates(t *testing.T) {
	env := map[string]string{
		"SPARK_DATABASE_URL": "postgres://x", "SPARK_SECRET_KEY": "MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=",
		"SPARK_SMS_DRIVER": "aliyun", "SPARK_ALIYUN_ACCESS_KEY_ID": "k", "SPARK_ALIYUN_ACCESS_KEY_SECRET": "s",
		"SPARK_SMS_SIGN": "sign", "SPARK_SMS_TPL_LOGIN": "L", "SPARK_SMS_TPL_EXPIRY": "E",
	}
	_, err := config.Load(func(k string) string { return env[k] })
	if err == nil || !strings.Contains(err.Error(), "SPARK_SMS_TPL_AUTO_RENEW_FAILED") || !strings.Contains(err.Error(), "SPARK_SMS_TPL_BOOKING_CANCELLED") {
		t.Fatalf("want missing template error, got %v", err)
	}
	env["SPARK_SMS_TPL_AUTO_RENEW_FAILED"], env["SPARK_SMS_TPL_BOOKING_CANCELLED"] = "A", "B"
	cfg, err := config.Load(func(k string) string { return env[k] })
	if err != nil || cfg.Aliyun.SMSTplAutoRenewFailed != "A" || cfg.Aliyun.SMSTplBookingCancelled != "B" {
		t.Fatalf("load = %+v, %v", cfg.Aliyun, err)
	}
}

func TestLoadAppDefaultsAndValues(t *testing.T) {
	base := func(kv ...string) map[string]string {
		m := map[string]string{"SPARK_DATABASE_URL": "x", "SPARK_SECRET_KEY": validKey}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	c, err := config.Load(env(base()))
	if err != nil {
		t.Fatal(err)
	}
	if c.AppHFEndpoint != "https://hf-mirror.com" || c.AppPipIndexURL != "https://pypi.tuna.tsinghua.edu.cn/simple" || !c.AppRunner {
		t.Fatalf("app defaults = %q %q %v", c.AppHFEndpoint, c.AppPipIndexURL, c.AppRunner)
	}
	c, err = config.Load(env(base("SPARK_APP_HF_ENDPOINT", "http://hf.internal:8080", "SPARK_APP_PIP_INDEX_URL", "http://pypi.internal/simple",
		"SPARK_APP_RUNNER", "off")))
	if err != nil {
		t.Fatal(err)
	}
	if c.AppHFEndpoint != "http://hf.internal:8080" || c.AppPipIndexURL != "http://pypi.internal/simple" || c.AppRunner {
		t.Fatalf("app values = %q %q %v", c.AppHFEndpoint, c.AppPipIndexURL, c.AppRunner)
	}
	if c, err = config.Load(env(base("SPARK_APP_RUNNER", "on"))); err != nil || !c.AppRunner {
		t.Fatalf("SPARK_APP_RUNNER=on: %v %v", c.AppRunner, err)
	}
	for _, bad := range []map[string]string{
		base("SPARK_APP_RUNNER", "true"),
		base("SPARK_APP_RUNNER", "yes"),
		base("SPARK_APP_HF_ENDPOINT", "hf-mirror.com"),
		base("SPARK_APP_PIP_INDEX_URL", "ftp://pypi/simple"),
	} {
		if _, err := config.Load(env(bad)); err == nil || !strings.Contains(err.Error(), "SPARK_APP_") {
			t.Fatalf("%v: err = %v, want SPARK_APP_* error", bad, err)
		}
	}
	// 字面量（测试 / e2e-server）补默认值；已设置的不覆盖
	lit := config.Config{}.FillAppDefaults()
	if lit.AppHFEndpoint != "https://hf-mirror.com" || lit.AppPipIndexURL != "https://pypi.tuna.tsinghua.edu.cn/simple" {
		t.Fatalf("literal app defaults = %+v", lit)
	}
	if got := (config.Config{AppHFEndpoint: "http://x"}).FillAppDefaults().AppHFEndpoint; got != "http://x" {
		t.Fatalf("FillAppDefaults overwrote %q", got)
	}
}

// 计划 12：AI 管家的 LLM 配置。
func TestLoadLLMDefaults(t *testing.T) {
	base := func(kv ...string) map[string]string {
		m := map[string]string{"SPARK_DATABASE_URL": "x", "SPARK_SECRET_KEY": validKey}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	// 默认 provider openai；配了 BaseURL 时管家开启、模型 qwen3-32b、输出 1024、价格 0、工具开
	c, err := config.Load(env(base("SPARK_LLM_BASE_URL", "http://token-mall.spark.internal/v1/")))
	if err != nil {
		t.Fatal(err)
	}
	l := c.ButlerLLM
	if !c.Butler || c.ButlerOffReason != "" || l.Provider != "openai" || l.BaseURL != "http://token-mall.spark.internal/v1" || l.Model != "qwen3-32b" ||
		l.MaxOutputTokens != 1024 || l.PriceInCentsPerM != 0 || l.PriceOutCentsPerM != 0 || !l.Tools || l.APIKey != "" || l.Stream {
		t.Fatalf("llm defaults = %+v butler=%v reason=%q", l, c.Butler, c.ButlerOffReason)
	}
	// SPARK_LLM_STREAM：openai 默认 off（vLLM 0.10.1 hermes 流式工具调用会丢参数片段），anthropic 默认 on，显式值优先
	for _, tc := range []struct {
		provider, stream string
		want             bool
	}{{"openai", "", false}, {"openai", "on", true}, {"anthropic", "", true}, {"anthropic", "off", false}, {"mock", "", true}} {
		kv := []string{"SPARK_LLM_PROVIDER", tc.provider, "SPARK_LLM_BASE_URL", "http://token-mall.spark.internal/v1"}
		if tc.stream != "" {
			kv = append(kv, "SPARK_LLM_STREAM", tc.stream)
		}
		c, err := config.Load(env(base(kv...)))
		if err != nil || c.ButlerLLM.Stream != tc.want {
			t.Fatalf("%s stream=%q: Stream = %v err=%v", tc.provider, tc.stream, c.ButlerLLM.Stream, err)
		}
	}
	// 模型默认值按 provider
	for provider, model := range map[string]string{"anthropic": "claude-opus-5-5", "mock": "mock"} {
		c, err := config.Load(env(base("SPARK_LLM_PROVIDER", provider)))
		if err != nil || !c.Butler || c.ButlerLLM.Model != model {
			t.Fatalf("%s: model = %q butler=%v err=%v", provider, c.ButlerLLM.Model, c.Butler, err)
		}
	}
	// 显式值
	c, err = config.Load(env(base("SPARK_LLM_PROVIDER", "anthropic", "SPARK_LLM_MODEL", "claude-x", "SPARK_LLM_API_KEY", "test-key",
		"SPARK_LLM_MAX_OUTPUT_TOKENS", "4096", "SPARK_LLM_PRICE_INPUT_CENTS_PER_M", "3600", "SPARK_LLM_PRICE_OUTPUT_CENTS_PER_M", "18000",
		"SPARK_LLM_TOOLS", "off", "SPARK_BUTLER", "on")))
	if err != nil {
		t.Fatal(err)
	}
	if l := c.ButlerLLM; l.Model != "claude-x" || l.APIKey != "test-key" || l.MaxOutputTokens != 4096 || l.PriceInCentsPerM != 3600 ||
		l.PriceOutCentsPerM != 18000 || l.Tools || l.BaseURL != "" || !c.Butler {
		t.Fatalf("llm values = %+v", l)
	}
	// SPARK_BUTLER=off：不校验 BaseURL
	c, err = config.Load(env(base("SPARK_BUTLER", "off")))
	if err != nil || c.Butler || c.ButlerOffReason != "" {
		t.Fatalf("SPARK_BUTLER=off: butler=%v reason=%q err=%v", c.Butler, c.ButlerOffReason, err)
	}
	// 开发环境未显式开启管家、openai 又没有可用端点：不报错，管家按配置关闭并给出原因（make run / 后台 CLI 开箱可用）。
	// 网关默认 on：BaseURL 空 = 走本进程网关，缺的是 Token 商城 API Key
	c, err = config.Load(env(base()))
	if err != nil || c.Butler || !strings.Contains(c.ButlerOffReason, "SPARK_LLM_API_KEY") {
		t.Fatalf("dev without key: butler=%v reason=%q err=%v", c.Butler, c.ButlerOffReason, err)
	}
	// 网关关闭、又没有 BaseURL：原因是缺端点
	c, err = config.Load(env(base("SPARK_LLM_GATEWAY", "off")))
	if err != nil || c.Butler || !strings.Contains(c.ButlerOffReason, "SPARK_LLM_BASE_URL") {
		t.Fatalf("dev gateway off without base url: butler=%v reason=%q err=%v", c.Butler, c.ButlerOffReason, err)
	}
	// 默认端点（计划 9 网关，进程内）：BaseURL 空 + 网关 on + Key → 管家开启，BaseURL 保持空
	for _, kv := range [][]string{
		{"SPARK_LLM_API_KEY", "sk-spark-test"},
		{"SPARK_ENV", "staging", "SPARK_LLM_API_KEY", "sk-spark-test"}, // 开发环境之外必须有 Token 商城 Key
		{"SPARK_BUTLER", "on"},                                         // 开发环境显式开启：没 Key 也开（请求会 401，由使用者负责）
		{"SPARK_LLM_GATEWAY", "on", "SPARK_LLM_MODEL", "qwen3-4b"},     // 模型 = Token 商城的模型 slug
	} {
		if kv[0] == "SPARK_LLM_GATEWAY" {
			kv = append(kv, "SPARK_LLM_API_KEY", "sk-spark-test")
		}
		c, err := config.Load(env(base(kv...)))
		if err != nil || !c.Butler || c.ButlerOffReason != "" || c.ButlerLLM.BaseURL != "" || !c.LLMGateway {
			t.Fatalf("%v: butler=%v reason=%q base=%q err=%v", kv, c.Butler, c.ButlerOffReason, c.ButlerLLM.BaseURL, err)
		}
		if kv[0] == "SPARK_LLM_GATEWAY" && c.ButlerLLM.Model != "qwen3-4b" {
			t.Fatalf("model = %q", c.ButlerLLM.Model)
		}
	}
	// 生产：BaseURL 空 + 网关 on + Key → 进程内网关
	pm := prodEnv()
	pm["SPARK_LLM_BASE_URL"], pm["SPARK_LLM_API_KEY"] = "", "sk-spark-test"
	c, err = config.Load(env(pm))
	if err != nil || !c.Butler || c.ButlerLLM.BaseURL != "" || c.ButlerLLM.APIKey != "sk-spark-test" {
		t.Fatalf("prod in-process: butler=%v base=%q err=%v", c.Butler, c.ButlerLLM.BaseURL, err)
	}
	// 生产环境配齐：BaseURL + APIKey
	c, err = config.Load(env(prodEnv()))
	if err != nil || !c.Butler || c.ButlerLLM.APIKey != "test-key" {
		t.Fatalf("prod: butler=%v err=%v", c.Butler, err)
	}
}

func TestLoadLLMErrors(t *testing.T) {
	base := func(kv ...string) map[string]string {
		m := map[string]string{"SPARK_DATABASE_URL": "x", "SPARK_SECRET_KEY": validKey}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	prod := func(kv ...string) map[string]string {
		m := prodEnv()
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"bad provider", base("SPARK_LLM_PROVIDER", "gemini"), "SPARK_LLM_PROVIDER"},
		{"prod mock", prod("SPARK_LLM_PROVIDER", "mock"), "生产环境禁止使用 mock LLM 提供方"},
		{"prod mock butler off", prod("SPARK_LLM_PROVIDER", "mock", "SPARK_BUTLER", "off"), "生产环境禁止使用 mock LLM 提供方"},
		{"dev openai explicit on without base url, gateway off", base("SPARK_BUTLER", "on", "SPARK_LLM_GATEWAY", "off"), "SPARK_LLM_GATEWAY=on"},
		{"staging openai without base url, gateway off", base("SPARK_ENV", "staging", "SPARK_LLM_GATEWAY", "off"), "SPARK_LLM_BASE_URL"},
		{"prod openai without base url, gateway off", prod("SPARK_LLM_BASE_URL", "", "SPARK_LLM_GATEWAY", "off"), "SPARK_LLM_BASE_URL"},
		{"prod without api key", prod("SPARK_LLM_API_KEY", ""), "SPARK_LLM_API_KEY"},
		{"prod in-process gateway without api key", prod("SPARK_LLM_API_KEY", "", "SPARK_LLM_BASE_URL", ""), "SPARK_LLM_API_KEY"},
		{"staging in-process gateway without api key", base("SPARK_ENV", "staging"), "SPARK_LLM_API_KEY"},
		{"staging in-process gateway explicit on without api key", base("SPARK_ENV", "staging", "SPARK_BUTLER", "on"), "SPARK_LLM_API_KEY"},
		{"dev in-process gateway non token-mall key", base("SPARK_LLM_API_KEY", "sk-ant-secret-value"), "sk-spark-"},
		{"prod in-process gateway non token-mall key", prod("SPARK_LLM_BASE_URL", "", "SPARK_LLM_API_KEY", "test-key"), "sk-spark-"},
		{"prod anthropic without api key", prod("SPARK_LLM_PROVIDER", "anthropic", "SPARK_LLM_API_KEY", ""), "SPARK_LLM_API_KEY"},
		{"base url not http", base("SPARK_LLM_BASE_URL", "token-mall:8000/v1"), "SPARK_LLM_BASE_URL"},
		{"base url ftp", base("SPARK_LLM_PROVIDER", "anthropic", "SPARK_LLM_BASE_URL", "ftp://x/v1"), "SPARK_LLM_BASE_URL"},
		{"bad butler", base("SPARK_BUTLER", "true"), "SPARK_BUTLER"},
		{"bad tools", base("SPARK_LLM_PROVIDER", "mock", "SPARK_LLM_TOOLS", "yes"), "SPARK_LLM_TOOLS"},
		{"bad stream", base("SPARK_LLM_PROVIDER", "mock", "SPARK_LLM_STREAM", "true"), "SPARK_LLM_STREAM"},
		{"bad max tokens", base("SPARK_LLM_PROVIDER", "mock", "SPARK_LLM_MAX_OUTPUT_TOKENS", "0"), "SPARK_LLM_MAX_OUTPUT_TOKENS"},
		{"bad price", base("SPARK_LLM_PROVIDER", "mock", "SPARK_LLM_PRICE_INPUT_CENTS_PER_M", "-1"), "SPARK_LLM_PRICE_INPUT_CENTS_PER_M"},
		{"bad out price", base("SPARK_LLM_PROVIDER", "mock", "SPARK_LLM_PRICE_OUTPUT_CENTS_PER_M", "x"), "SPARK_LLM_PRICE_OUTPUT_CENTS_PER_M"},
	}
	for _, tc := range cases {
		_, err := config.Load(env(tc.env))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want contains %q", tc.name, err, tc.want)
		}
	}
	// BaseURL 带凭据：拒绝，且错误信息不回显密码
	for _, v := range []string{"http://user:s3cret-pw@token-mall/v1", "ftp://user:s3cret-pw@token-mall/v1", "http://user:s3cret-pw@[::1/v1"} {
		_, err := config.Load(env(base("SPARK_LLM_BASE_URL", v)))
		if err == nil || !strings.Contains(err.Error(), "SPARK_LLM_BASE_URL") || strings.Contains(err.Error(), "s3cret-pw") {
			t.Errorf("%s: err = %v, want SPARK_LLM_BASE_URL error without password", v, err)
		}
	}
	// Key 前缀不对：错误信息不回显 Key
	if _, err := config.Load(env(base("SPARK_LLM_API_KEY", "sk-ant-secret-value"))); err == nil || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("bad prefix err = %v", err)
	}
	// 外部端点（BaseURL 非空）不校验前缀
	if c, err := config.Load(env(base("SPARK_LLM_BASE_URL", "http://x/v1", "SPARK_LLM_API_KEY", "test-key"))); err != nil || !c.Butler {
		t.Fatalf("external endpoint key: butler=%v err=%v", c.Butler, err)
	}
	// prod 关闭管家时不要求 BaseURL / APIKey
	if c, err := config.Load(env(prod("SPARK_BUTLER", "off", "SPARK_LLM_BASE_URL", "", "SPARK_LLM_API_KEY", ""))); err != nil || c.Butler {
		t.Fatalf("prod butler off: butler=%v err=%v", c.Butler, err)
	}
}

// 计划 9：Token 商城的两个开关。
func TestLoadLLMSwitches(t *testing.T) {
	base := func(kv ...string) map[string]string {
		m := map[string]string{"SPARK_DATABASE_URL": "x", "SPARK_SECRET_KEY": validKey}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	c, err := config.Load(env(base()))
	if err != nil {
		t.Fatal(err)
	}
	if !c.LLMGateway || !c.LLMController {
		t.Fatalf("llm defaults = gateway %v controller %v, want both on", c.LLMGateway, c.LLMController)
	}
	for _, tc := range []struct {
		gw, ctl         string
		wantGW, wantCtl bool
	}{{"off", "on", false, true}, {"on", "off", true, false}, {"off", "off", false, false}, {"on", "on", true, true}} {
		c, err := config.Load(env(base("SPARK_LLM_GATEWAY", tc.gw, "SPARK_LLM_CONTROLLER", tc.ctl)))
		if err != nil {
			t.Fatal(err)
		}
		if c.LLMGateway != tc.wantGW || c.LLMController != tc.wantCtl {
			t.Fatalf("gateway=%s controller=%s → %v %v", tc.gw, tc.ctl, c.LLMGateway, c.LLMController)
		}
	}
	for _, bad := range []map[string]string{
		base("SPARK_LLM_GATEWAY", "true"),
		base("SPARK_LLM_GATEWAY", "OFF"),
		base("SPARK_LLM_CONTROLLER", "1"),
		base("SPARK_LLM_CONTROLLER", "disabled"),
	} {
		if _, err := config.Load(env(bad)); err == nil || !strings.Contains(err.Error(), "SPARK_LLM_") {
			t.Fatalf("%v: err = %v, want SPARK_LLM_* error", bad, err)
		}
	}
}

// 计划 11：SPARK_APP_GIT_MIRROR 默认 https://gh-proxy.com/；off = 直连（空）；非空须 https:// 开头、/ 结尾。
func TestLoadAppGitMirror(t *testing.T) {
	base := func(kv ...string) map[string]string {
		m := map[string]string{"SPARK_DATABASE_URL": "x", "SPARK_SECRET_KEY": validKey}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	c, err := config.Load(env(base()))
	if err != nil || c.AppGitMirror != "https://gh-proxy.com/" {
		t.Fatalf("default = %q, %v", c.AppGitMirror, err)
	}
	for in, want := range map[string]string{"off": "", " https://ghproxy.internal/x/ ": "https://ghproxy.internal/x/"} {
		c, err := config.Load(env(base("SPARK_APP_GIT_MIRROR", in)))
		if err != nil || c.AppGitMirror != want {
			t.Fatalf("%q → %q, %v; want %q", in, c.AppGitMirror, err, want)
		}
		// app.go 在 Load 之后还会调 FillAppDefaults：不得把「直连」补回默认值
		if got := c.FillAppDefaults().AppGitMirror; got != want {
			t.Fatalf("FillAppDefaults changed %q to %q", want, got)
		}
	}
	for _, bad := range []string{"http://gh-proxy.com/", "https://gh-proxy.com", "gh-proxy.com/", "https:///", "https://a b/", "https://x/'$(id)'/", "OFF"} {
		if _, err := config.Load(env(base("SPARK_APP_GIT_MIRROR", bad))); err == nil || !strings.Contains(err.Error(), "SPARK_APP_GIT_MIRROR") {
			t.Fatalf("%q: err = %v", bad, err)
		}
	}
}

// config 不导入 internal/llm，复制了 Token 商城 API Key 的前缀：两者必须一致。
func TestLLMGatewayKeyPrefixMatchesLLM(t *testing.T) {
	if config.LLMGatewayKeyPrefix != llm.KeyPrefix {
		t.Fatalf("config.LLMGatewayKeyPrefix = %q, llm.KeyPrefix = %q", config.LLMGatewayKeyPrefix, llm.KeyPrefix)
	}
}
