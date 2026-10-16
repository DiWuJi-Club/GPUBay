package api_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"spark-cloud/internal/api"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/plugins/pay"
)

// verifiedClient 登录并完成实名，返回客户端与用户 ID。
func (e *testEnv) verifiedClient(phone string) (*client, int64) {
	c := e.client()
	u := c.login(phone)
	if code, out := c.do("POST", "/kyc/personal", map[string]string{"name": "张三", "id_number": "11010119900101123X"}); code != 200 {
		e.t.Fatalf("kyc = %d %v", code, out)
	}
	return c, int64(u["id"].(float64))
}

func TestRechargeFlowWithDevPay(t *testing.T) {
	e := newTestEnv(t)
	c, _ := e.verifiedClient("13800000011")
	if code, out := c.do("GET", "/pay/channels", nil); code != 200 || len(out["items"].([]any)) != 2 {
		t.Fatalf("channels = %d %v", code, out)
	}
	code, out := c.do("POST", "/recharges", map[string]any{"amount": 5000, "channel": "wechat"})
	if code != 201 || out["status"] != "pending" || !strings.HasPrefix(out["qr_code"].(string), "mock://pay/rch-") {
		t.Fatalf("create = %d %v", code, out)
	}
	if _, ok := out["user_id"]; ok {
		t.Fatal("user_id must not be exposed")
	}
	id := out["id"].(string)
	if code, out := c.do("POST", "/dev/recharges/"+id+"/pay", map[string]any{}); code != 200 || out["status"] != "paid" {
		t.Fatalf("dev pay = %d %v", code, out)
	}
	if code, out := c.do("GET", "/recharges/"+id, nil); code != 200 || out["status"] != "paid" || out["qr_code"] != nil {
		t.Fatalf("get = %d %v", code, out)
	}
	if code, out := c.do("GET", "/wallet", nil); code != 200 || out["balance"].(float64) != 5000 {
		t.Fatalf("wallet = %d %v", code, out)
	}
	if code, out := c.do("GET", "/recharges?page=1&page_size=10", nil); code != 200 || out["total"].(float64) != 1 {
		t.Fatalf("list = %d %v", code, out)
	}
	if code, out := c.do("POST", "/dev/recharges/"+id+"/pay", map[string]any{}); code != 409 || out["code"] != "invalid_state" {
		t.Fatalf("dev pay twice = %d %v", code, out)
	}
	other, _ := e.verifiedClient("13800000012")
	if code, _ := other.do("GET", "/recharges/"+id, nil); code != 404 {
		t.Fatalf("other user's recharge = %d", code)
	}
	if code, _ := other.do("POST", "/dev/recharges/"+id+"/pay", map[string]any{}); code != 404 {
		t.Fatalf("other user dev pay = %d", code)
	}
}

func TestRechargeRequiresLoginAndKYC(t *testing.T) {
	e := newTestEnv(t)
	c := e.client()
	if code, _ := c.do("POST", "/recharges", map[string]any{"amount": 5000, "channel": "wechat"}); code != 401 {
		t.Fatalf("anonymous = %d", code)
	}
	c.login("13800000013")
	if code, out := c.do("POST", "/recharges", map[string]any{"amount": 5000, "channel": "wechat"}); code != 403 || out["code"] != "kyc_required" {
		t.Fatalf("without kyc = %d %v", code, out)
	}
}

func TestPayNotifyEndpoint(t *testing.T) {
	e := newTestEnv(t)
	c, _ := e.verifiedClient("13800000014")
	_, out := c.do("POST", "/recharges", map[string]any{"amount": 800, "channel": "alipay"})
	id := out["id"].(string)
	notify := func(sig, body string) (int, string) {
		req, _ := http.NewRequest("POST", e.ts.URL+"/api/v1/pay/notify/alipay", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded") // 渠道回调不是 JSON，不能被统一的 JSON 检查拦下
		req.Header.Set(pay.MockSignatureHeader, sig)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	body := `{"out_trade_no":"` + id + `","channel_trade_no":"A1","amount":800,"paid":true}`
	if code, text := notify("forged", body); code != 400 || text != "fail" {
		t.Fatalf("forged = %d %q", code, text)
	}
	if code, text := notify("test-secret", body); code != 200 || text != "success" {
		t.Fatalf("notify = %d %q", code, text)
	}
	if code, text := notify("test-secret", body); code != 200 || text != "success" {
		t.Fatalf("duplicate notify = %d %q", code, text)
	}
	if _, out := c.do("GET", "/wallet", nil); out["balance"].(float64) != 800 {
		t.Fatalf("balance = %v", out["balance"])
	}
	req, _ := http.NewRequest("POST", e.ts.URL+"/api/v1/pay/notify/paypal", strings.NewReader("{}"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("unknown channel = %d", resp.StatusCode)
	}
}

func TestDevPayNotRegisteredWhenDisabled(t *testing.T) {
	srv := &api.Server{Clock: clock.NewFake(dbtest.T0), Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DevPay: false}
	ts := httptest.NewServer(srv.Router())
	defer ts.Close()
	req, _ := http.NewRequestWithContext(context.Background(), "POST", ts.URL+"/api/v1/dev/recharges/rch-1/pay", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("dev pay route without DevPay = %d, want 404", resp.StatusCode)
	}
}
