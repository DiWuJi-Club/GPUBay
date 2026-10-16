// Package app 按配置组装所有服务，供 cmd/platform 与端到端测试共用。
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wechatpay-apiv3/wechatpay-go/utils"

	"spark-cloud/internal/api"
	"spark-cloud/internal/billing"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/identity"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/config"
	"spark-cloud/internal/platform/randx"
	"spark-cloud/internal/platform/secretbox"
	"spark-cloud/internal/plugins/aliyun"
	"spark-cloud/internal/plugins/kyc"
	"spark-cloud/internal/plugins/pay"
	"spark-cloud/internal/plugins/sms"
	"spark-cloud/internal/rental"
	"spark-cloud/internal/storage"
	"spark-cloud/internal/wallet"
)

// Options 用于测试时替换时钟与插件；零值表示按配置创建。
type Options struct {
	Clock clock.Clock
	SMS   sms.Sender
	KYC   kyc.Verifier
	Pay   map[string]pay.Provider // 测试注入的支付渠道
}

type App struct {
	Identity *identity.Service
	Wallet   *wallet.Service
	Catalog  *catalog.Service
	Fleet    *fleet.Service
	Rental   *rental.Service
	Billing  *billing.Service
	Uploads  *storage.Service
	Handler  http.Handler
}

func Build(cfg config.Config, pool *pgxpool.Pool, log *slog.Logger, opt Options) (*App, error) {
	if cfg.Env == "prod" && ((opt.SMS == nil && cfg.SMSDriver == "mock") || (opt.KYC == nil && cfg.KYCDriver == "mock") ||
		(opt.Pay == nil && cfg.PayDriver != "real")) {
		return nil, errors.New("生产环境禁止使用 mock 插件")
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
					sms.TemplateLoginCode:      cfg.Aliyun.SMSTplLogin,
					sms.TemplateExpiryReminder: cfg.Aliyun.SMSTplExpiry,
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

	ids := identity.NewService(identity.Deps{Pool: pool, Clock: clk, SMS: smsSender, KYC: verifier, Box: box, Uploads: uploads, FixedCode: fixedCode})
	w := wallet.NewService(pool, clk)
	c := catalog.NewService(pool)
	f := fleet.NewService(pool, clk)
	r := rental.NewService(rental.Deps{
		Pool: pool, Clock: clk, Box: box, Log: log, Users: ids, Wallet: w, Catalog: c, Fleet: f,
		Notifier: expiryNotifier{ids: ids, sms: smsSender},
	})
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
	srv := &api.Server{
		Identity: ids, Wallet: w, Catalog: c, Fleet: f, Rental: r, Uploads: uploads, Billing: bill, DevPay: devPay, Clock: clk, Log: log,
		Domain: cfg.Domain, SecureCookie: cfg.Env == "prod", TrustProxy: cfg.TrustProxy,
	}
	return &App{Identity: ids, Wallet: w, Catalog: c, Fleet: f, Rental: r, Billing: bill, Uploads: uploads, Handler: srv.Router()}, nil
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
