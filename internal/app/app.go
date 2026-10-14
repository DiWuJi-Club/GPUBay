// Package app 按配置组装所有服务，供 cmd/platform 与端到端测试共用。
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/api"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/identity"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/config"
	"spark-cloud/internal/platform/secretbox"
	"spark-cloud/internal/plugins/kyc"
	"spark-cloud/internal/plugins/sms"
	"spark-cloud/internal/rental"
	"spark-cloud/internal/wallet"
)

// Options 用于测试时替换时钟与插件；零值表示按配置创建。
type Options struct {
	Clock clock.Clock
	SMS   sms.Sender
	KYC   kyc.Verifier
}

type App struct {
	Identity *identity.Service
	Wallet   *wallet.Service
	Catalog  *catalog.Service
	Fleet    *fleet.Service
	Rental   *rental.Service
	Handler  http.Handler
}

func Build(cfg config.Config, pool *pgxpool.Pool, log *slog.Logger, opt Options) (*App, error) {
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
		default:
			return nil, fmt.Errorf("短信驱动 %q 尚未实现（计划 4 接入阿里云）", cfg.SMSDriver)
		}
	}
	verifier := opt.KYC
	if verifier == nil {
		switch cfg.KYCDriver {
		case "mock":
			verifier = kyc.Mock{}
		default:
			return nil, fmt.Errorf("实名驱动 %q 尚未实现（计划 4 接入阿里云）", cfg.KYCDriver)
		}
	}
	fixedCode := ""
	if cfg.SMSDriver == "mock" && cfg.Env != "prod" {
		fixedCode = "000000"
	}

	ids := identity.NewService(identity.Deps{Pool: pool, Clock: clk, SMS: smsSender, KYC: verifier, Box: box, FixedCode: fixedCode})
	w := wallet.NewService(pool, clk)
	c := catalog.NewService(pool)
	f := fleet.NewService(pool, clk)
	r := rental.NewService(rental.Deps{
		Pool: pool, Clock: clk, Box: box, Log: log, Users: ids, Wallet: w, Catalog: c, Fleet: f,
		Notifier: expiryNotifier{ids: ids, sms: smsSender},
	})
	srv := &api.Server{
		Identity: ids, Wallet: w, Catalog: c, Fleet: f, Rental: r, Clock: clk, Log: log,
		Domain: cfg.Domain, SecureCookie: cfg.Env == "prod", TrustProxy: cfg.TrustProxy,
	}
	return &App{Identity: ids, Wallet: w, Catalog: c, Fleet: f, Rental: r, Handler: srv.Router()}, nil
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
