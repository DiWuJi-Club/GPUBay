package config_test

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

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
		c.PublicBaseURL != "" || c.Aliyun.SMSEndpoint != "https://dysmsapi.aliyuncs.com" ||
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
		"SPARK_PUBLIC_BASE_URL": "https://spark.example.com/",
		"SPARK_WECHAT_MCH_ID":   "1900000001", "SPARK_WECHAT_APP_ID": "wx123", "SPARK_WECHAT_API_V3_KEY": "0123456789abcdef0123456789abcdef",
		"SPARK_WECHAT_CERT_SERIAL": "SERIAL", "SPARK_WECHAT_PRIVATE_KEY_PATH": "/etc/spark/wx.pem",
		"SPARK_WECHAT_PUBLIC_KEY_ID": "PUB_KEY_ID_1", "SPARK_WECHAT_PUBLIC_KEY_PATH": "/etc/spark/wxpub.pem",
		"SPARK_ALIPAY_APP_ID": "2021000000000001", "SPARK_ALIPAY_PRIVATE_KEY_PATH": "/etc/spark/ali.pem",
		"SPARK_ALIPAY_PUBLIC_KEY_PATH": "/etc/spark/alipub.pem", "SPARK_ALIPAY_SANDBOX": "false",
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
