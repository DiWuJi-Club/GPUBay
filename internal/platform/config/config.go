// Package config 从环境变量读取 platform 配置。
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

type Config struct {
	Env           string // dev / staging / prod
	HTTPAddr      string
	DatabaseURL   string
	Domain        string
	SecretKey     []byte // 32 字节，用于加密身份证号与实例密码
	SMSDriver     string // mock / aliyun（计划 4）
	KYCDriver     string // mock / aliyun（计划 4）
	SweepInterval time.Duration
	TrustProxy    bool // 仅当部署在会改写 X-Forwarded-For 的反向代理之后才设为 true
}

func Load(getenv func(string) string) (Config, error) {
	or := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}
	c := Config{
		Env:         or("SPARK_ENV", "dev"),
		HTTPAddr:    or("SPARK_HTTP_ADDR", ":8080"),
		DatabaseURL: getenv("SPARK_DATABASE_URL"),
		Domain:      or("SPARK_DOMAIN", "spark.local"),
		SMSDriver:   or("SPARK_SMS_DRIVER", "mock"),
		KYCDriver:   or("SPARK_KYC_DRIVER", "mock"),
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
	if c.Env == "prod" && (c.SMSDriver == "mock" || c.KYCDriver == "mock") {
		return Config{}, errors.New("生产环境禁止使用 mock 插件（SPARK_SMS_DRIVER / SPARK_KYC_DRIVER）")
	}
	switch v := getenv("SPARK_TRUST_PROXY"); v {
	case "", "false", "0":
	case "true", "1":
		c.TrustProxy = true
	default:
		return Config{}, fmt.Errorf("SPARK_TRUST_PROXY 只能是 true、false、1 或 0，当前为 %q", v)
	}
	c.SweepInterval = time.Minute
	if v := getenv("SPARK_SWEEP_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("SPARK_SWEEP_INTERVAL 格式不正确：%q", v)
		}
		c.SweepInterval = d
	}
	return c, nil
}
