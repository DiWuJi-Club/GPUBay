package app_test

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/app"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/butler/llm"
	"spark-cloud/internal/butler/llm/mock"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/config"
	"spark-cloud/internal/platform/dbtest"
)

// butlerConfig 走真实的 config.Load（与 cmd/platform 相同的环境变量解析与默认值），再换上测试用的密钥、上传目录与上游密钥。
func butlerConfig(t *testing.T, env map[string]string) config.Config {
	t.Helper()
	cfg, err := config.Load(func(k string) string {
		if v, ok := env[k]; ok {
			return v
		}
		switch k {
		case "SPARK_DOMAIN":
			return "spark.test"
		case "SPARK_SECRET_KEY":
			return base64.StdEncoding.EncodeToString(dbtest.Key)
		case "SPARK_DATABASE_URL":
			return "postgres://unused"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg.UploadDir, cfg.UpstreamSSHKeyFile = t.TempDir(), upstreamKeyFile(t)
	return cfg
}

// sse 发一个 SSE POST，读完整个响应体（Send 的 handler 在本轮结束后才返回）。
func (c *client) sse(path string, body any) (int, string) {
	c.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, c.base+path, strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// TestBuildWiresButler：dev + SPARK_LLM_PROVIDER=mock → Build 组装管家（App.Butler / App.LLM、控制台与后台路由）；
// 工具注册表拿到真实服务：mock 规则调 list_products 走完一轮；队列里的 get_instance 返回与控制台实例视图同源的
// machine_label（fleet 机器展示名）；后台用量页带上 SPARK_LLM_PRICE_* 单价。
func TestBuildWiresButler(t *testing.T) {
	cfg := butlerConfig(t, map[string]string{"SPARK_LLM_PROVIDER": "mock",
		"SPARK_LLM_PRICE_INPUT_CENTS_PER_M": "120", "SPARK_LLM_PRICE_OUTPUT_CENTS_PER_M": "480"})
	if !cfg.Butler || cfg.ButlerLLM.Provider != config.LLMMock || !cfg.ButlerLLM.Tools {
		t.Fatalf("config = butler %v llm %+v", cfg.Butler, cfg.ButlerLLM)
	}
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	a, err := app.Build(cfg, pool, discardLog(), app.Options{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	if a.Butler == nil || a.ButlerLLM == nil || a.ButlerLLM.Name() != "mock" {
		t.Fatalf("Butler / LLM = %v / %v", a.Butler, a.ButlerLLM)
	}
	m, ok := a.ButlerLLM.(*mock.Mock)
	if !ok {
		t.Fatalf("LLM = %T, want *mock.Mock", a.ButlerLLM)
	}
	ts := httptest.NewServer(a.Handler)
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	c := &client{t: t, base: ts.URL + "/api/v1", http: &http.Client{Jar: jar}}
	c.do("POST", "/auth/sms-code", map[string]string{"phone": "13800005555"})
	c.do("POST", "/auth/login", map[string]string{"phone": "13800005555", "code": "000000"})

	st := c.do("GET", "/butler/status", nil)
	if st["enabled"] != true || st["day_limit"] != float64(300000) || st["provider_label"] == "" || st["ingress_domain"] != "spark.test" {
		t.Fatalf("status = %v", st)
	}

	// mock 规则：「选」→ list_products → 推荐文本
	code, body := c.sse("/butler/conversations/new/messages", map[string]any{"text": "帮我选一台机器"})
	if code != 200 || !strings.Contains(body, "event: done") || !strings.Contains(body, "list_products") {
		t.Fatalf("send = %d %s", code, body)
	}

	// 队列：get_instance(下单得到的实例) → machine_label 与控制台实例视图一致
	dbtest.CreateMachine(t, pool, "SN-BOT1")
	c.do("POST", "/kyc/personal", map[string]string{"name": "李四", "id_number": "110101199001011234"})
	rch := c.do("POST", "/recharges", map[string]any{"amount": 1_000_000, "channel": "wechat"})
	c.do("POST", "/dev/recharges/"+rch["id"].(string)+"/pay", map[string]any{})
	order := c.do("POST", "/orders", map[string]any{"product_id": dbtest.ProductID(t, pool), "duration": "1d", "image_id": dbtest.ImageID(t, pool)})
	instID := order["instance"].(map[string]any)["id"].(string)
	label := c.do("GET", "/instances/"+instID, nil)["machine_label"].(string)
	if label == "" {
		t.Fatal("console instance view has no machine_label")
	}
	m.Enqueue(mock.Reply{ToolCalls: []llm.ToolCall{{Name: "get_instance", Args: json.RawMessage(`{"instance_id":"` + instID + `"}`)}}})
	m.Enqueue(mock.Reply{Text: "查到了"})
	if code, body := c.sse("/butler/conversations/new/messages", map[string]any{"text": "看看我的实例"}); code != 200 || !strings.Contains(body, "event: done") {
		t.Fatalf("send = %d %s", code, body)
	}
	var raw []byte
	if err := pool.QueryRow(t.Context(), `SELECT content FROM butler_messages WHERE role = 'tool' ORDER BY id DESC LIMIT 1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var blocks []llm.Block
	if err := json.Unmarshal(raw, &blocks); err != nil || len(blocks) != 1 || !blocks[0].OK {
		t.Fatalf("tool message = %s (%v)", raw, err)
	}
	if !strings.Contains(blocks[0].Content, `"machine_label":"`+label+`"`) || !strings.Contains(blocks[0].Content, instID) {
		t.Fatalf("get_instance result lacks machine_label %q: %s", label, blocks[0].Content)
	}

	// 后台：设置与用量路由已注册，用量页带配置的单价
	adminTS := httptest.NewServer(a.AdminHandler)
	t.Cleanup(adminTS.Close)
	if _, err := a.Admins.Create(t.Context(), audit.Actor{}, "root", "correct horse battery", admin.RoleSuper); err != nil {
		t.Fatal(err)
	}
	ajar, _ := cookiejar.New(nil)
	ac := &client{t: t, base: adminTS.URL + "/admin/api/v1", http: &http.Client{Jar: ajar}}
	login := ac.do("POST", "/auth/login", map[string]string{"username": "root", "password": "correct horse battery"})
	u, _ := url.Parse(login["otpauth_uri"].(string))
	clk.Advance(30 * time.Second)
	totp, _ := admin.TOTPCode(u.Query().Get("secret"), admin.TOTPStep(clk.Now()))
	ac.do("POST", "/auth/totp/setup", map[string]string{"code": totp})
	if s := ac.do("GET", "/butler/settings", nil); s["enabled"] != true {
		t.Fatalf("admin settings = %v", s)
	}
	// 控制台与后台共用同一个 *butler.Service：后台关闭总开关后控制台立即看到（同副本设置缓存当场失效）
	ac.do("PUT", "/butler/settings", map[string]any{"enabled": false, "daily_token_limit": 300000, "max_turns": 60, "notice": "维护中"})
	if st := c.do("GET", "/butler/status", nil); st["enabled"] != false || st["notice"] != "维护中" {
		t.Fatalf("console status after admin PUT = %v", st)
	}
	usage := ac.do("GET", "/butler/usage", nil)
	if usage["price_in_cents_per_m"] != float64(120) || usage["price_out_cents_per_m"] != float64(480) {
		t.Fatalf("admin usage prices = %v", usage)
	}
	if items := usage["items"].([]any); len(items) != 1 || items[0].(map[string]any)["users"] != float64(1) {
		t.Fatalf("admin usage items = %v", usage["items"])
	}
}

// SPARK_BUTLER=off：App.Butler 为 nil，控制台 /butler/status 与后台 /butler/settings 都不注册（404）。
func TestBuildButlerOff(t *testing.T) {
	cfg := butlerConfig(t, map[string]string{"SPARK_LLM_PROVIDER": "mock", "SPARK_BUTLER": "off"})
	if cfg.Butler {
		t.Fatal("SPARK_BUTLER=off must disable the butler")
	}
	a, err := app.Build(cfg, dbtest.New(t), discardLog(), app.Options{Clock: clock.NewFake(dbtest.T0), ButlerLLM: mock.New()})
	if err != nil {
		t.Fatal(err)
	}
	if a.Butler != nil || a.ButlerLLM != nil {
		t.Fatalf("Butler / LLM = %v / %v, want nil", a.Butler, a.ButlerLLM)
	}
	ts := httptest.NewServer(a.Handler)
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar}
	c := &client{t: t, base: ts.URL + "/api/v1", http: hc}
	c.do("POST", "/auth/sms-code", map[string]string{"phone": "13800005556"})
	c.do("POST", "/auth/login", map[string]string{"phone": "13800005556", "code": "000000"})
	resp, err := hc.Get(ts.URL + "/api/v1/butler/status")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /butler/status = %d, want 404", resp.StatusCode)
	}
	rec := httptest.NewRecorder()
	a.AdminHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/api/v1/butler/settings", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("admin GET /butler/settings = %d", rec.Code)
	}
}

// 开发环境 openai 没配 SPARK_LLM_BASE_URL（也没显式 SPARK_BUTLER=on）：config 自动关闭管家，Build 正常、不注册路由。
func TestBuildButlerDevWithoutBaseURL(t *testing.T) {
	cfg := butlerConfig(t, nil)
	if cfg.Butler || cfg.ButlerOffReason == "" {
		t.Fatalf("butler %v reason %q, want auto-off in dev", cfg.Butler, cfg.ButlerOffReason)
	}
	a, err := app.Build(cfg, dbtest.New(t), discardLog(), app.Options{Clock: clock.NewFake(dbtest.T0)})
	if err != nil || a.Butler != nil {
		t.Fatalf("Build = %v, Butler %v", err, a)
	}
}

// openai 提供方构造真实适配器（不发请求）；配置字面量绕过 config 校验、BaseURL 为空时 Build 报错。
func TestBuildButlerOpenAIProvider(t *testing.T) {
	cfg := butlerConfig(t, map[string]string{"SPARK_LLM_BASE_URL": "http://127.0.0.1:1/v1", "SPARK_LLM_API_KEY": "test-key"})
	pool := dbtest.New(t)
	a, err := app.Build(cfg, pool, discardLog(), app.Options{Clock: clock.NewFake(dbtest.T0)})
	if err != nil {
		t.Fatal(err)
	}
	if a.Butler == nil || a.ButlerLLM == nil || a.ButlerLLM.Name() != config.LLMOpenAI || a.ButlerLLM.Model() != config.DefaultLLMModel(config.LLMOpenAI) {
		t.Fatalf("LLM = %v", a.ButlerLLM)
	}
	cfg.ButlerLLM.BaseURL = ""
	if _, err := app.Build(cfg, pool, discardLog(), app.Options{Clock: clock.NewFake(dbtest.T0)}); err == nil || !strings.Contains(err.Error(), "SPARK_LLM_BASE_URL") {
		t.Fatalf("err = %v, want missing base URL", err)
	}
}

func prodConfig(t *testing.T) config.Config {
	return config.Config{Env: "prod", Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "aliyun", KYCDriver: "aliyun",
		PayDriver: "real", PDUDriver: "snmp", UploadDir: t.TempDir(), Butler: true,
		ButlerLLM: config.ButlerLLMConfig{Provider: config.LLMOpenAI, BaseURL: "http://127.0.0.1:1/v1", APIKey: "test-key", Tools: true}}
}

// 测试注入的 LLM 提供方绝不能用于生产（与 DisableRateLimits 同款保护）。
func TestBuildButlerRejectsInjectedLLMInProd(t *testing.T) {
	if _, err := app.Build(prodConfig(t), nil, discardLog(), app.Options{ButlerLLM: mock.New()}); err == nil || !strings.Contains(err.Error(), "LLM") {
		t.Fatalf("err = %v, want injected LLM rejection", err)
	}
}

// 生产环境的 mock 提供方（配置字面量绕过 config.Load 的同一校验）同样报错。
func TestBuildButlerRejectsMockLLMInProd(t *testing.T) {
	cfg := prodConfig(t)
	cfg.ButlerLLM.Provider = config.LLMMock
	if _, err := app.Build(cfg, nil, discardLog(), app.Options{}); err == nil || !strings.Contains(err.Error(), "mock LLM") {
		t.Fatalf("err = %v, want mock LLM rejection", err)
	}
	cfg.Butler = false // 部署级关闭也不放过：与 config.Load 一致
	if _, err := app.Build(cfg, nil, discardLog(), app.Options{}); err == nil || !strings.Contains(err.Error(), "mock LLM") {
		t.Fatalf("butler off: err = %v, want mock LLM rejection", err)
	}
}

// 生产环境开启管家而没有 SPARK_LLM_API_KEY（配置字面量绕过 config.Load）→ 报错。
func TestBuildButlerRequiresAPIKeyInProd(t *testing.T) {
	cfg := prodConfig(t)
	cfg.ButlerLLM.APIKey = ""
	if _, err := app.Build(cfg, nil, discardLog(), app.Options{}); err == nil || !strings.Contains(err.Error(), "SPARK_LLM_API_KEY") {
		t.Fatalf("err = %v, want missing API key", err)
	}
}
