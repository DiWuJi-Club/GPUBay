package pay

import (
	"bytes"
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/wechatpay-apiv3/wechatpay-go/core"
	"github.com/wechatpay-apiv3/wechatpay-go/services/payments"
	"github.com/wechatpay-apiv3/wechatpay-go/services/payments/native"
)

const testV3Key = "0123456789abcdef0123456789abcdef"

type fakeWechatAPI struct {
	prepay    native.PrepayRequest
	codeURL   string
	prepayErr error
	queryTx   *payments.Transaction
	queryErr  error
	closeErr  error
	closed    []string
}

func (f *fakeWechatAPI) Prepay(_ context.Context, req native.PrepayRequest) (*native.PrepayResponse, *core.APIResult, error) {
	f.prepay = req
	if f.prepayErr != nil {
		return nil, nil, f.prepayErr
	}
	return &native.PrepayResponse{CodeUrl: core.String(f.codeURL)}, nil, nil
}

func (f *fakeWechatAPI) QueryOrderByOutTradeNo(context.Context, native.QueryOrderByOutTradeNoRequest) (*payments.Transaction, *core.APIResult, error) {
	return f.queryTx, nil, f.queryErr
}

func (f *fakeWechatAPI) CloseOrder(_ context.Context, req native.CloseOrderRequest) (*core.APIResult, error) {
	if f.closeErr != nil {
		return nil, f.closeErr
	}
	f.closed = append(f.closed, *req.OutTradeNo)
	return nil, nil
}

func newTestWechat(t *testing.T) (*Wechat, *rsa.PrivateKey, *fakeWechatAPI) {
	t.Helper()
	platformKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	merchantKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeWechatAPI{}
	w, err := newWechat(WechatConfig{MchID: "1900000001", AppID: "wx123", APIv3Key: testV3Key, CertSerial: "MCHSERIAL",
		PrivateKey: merchantKey, PublicKeyID: "PUB_KEY_ID_1", PublicKey: &platformKey.PublicKey}, fake)
	if err != nil {
		t.Fatal(err)
	}
	return w, platformKey, fake
}

// signedNotify 按微信支付 APIv3 规则加密 resource 并用平台私钥签名，得到一个回调请求。
func signedNotify(t *testing.T, key *rsa.PrivateKey, serial string, tx map[string]any) *http.Request {
	t.Helper()
	plain, _ := json.Marshal(tx)
	block, _ := aes.NewCipher([]byte(testV3Key))
	gcm, _ := cipher.NewGCM(block)
	nonce := "abcdefghijkl"
	ct := gcm.Seal(nil, []byte(nonce), plain, []byte("transaction"))
	body, _ := json.Marshal(map[string]any{
		"id": "EV-1", "event_type": "TRANSACTION.SUCCESS", "resource_type": "encrypt-resource", "summary": "支付成功",
		"resource": map[string]string{"algorithm": "AEAD_AES_256_GCM", "ciphertext": base64.StdEncoding.EncodeToString(ct),
			"associated_data": "transaction", "nonce": nonce, "original_type": "transaction"},
	})
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sum := sha256.Sum256([]byte(ts + "\nNONCE123\n" + string(body) + "\n"))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/v1/pay/notify/wechat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Wechatpay-Timestamp", ts)
	req.Header.Set("Wechatpay-Nonce", "NONCE123")
	req.Header.Set("Wechatpay-Signature", base64.StdEncoding.EncodeToString(sig))
	req.Header.Set("Wechatpay-Serial", serial)
	return req
}

func successTx(state string) map[string]any {
	return map[string]any{"mchid": "1900000001", "appid": "wx123", "out_trade_no": "rch-abc", "transaction_id": "4200001",
		"trade_state": state, "amount": map[string]any{"total": 5000, "currency": "CNY"}}
}

func TestWechatCreate(t *testing.T) {
	w, _, fake := newTestWechat(t)
	fake.codeURL = "weixin://wxpay/bizpayurl?pr=abc"
	exp := time.Date(2026, 1, 1, 0, 15, 0, 0, time.UTC)
	qr, err := w.Create(context.Background(), Recharge{ID: "rch-abc", Amount: 5000, Subject: "充值", ExpiresAt: exp, NotifyURL: "https://spark.test/api/v1/pay/notify/wechat"})
	if err != nil || qr != fake.codeURL {
		t.Fatalf("create = %q, %v", qr, err)
	}
	p := fake.prepay
	if *p.Appid != "wx123" || *p.Mchid != "1900000001" || *p.OutTradeNo != "rch-abc" || *p.Description != "充值" ||
		*p.Amount.Total != 5000 || *p.Amount.Currency != "CNY" || *p.NotifyUrl != "https://spark.test/api/v1/pay/notify/wechat" || !p.TimeExpire.Equal(exp) {
		t.Fatalf("prepay = %+v", p)
	}
	fake.codeURL = ""
	if _, err := w.Create(context.Background(), Recharge{ID: "rch-x", Amount: 1}); err == nil {
		t.Fatal("empty code_url must fail")
	}
	fake.prepayErr = errors.New("network down")
	if _, err := w.Create(context.Background(), Recharge{ID: "rch-y", Amount: 1}); err == nil {
		t.Fatal("prepay error must propagate")
	}
}

func TestWechatParseNotify(t *testing.T) {
	w, key, _ := newTestWechat(t)
	n, err := w.ParseNotify(signedNotify(t, key, "PUB_KEY_ID_1", successTx("SUCCESS")))
	if err != nil || n != (Notification{OutTradeNo: "rch-abc", ChannelTradeNo: "4200001", Amount: 5000, Paid: true}) {
		t.Fatalf("notify = %+v, %v", n, err)
	}
	if n, err := w.ParseNotify(signedNotify(t, key, "PUB_KEY_ID_1", successTx("NOTPAY"))); err != nil || n.Paid {
		t.Fatalf("NOTPAY = %+v, %v", n, err)
	}
	if _, err := w.ParseNotify(signedNotify(t, key, "OTHER_KEY", successTx("SUCCESS"))); err == nil {
		t.Fatal("wrong serial accepted")
	}
	forger, _ := rsa.GenerateKey(rand.Reader, 2048)
	if _, err := w.ParseNotify(signedNotify(t, forger, "PUB_KEY_ID_1", successTx("SUCCESS"))); err == nil {
		t.Fatal("forged signature accepted")
	}
	other := successTx("SUCCESS")
	other["mchid"] = "1900000999"
	if _, err := w.ParseNotify(signedNotify(t, key, "PUB_KEY_ID_1", other)); err == nil {
		t.Fatal("notification for another merchant accepted")
	}
}

func TestWechatQueryAndClose(t *testing.T) {
	w, _, fake := newTestWechat(t)
	ctx := context.Background()
	fake.queryErr = &core.APIError{StatusCode: 404, Code: "ORDER_NOT_EXIST", Message: "订单不存在"}
	if n, err := w.Query(ctx, "rch-abc"); err != nil || n.Paid || n.OutTradeNo != "rch-abc" {
		t.Fatalf("not exist = %+v, %v", n, err)
	}
	fake.queryErr = errors.New("timeout")
	if _, err := w.Query(ctx, "rch-abc"); err == nil {
		t.Fatal("query error must propagate")
	}
	fake.queryErr = nil
	fake.queryTx = &payments.Transaction{Mchid: core.String("1900000001"), OutTradeNo: core.String("rch-abc"),
		TransactionId: core.String("4200001"), TradeState: core.String("SUCCESS"), Amount: &payments.TransactionAmount{Total: core.Int64(5000)}}
	if n, err := w.Query(ctx, "rch-abc"); err != nil || !n.Paid || n.Amount != 5000 {
		t.Fatalf("paid = %+v, %v", n, err)
	}
	if err := w.Close(ctx, "rch-abc"); err != nil || len(fake.closed) != 1 {
		t.Fatalf("close = %v %v", err, fake.closed)
	}
	fake.closeErr = &core.APIError{StatusCode: 404, Code: "ORDER_NOT_EXIST"}
	if err := w.Close(ctx, "rch-gone"); err != nil {
		t.Fatalf("close missing = %v", err)
	}
	fake.closeErr = errors.New("timeout")
	if err := w.Close(ctx, "rch-abc"); err == nil {
		t.Fatal("close error must propagate")
	}
}

func TestWechatNotifyAck(t *testing.T) {
	w, _, _ := newTestWechat(t)
	rec := httptest.NewRecorder()
	w.NotifyAck(rec, true)
	if rec.Code != 200 || rec.Body.Len() != 0 {
		t.Fatalf("ok ack = %d %q", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	w.NotifyAck(rec, false)
	if rec.Code != 500 || rec.Body.String() != `{"code":"FAIL","message":"失败"}` {
		t.Fatalf("fail ack = %d %q", rec.Code, rec.Body.String())
	}
}
