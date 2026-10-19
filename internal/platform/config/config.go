// Package config 从环境变量读取 platform 配置。
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
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
	Aliyun        AliyunConfig
	Wechat        WechatConfig
	Alipay        AlipayConfig
}

// AliyunConfig 阿里云短信与号码百科（三要素核验）共用一组 AccessKey。
type AliyunConfig struct {
	AccessKeyID     string
	AccessKeySecret string
	SMSSign         string
	SMSTplLogin     string
	SMSTplExpiry    string
	KYCAuthCode     string
	SMSEndpoint     string
	KYCEndpoint     string
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
			AccessKeyID:     getenv("SPARK_ALIYUN_ACCESS_KEY_ID"),
			AccessKeySecret: getenv("SPARK_ALIYUN_ACCESS_KEY_SECRET"),
			SMSSign:         getenv("SPARK_SMS_SIGN"),
			SMSTplLogin:     getenv("SPARK_SMS_TPL_LOGIN"),
			SMSTplExpiry:    getenv("SPARK_SMS_TPL_EXPIRY"),
			KYCAuthCode:     getenv("SPARK_KYC_AUTH_CODE"),
			SMSEndpoint:     or("SPARK_ALIYUN_SMS_ENDPOINT", "https://dysmsapi.aliyuncs.com"),
			KYCEndpoint:     or("SPARK_ALIYUN_KYC_ENDPOINT", "https://dytnsapi.aliyuncs.com"),
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
			"SPARK_SMS_SIGN", a.SMSSign, "SPARK_SMS_TPL_LOGIN", a.SMSTplLogin, "SPARK_SMS_TPL_EXPIRY", a.SMSTplExpiry); err != nil {
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
