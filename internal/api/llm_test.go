package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/api"
	"spark-cloud/internal/llm"
	"spark-cloud/internal/llmgw"
	"spark-cloud/internal/llmgw/llmtest"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/secretbox"
	"spark-cloud/internal/platform/tz"
)

// llmEnv 在 testEnv 上组装 Token 商城：Server.LLM + Server.LLMGateway（网关挂 /v1）。
// 网关的上游 Transport 按节点 ID 拨到 nodeAddrs 里登记的 FakeVLLM 地址（见 readyNode）。
type llmEnv struct {
	*testEnv
	svc       *llm.Service
	srv       *api.Server
	box       *secretbox.Box
	imageID   int64
	nodeAddrs *sync.Map // nodeID → 假节点监听地址
}

func newLLMTestEnv(t *testing.T, mods ...func(*api.Server)) *llmEnv {
	t.Helper()
	var (
		svc   *llm.Service
		srv   *api.Server
		addrs sync.Map
	)
	box, err := secretbox.New(dbtest.Key)
	if err != nil {
		t.Fatal(err)
	}
	e := buildTestEnvPool(t, envPlain, func(s *api.Server, pool *pgxpool.Pool) {
		quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
		svc = llm.NewService(llm.Deps{Pool: pool, Clock: s.Clock, Log: quiet, Box: box, Wallet: s.Wallet, Users: s.Identity})
		cfg := llmgw.Config{Transport: func(node llm.RouteNode) http.RoundTripper {
			return &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				addr, _ := addrs.Load(node.ID)
				var d net.Dialer
				return d.DialContext(ctx, "tcp", fmt.Sprint(addr))
			}}
		}}
		s.LLM = svc
		s.LLMGateway = llmgw.New(svc, cfg, s.Clock, quiet).Router()
		for _, m := range mods {
			m(s)
		}
		srv = s
	})
	return &llmEnv{testEnv: e, svc: svc, srv: srv, box: box, imageID: dbtest.ImageID(t, e.pool), nodeAddrs: &addrs}
}

// readyNode 给模型建一个可路由的 ready 节点：机器 + 运行中的系统实例 + llm_nodes 行（节点密钥 = fake.RequireKey），
// 背后是跑 fake 的 httptest.Server；刷新路由表。
func (e *llmEnv) readyNode(id string, modelID int64, fake *llmtest.FakeVLLM) {
	e.t.Helper()
	ctx := context.Background()
	key := "nodekey-" + id + strings.Repeat("x", 32)
	fake.RequireKey = key
	up := httptest.NewServer(fake)
	e.t.Cleanup(up.Close)
	mach := dbtest.CreateMachine(e.t, e.pool, "SN-"+id)
	var sysUser int64
	if err := e.pool.QueryRow(ctx, `SELECT id FROM users WHERE system AND phone = $1`, llm.SystemUserPhone).Scan(&sysUser); err != nil {
		e.t.Fatal(err)
	}
	ins := "ins-" + id
	e.exec(`INSERT INTO instances (id, user_id, machine_id, product_id, image_id, state, ssh_password_enc, system, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'running', 'x', true, $6, $6)`, ins, sysUser, mach, dbtest.ProductID(e.t, e.pool), e.imageID, e.clk.Now())
	e.exec(`INSERT INTO llm_nodes (id, model_id, machine_id, instance_id, state, api_key_enc, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'ready', $5, $6, $6)`, id, modelID, mach, ins, e.box.Seal([]byte(key)), e.clk.Now())
	e.nodeAddrs.Store(id, up.Listener.Addr().String())
	if err := e.svc.Router().Refresh(ctx); err != nil {
		e.t.Fatal(err)
	}
}

// model 直接插一个模型（价格 100 / 400 分每百万 token），返回 ID。
func (e *llmEnv) model(slug, status string, sort int) int64 {
	e.t.Helper()
	var id int64
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO llm_models (slug, name, description, kind, source_id, image_id,
			context_len, input_price, output_price, features, status, sort, created_at, updated_at)
		VALUES ($1, $1 || ' 名称', '描述', 'chat', 'Qwen/Qwen3-4B', $2, 32768, 100, 400, '{tools}', $3, $4, $5, $5) RETURNING id`,
		slug, e.imageID, status, sort, dbtest.T0).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// node 插一台机器和该模型的一个节点行（不建实例）。
func (e *llmEnv) node(id string, modelID int64, state string) {
	e.t.Helper()
	m := dbtest.CreateMachine(e.t, e.pool, "SN-"+id)
	e.exec(`INSERT INTO llm_nodes (id, model_id, machine_id, state, api_key_enc, created_at, updated_at)
		VALUES ($1, $2, $3, $4, '\x00', $5, $5)`, id, modelID, m, state, dbtest.T0)
}

var llmReqSeq int

// usage 经 RecordUsage 记一条 ok 用量（与网关同一路径：unsettled 与 Key 月计数一起更新）。
func (e *llmEnv) usage(userID, keyID, modelID int64, prompt, completion int, stream bool) {
	e.t.Helper()
	llmReqSeq++
	var ttft *int
	if stream {
		v := 120
		ttft = &v
	}
	u := llm.Usage{RequestID: fmt.Sprintf("req-test%012d", llmReqSeq), UserID: userID, KeyID: keyID, ModelID: modelID,
		Endpoint: llm.EndpointChat, Stream: stream, PromptTokens: prompt, CompletionTokens: completion, InputPrice: 100, OutputPrice: 400,
		CostMicro: int64(prompt)*100 + int64(completion)*400, Status: llm.UsageOK, HTTPStatus: 200, LatencyMS: 900, TTFTMS: ttft,
		ClientIP: "203.0.113.9"}
	if err := e.svc.RecordUsage(context.Background(), u); err != nil {
		e.t.Fatal(err)
	}
}

func items(t *testing.T, out map[string]any) []map[string]any {
	t.Helper()
	raw, ok := out["items"].([]any)
	if !ok {
		t.Fatalf("items missing: %v", out)
	}
	res := make([]map[string]any, len(raw))
	for i, x := range raw {
		res[i] = x.(map[string]any)
	}
	return res
}

func TestLLMModelsAnonymous(t *testing.T) {
	e := newLLMTestEnv(t)
	a := e.model("m-a", "online", 2)
	b := e.model("m-b", "online", 1)
	e.model("m-draft", "draft", 0)
	off := e.model("m-off", "offline", 0)
	e.node("node-aaaaaaaaaaaa", a, "ready")
	e.node("node-bbbbbbbbbbbb", b, "provisioning")
	e.node("node-cccccccccccc", off, "ready")

	c := e.client() // 未登录
	code, out := c.do("GET", "/llm/models", nil)
	if code != 200 || out["base_url"] != "http://spark.test/v1" {
		t.Fatalf("models = %d %v", code, out)
	}
	list := items(t, out)
	if len(list) != 2 || list[0]["slug"] != "m-b" || list[1]["slug"] != "m-a" {
		t.Fatalf("items = %v", list)
	}
	if list[0]["available"] != false || list[1]["available"] != true {
		t.Fatalf("available = %v / %v", list[0]["available"], list[1]["available"])
	}
	want := []string{"slug", "name", "description", "kind", "context_len", "max_output_tokens", "input_price", "output_price", "features", "available"}
	for _, k := range want {
		if _, ok := list[1][k]; !ok {
			t.Fatalf("missing %s: %v", k, list[1])
		}
	}
	if len(list[1]) != len(want) {
		t.Fatalf("unexpected fields: %v", list[1])
	}
	if list[1]["input_price"] != float64(100) || list[1]["output_price"] != float64(400) || list[1]["kind"] != "chat" {
		t.Fatalf("item = %v", list[1])
	}
	if f := list[1]["features"].([]any); len(f) != 1 || f[0] != "tools" {
		t.Fatalf("features = %v", f)
	}
	// 其它 /llm/* 需要登录
	for _, p := range []string{"/llm/account", "/llm/keys", "/llm/usage", "/llm/usage/daily", "/llm/settlements"} {
		if code, out := c.do("GET", p, nil); code != 401 {
			t.Fatalf("anonymous %s = %d %v", p, code, out)
		}
	}
}

func TestLLMRoutesAbsentWithoutService(t *testing.T) {
	e := newTestEnv(t)
	c := e.client()
	if code, out := c.do("GET", "/llm/models", nil); code != 404 || out["code"] != "not_found" {
		t.Fatalf("models without LLM = %d %v", code, out)
	}
	// 网关未挂载：/v1/* 也是 JSON 404，不回退控制台页面
	resp, err := http.Post(e.ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode != 404 || body["code"] != "not_found" {
		t.Fatalf("/v1 without gateway = %d %v", resp.StatusCode, body)
	}
}

func TestLLMKeysFlow(t *testing.T) {
	e := newLLMTestEnv(t)
	e.model("m-a", "online", 0)

	// 未实名不能建 Key
	u := e.client()
	u.login("13800000101")
	if code, out := u.do("POST", "/llm/keys", map[string]any{"name": "k"}); code != 403 || out["code"] != "kyc_required" {
		t.Fatalf("unverified create = %d %v", code, out)
	}

	c, uid := e.verifiedClient("13800000102")
	code, out := c.do("POST", "/llm/keys", map[string]any{"name": " 生产 ", "model_slugs": []string{"m-a"}, "monthly_cap": 500,
		"expires_at": "2099-01-31T23:59:59+08:00"})
	if code != 201 {
		t.Fatalf("create = %d %v", code, out)
	}
	plain, _ := out["key"].(string)
	if !strings.HasPrefix(plain, "sk-spark-") {
		t.Fatalf("key = %v", out)
	}
	item := out["item"].(map[string]any)
	id := int64(item["id"].(float64))
	if item["name"] != "生产" || item["status"] != "active" || item["monthly_cap"] != float64(500) || item["month_cost"] != float64(0) ||
		item["key_hint"] == "" || item["last_used_at"] != nil {
		t.Fatalf("item = %v", item)
	}
	if exp, _ := time.Parse(time.RFC3339, item["expires_at"].(string)); !exp.Equal(time.Date(2099, 1, 31, 15, 59, 59, 0, time.UTC)) {
		t.Fatalf("expires_at = %v", item["expires_at"])
	}
	if s := item["model_slugs"].([]any); len(s) != 1 || s[0] != "m-a" {
		t.Fatalf("model_slugs = %v", s)
	}
	for _, k := range []string{"key_hash", "user_id", "month_cost_micro", "key"} {
		if _, ok := item[k]; ok {
			t.Fatalf("item exposes %s: %v", k, item)
		}
	}
	if n := e.countRows(`SELECT count(*) FROM audit_logs WHERE action = 'llm.key.create' AND actor_type = 'user' AND actor_id = $1 AND ip <> ''`, uid); n != 1 {
		t.Fatalf("audit create = %d", n)
	}

	// 列表只有 hint，明文不再出现
	code, out = c.do("GET", "/llm/keys", nil)
	if code != 200 || len(items(t, out)) != 1 || strings.Contains(rawJSON(out), plain) {
		t.Fatalf("list = %d %v", code, out)
	}

	// 更新：名称 + 停用 + 清空月上限（null）
	code, out = c.do("PUT", fmt.Sprintf("/llm/keys/%d", id), map[string]any{"name": "测试", "status": "disabled", "monthly_cap": nil})
	if code != 200 || out["name"] != "测试" || out["status"] != "disabled" || out["monthly_cap"] != nil {
		t.Fatalf("update = %d %v", code, out)
	}
	// 只给 monthly_cap；再只改 model_slugs 为空（全部模型），月上限保留
	if code, out = c.do("PUT", fmt.Sprintf("/llm/keys/%d", id), map[string]any{"monthly_cap": 1000}); code != 200 || out["monthly_cap"] != float64(1000) {
		t.Fatalf("set cap = %d %v", code, out)
	}
	code, out = c.do("PUT", fmt.Sprintf("/llm/keys/%d", id), map[string]any{"model_slugs": []string{}, "status": "active"})
	if code != 200 || out["monthly_cap"] != float64(1000) || len(out["model_slugs"].([]any)) != 0 || out["status"] != "active" || out["name"] != "测试" {
		t.Fatalf("set slugs = %d %v", code, out)
	}
	if code, out = c.do("PUT", fmt.Sprintf("/llm/keys/%d", id), map[string]any{"status": "revoked"}); code != 400 || out["code"] != "llm_key_invalid" {
		t.Fatalf("status revoked via PUT = %d %v", code, out)
	}
	if code, out = c.do("PUT", fmt.Sprintf("/llm/keys/%d", id), map[string]any{"expires_at": "2099-01-01T00:00:00Z"}); code != 400 {
		t.Fatalf("expires_at via PUT = %d %v", code, out)
	}
	if code, out = c.do("POST", "/llm/keys", map[string]any{"name": "x", "expires_at": "明天"}); code != 400 {
		t.Fatalf("bad expires_at = %d %v", code, out)
	}

	// 别人的 Key / 不存在 / 非数字 → 404
	other, _ := e.verifiedClient("13800000103")
	if code, out := other.do("PUT", fmt.Sprintf("/llm/keys/%d", id), map[string]any{"name": "x"}); code != 404 || out["code"] != "llm_key_not_found" {
		t.Fatalf("other update = %d %v", code, out)
	}
	if code, out := other.do("DELETE", fmt.Sprintf("/llm/keys/%d", id), nil); code != 404 || out["code"] != "llm_key_not_found" {
		t.Fatalf("other revoke = %d %v", code, out)
	}
	if code, out := c.do("DELETE", "/llm/keys/abc", nil); code != 404 || out["code"] != "llm_key_not_found" {
		t.Fatalf("bad id = %d %v", code, out)
	}

	// 撤销
	if code, out := c.do("DELETE", fmt.Sprintf("/llm/keys/%d", id), nil); code != 200 || out["ok"] != true {
		t.Fatalf("revoke = %d %v", code, out)
	}
	code, out = c.do("GET", "/llm/keys", nil)
	if list := items(t, out); code != 200 || len(list) != 1 || list[0]["status"] != "revoked" {
		t.Fatalf("list after revoke = %d %v", code, out)
	}
	if code, out := c.do("PUT", fmt.Sprintf("/llm/keys/%d", id), map[string]any{"name": "x"}); code != 409 || out["code"] != "invalid_state" {
		t.Fatalf("update revoked = %d %v", code, out)
	}

	// 上限 20（撤销的不计）
	for i := 0; i < 20; i++ {
		if code, out := c.do("POST", "/llm/keys", map[string]any{"name": fmt.Sprintf("k%d", i)}); code != 201 {
			t.Fatalf("create #%d = %d %v", i, code, out)
		}
	}
	if code, out := c.do("POST", "/llm/keys", map[string]any{"name": "k21"}); code != 409 || out["code"] != "llm_key_limit" {
		t.Fatalf("21st = %d %v", code, out)
	}
}

func TestLLMAccountAndUsage(t *testing.T) {
	e := newLLMTestEnv(t)
	mid := e.model("m-a", "online", 0)
	c, uid := e.fundedClient("13800000111")
	_, out := c.do("POST", "/llm/keys", map[string]any{"name": "k"})
	kid := int64(out["item"].(map[string]any)["id"].(float64))

	// 空账户：全零 + 默认限额
	code, out := c.do("GET", "/llm/account", nil)
	if code != 200 || out["debt"] != float64(0) || out["month_cost"] != float64(0) || out["kyc_verified"] != true || out["base_url"] != "http://spark.test/v1" {
		t.Fatalf("empty account = %d %v", code, out)
	}
	lim := out["limits"].(map[string]any)
	if lim["rpm"] != float64(llm.DefaultRPM) || lim["tpm"] != float64(llm.DefaultTPM) || lim["concurrency"] != float64(llm.DefaultConcurrency) {
		t.Fatalf("limits = %v", lim)
	}

	e.usage(uid, kid, mid, 100_000, 50_000, false) // 3×10^7 微分 = 30 分
	e.usage(uid, kid, mid, 200_000, 0, true)       // 2×10^7 微分 = 20 分

	code, out = c.do("GET", "/llm/account", nil)
	if code != 200 || out["unsettled"] != float64(50) || out["month_cost"] != float64(50) || out["today_cost"] != float64(50) ||
		out["month_tokens"] != float64(350_000) || out["blocked"] != false || out["blocked_reason"] != "" {
		t.Fatalf("account before settle = %d %v", code, out)
	}

	// 结算
	e.clk.Advance(11 * time.Minute)
	if err := e.svc.Settle(context.Background()); err != nil {
		t.Fatal(err)
	}
	code, out = c.do("GET", "/llm/account", nil)
	if code != 200 || out["unsettled"] != float64(0) || out["month_cost"] != float64(50) || out["debt"] != float64(0) {
		t.Fatalf("account after settle = %d %v", code, out)
	}
	code, out = c.do("GET", "/llm/settlements?page=1&page_size=10", nil)
	if code != 200 || out["total"] != float64(1) {
		t.Fatalf("settlements = %d %v", code, out)
	}
	st := items(t, out)[0]
	if st["requests"] != float64(2) || st["usage_amount"] != float64(50) || st["charged"] != float64(50) || st["debt_amount"] != float64(0) ||
		st["prompt_tokens"] != float64(300_000) || st["completion_tokens"] != float64(50_000) || st["wallet_ledger_id"] == nil ||
		st["period_from"] == nil || st["period_to"] == nil || st["created_at"] == nil {
		t.Fatalf("settlement = %v", st)
	}
	if _, ok := st["user_id"]; ok {
		t.Fatalf("settlement exposes user_id: %v", st)
	}
	if code, out := c.do("GET", "/llm/settlements?page=0", nil); code != 400 {
		t.Fatalf("bad page = %d %v", code, out)
	}

	// Key 月消费
	_, out = c.do("GET", "/llm/keys", nil)
	if k := items(t, out)[0]; k["month_cost"] != float64(50) || k["last_used_at"] == nil {
		t.Fatalf("key = %v", k)
	}

	// 明细：默认最近 7 天
	code, out = c.do("GET", "/llm/usage", nil)
	if code != 200 || out["total"] != float64(2) {
		t.Fatalf("usage = %d %v", code, out)
	}
	rows := items(t, out)
	r0 := rows[0] // 新的在前：第二条（流式）
	if r0["model"] != "m-a" || r0["key_name"] != "k" || r0["key_hint"] == "" || r0["endpoint"] != "chat" || r0["stream"] != true ||
		r0["prompt_tokens"] != float64(200_000) || r0["cost_micro"] != float64(20_000_000) || r0["status"] != "ok" ||
		r0["http_status"] != float64(200) || r0["latency_ms"] != float64(900) || r0["ttft_ms"] != float64(120) || r0["estimated"] != false ||
		!strings.HasPrefix(r0["request_id"].(string), "req-") || r0["created_at"] == nil {
		t.Fatalf("usage row = %v", r0)
	}
	if rows[1]["ttft_ms"] != nil {
		t.Fatalf("non-stream ttft = %v", rows[1]["ttft_ms"])
	}
	for _, k := range []string{"user_id", "node_id", "client_ip", "settlement_id", "user_phone", "key_id", "model_id"} {
		if _, ok := r0[k]; ok {
			t.Fatalf("usage row exposes %s: %v", k, r0)
		}
	}
	today := e.clk.Now().In(tz.CST).Format("2006-01-02")
	yesterday := e.clk.Now().In(tz.CST).AddDate(0, 0, -1).Format("2006-01-02")
	cases := []struct {
		q     string
		code  int
		total float64
	}{
		{"?from=" + today + "&to=" + today, 200, 2},
		{"?from=" + yesterday + "&to=" + yesterday, 200, 0},
		{"?to=" + today, 200, 2},
		{"?model=m-a&key_id=" + fmt.Sprint(kid) + "&page=1&page_size=1", 200, 2},
		{"?model=m-x", 200, 0},
		{"?key_id=999999", 200, 0},
		{"?from=2025-10-01&to=2025-12-30", 400, 0}, // 91 天
		{"?from=2025-10-03&to=2025-12-31", 200, 0}, // 恰好 90 天
		{"?from=" + today + "&to=" + yesterday, 400, 0},
		{"?from=2026/01/01", 400, 0},
		{"?key_id=abc", 400, 0},
		{"?page_size=1000", 400, 0},
	}
	for _, tc := range cases {
		code, out := c.do("GET", "/llm/usage"+tc.q, nil)
		if code != tc.code || (code == 200 && out["total"] != tc.total) {
			t.Fatalf("usage%s = %d %v", tc.q, code, out)
		}
	}
	if _, out := c.do("GET", "/llm/usage?page_size=1&page=2", nil); len(items(t, out)) != 1 || out["total"] != float64(2) {
		t.Fatalf("paged = %v", out)
	}

	// 按天汇总
	code, out = c.do("GET", "/llm/usage/daily?days=30", nil)
	if code != 200 {
		t.Fatalf("daily = %d %v", code, out)
	}
	daily := items(t, out)
	if len(daily) != 1 || daily[0]["day"] != today || daily[0]["model"] != "m-a" || daily[0]["requests"] != float64(2) ||
		daily[0]["prompt_tokens"] != float64(300_000) || daily[0]["completion_tokens"] != float64(50_000) || daily[0]["cost_micro"] != float64(50_000_000) {
		t.Fatalf("daily = %v", daily)
	}
	if _, ok := daily[0]["user_id"]; ok {
		t.Fatalf("daily exposes user_id: %v", daily[0])
	}
	if code, _ := c.do("GET", "/llm/usage/daily", nil); code != 200 {
		t.Fatalf("daily default = %d", code)
	}
	for _, q := range []string{"?days=0", "?days=91", "?days=x"} {
		if code, out := c.do("GET", "/llm/usage/daily"+q, nil); code != 400 {
			t.Fatalf("daily%s = %d %v", q, code, out)
		}
	}

	// 别的用户看不到
	o, _ := e.verifiedClient("13800000112")
	if _, out := o.do("GET", "/llm/usage", nil); out["total"] != float64(0) {
		t.Fatalf("other usage = %v", out)
	}
	if _, out := o.do("GET", "/llm/settlements", nil); out["total"] != float64(0) {
		t.Fatalf("other settlements = %v", out)
	}
}

func TestMeIncludesLLMMonthCost(t *testing.T) {
	e := newLLMTestEnv(t)
	mid := e.model("m-a", "online", 0)
	c, uid := e.verifiedClient("13800000121")
	if _, out := c.do("GET", "/me", nil); out["phone"] != "13800000121" {
		t.Fatalf("me = %v", out)
	} else if _, ok := out["llm_month_cost"]; ok {
		t.Fatalf("zero llm_month_cost must be omitted: %v", out)
	}
	_, out := c.do("POST", "/llm/keys", map[string]any{"name": "k"})
	kid := int64(out["item"].(map[string]any)["id"].(float64))
	e.usage(uid, kid, mid, 456_700, 0, false) // 4567 万微分 = 45 分（向下取整）
	code, out := c.do("GET", "/me", nil)
	if code != 200 || out["llm_month_cost"] != float64(45) || out["id"] != float64(uid) || out["kyc_status"] != "verified" {
		t.Fatalf("me = %d %v", code, out)
	}

	// 没有 Token 商城时不带该字段
	e2 := newTestEnv(t)
	c2 := e2.client()
	c2.login("13800000122")
	if _, out := c2.do("GET", "/me", nil); out["phone"] != "13800000122" {
		t.Fatalf("me = %v", out)
	} else if _, ok := out["llm_month_cost"]; ok {
		t.Fatalf("llm_month_cost without LLM: %v", out)
	}
}

// v1Do 直接请求网关（不经 /api/v1），返回状态码、响应头与 JSON。
func v1Do(t *testing.T, e *llmEnv, method, path, contentType, body string, headers map[string]string) (int, http.Header, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, e.ts.URL+path, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, resp.Header, out
}

func openAIErr(t *testing.T, out map[string]any) (typ, code string) {
	t.Helper()
	e, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("not an OpenAI error body: %v", out)
	}
	typ, _ = e["type"].(string)
	code, _ = e["code"].(string)
	return typ, code
}

func TestV1MountedAndExempt(t *testing.T) {
	e := newLLMTestEnv(t)
	e.model("m-a", "online", 0)
	body := `{"model":"m-a","messages":[{"role":"user","content":"hi"}]}`

	// 跨站 Origin + JSON：同站防护不拦，到达网关 → 401 OpenAI 格式
	code, h, out := v1Do(t, e, "POST", "/v1/chat/completions", "application/json", body,
		map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"})
	if typ, c := openAIErr(t, out); code != 401 || typ != "authentication_error" || c == "" {
		t.Fatalf("no key = %d %v", code, out)
	}
	if !strings.HasPrefix(h.Get("X-Request-Id"), "req-") || h.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("headers = %v", h)
	}

	// 有效 Key + text/plain：requireJSONForWrites 不拦，网关自己回 400 OpenAI 格式
	c, _ := e.fundedClient("13800000131")
	_, kout := c.do("POST", "/llm/keys", map[string]any{"name": "k"})
	plain := kout["key"].(string)
	auth := map[string]string{"Authorization": "Bearer " + plain, "Origin": "https://evil.example"}
	code, _, out = v1Do(t, e, "POST", "/v1/chat/completions", "text/plain", body, auth)
	if typ, _ := openAIErr(t, out); code != 400 || typ != "invalid_request_error" {
		t.Fatalf("text/plain = %d %v", code, out)
	}
	if _, ok := out["code"]; ok {
		t.Fatalf("console error format leaked: %v", out)
	}

	// GET /v1/models 带 Key：OpenAI 列表
	code, _, out = v1Do(t, e, "GET", "/v1/models", "", "", auth)
	if code != 200 || out["object"] != "list" {
		t.Fatalf("/v1/models = %d %v", code, out)
	}

	// 未知路径 → OpenAI 格式 404
	code, _, out = v1Do(t, e, "GET", "/v1/unknown", "", "", nil)
	if typ, c := openAIErr(t, out); code != 404 || typ != "invalid_request_error" || c != "not_found" {
		t.Fatalf("/v1/unknown = %d %v", code, out)
	}

	// OPTIONS 预检
	code, h, _ = v1Do(t, e, "OPTIONS", "/v1/chat/completions", "", "", map[string]string{"Origin": "https://evil.example"})
	if code != 200 || h.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("preflight = %d %v", code, h)
	}

	// /api/v1 的写请求仍受同站防护与 JSON 要求约束
	if code, out := c.doH("POST", "/llm/keys", map[string]any{"name": "x"}, map[string]string{"Origin": "https://evil.example"}); code != 403 {
		t.Fatalf("console cross-site = %d %v", code, out)
	}
	if code, out := c.doH("POST", "/llm/keys", map[string]any{"name": "x"}, map[string]string{"Content-Type": "text/plain"}); code != 400 || out["code"] != "bad_request" {
		t.Fatalf("console text/plain = %d %v", code, out)
	}
	// /v1x 之类不属于网关
	if code, _, out := v1Do(t, e, "POST", "/v1x/chat", "text/plain", "x", nil); code != 400 || out["code"] != "bad_request" {
		t.Fatalf("/v1x = %d %v", code, out)
	}
}

func TestLLMModelsRateLimited(t *testing.T) {
	e := newLLMTestEnv(t, func(s *api.Server) { s.CalendarLimit = 3 })
	e.model("m-a", "online", 0)
	c := e.client()
	for i := 0; i < 3; i++ {
		if code, out := c.do("GET", "/llm/models", nil); code != 200 {
			t.Fatalf("#%d = %d %v", i, code, out)
		}
	}
	if code, out := c.do("GET", "/llm/models", nil); code != 429 || out["code"] != "rate_limited" {
		t.Fatalf("4th = %d %v", code, out)
	}
	e.clk.Advance(time.Minute)
	if code, out := c.do("GET", "/llm/models", nil); code != 200 {
		t.Fatalf("after window = %d %v", code, out)
	}
}

// TestV1StreamOutlivesServerTimeouts 流式响应经 api.Server 的全部中间件，跑在 WriteTimeout / ReadTimeout = 1 秒的 http.Server 上，
// 持续 2.5 秒仍完整送达 [DONE]：网关用 ResponseController 延长了本连接的期限，中间件没有包装 ResponseWriter 挡住它。
func TestV1StreamOutlivesServerTimeouts(t *testing.T) {
	e := newLLMTestEnv(t)
	mid := e.model("m-a", "online", 0)
	fake := &llmtest.FakeVLLM{Chunks: []string{"一", "二", "三", "四", "五"}, Delay: 500 * time.Millisecond, PromptTokens: 7, CompletionTokens: 5}
	e.readyNode("node-stream00001", mid, fake)
	c, _ := e.fundedClient("13800000141")
	_, kout := c.do("POST", "/llm/keys", map[string]any{"name": "k"})
	plain := kout["key"].(string)

	ts := httptest.NewUnstartedServer(e.srv.Router())
	ts.Config.WriteTimeout = time.Second
	ts.Config.ReadTimeout = time.Second
	ts.Start()
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest("POST", ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m-a","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+plain)
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read stream after %v: %v (got %q)", time.Since(start), err, b)
	}
	if el := time.Since(start); el < 2*time.Second {
		t.Fatalf("stream finished too fast (%v) to exercise the timeouts", el)
	}
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status = %d %v body %s", resp.StatusCode, resp.Header, b)
	}
	body := string(b)
	if !strings.Contains(body, "data: [DONE]") || !strings.Contains(body, "五") || !strings.Contains(body, `"usage"`) {
		t.Fatalf("stream body = %s", body)
	}
}
