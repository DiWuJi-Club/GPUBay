package billing_test

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/billing"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/plugins/pay"
)

func paid(r billing.Recharge) pay.Notification {
	return pay.Notification{OutTradeNo: r.ID, ChannelTradeNo: "T-" + r.ID, Amount: r.Amount, Paid: true}
}

func TestCreateValidates(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		channel string
		amount  int64
		want    string
	}{
		{pay.ChannelWechat, 99, "bad_request"},
		{pay.ChannelWechat, 100_000_001, "bad_request"},
		{"paypal", 100, "bad_request"},
		{billing.ChannelManual, 100, "bad_request"},
	} {
		if _, err := e.svc.Create(e.ctx, e.userID, c.channel, c.amount); codeOf(err) != c.want {
			t.Fatalf("Create(%s, %d) = %v, want %s", c.channel, c.amount, err, c.want)
		}
	}
	unverified := dbtest.CreateUser(t, e.pool, "13700000002", false)
	if _, err := e.svc.Create(e.ctx, unverified, pay.ChannelWechat, 100); codeOf(err) != "kyc_required" {
		t.Fatalf("unverified = %v", err)
	}
	r := e.create(pay.ChannelAlipay, 5000)
	if !strings.HasPrefix(r.ID, "rch-") || len(r.ID) != 16 || r.Status != billing.StatusPending || r.QRCode != "mock://pay/"+r.ID ||
		!r.ExpiresAt.Equal(dbtest.T0.Add(billing.QRTTL)) || r.Channel != pay.ChannelAlipay {
		t.Fatalf("recharge = %+v", r)
	}
	if got := e.svc.Channels(); len(got) != 2 || got[0] != "alipay" || got[1] != "wechat" {
		t.Fatalf("channels = %v", got)
	}
}

func TestCreatePendingLimit(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < billing.MaxPending; i++ {
		e.create(pay.ChannelWechat, 100)
	}
	if _, err := e.svc.Create(e.ctx, e.userID, pay.ChannelWechat, 100); codeOf(err) != "too_many_pending" {
		t.Fatalf("6th pending = %v", err)
	}
	e.clk.Advance(billing.QRTTL) // 二维码过期的单不再占名额
	e.create(pay.ChannelWechat, 100)
}

func TestCreateProviderFailureClosesRecharge(t *testing.T) {
	e := newEnv(t)
	e.wechat.FailCreate = true
	if _, err := e.svc.Create(e.ctx, e.userID, pay.ChannelWechat, 100); codeOf(err) != "pay_unavailable" {
		t.Fatalf("create = %v", err)
	}
	var status string
	_ = e.pool.QueryRow(e.ctx, `SELECT status FROM recharges WHERE user_id = $1`, e.userID).Scan(&status)
	if status != billing.StatusClosed {
		t.Fatalf("status = %s", status)
	}
}

func TestSettleCreditsOnceUnderConcurrency(t *testing.T) {
	e := newEnv(t)
	r := e.create(pay.ChannelWechat, 12345)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := e.svc.Settle(e.ctx, pay.ChannelWechat, paid(r)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if e.balance() != 12345 || e.status(r.ID) != billing.StatusPaid {
		t.Fatalf("balance = %d status = %s", e.balance(), e.status(r.ID))
	}
	var rows int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM wallet_ledger WHERE ref_type = 'recharge' AND ref_id = $1`, r.ID).Scan(&rows)
	if rows != 1 {
		t.Fatalf("ledger rows = %d", rows)
	}
	got, err := e.svc.Get(e.ctx, e.userID, r.ID)
	if err != nil || got.ChannelTradeNo != "T-"+r.ID || got.PaidAt == nil {
		t.Fatalf("recharge = %+v, %v", got, err)
	}
}

func TestSettleIgnoresUnpaid(t *testing.T) {
	e := newEnv(t)
	r := e.create(pay.ChannelWechat, 100)
	n := paid(r)
	n.Paid = false
	if err := e.svc.Settle(e.ctx, pay.ChannelWechat, n); err != nil || e.status(r.ID) != billing.StatusPending || e.balance() != 0 {
		t.Fatalf("err=%v status=%s balance=%d", err, e.status(r.ID), e.balance())
	}
}

func TestSettleAmountMismatchRaisesAlertOnce(t *testing.T) {
	e := newEnv(t)
	r := e.create(pay.ChannelWechat, 100)
	n := paid(r)
	n.Amount = 1
	for i := 0; i < 3; i++ {
		if err := e.svc.Settle(e.ctx, pay.ChannelWechat, n); err == nil {
			t.Fatal("amount mismatch must fail")
		}
	}
	if e.balance() != 0 || e.status(r.ID) != billing.StatusPending || e.alerts("recharge_amount_mismatch") != 1 {
		t.Fatalf("balance=%d status=%s alerts=%d", e.balance(), e.status(r.ID), e.alerts("recharge_amount_mismatch"))
	}
}

func TestSettleUnknownTradeIsRejected(t *testing.T) {
	e := newEnv(t)
	r := e.create(pay.ChannelWechat, 100)
	if err := e.svc.Settle(e.ctx, pay.ChannelWechat, pay.Notification{OutTradeNo: "rch-notmine00000", Amount: 100, Paid: true}); err == nil {
		t.Fatal("unknown out_trade_no must fail")
	}
	if err := e.svc.Settle(e.ctx, pay.ChannelAlipay, paid(r)); err == nil {
		t.Fatal("notification via another channel must fail")
	}
	if e.balance() != 0 || e.status(r.ID) != billing.StatusPending {
		t.Fatalf("balance=%d status=%s", e.balance(), e.status(r.ID))
	}
}

func TestSweepQueriesLostCallbacksAndCloses(t *testing.T) {
	e := newEnv(t)
	lost := e.create(pay.ChannelWechat, 300)
	abandoned := e.create(pay.ChannelAlipay, 500)
	e.wechat.MarkPaid(lost.ID, 300)

	e.clk.Advance(30 * time.Second) // 创建不足 1 分钟不查询
	if err := e.svc.Sweep(e.ctx); err != nil {
		t.Fatal(err)
	}
	if e.status(lost.ID) != billing.StatusPending {
		t.Fatal("queried too early")
	}
	e.clk.Advance(30 * time.Second)
	if err := e.svc.Sweep(e.ctx); err != nil {
		t.Fatal(err)
	}
	if e.status(lost.ID) != billing.StatusPaid || e.balance() != 300 {
		t.Fatalf("lost callback not recovered: status=%s balance=%d", e.status(lost.ID), e.balance())
	}
	e.clk.Advance(billing.CloseAfter)
	if err := e.svc.Sweep(e.ctx); err != nil {
		t.Fatal(err)
	}
	if e.status(abandoned.ID) != billing.StatusClosed || !e.alipay.Closed(abandoned.ID) {
		t.Fatalf("abandoned recharge: status=%s closedAtChannel=%v", e.status(abandoned.ID), e.alipay.Closed(abandoned.ID))
	}
}

func TestSweepSettlesInsteadOfClosingWhenPaidAtLastCheck(t *testing.T) {
	e := newEnv(t)
	r := e.create(pay.ChannelAlipay, 700)
	e.clk.Advance(billing.CloseAfter + time.Minute)
	e.alipay.MarkPaid(r.ID, 700)
	if err := e.svc.Sweep(e.ctx); err != nil {
		t.Fatal(err)
	}
	if e.status(r.ID) != billing.StatusPaid || e.balance() != 700 || e.alipay.Closed(r.ID) {
		t.Fatalf("status=%s balance=%d closed=%v", e.status(r.ID), e.balance(), e.alipay.Closed(r.ID))
	}
}

func TestPaidAfterCloseIsCreditedWithWarning(t *testing.T) {
	e := newEnv(t)
	r := e.create(pay.ChannelWechat, 900)
	e.clk.Advance(billing.CloseAfter + time.Minute)
	if err := e.svc.Sweep(e.ctx); err != nil {
		t.Fatal(err)
	}
	if e.status(r.ID) != billing.StatusClosed {
		t.Fatal("not closed")
	}
	if err := e.svc.Settle(e.ctx, pay.ChannelWechat, paid(r)); err != nil {
		t.Fatal(err)
	}
	if e.status(r.ID) != billing.StatusPaid || e.balance() != 900 || e.alerts("recharge_paid_after_close") != 1 {
		t.Fatalf("status=%s balance=%d alerts=%d", e.status(r.ID), e.balance(), e.alerts("recharge_paid_after_close"))
	}
}

func TestHandleNotify(t *testing.T) {
	e := newEnv(t)
	r := e.create(pay.ChannelWechat, 100)
	body := `{"out_trade_no":"` + r.ID + `","channel_trade_no":"T1","amount":100,"paid":true}`

	req := httptest.NewRequest("POST", "/api/v1/pay/notify/wechat", strings.NewReader(body))
	req.Header.Set(pay.MockSignatureHeader, "forged")
	w := httptest.NewRecorder()
	e.svc.HandleNotify(e.ctx, pay.ChannelWechat, w, req)
	if w.Code != 400 || e.balance() != 0 {
		t.Fatalf("forged notify = %d balance %d", w.Code, e.balance())
	}

	req = httptest.NewRequest("POST", "/api/v1/pay/notify/wechat", strings.NewReader(body))
	req.Header.Set(pay.MockSignatureHeader, "wx-secret")
	w = httptest.NewRecorder()
	e.svc.HandleNotify(e.ctx, pay.ChannelWechat, w, req)
	if w.Code != 200 || w.Body.String() != "success" || e.balance() != 100 {
		t.Fatalf("notify = %d %q balance %d", w.Code, w.Body.String(), e.balance())
	}

	w = httptest.NewRecorder()
	e.svc.HandleNotify(e.ctx, "paypal", w, httptest.NewRequest("POST", "/api/v1/pay/notify/paypal", nil))
	if w.Code != 404 {
		t.Fatalf("unknown channel = %d", w.Code)
	}
}

func TestManualRecharge(t *testing.T) {
	e := newEnv(t)
	actor := audit.Actor{AdminID: 7, IP: "10.0.0.2"}
	if _, err := e.svc.ManualRecharge(e.ctx, actor, e.userID, 10000, " "); codeOf(err) != "bad_request" {
		t.Fatalf("empty remark = %v", err)
	}
	if _, err := e.svc.ManualRecharge(e.ctx, actor, e.userID, 0, "转账"); codeOf(err) != "bad_request" {
		t.Fatalf("zero amount = %v", err)
	}
	if _, err := e.svc.ManualRecharge(e.ctx, actor, 9999, 10000, "转账"); codeOf(err) != "not_found" {
		t.Fatalf("unknown user = %v", err)
	}
	r, err := e.svc.ManualRecharge(e.ctx, actor, e.userID, 10000, "对公转账 流水 123")
	if err != nil {
		t.Fatal(err)
	}
	if r.Channel != billing.ChannelManual || r.Status != billing.StatusPaid || r.OperatorID == nil || *r.OperatorID != 7 || e.balance() != 10000 {
		t.Fatalf("recharge = %+v balance=%d", r, e.balance())
	}
	var kind string
	var operator int64
	_ = e.pool.QueryRow(e.ctx, `SELECT kind, operator_id FROM wallet_ledger WHERE ref_id = $1`, r.ID).Scan(&kind, &operator)
	var auditN int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM audit_logs WHERE action = 'wallet.manual_recharge' AND actor_id = 7 AND ip = '10.0.0.2'`).Scan(&auditN)
	if kind != "recharge" || operator != 7 || auditN != 1 {
		t.Fatalf("ledger kind=%s operator=%d audit=%d", kind, operator, auditN)
	}
}

func TestListAndGetForUser(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 3; i++ {
		e.create(pay.ChannelWechat, 100)
		e.clk.Advance(time.Second)
	}
	items, total, err := e.svc.ListForUser(e.ctx, e.userID, paging.Page{Number: 1, Size: 2})
	if err != nil || total != 3 || len(items) != 2 || !items[0].CreatedAt.After(items[1].CreatedAt) {
		t.Fatalf("list = %d items, total %d, %v", len(items), total, err)
	}
	other := dbtest.CreateUser(t, e.pool, "13700000003", true)
	if _, err := e.svc.Get(e.ctx, other, items[0].ID); codeOf(err) != "not_found" {
		t.Fatalf("get other's recharge = %v", err)
	}
}
