// Package pay 定义支付渠道插件：微信支付 Native、支付宝当面付，以及开发与测试用的 mock。
package pay

import (
	"context"
	"net/http"
	"time"
)

const (
	ChannelWechat = "wechat"
	ChannelAlipay = "alipay"
)

// Recharge 是发起扫码支付所需的充值单信息。
type Recharge struct {
	ID        string // 商户订单号（out_trade_no）
	Amount    int64  // 分
	Subject   string
	ExpiresAt time.Time // 二维码失效时间
	NotifyURL string
}

// Notification 是支付回调或主动查询得到的支付结果。
type Notification struct {
	OutTradeNo     string
	ChannelTradeNo string
	Amount         int64 // 分
	Paid           bool
}

type Provider interface {
	// Create 在渠道下单，返回二维码内容（前端据此生成二维码）。
	Create(ctx context.Context, r Recharge) (qrCode string, err error)
	// ParseNotify 验签并解析回调；验签失败返回错误。
	ParseNotify(req *http.Request) (Notification, error)
	// Query 主动查询订单；渠道上不存在的订单返回 Paid=false 而不是错误。
	Query(ctx context.Context, outTradeNo string) (Notification, error)
	// Close 关闭未支付的订单；订单不存在视为成功。
	Close(ctx context.Context, outTradeNo string) error
	// NotifyAck 按渠道要求的格式回复回调。
	NotifyAck(w http.ResponseWriter, ok bool)
}
