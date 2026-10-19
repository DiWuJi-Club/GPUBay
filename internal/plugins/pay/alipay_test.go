package pay

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/smartwalle/alipay/v3"
	"github.com/smartwalle/nsign"
)

const testAlipayAppID = "2021000000000001"

func pemPrivate(k *rsa.PrivateKey) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
}

func pemPublic(t *testing.T, k *rsa.PublicKey) string {
	b, err := x509.MarshalPKIXPublicKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: b}))
}

// fakeAlipayAPI 用真实客户端验签回调，其余接口返回预设结果。
type fakeAlipayAPI struct {
	*alipay.Client
	precreate alipay.TradePreCreate
	preRsp    *alipay.TradePreCreateRsp
	preErr    error
	queryRsp  *alipay.TradeQueryRsp
	queryErr  error
	closeRsp  *alipay.TradeCloseRsp
	closeErr  error
}

func (f *fakeAlipayAPI) TradePreCreate(_ context.Context, p alipay.TradePreCreate) (*alipay.TradePreCreateRsp, error) {
	f.precreate = p
	return f.preRsp, f.preErr
}

func (f *fakeAlipayAPI) TradeQuery(context.Context, alipay.TradeQuery) (*alipay.TradeQueryRsp, error) {
	return f.queryRsp, f.queryErr
}

func (f *fakeAlipayAPI) TradeClose(context.Context, alipay.TradeClose) (*alipay.TradeCloseRsp, error) {
	return f.closeRsp, f.closeErr
}

// newTestAlipay 返回插件、假接口，以及模拟支付宝签名回调的客户端（用「支付宝私钥」签名）。
func newTestAlipay(t *testing.T) (*Alipay, *fakeAlipayAPI, *alipay.Client) {
	t.Helper()
	appKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	aliKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	a, err := NewAlipay(AlipayConfig{AppID: testAlipayAppID, PrivateKeyPEM: pemPrivate(appKey),
		AlipayPublicKeyPEM: pemPublic(t, &aliKey.PublicKey), Production: false})
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeAlipayAPI{Client: a.api.(*alipay.Client)}
	a.api = fake
	signer, err := alipay.New(testAlipayAppID, pemPrivate(aliKey), false)
	if err != nil {
		t.Fatal(err)
	}
	return a, fake, signer
}

func signedAlipayNotify(t *testing.T, signer *alipay.Client, v url.Values) *http.Request {
	t.Helper()
	sig, err := signer.SignValues(v, nsign.WithIgnore("sign", "sign_type"))
	if err != nil {
		t.Fatal(err)
	}
	v.Set("sign", base64.StdEncoding.EncodeToString(sig))
	v.Set("sign_type", "RSA2")
	req := httptest.NewRequest("POST", "/api/v1/pay/notify/alipay", strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

func notifyValues(status, amount string) url.Values {
	return url.Values{"app_id": {testAlipayAppID}, "out_trade_no": {"rch-abc"}, "trade_no": {"2026010122001"},
		"trade_status": {status}, "total_amount": {amount}, "notify_id": {"n1"}}
}

func TestAlipayCreate(t *testing.T) {
	a, fake, _ := newTestAlipay(t)
	fake.preRsp = &alipay.TradePreCreateRsp{Error: alipay.Error{Code: "10000"}, QRCode: "https://qr.alipay.com/abc"}
	exp := time.Date(2026, 1, 1, 0, 15, 0, 0, time.UTC)
	qr, err := a.Create(context.Background(), Recharge{ID: "rch-abc", Amount: 5000, Subject: "充值", ExpiresAt: exp, NotifyURL: "https://spark.test/n"})
	if err != nil || qr != "https://qr.alipay.com/abc" {
		t.Fatalf("create = %q, %v", qr, err)
	}
	p := fake.precreate
	if p.OutTradeNo != "rch-abc" || p.TotalAmount != "50.00" || p.Subject != "充值" || p.NotifyURL != "https://spark.test/n" ||
		p.TimeExpire != "2026-01-01 08:15:00" {
		t.Fatalf("precreate = %+v", p)
	}
	fake.preRsp = &alipay.TradePreCreateRsp{Error: alipay.Error{Code: "40004", SubCode: "ACQ.INVALID_PARAMETER", SubMsg: "参数无效"}}
	if _, err := a.Create(context.Background(), Recharge{ID: "rch-x", Amount: 1, ExpiresAt: exp}); err == nil || !strings.Contains(err.Error(), "ACQ.INVALID_PARAMETER") {
		t.Fatalf("business failure = %v", err)
	}
	fake.preRsp, fake.preErr = nil, &alipay.Error{Code: "40002", SubCode: "isv.invalid-app-id"}
	if _, err := a.Create(context.Background(), Recharge{ID: "rch-y", Amount: 1, ExpiresAt: exp}); err == nil {
		t.Fatal("SDK error must propagate")
	}
}

func TestAlipayParseNotify(t *testing.T) {
	a, _, signer := newTestAlipay(t)
	n, err := a.ParseNotify(signedAlipayNotify(t, signer, notifyValues("TRADE_SUCCESS", "50.00")))
	if err != nil || n != (Notification{OutTradeNo: "rch-abc", ChannelTradeNo: "2026010122001", Amount: 5000, Paid: true}) {
		t.Fatalf("notify = %+v, %v", n, err)
	}
	if n, err := a.ParseNotify(signedAlipayNotify(t, signer, notifyValues("TRADE_FINISHED", "0.1"))); err != nil || !n.Paid || n.Amount != 10 {
		t.Fatalf("finished = %+v, %v", n, err)
	}
	if n, err := a.ParseNotify(signedAlipayNotify(t, signer, notifyValues("WAIT_BUYER_PAY", "50.00"))); err != nil || n.Paid {
		t.Fatalf("waiting = %+v, %v", n, err)
	}
	// alipay_cert_sn 不参与签名，由攻击者附加也不应影响验签，更不能触发证书下载
	snReq := signedAlipayNotify(t, signer, notifyValues("TRADE_SUCCESS", "50.00"))
	snBody := mustRead(t, snReq) + "&alipay_cert_sn=deadbeef"
	snReq = httptest.NewRequest("POST", "/api/v1/pay/notify/alipay", strings.NewReader(snBody))
	snReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if n, err := a.ParseNotify(snReq); err != nil || !n.Paid || n.Amount != 5000 {
		t.Fatalf("notify with alipay_cert_sn = %+v, %v", n, err)
	}
	other := notifyValues("TRADE_SUCCESS", "50.00")
	other.Set("app_id", "2021999999999999")
	if _, err := a.ParseNotify(signedAlipayNotify(t, signer, other)); err == nil {
		t.Fatal("notification for another app accepted")
	}
	if _, err := a.ParseNotify(signedAlipayNotify(t, signer, notifyValues("TRADE_SUCCESS", "abc"))); err == nil {
		t.Fatal("malformed amount accepted")
	}
	tampered := signedAlipayNotify(t, signer, notifyValues("TRADE_SUCCESS", "50.00"))
	body := strings.Replace(mustRead(t, tampered), "total_amount=50.00", "total_amount=5000.00", 1)
	forged := httptest.NewRequest("POST", "/api/v1/pay/notify/alipay", strings.NewReader(body))
	forged.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, err := a.ParseNotify(forged); err == nil {
		t.Fatal("tampered amount accepted")
	}
}

func mustRead(t *testing.T, r *http.Request) string {
	t.Helper()
	b := new(strings.Builder)
	if _, err := io.Copy(b, r.Body); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestAlipayQueryAndClose(t *testing.T) {
	a, fake, _ := newTestAlipay(t)
	ctx := context.Background()
	fake.queryErr = &alipay.Error{Code: "40004", SubCode: "ACQ.TRADE_NOT_EXIST"}
	if n, err := a.Query(ctx, "rch-abc"); err != nil || n.Paid || n.OutTradeNo != "rch-abc" {
		t.Fatalf("not exist (error) = %+v, %v", n, err)
	}
	fake.queryErr = nil
	fake.queryRsp = &alipay.TradeQueryRsp{Error: alipay.Error{Code: "40004", SubCode: "ACQ.TRADE_NOT_EXIST"}}
	if n, err := a.Query(ctx, "rch-abc"); err != nil || n.Paid {
		t.Fatalf("not exist (body) = %+v, %v", n, err)
	}
	fake.queryRsp = &alipay.TradeQueryRsp{Error: alipay.Error{Code: "10000"}, OutTradeNo: "rch-abc", TradeNo: "T9",
		TradeStatus: alipay.TradeStatusSuccess, TotalAmount: "8.00"}
	if n, err := a.Query(ctx, "rch-abc"); err != nil || !n.Paid || n.Amount != 800 || n.ChannelTradeNo != "T9" {
		t.Fatalf("paid = %+v, %v", n, err)
	}
	fake.queryRsp = &alipay.TradeQueryRsp{Error: alipay.Error{Code: "20000", SubCode: "aop.ACQ.SYSTEM_ERROR"}}
	if _, err := a.Query(ctx, "rch-abc"); err == nil {
		t.Fatal("system error must propagate")
	}
	fake.closeRsp, fake.closeErr = nil, &alipay.Error{Code: "40004", SubCode: "ACQ.TRADE_NOT_EXIST"}
	if err := a.Close(ctx, "rch-abc"); err != nil {
		t.Fatalf("close missing = %v", err)
	}
	fake.closeRsp, fake.closeErr = &alipay.TradeCloseRsp{Error: alipay.Error{Code: "10000"}}, nil
	if err := a.Close(ctx, "rch-abc"); err != nil {
		t.Fatalf("close = %v", err)
	}
	fake.closeRsp = &alipay.TradeCloseRsp{Error: alipay.Error{Code: "40004", SubCode: "ACQ.REASON_TRADE_STATUS_INVALID"}}
	if err := a.Close(ctx, "rch-abc"); err == nil {
		t.Fatal("close failure must propagate")
	}
}

func TestAlipayNotifyAck(t *testing.T) {
	a, _, _ := newTestAlipay(t)
	rec := httptest.NewRecorder()
	a.NotifyAck(rec, true)
	if rec.Code != 200 || rec.Body.String() != "success" {
		t.Fatalf("ok = %d %q", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	a.NotifyAck(rec, false)
	if rec.Body.String() != "fail" {
		t.Fatalf("fail = %d %q", rec.Code, rec.Body.String())
	}
}
