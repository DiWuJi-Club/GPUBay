package pay_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/plugins/pay"
)

func TestMockLifecycle(t *testing.T) {
	m := pay.NewMock("s3cret")
	ctx := context.Background()
	qr, err := m.Create(ctx, pay.Recharge{ID: "rch-1", Amount: 100, ExpiresAt: time.Now().Add(time.Minute)})
	if err != nil || qr != "mock://pay/rch-1" {
		t.Fatalf("create = %q, %v", qr, err)
	}
	if n, err := m.Query(ctx, "rch-1"); err != nil || n.Paid || n.OutTradeNo != "rch-1" {
		t.Fatalf("query unpaid = %+v, %v", n, err)
	}
	m.MarkPaid("rch-1", 100)
	if n, _ := m.Query(ctx, "rch-1"); !n.Paid || n.Amount != 100 || n.ChannelTradeNo != "mock-rch-1" {
		t.Fatalf("query paid = %+v", n)
	}
	if err := m.Close(ctx, "rch-1"); err != nil || !m.Closed("rch-1") {
		t.Fatal("close not recorded")
	}
	m.FailCreate = true
	if _, err := m.Create(ctx, pay.Recharge{ID: "rch-2"}); err == nil {
		t.Fatal("FailCreate ignored")
	}
}

func TestMockParseNotifyChecksSignature(t *testing.T) {
	m := pay.NewMock("s3cret")
	body := `{"out_trade_no":"rch-1","channel_trade_no":"T1","amount":100,"paid":true}`
	req := httptest.NewRequest("POST", "/notify", strings.NewReader(body))
	req.Header.Set(pay.MockSignatureHeader, "s3cret")
	n, err := m.ParseNotify(req)
	if err != nil || n != (pay.Notification{OutTradeNo: "rch-1", ChannelTradeNo: "T1", Amount: 100, Paid: true}) {
		t.Fatalf("notify = %+v, %v", n, err)
	}
	bad := httptest.NewRequest("POST", "/notify", strings.NewReader(body))
	bad.Header.Set(pay.MockSignatureHeader, "guess")
	if _, err := m.ParseNotify(bad); err == nil {
		t.Fatal("bad signature accepted")
	}
	w := httptest.NewRecorder()
	m.NotifyAck(w, true)
	if w.Code != 200 || w.Body.String() != "success" {
		t.Fatalf("ack ok = %d %q", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	m.NotifyAck(w, false)
	if w.Code != 400 || w.Body.String() != "fail" {
		t.Fatalf("ack fail = %d %q", w.Code, w.Body.String())
	}
}
