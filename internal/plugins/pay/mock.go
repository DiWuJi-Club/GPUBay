package pay

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
)

// MockSignatureHeader 是 mock 回调的「签名」请求头，值必须等于 Mock 的密钥。
const MockSignatureHeader = "X-Mock-Signature"

// Mock 是开发与测试用的支付渠道：二维码为 mock://pay/<单号>，回调为带共享密钥请求头的 JSON。
type Mock struct {
	FailCreate bool // 为 true 时 Create 返回错误，用于测试渠道故障

	secret string
	mu     sync.Mutex
	paid   map[string]Notification
	closed map[string]bool
}

func NewMock(secret string) *Mock {
	return &Mock{secret: secret, paid: map[string]Notification{}, closed: map[string]bool{}}
}

func (m *Mock) Create(_ context.Context, r Recharge) (string, error) {
	if m.FailCreate {
		return "", errors.New("mock pay: create failed")
	}
	return "mock://pay/" + r.ID, nil
}

func (m *Mock) ParseNotify(req *http.Request) (Notification, error) {
	sig := req.Header.Get(MockSignatureHeader)
	if m.secret == "" || subtle.ConstantTimeCompare([]byte(sig), []byte(m.secret)) != 1 {
		return Notification{}, errors.New("mock pay: bad signature")
	}
	var body struct {
		OutTradeNo     string `json:"out_trade_no"`
		ChannelTradeNo string `json:"channel_trade_no"`
		Amount         int64  `json:"amount"`
		Paid           bool   `json:"paid"`
	}
	if err := json.NewDecoder(io.LimitReader(req.Body, 64<<10)).Decode(&body); err != nil {
		return Notification{}, err
	}
	return Notification{OutTradeNo: body.OutTradeNo, ChannelTradeNo: body.ChannelTradeNo, Amount: body.Amount, Paid: body.Paid}, nil
}

func (m *Mock) Query(_ context.Context, outTradeNo string) (Notification, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n, ok := m.paid[outTradeNo]; ok {
		return n, nil
	}
	return Notification{OutTradeNo: outTradeNo}, nil
}

func (m *Mock) Close(_ context.Context, outTradeNo string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed[outTradeNo] = true
	return nil
}

func (m *Mock) NotifyAck(w http.ResponseWriter, ok bool) {
	if ok {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "success")
		return
	}
	w.WriteHeader(http.StatusBadRequest)
	_, _ = io.WriteString(w, "fail")
}

// MarkPaid 让 Query 返回已支付（模拟用户已付款但回调丢失）。
func (m *Mock) MarkPaid(id string, amount int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.paid[id] = Notification{OutTradeNo: id, ChannelTradeNo: "mock-" + id, Amount: amount, Paid: true}
}

func (m *Mock) Closed(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed[id]
}
