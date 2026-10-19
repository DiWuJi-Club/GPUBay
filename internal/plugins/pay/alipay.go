package pay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/smartwalle/alipay/v3"
)

// AlipayConfig 支付宝开放平台应用配置（公钥模式）。
type AlipayConfig struct {
	AppID              string
	PrivateKeyPEM      string // 应用私钥
	AlipayPublicKeyPEM string // 支付宝公钥
	Production         bool   // false 时使用沙箱网关
}

// alipayAPI 是当面付用到的 SDK 方法，测试时替换为假实现。
type alipayAPI interface {
	TradePreCreate(ctx context.Context, p alipay.TradePreCreate) (*alipay.TradePreCreateRsp, error)
	TradeQuery(ctx context.Context, p alipay.TradeQuery) (*alipay.TradeQueryRsp, error)
	TradeClose(ctx context.Context, p alipay.TradeClose) (*alipay.TradeCloseRsp, error)
	DecodeNotification(ctx context.Context, values url.Values) (*alipay.Notification, error)
}

// Alipay 是支付宝当面付扫码渠道。
type Alipay struct {
	appID string
	api   alipayAPI
}

var beijing = time.FixedZone("CST", 8*3600)

func NewAlipay(cfg AlipayConfig) (*Alipay, error) {
	c, err := alipay.New(cfg.AppID, cfg.PrivateKeyPEM, cfg.Production,
		alipay.WithHTTPClient(&http.Client{Timeout: 10 * time.Second}))
	if err != nil {
		return nil, fmt.Errorf("alipay client: %w", err)
	}
	if err := c.LoadAliPayPublicKey(cfg.AlipayPublicKeyPEM); err != nil {
		return nil, fmt.Errorf("alipay public key: %w", err)
	}
	return &Alipay{appID: cfg.AppID, api: c}, nil
}

// alipayFailure 统一业务失败：SDK 可能把失败放在响应体里（IsFailure），也可能作为 *alipay.Error 返回。
func alipayFailure(e alipay.Error, err error) error {
	if err != nil {
		return err
	}
	if e.IsFailure() {
		// alipay.Error.Error() 不含 SubCode，这里补上便于排查，errors.As 仍可取回原始错误
		return fmt.Errorf("%s: %w", e.SubCode, error(&e))
	}
	return nil
}

func isTradeNotExist(err error) bool {
	var e *alipay.Error
	return errors.As(err, &e) && e.SubCode == "ACQ.TRADE_NOT_EXIST"
}

func (a *Alipay) Create(ctx context.Context, r Recharge) (string, error) {
	p := alipay.TradePreCreate{Trade: alipay.Trade{
		NotifyURL:   r.NotifyURL,
		Subject:     r.Subject,
		OutTradeNo:  r.ID,
		TotalAmount: fenToYuan(r.Amount),
		TimeExpire:  r.ExpiresAt.In(beijing).Format("2006-01-02 15:04:05"),
	}}
	rsp, err := a.api.TradePreCreate(ctx, p)
	var e alipay.Error
	if rsp != nil {
		e = rsp.Error
	}
	if err := alipayFailure(e, err); err != nil {
		return "", fmt.Errorf("alipay precreate: %w", err)
	}
	if rsp == nil || rsp.QRCode == "" {
		return "", errors.New("alipay precreate: 响应缺少 qr_code")
	}
	return rsp.QRCode, nil
}

func (a *Alipay) ParseNotify(req *http.Request) (Notification, error) {
	if err := req.ParseForm(); err != nil {
		return Notification{}, fmt.Errorf("alipay notify: %w", err)
	}
	// 公钥模式不使用 alipay_cert_sn；该字段未签名，交给 SDK 会触发证书下载并切换验签证书，必须丢弃
	form := url.Values{}
	for k, v := range req.PostForm {
		form[k] = append([]string(nil), v...)
	}
	form.Del("alipay_cert_sn")
	n, err := a.api.DecodeNotification(req.Context(), form)
	if err != nil {
		return Notification{}, fmt.Errorf("alipay notify: %w", err)
	}
	if n.AppId != a.appID {
		return Notification{}, fmt.Errorf("alipay notify: app_id 不符 %s", n.AppId)
	}
	amount, err := yuanToFen(n.TotalAmount)
	if err != nil {
		return Notification{}, err
	}
	return Notification{OutTradeNo: n.OutTradeNo, ChannelTradeNo: n.TradeNo, Amount: amount,
		Paid: n.TradeStatus == alipay.TradeStatusSuccess || n.TradeStatus == alipay.TradeStatusFinished}, nil
}

func (a *Alipay) Query(ctx context.Context, outTradeNo string) (Notification, error) {
	rsp, err := a.api.TradeQuery(ctx, alipay.TradeQuery{OutTradeNo: outTradeNo})
	var e alipay.Error
	if rsp != nil {
		e = rsp.Error
	}
	if err := alipayFailure(e, err); err != nil {
		if isTradeNotExist(err) {
			return Notification{OutTradeNo: outTradeNo}, nil
		}
		return Notification{}, fmt.Errorf("alipay query: %w", err)
	}
	paid := rsp.TradeStatus == alipay.TradeStatusSuccess || rsp.TradeStatus == alipay.TradeStatusFinished
	n := Notification{OutTradeNo: outTradeNo, ChannelTradeNo: rsp.TradeNo, Paid: paid}
	if paid {
		if n.Amount, err = yuanToFen(rsp.TotalAmount); err != nil {
			return Notification{}, err
		}
	}
	return n, nil
}

func (a *Alipay) Close(ctx context.Context, outTradeNo string) error {
	rsp, err := a.api.TradeClose(ctx, alipay.TradeClose{OutTradeNo: outTradeNo})
	var e alipay.Error
	if rsp != nil {
		e = rsp.Error
	}
	if err := alipayFailure(e, err); err != nil && !isTradeNotExist(err) {
		return fmt.Errorf("alipay close: %w", err)
	}
	return nil
}

// NotifyAck 支付宝要求成功时响应体为 success，其他内容都会触发重试。
func (a *Alipay) NotifyAck(w http.ResponseWriter, ok bool) {
	if ok {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "success")
		return
	}
	w.WriteHeader(http.StatusBadRequest)
	_, _ = io.WriteString(w, "fail")
}
