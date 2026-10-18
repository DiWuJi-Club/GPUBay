package app_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/app"
	"spark-cloud/internal/audit"
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
	cfg := config.Config{UpstreamSSHKeyFile: upstreamKeyFile(t), Env: "dev", UploadDir: t.TempDir(), Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "mock", KYCDriver: "mock"}
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
	c.do("POST", "/auth/login", map[string]string{"phone": "13800001234", "code": "000000"})
	c.do("POST", "/kyc/personal", map[string]string{"name": "李四", "id_number": "110101199001011234"})
	rch := c.do("POST", "/recharges", map[string]any{"amount": 100000, "channel": "wechat"})
	if paid := c.do("POST", "/dev/recharges/"+rch["id"].(string)+"/pay", map[string]any{}); paid["status"] != "paid" {
		t.Fatalf("recharge = %v", paid)
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
	cfg := config.Config{UpstreamSSHKeyFile: upstreamKeyFile(t), Env: "dev", Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "aliyun", KYCDriver: "aliyun",
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

func TestBuildRejectsMockPayInProd(t *testing.T) {
	cfg := config.Config{Env: "prod", Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "aliyun", KYCDriver: "aliyun",
		PayDriver: "mock", UploadDir: t.TempDir()}
	if _, err := app.Build(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), app.Options{}); err == nil {
		t.Fatal("expected error for mock pay in prod")
	}
}

// writeKeys 生成一对测试 RSA 密钥，按 PKCS#8 私钥与 PKIX 公钥写入 PEM 文件。
func writeKeys(t *testing.T, dir, name string) (string, string) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privDER, _ := x509.MarshalPKCS8PrivateKey(k)
	pubDER, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	priv, pub := filepath.Join(dir, name+".pem"), filepath.Join(dir, name+"_pub.pem")
	if err := os.WriteFile(priv, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pub, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return priv, pub
}

func TestBuildWiresRealPayProviders(t *testing.T) {
	dir := t.TempDir()
	wxPriv, wxPub := writeKeys(t, dir, "wechat")
	aliPriv, aliPub := writeKeys(t, dir, "alipay")
	cfg := config.Config{UpstreamSSHKeyFile: upstreamKeyFile(t), Env: "dev", Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "mock", KYCDriver: "mock",
		PayDriver: "real", PublicBaseURL: "https://spark.test", UploadDir: t.TempDir(),
		Wechat: config.WechatConfig{MchID: "1900000001", AppID: "wx123", APIv3Key: "0123456789abcdef0123456789abcdef",
			CertSerial: "SERIAL", PrivateKeyPath: wxPriv, PublicKeyID: "PUB_KEY_ID_1", PublicKeyPath: wxPub},
		Alipay: config.AlipayConfig{AppID: "2021000000000001", PrivateKeyPath: aliPriv, PublicKeyPath: aliPub, Sandbox: true}}
	a, err := app.Build(cfg, dbtest.New(t), slog.New(slog.NewTextHandler(io.Discard, nil)), app.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := a.Billing.Channels(); len(got) != 2 || got[0] != "alipay" || got[1] != "wechat" {
		t.Fatalf("channels = %v", got)
	}
	cfg.Wechat.PrivateKeyPath = filepath.Join(dir, "missing.pem")
	if _, err := app.Build(cfg, dbtest.New(t), slog.New(slog.NewTextHandler(io.Discard, nil)), app.Options{}); err == nil {
		t.Fatal("missing key file must fail")
	}
}

func TestBuildRejectsMockPDUInProd(t *testing.T) {
	cfg := config.Config{Env: "prod", Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "aliyun", KYCDriver: "aliyun",
		PayDriver: "real", PDUDriver: "mock", UploadDir: t.TempDir()}
	if _, err := app.Build(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), app.Options{}); err == nil {
		t.Fatal("expected error for mock PDU in prod")
	}
}

// TestAdminRefundEndToEnd 走通：命令行创建管理员 → 后台登录绑定 TOTP → 登记机器并上线 →
// 用户登录、实名、充值、下单 → 开通 → 后台退款并释放 → 洗机 → 余额、流水、审计正确。
func TestAdminRefundEndToEnd(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	cfg := config.Config{UpstreamSSHKeyFile: upstreamKeyFile(t), Env: "dev", Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "mock", KYCDriver: "mock", UploadDir: t.TempDir()}
	a, err := app.Build(cfg, pool, slog.New(slog.NewTextHandler(io.Discard, nil)), app.Options{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	userTS := httptest.NewServer(a.Handler)
	defer userTS.Close()
	adminTS := httptest.NewServer(a.AdminHandler)
	defer adminTS.Close()

	if _, err := a.Admins.Create(ctx, audit.Actor{}, "root", "correct horse battery", admin.RoleSuper); err != nil {
		t.Fatal(err)
	}
	adminJar, _ := cookiejar.New(nil)
	ac := &client{t: t, base: adminTS.URL + "/admin/api/v1", http: &http.Client{Jar: adminJar}}
	login := ac.do("POST", "/auth/login", map[string]string{"username": "root", "password": "correct horse battery"})
	u, _ := url.Parse(login["otpauth_uri"].(string))
	secret := u.Query().Get("secret")
	code := func() string {
		clk.Advance(30 * time.Second)
		c, _ := admin.TOTPCode(secret, admin.TOTPStep(clk.Now()))
		return c
	}
	ac.do("POST", "/auth/totp/setup", map[string]string{"code": code()})

	products := ac.do("GET", "/products", nil)["items"].([]any)
	pid := products[0].(map[string]any)["id"]
	m := ac.do("POST", "/machines", map[string]any{"serial": "SN-E2E", "rack": "A01", "slot": "01", "lan_ip": "10.0.1.1", "product_id": pid})
	mid := int64(m["id"].(float64))
	if _, err := a.Fleet.Heartbeat(ctx, mid); err != nil { // Agent 上线
		t.Fatal(err)
	}
	ac.do("POST", fmt.Sprintf("/machines/%d/maintenance", mid), map[string]bool{"on": false})

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

	userJar, _ := cookiejar.New(nil)
	uc := &client{t: t, base: userTS.URL + "/api/v1", http: &http.Client{Jar: userJar}}
	uc.do("POST", "/auth/sms-code", map[string]string{"phone": "13800005678"})
	user := uc.do("POST", "/auth/login", map[string]string{"phone": "13800005678", "code": "000000"})
	uc.do("POST", "/kyc/personal", map[string]string{"name": "王五", "id_number": "110101199001011234"})
	rch := uc.do("POST", "/recharges", map[string]any{"amount": 100000, "channel": "alipay"})
	uc.do("POST", "/dev/recharges/"+rch["id"].(string)+"/pay", map[string]any{})
	images := uc.do("GET", "/images", nil)["items"].([]any)
	order := uc.do("POST", "/orders", map[string]any{"product_id": pid, "duration": "1d", "image_id": images[0].(map[string]any)["id"]})
	id := order["instance"].(map[string]any)["id"].(string)
	agent()

	quote := ac.do("GET", "/instances/"+id+"/refund-quote", nil)
	refund := ac.do("POST", "/instances/"+id+"/refund-release", map[string]any{"amount": quote["suggested"], "reason": "机器故障", "totp": code()})
	agent()
	if inst := uc.do("GET", "/instances/"+id, nil); inst["state"] != "released" {
		t.Fatalf("instance after refund-release = %v", inst)
	}
	refunded := int64(refund["refunded"].(float64))
	if refunded <= 0 || refunded > 6900 {
		t.Fatalf("refunded = %d", refunded)
	}
	if bal := uc.do("GET", "/wallet", nil)["balance"].(float64); int64(bal) != 100000-6900+refunded {
		t.Fatalf("balance = %v", bal)
	}
	kinds := map[string]int{}
	for _, row := range uc.do("GET", "/wallet/ledger", nil)["items"].([]any) {
		kinds[row.(map[string]any)["kind"].(string)]++
	}
	if kinds["recharge"] != 1 || kinds["order_pay"] != 1 || kinds["refund"] != 1 {
		t.Fatalf("ledger kinds = %v", kinds)
	}
	uid := int64(user["id"].(float64))
	detail := ac.do("GET", fmt.Sprintf("/users/%d", uid), nil)
	if int64(detail["balance"].(float64)) != 100000-6900+refunded {
		t.Fatalf("admin user detail = %v", detail)
	}
	for _, action := range []string{"machine.register", "machine.maintenance", "instance.refund_release"} {
		if got := ac.do("GET", "/audit?action="+action, nil); got["total"].(float64) != 1 {
			t.Fatalf("audit %s = %v", action, got)
		}
	}
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestBuildRejectsDisabledRateLimitsInProd(t *testing.T) {
	cfg := config.Config{Env: "prod", Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "aliyun", KYCDriver: "aliyun",
		PayDriver: "real", PDUDriver: "snmp", UploadDir: t.TempDir()}
	if _, err := app.Build(cfg, nil, discardLog(), app.Options{DisableRateLimits: true}); err == nil || !strings.Contains(err.Error(), "速率限制") {
		t.Fatalf("err = %v, want 生产环境禁止关闭速率限制", err)
	}
}

func TestServersUseConfiguredAddressesAndTimeouts(t *testing.T) {
	cfg := config.Config{UpstreamSSHKeyFile: upstreamKeyFile(t), Env: "dev", Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "mock", KYCDriver: "mock", UploadDir: t.TempDir()}
	a, err := app.Build(cfg, dbtest.New(t), discardLog(), app.Options{})
	if err != nil {
		t.Fatal(err)
	}
	cfg.HTTPAddr, cfg.AdminAddr = "127.0.0.1:18080", "127.0.0.1:18081"
	pub, adm := a.Servers(cfg)
	if pub.Addr != "127.0.0.1:18080" || adm.Addr != "127.0.0.1:18081" || pub.Handler == nil || adm.Handler == nil ||
		pub.ReadHeaderTimeout != 10*time.Second || pub.WriteTimeout != 60*time.Second || adm.WriteTimeout != 120*time.Second ||
		pub.IdleTimeout != 120*time.Second {
		t.Fatalf("servers = %+v / %+v", pub, adm)
	}
}

func TestSecureCookieOutsideDev(t *testing.T) {
	for env, want := range map[string]struct {
		name   string
		secure bool
	}{"dev": {"sc_session", false}, "staging": {"__Host-sc_session", true}} {
		cfg := config.Config{UpstreamSSHKeyFile: upstreamKeyFile(t), Env: env, Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "mock", KYCDriver: "mock", UploadDir: t.TempDir()}
		a, err := app.Build(cfg, dbtest.New(t), discardLog(), app.Options{Clock: clock.NewFake(dbtest.T0)})
		if err != nil {
			t.Fatal(err)
		}
		ts := httptest.NewServer(a.Handler)
		post := func(path, body string) *http.Response {
			resp, err := http.Post(ts.URL+"/api/v1"+path, "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			return resp
		}
		post("/auth/sms-code", `{"phone":"13800007777"}`).Body.Close()
		resp := post("/auth/login", `{"phone":"13800007777","code":"000000"}`)
		resp.Body.Close()
		secure := false
		for _, c := range resp.Cookies() {
			if c.Name == want.name {
				secure = c.Secure
			}
		}
		if resp.StatusCode != 200 || secure != want.secure {
			t.Fatalf("%s: status %d secure %v, want %v (cookie %q)", env, resp.StatusCode, secure, want.secure, want.name)
		}
		ts.Close()
	}
}

// TestDisableRateLimitsLiftsBothLimits 核对 DisableRateLimits 确实放开了验证码按 IP 每小时上限（默认 30）
// 与后台登录按 IP 每分钟上限（默认 20）；未设置时同样的请求序列会在第 31 / 21 次被限流。
func TestDisableRateLimitsLiftsBothLimits(t *testing.T) {
	const smsTries, loginTries = 31, 21
	for _, disabled := range []bool{false, true} {
		cfg := config.Config{UpstreamSSHKeyFile: upstreamKeyFile(t), Env: "dev", Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "mock", KYCDriver: "mock", UploadDir: t.TempDir()}
		a, err := app.Build(cfg, dbtest.New(t), discardLog(), app.Options{Clock: clock.NewFake(dbtest.T0), DisableRateLimits: disabled})
		if err != nil {
			t.Fatal(err)
		}
		post := func(h http.Handler, path, body string) int {
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.RemoteAddr = "192.0.2.7:4000"
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			return rec.Code
		}
		smsLast := 0
		for i := 0; i < smsTries; i++ { // 不同手机号、同一 IP，避开按手机号的间隔与日上限
			smsLast = post(a.Handler, "/api/v1/auth/sms-code", fmt.Sprintf(`{"phone":"1380000%04d"}`, i))
		}
		loginLast := 0
		for i := 0; i < loginTries; i++ {
			loginLast = post(a.AdminHandler, "/admin/api/v1/auth/login", `{"username":"nobody","password":"wrong-password"}`)
		}
		wantSMS, wantLogin := http.StatusTooManyRequests, http.StatusTooManyRequests
		if disabled {
			wantSMS, wantLogin = http.StatusOK, http.StatusUnauthorized
		}
		if smsLast != wantSMS || loginLast != wantLogin {
			t.Fatalf("disabled=%v: sms #%d = %d (want %d), admin login #%d = %d (want %d)",
				disabled, smsTries, smsLast, wantSMS, loginTries, loginLast, wantLogin)
		}
	}
}
