package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"spark-cloud/internal/app"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/config"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/plugins/sms"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

type client struct {
	t    *testing.T
	base string
	http *http.Client
}

func (c *client) do(method, path string, body any) map[string]any {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode >= 300 {
		c.t.Fatalf("%s %s = %d %v", method, path, resp.StatusCode, out)
	}
	return out
}

// TestFullRentalLifecycle 走通：登录 → 实名 → 充值 → 下单 → 开通 → 到期提醒 → 到期停机 → 续费 → 重新启动 → 释放 → 洗机 → 机器回到库存。
func TestFullRentalLifecycle(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	smsMock := &sms.Mock{}
	cfg := config.Config{Env: "dev", UploadDir: t.TempDir(), Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "mock", KYCDriver: "mock"}
	a, err := app.Build(cfg, pool, slog.New(slog.NewTextHandler(io.Discard, nil)), app.Options{Clock: clk, SMS: smsMock})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(a.Handler)
	defer ts.Close()
	jar, _ := cookiejar.New(nil)
	c := &client{t: t, base: ts.URL + "/api/v1", http: &http.Client{Jar: jar}}
	mid := dbtest.CreateMachine(t, pool, "SN001")

	// agent 模拟 Agent：心跳并成功执行全部待处理指令
	agent := func() {
		t.Helper()
		if _, err := a.Fleet.Heartbeat(ctx, mid); err != nil {
			t.Fatal(err)
		}
		cmds, err := a.Fleet.PendingCommands(ctx, mid)
		if err != nil {
			t.Fatal(err)
		}
		for _, cmd := range cmds {
			if err := a.Rental.HandleCommandResult(ctx, cmd.ID, true, "ok"); err != nil {
				t.Fatal(err)
			}
		}
	}
	sweep := func() {
		t.Helper()
		if _, err := a.Fleet.Heartbeat(ctx, mid); err != nil {
			t.Fatal(err)
		}
		if err := a.Rental.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
	}

	c.do("POST", "/auth/sms-code", map[string]string{"phone": "13800001234"})
	user := c.do("POST", "/auth/login", map[string]string{"phone": "13800001234", "code": "000000"})
	c.do("POST", "/kyc/personal", map[string]string{"name": "李四", "id_number": "110101199001011234"})
	if _, err := a.Wallet.AdminAdjust(ctx, 1, int64(user["id"].(float64)), 100000, "线下转账", ""); err != nil {
		t.Fatal(err)
	}

	products := c.do("GET", "/products", nil)["items"].([]any)
	images := c.do("GET", "/images", nil)["items"].([]any)
	order := c.do("POST", "/orders", map[string]any{
		"product_id": products[0].(map[string]any)["id"], "duration": "1d", "image_id": images[0].(map[string]any)["id"],
	})
	id := order["instance"].(map[string]any)["id"].(string)

	clk.Advance(90 * time.Second)
	agent()
	inst := c.do("GET", "/instances/"+id, nil)
	if inst["state"] != "running" || inst["remaining_seconds"].(float64) != 86400 || inst["ssh_command"] != "ssh "+id+"@ssh.spark.test" {
		t.Fatalf("after create = %v", inst)
	}

	clk.Advance(23 * time.Hour)
	sweep()
	sent := smsMock.Sent()
	last := sent[len(sent)-1]
	if last.Template != sms.TemplateExpiryReminder || last.Params["instance"] != id || last.Params["hours"] != "1" {
		t.Fatalf("reminder sms = %+v", last)
	}

	clk.Advance(time.Hour)
	sweep()
	agent()
	inst = c.do("GET", "/instances/"+id, nil)
	if inst["state"] != "stopped" || inst["retain_until"] == nil {
		t.Fatalf("after expiry = %v", inst)
	}

	clk.Advance(time.Hour)
	c.do("POST", "/instances/"+id+"/renew", map[string]string{"duration": "1w"})
	agent()
	inst = c.do("GET", "/instances/"+id, nil)
	if inst["state"] != "running" || inst["remaining_seconds"].(float64) != 7*86400 {
		t.Fatalf("after renew = %v", inst)
	}

	c.do("POST", "/instances/"+id+"/release", map[string]any{})
	agent()
	if inst = c.do("GET", "/instances/"+id, nil); inst["state"] != "released" {
		t.Fatalf("after release = %v", inst)
	}
	if p := c.do("GET", "/products", nil)["items"].([]any)[0].(map[string]any); p["idle_count"].(float64) != 1 {
		t.Fatalf("machine not back in stock: %v", p)
	}
	if bal := c.do("GET", "/wallet", nil)["balance"].(float64); bal != 100000-6900-41400 {
		t.Fatalf("balance = %v", bal)
	}
	if rows := c.do("GET", "/wallet/ledger", nil)["items"].([]any); len(rows) != 3 {
		t.Fatalf("ledger rows = %d", len(rows))
	}
}

func TestBuildRejectsMockPluginsInProd(t *testing.T) {
	cfg := config.Config{Env: "prod", Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "mock", KYCDriver: "mock"}
	if _, err := app.Build(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), app.Options{}); err == nil {
		t.Fatal("expected error for mock drivers in prod")
	}
}

// TestBuildWiresAliyunPlugins 验证 aliyun 驱动按配置的 endpoint 与凭证发出请求。
func TestBuildWiresAliyunPlugins(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	var mu sync.Mutex
	var actions []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		mu.Lock()
		actions = append(actions, q.Get("Action")+":"+q.Get("AccessKeyId"))
		mu.Unlock()
		switch q.Get("Action") {
		case "SendSms":
			_, _ = w.Write([]byte(`{"Code":"OK"}`))
		case "ThreeElementsVerification":
			_, _ = w.Write([]byte(`{"Code":"OK","Data":{"IsConsistent":1}}`))
		}
	}))
	defer srv.Close()
	cfg := config.Config{Env: "dev", Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "aliyun", KYCDriver: "aliyun",
		UploadDir: t.TempDir(), Aliyun: config.AliyunConfig{AccessKeyID: "ak", AccessKeySecret: "sk", SMSSign: "Spark",
			SMSTplLogin: "SMS_1", SMSTplExpiry: "SMS_2", KYCAuthCode: "auth", SMSEndpoint: srv.URL, KYCEndpoint: srv.URL}}
	a, err := app.Build(cfg, pool, slog.New(slog.NewTextHandler(io.Discard, nil)), app.Options{Clock: clock.NewFake(dbtest.T0)})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Identity.SendLoginCode(ctx, "13800001234", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	uid := dbtest.CreateUser(t, pool, "13800009999", false)
	if u, err := a.Identity.SubmitPersonalKYC(ctx, uid, "张三", "11010119900101123X"); err != nil || u.KYCStatus != "verified" {
		t.Fatalf("kyc = %+v, %v", u, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(actions) != 2 || actions[0] != "SendSms:ak" || actions[1] != "ThreeElementsVerification:ak" {
		t.Fatalf("actions = %v", actions)
	}
}
