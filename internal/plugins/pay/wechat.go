package pay

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/wechatpay-apiv3/wechatpay-go/core"
	"github.com/wechatpay-apiv3/wechatpay-go/core/auth/verifiers"
	"github.com/wechatpay-apiv3/wechatpay-go/core/notify"
	"github.com/wechatpay-apiv3/wechatpay-go/core/option"
	"github.com/wechatpay-apiv3/wechatpay-go/services/payments"
	"github.com/wechatpay-apiv3/wechatpay-go/services/payments/native"
)

// WechatConfig 微信支付 APIv3 商户配置（使用「微信支付公钥」验签，启动时无需联网下载平台证书）。
type WechatConfig struct {
	MchID       string
	AppID       string
	APIv3Key    string
	CertSerial  string          // 商户 API 证书序列号
	PrivateKey  *rsa.PrivateKey // 商户 API 私钥
	PublicKeyID string          // 微信支付公钥 ID
	PublicKey   *rsa.PublicKey  // 微信支付公钥
}

// wechatAPI 是 Native 支付用到的 SDK 方法，测试时替换为假实现。
type wechatAPI interface {
	Prepay(ctx context.Context, req native.PrepayRequest) (*native.PrepayResponse, *core.APIResult, error)
	QueryOrderByOutTradeNo(ctx context.Context, req native.QueryOrderByOutTradeNoRequest) (*payments.Transaction, *core.APIResult, error)
	CloseOrder(ctx context.Context, req native.CloseOrderRequest) (*core.APIResult, error)
}

// Wechat 是微信支付 Native 扫码渠道。
type Wechat struct {
	cfg    WechatConfig
	api    wechatAPI
	notify *notify.Handler
}

func NewWechat(ctx context.Context, cfg WechatConfig) (*Wechat, error) {
	client, err := core.NewClient(ctx, option.WithWechatPayPublicKeyAuthCipher(
		cfg.MchID, cfg.CertSerial, cfg.PrivateKey, cfg.PublicKeyID, cfg.PublicKey))
	if err != nil {
		return nil, fmt.Errorf("wechat pay client: %w", err)
	}
	return newWechat(cfg, &native.NativeApiService{Client: client})
}

func newWechat(cfg WechatConfig, api wechatAPI) (*Wechat, error) {
	if cfg.PublicKey == nil || cfg.PrivateKey == nil {
		return nil, errors.New("wechat pay: 缺少商户私钥或微信支付公钥")
	}
	h, err := notify.NewRSANotifyHandler(cfg.APIv3Key, verifiers.NewSHA256WithRSAPubkeyVerifier(cfg.PublicKeyID, *cfg.PublicKey))
	if err != nil {
		return nil, fmt.Errorf("wechat pay notify handler: %w", err)
	}
	return &Wechat{cfg: cfg, api: api, notify: h}, nil
}

func (w *Wechat) Create(ctx context.Context, r Recharge) (string, error) {
	resp, _, err := w.api.Prepay(ctx, native.PrepayRequest{
		Appid:       core.String(w.cfg.AppID),
		Mchid:       core.String(w.cfg.MchID),
		Description: core.String(r.Subject),
		OutTradeNo:  core.String(r.ID),
		TimeExpire:  core.Time(r.ExpiresAt),
		NotifyUrl:   core.String(r.NotifyURL),
		Amount:      &native.Amount{Total: core.Int64(r.Amount), Currency: core.String("CNY")},
	})
	if err != nil {
		return "", fmt.Errorf("wechat prepay: %w", err)
	}
	if resp == nil || resp.CodeUrl == nil || *resp.CodeUrl == "" {
		return "", errors.New("wechat prepay: 响应缺少 code_url")
	}
	return *resp.CodeUrl, nil
}

func (w *Wechat) ParseNotify(req *http.Request) (Notification, error) {
	var tx payments.Transaction
	if _, err := w.notify.ParseNotifyRequest(req.Context(), req, &tx); err != nil {
		return Notification{}, fmt.Errorf("wechat notify: %w", err)
	}
	return w.toNotification(&tx)
}

func (w *Wechat) toNotification(tx *payments.Transaction) (Notification, error) {
	if tx == nil || tx.OutTradeNo == nil {
		return Notification{}, errors.New("wechat: 交易缺少 out_trade_no")
	}
	if tx.Mchid != nil && *tx.Mchid != w.cfg.MchID {
		return Notification{}, fmt.Errorf("wechat: 商户号不符 %s", *tx.Mchid)
	}
	n := Notification{OutTradeNo: *tx.OutTradeNo, Paid: tx.TradeState != nil && *tx.TradeState == "SUCCESS"}
	if tx.TransactionId != nil {
		n.ChannelTradeNo = *tx.TransactionId
	}
	if tx.Amount != nil && tx.Amount.Total != nil {
		n.Amount = *tx.Amount.Total
	}
	return n, nil
}

func (w *Wechat) Query(ctx context.Context, outTradeNo string) (Notification, error) {
	tx, _, err := w.api.QueryOrderByOutTradeNo(ctx, native.QueryOrderByOutTradeNoRequest{
		OutTradeNo: core.String(outTradeNo), Mchid: core.String(w.cfg.MchID)})
	if core.IsAPIError(err, "ORDER_NOT_EXIST") {
		return Notification{OutTradeNo: outTradeNo}, nil
	}
	if err != nil {
		return Notification{}, fmt.Errorf("wechat query: %w", err)
	}
	return w.toNotification(tx)
}

func (w *Wechat) Close(ctx context.Context, outTradeNo string) error {
	_, err := w.api.CloseOrder(ctx, native.CloseOrderRequest{OutTradeNo: core.String(outTradeNo), Mchid: core.String(w.cfg.MchID)})
	if err != nil && !core.IsAPIError(err, "ORDER_NOT_EXIST") {
		return fmt.Errorf("wechat close: %w", err)
	}
	return nil
}

// NotifyAck 成功回复 200 空响应；失败回复 5xx 与 FAIL，微信会按策略重试。
func (w *Wechat) NotifyAck(rw http.ResponseWriter, ok bool) {
	if ok {
		rw.WriteHeader(http.StatusOK)
		return
	}
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusInternalServerError)
	_, _ = io.WriteString(rw, `{"code":"FAIL","message":"失败"}`)
}
