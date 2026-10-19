package billing_test

import (
	"testing"
	"time"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/billing"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/plugins/pay"
)

func TestAdminListAndExportRecharges(t *testing.T) {
	e := newEnv(t)
	e.create(pay.ChannelWechat, 100)
	e.clk.Advance(time.Minute)
	if _, err := e.svc.ManualRecharge(e.ctx, audit.Actor{AdminID: 1}, e.userID, 500, "转账"); err != nil {
		t.Fatal(err)
	}
	list, total, err := e.svc.AdminList(e.ctx, billing.RechargeFilter{}, paging.Page{})
	if err != nil || total != 2 || list[0].Channel != billing.ChannelManual {
		t.Fatalf("all = %+v, %d, %v", list, total, err)
	}
	if _, total, _ := e.svc.AdminList(e.ctx, billing.RechargeFilter{Status: billing.StatusPending, UserID: e.userID}, paging.Page{}); total != 1 {
		t.Fatalf("pending = %d", total)
	}
	if _, _, err := e.svc.AdminList(e.ctx, billing.RechargeFilter{Channel: "paypal"}, paging.Page{}); codeOf(err) != "bad_request" {
		t.Fatalf("bad channel = %v", err)
	}
	var channels []string
	if err := e.svc.Export(e.ctx, billing.RechargeFilter{}, func(r billing.Recharge) error { channels = append(channels, r.Channel); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(channels) != 2 || channels[0] != pay.ChannelWechat {
		t.Fatalf("export = %v", channels)
	}
}

func TestPaidAmount(t *testing.T) {
	e := newEnv(t)
	r := e.create(pay.ChannelWechat, 300)
	if err := e.svc.Settle(e.ctx, pay.ChannelWechat, paid(r)); err != nil {
		t.Fatal(err)
	}
	e.create(pay.ChannelWechat, 999) // 未支付不计入
	if _, err := e.svc.ManualRecharge(e.ctx, audit.Actor{AdminID: 1}, e.userID, 700, "转账"); err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.PaidAmount(e.ctx, e.clk.Now().Add(-time.Hour), e.clk.Now().Add(time.Hour))
	if err != nil || got != 1000 {
		t.Fatalf("paid = %d, %v", got, err)
	}
}
