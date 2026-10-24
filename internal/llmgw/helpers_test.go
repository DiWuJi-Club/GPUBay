package llmgw

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
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/fleet"
	"spark-cloud/internal/llm"
	"spark-cloud/internal/llmgw/llmtest"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/secretbox"
	"spark-cloud/internal/wallet"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

// allowAll 满足 llm.UserLookup：建 Key 时总是放行（门槛由测试直接改 users 行，经网关的 Authorize 检查）。
type allowAll struct{}

func (allowAll) CanRent(context.Context, int64) error { return nil }

// genv 是网关测试环境：嵌入式 PG、静止的假时钟（dbtest.T0）、真实 llm.Service（真实钱包 / fleet），
// 网关挂在 httptest.Server 的 /v1（与 api.Server 的挂载方式一致），每个节点一个 httptest.Server 跑 FakeVLLM。
type genv struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool
	clk  *clock.Fake
	box  *secretbox.Box
	svc  *llm.Service
	gw   *Server
	srv  *httptest.Server

	imageID, productID, systemUserID int64

	mu    sync.Mutex
	nodes map[string]string        // nodeID → 假节点监听地址（dialTimeoutAddr = 拨号超时）
	conns map[string]*atomic.Int32 // nodeID → 假节点收到的新 TCP 连接数
}

// dialTimeoutAddr 作为节点地址时，测试 Transport 的 DialContext 返回一个超时的 dialFailure（模拟 SSH 拨号超时）。
const dialTimeoutAddr = "dial-timeout"

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

var seq atomic.Int64

func newGEnv(t *testing.T, mod ...func(*Config)) *genv {
	t.Helper()
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	box, err := secretbox.New(dbtest.Key)
	if err != nil {
		t.Fatal(err)
	}
	e := &genv{t: t, ctx: context.Background(), pool: pool, clk: clk, box: box, nodes: map[string]string{}, conns: map[string]*atomic.Int32{}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e.svc = llm.NewService(llm.Deps{Pool: pool, Clock: clk, Log: log, Box: box, Wallet: wallet.NewService(pool, clk),
		Users: allowAll{}, Fleet: fleet.NewService(pool, clk)})
	e.imageID = dbtest.ImageID(t, pool)
	e.productID = dbtest.ProductID(t, pool)
	e.systemUserID = e.queryInt(`SELECT id FROM users WHERE system AND phone = $1`, llm.SystemUserPhone)
	cfg := Config{Transport: func(node llm.RouteNode) http.RoundTripper {
		return &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			e.mu.Lock()
			addr := e.nodes[node.ID]
			e.mu.Unlock()
			if addr == dialTimeoutAddr {
				return nil, &dialFailure{err: timeoutErr{}}
			}
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		}}
	}}
	for _, m := range mod {
		m(&cfg)
	}
	e.gw = New(e.svc, cfg, clk, log)
	r := chi.NewRouter()
	r.Mount("/v1", e.gw.Router())
	e.srv = httptest.NewServer(r)
	t.Cleanup(e.srv.Close)
	return e
}

func (e *genv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

func (e *genv) queryInt(sql string, args ...any) int64 {
	e.t.Helper()
	var n int64
	if err := e.pool.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// model 直接插一个 online 模型（价格：输入 100、输出 400 分 / 百万；default_max_tokens 4096、max_output_tokens 8192）。
func (e *genv) model(slug, kind string, mods ...func(m map[string]any)) llm.Model {
	e.t.Helper()
	f := map[string]any{"max_concurrency": 32, "status": llm.ModelOnline, "input_price": 100, "output_price": 400}
	for _, m := range mods {
		m(f)
	}
	id := e.queryInt(`INSERT INTO llm_models (slug, name, kind, source_id, image_id, context_len, max_output_tokens, default_max_tokens,
			input_price, output_price, max_concurrency, status, created_at, updated_at)
		VALUES ($1, $1, $2, 'Qwen/Qwen3-4B', $3, 32768, 8192, 4096, $4, $5, $6, $7, $8, $8) RETURNING id`,
		slug, kind, e.imageID, f["input_price"], f["output_price"], f["max_concurrency"], f["status"], e.clk.Now())
	m, err := e.svc.GetModel(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	e.gw.flushCaches()
	return m
}

// node 给模型建一个 ready 节点（机器 + 运行中的系统实例 + llm_nodes 行，节点密钥随机），节点背后是一个跑 fake 的 httptest.Server；
// fake.RequireKey 设为节点密钥。id 为空时自动编号。刷新路由表。返回节点 ID 与假节点服务。
func (e *genv) node(m llm.Model, fake *llmtest.FakeVLLM, id string) (string, *httptest.Server) {
	e.t.Helper()
	n := seq.Add(1)
	if id == "" {
		id = fmt.Sprintf("node-%012d", n)
	}
	key := fmt.Sprintf("nodekey-%d-%s", n, strings.Repeat("x", 32))
	fake.RequireKey = key
	if len(fake.Served) == 0 {
		fake.Served = []string{m.Slug}
	}
	conns := &atomic.Int32{}
	srv := httptest.NewUnstartedServer(fake)
	srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	e.t.Cleanup(srv.Close)
	mach := dbtest.CreateMachine(e.t, e.pool, fmt.Sprintf("SN-GW-%d", n))
	ins := fmt.Sprintf("ins-gw%06d", n)
	now := e.clk.Now()
	e.exec(`INSERT INTO instances (id, user_id, machine_id, product_id, image_id, state, ssh_password_enc, system, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'running', 'x', true, $6, $6)`, ins, e.systemUserID, mach, e.productID, e.imageID, now)
	e.exec(`INSERT INTO llm_nodes (id, model_id, machine_id, instance_id, state, api_key_enc, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'ready', $5, $6, $6)`, id, m.ID, mach, ins, e.box.Seal([]byte(key)), now)
	e.mu.Lock()
	e.nodes[id] = srv.Listener.Addr().String()
	e.conns[id] = conns
	e.mu.Unlock()
	e.refresh()
	return id, srv
}

// setNodeAddr 改节点的拨号地址（如 dialTimeoutAddr）。
func (e *genv) setNodeAddr(id, addr string) {
	e.mu.Lock()
	e.nodes[id] = addr
	e.mu.Unlock()
}

// nodeConns 假节点收到的新 TCP 连接数。
func (e *genv) nodeConns(id string) int32 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.conns[id].Load()
}

func (e *genv) refresh() {
	e.t.Helper()
	if err := e.svc.Router().Refresh(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}

// user 建一个已实名用户并充值 balance 分。
func (e *genv) user(balance int64) int64 {
	e.t.Helper()
	id := dbtest.CreateUser(e.t, e.pool, fmt.Sprintf("137%08d", seq.Add(1)), true)
	if balance > 0 {
		dbtest.Fund(e.t, e.pool, id, balance)
	}
	return id
}

// key 给用户建一把 Key，返回明文与 Key。
func (e *genv) key(userID int64, mods ...func(*llm.KeyInput)) (string, llm.Key) {
	e.t.Helper()
	in := llm.KeyInput{Name: fmt.Sprintf("k%d", seq.Add(1))}
	for _, m := range mods {
		m(&in)
	}
	plain, k, err := e.svc.CreateKey(e.ctx, userID, in, "127.0.0.1")
	if err != nil {
		e.t.Fatalf("CreateKey: %v", err)
	}
	return plain, k
}

// userKey 有余额的用户 + 一把 Key。
func (e *genv) userKey(mods ...func(*llm.KeyInput)) (int64, string) {
	e.t.Helper()
	u := e.user(100_000)
	k, _ := e.key(u, mods...)
	return u, k
}

// request 发一个请求；ctype 为空时用 application/json；key 为空不带 Authorization。
func (e *genv) request(ctx context.Context, method, path, key, ctype, body string) *http.Response {
	e.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, e.srv.URL+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != "" {
		if ctype == "" {
			ctype = "application/json"
		}
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// post 发 JSON POST 并读完响应体。
func (e *genv) post(path, key, body string) (*http.Response, []byte) {
	e.t.Helper()
	resp := e.request(e.ctx, http.MethodPost, path, key, "", body)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp, b
}

func (e *genv) get(path, key string) (*http.Response, []byte) {
	e.t.Helper()
	resp := e.request(e.ctx, http.MethodGet, path, key, "", "")
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func chatBody(model string, extra ...string) string {
	s := `{"model":"` + model + `","messages":[{"role":"user","content":"你好"}]`
	for _, x := range extra {
		s += "," + x
	}
	return s + "}"
}

// openaiErr 解析 OpenAI 错误体。
func openaiErr(t *testing.T, b []byte) errorDetail {
	t.Helper()
	var eb errorBody
	if err := json.Unmarshal(b, &eb); err != nil || eb.Error.Code == "" {
		t.Fatalf("not an OpenAI error body: %s", b)
	}
	return eb.Error
}

// expectErr 断言状态码与 error.code，返回错误详情。
func expectErr(t *testing.T, resp *http.Response, b []byte, status int, code string) errorDetail {
	t.Helper()
	if resp.StatusCode != status {
		t.Fatalf("status = %d, want %d; body %s", resp.StatusCode, status, b)
	}
	d := openaiErr(t, b)
	if d.Code != code {
		t.Fatalf("code = %q, want %q; body %s", d.Code, code, b)
	}
	if want := codeType[code]; want != "" && d.Type != want {
		t.Fatalf("type = %q, want %q", d.Type, want)
	}
	return d
}

// usageRow 等到 request_id 的用量行出现（记账在响应写出之后）。
func (e *genv) usageRow(reqID string) llm.Usage {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var u llm.Usage
		err := e.pool.QueryRow(e.ctx, `SELECT id, request_id, user_id, key_id, model_id, node_id, endpoint, stream, prompt_tokens,
				completion_tokens, estimated, input_price, output_price, cost_micro, status, http_status, latency_ms, ttft_ms, client_ip, created_at
			FROM llm_usage WHERE request_id = $1`, reqID).Scan(&u.ID, &u.RequestID, &u.UserID, &u.KeyID, &u.ModelID, &u.NodeID, &u.Endpoint,
			&u.Stream, &u.PromptTokens, &u.CompletionTokens, &u.Estimated, &u.InputPrice, &u.OutputPrice, &u.CostMicro, &u.Status,
			&u.HTTPStatus, &u.LatencyMS, &u.TTFTMS, &u.ClientIP, &u.CreatedAt)
		if err == nil {
			return u
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("usage row %s not found: %v", reqID, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (e *genv) usageCount() int64 { return e.queryInt(`SELECT count(*) FROM llm_usage`) }

// waitFor 轮询直到 cond 为真（最多 5 秒）。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func nodeIDOf(u llm.Usage) string {
	if u.NodeID == nil {
		return ""
	}
	return *u.NodeID
}
