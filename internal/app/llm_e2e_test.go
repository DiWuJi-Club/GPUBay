package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/access/sshtest"
	"spark-cloud/internal/admin"
	"spark-cloud/internal/app"
	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/e2ehooks"
	"spark-cloud/internal/llm"
	"spark-cloud/internal/llmgw/llmtest"
	"spark-cloud/internal/pki"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/config"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/plugins/sms"
	"spark-cloud/internal/rental"
)

// Token 商城的集成测试（规格 §16.2）：真实 app.Build（控制台 API、后台 API、/v1 网关、rental / appdeploy / llm）+ 嵌入式 PG + 假时钟 +
// sshtest shell 模式充当所有实例的容器 sshd + 8000 端口上的 FakeVLLM（按节点密钥分发：每个节点可以有自己的假 vLLM）。
// Agent 用直接回报指令结果的方式模拟（与 appsEnv 相同）；Controller / Prober / Runner / 结算由测试逐轮驱动。
// 每个用例结束时（t.Cleanup）核对 llm.CheckInvariants 七条不变量与应用部署一致性。

const (
	llmSlug        = "qwen3-test"
	llmInputPrice  = 50  // 分 / 百万 token（¥0.5）
	llmOutputPrice = 150 // ¥1.5
)

// fakeNodes 是挂在假容器 8000 端口上的分发器：GET /health 一律 200（appdeploy Runner 的 HTTP 健康检查）；
// 其余请求按 Authorization 里的节点密钥找该节点的 handler（没登记的用 def）；down 的节点一律 503（探测与转发都失败）。
type fakeNodes struct {
	mu    sync.Mutex
	byKey map[string]http.Handler
	down  map[string]bool
	def   http.Handler
}

func (f *fakeNodes) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/health" {
		w.WriteHeader(http.StatusOK)
		return
	}
	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	h, ok := f.byKey[key]
	down := f.down[key]
	if !ok {
		h = f.def
	}
	f.mu.Unlock()
	if down {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"object":"error","message":"Service Unavailable","type":"Service Unavailable","code":503}`))
		return
	}
	h.ServeHTTP(w, r)
}

type llmEnv struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	clk    *clock.Fake
	a      *app.App
	up     *sshtest.Server
	ts     *httptest.Server // 公网入口（控制台 API + /v1）
	adm    *httptest.Server // 后台
	img    int64
	poolID int64
	nodes  *fakeNodes

	machines []int64
	phones   atomic.Int64
}

func newLLMEnv(t *testing.T, machines int) *llmEnv {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.New(t)
	t.Cleanup(func() { assertConsistent(t, pool) })
	clk := clock.NewFake(dbtest.T0)
	keyFile := upstreamKeyFile(t)
	upSigner, err := pki.LoadUpstreamSigner(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	hostPub, hostPEM, err := pki.GenerateInstanceHostKey()
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.ParsePrivateKey(hostPEM)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	up, err := sshtest.New(sshtest.Options{AuthorizedKey: upSigner.PublicKey(), HostKey: hostSigner, Shell: true, Root: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { up.Close(); e2ehooks.StopFakeApps(root) })
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "spark-app-ok|path=%s", r.URL.Path)
	}))
	t.Cleanup(health.Close)
	healthAddr := strings.TrimPrefix(health.URL, "http://")
	cfg := config.Config{UpstreamSSHKeyFile: keyFile, Env: "dev", UploadDir: t.TempDir(), Domain: "spark.test", SecretKey: dbtest.Key,
		SMSDriver: "mock", KYCDriver: "mock", UpstreamSSHPort: up.Port(), LLMGateway: true}
	a, err := app.Build(cfg, pool, discardLog(), app.Options{Clock: clk, SMS: &sms.Mock{}, DisableRateLimits: true,
		HostKeyGen:    func() (string, []byte, error) { return hostPub, hostPEM, nil },
		AppHealthAddr: func(int) string { return healthAddr },
		AppRootFor:    func(id string) string { return filepath.Join(root, id, "root", "data") }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Upstream.Close)
	e := &llmEnv{t: t, ctx: ctx, pool: pool, clk: clk, a: a, up: up, img: dbtest.ImageID(t, pool)}
	e.ts = httptest.NewServer(a.Handler)
	t.Cleanup(e.ts.Close)
	e.adm = httptest.NewServer(a.AdminHandler)
	t.Cleanup(e.adm.Close)
	for i := 0; i < machines; i++ {
		mid := dbtest.CreateMachine(t, pool, fmt.Sprintf("LLM%03d", i+1))
		e.exec(`UPDATE machines SET lan_ip = '127.0.0.1' WHERE id = $1`, mid) // 网关 / Runner / 探测拨 lan_ip:UpstreamSSHPort = sshtest
		e.machines = append(e.machines, mid)
	}
	if _, err := e2ehooks.SeedApps(ctx, a, up, healthAddr); err != nil { // 含平台内部应用 vllm-node
		t.Fatal(err)
	}
	// SeedApps 把 8000（vllm / vllm-node）转到 fake 应用服务；Token 节点改由 FakeVLLM 分发器接管
	e.nodes = &fakeNodes{byKey: map[string]http.Handler{}, down: map[string]bool{}, def: newFake(nil)}
	t.Cleanup(up.ServeHTTP(llm.NodePort, e.nodes))
	if e.poolID, err = a.Fleet.LLMPoolGroupID(ctx, pool); err != nil {
		t.Fatal(err)
	}
	// 最后注册、最先执行：每个用例结束时七条不变量都成立
	t.Cleanup(func() {
		if err := llm.CheckInvariants(context.Background(), pool); err != nil {
			t.Errorf("llm invariants: %v", err)
		}
	})
	return e
}

// newFake 默认的假 vLLM：只服务 qwen3-test，usage 100 / 50，三个内容块。tweak 可改字段（在开始服务之前）。
func newFake(tweak func(*llmtest.FakeVLLM)) *llmtest.FakeVLLM {
	f := &llmtest.FakeVLLM{Served: []string{llmSlug}, PromptTokens: 100, CompletionTokens: 50, Chunks: []string{"你好", "，", "世界"}}
	if tweak != nil {
		tweak(f)
	}
	return f
}

func (e *llmEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatal(err)
	}
}

func (e *llmEnv) scalar(dst any, sql string, args ...any) {
	e.t.Helper()
	if err := e.pool.QueryRow(e.ctx, sql, args...).Scan(dst); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

// setDefault 换掉没登记节点密钥时用的假 vLLM（换实例而不是改字段：FakeVLLM 运行中只读）。
func (e *llmEnv) setDefault(h http.Handler) {
	e.nodes.mu.Lock()
	e.nodes.def = h
	e.nodes.mu.Unlock()
}

// setNode 让节点（按其密钥）用 h；down 为真时该节点的一切请求 503。
func (e *llmEnv) setNode(nodeID string, h http.Handler) {
	e.t.Helper()
	key, err := e.a.LLM.NodeKey(e.ctx, nodeID)
	if err != nil {
		e.t.Fatal(err)
	}
	e.nodes.mu.Lock()
	e.nodes.byKey[key] = h
	e.nodes.mu.Unlock()
}

func (e *llmEnv) setDown(nodeID string, down bool) {
	e.t.Helper()
	key, err := e.a.LLM.NodeKey(e.ctx, nodeID)
	if err != nil {
		e.t.Fatal(err)
	}
	e.nodes.mu.Lock()
	e.nodes.down[key] = down
	e.nodes.mu.Unlock()
}

// agent 模拟全部机器的 Agent：心跳并成功执行全部待处理指令（Create / Wipe / 预拉取）。
func (e *llmEnv) agent() {
	e.t.Helper()
	for _, mid := range e.machines {
		if _, err := e.a.Fleet.Heartbeat(e.ctx, mid); err != nil {
			e.t.Fatal(err)
		}
		cmds, err := e.a.Fleet.PendingCommands(e.ctx, mid)
		if err != nil {
			e.t.Fatal(err)
		}
		for _, cmd := range cmds {
			if err := e.a.Rental.HandleCommandResult(e.ctx, cmd.ID, true, "ok"); err != nil {
				e.t.Fatal(err)
			}
		}
	}
}

// sweep 与 cmd/platform 的清扫器一致：心跳（机器不被判失联）→ Rental.Sweep → Billing.Sweep → LLM.Settle。
func (e *llmEnv) sweep() {
	e.t.Helper()
	for _, mid := range e.machines {
		if _, err := e.a.Fleet.Heartbeat(e.ctx, mid); err != nil {
			e.t.Fatal(err)
		}
	}
	if err := e.a.Rental.Sweep(e.ctx); err != nil {
		e.t.Fatal(err)
	}
	if err := e.a.Billing.Sweep(e.ctx); err != nil {
		e.t.Fatal(err)
	}
	if err := e.a.LLM.Settle(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}

// tick 一轮节点 Controller（不含探测）；单节点错误也算失败。
func (e *llmEnv) tick() llm.TickReport {
	e.t.Helper()
	rep, err := e.a.LLMController.Tick(e.ctx, e.clk.Now())
	if err != nil {
		e.t.Fatalf("controller tick: %v", err)
	}
	return rep
}

func (e *llmEnv) probe() {
	e.t.Helper()
	if err := e.a.LLMController.Prober().Tick(e.ctx, e.clk.Now()); err != nil {
		e.t.Fatalf("prober tick: %v", err)
	}
}

func (e *llmEnv) refresh() {
	e.t.Helper()
	if err := e.a.LLM.Router().Refresh(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}

// appsRound 一轮应用 Runner：推进 PollInterval、真实等一会儿让容器里的脚本推进，再 Tick。
func (e *llmEnv) appsRound(wait time.Duration) {
	e.t.Helper()
	e.clk.Advance(appdeploy.PollInterval)
	time.Sleep(wait)
	if _, locked, err := e.a.AppRunner.Tick(e.ctx); err != nil || !locked {
		e.t.Fatalf("apps tick: locked %v err %v", locked, err)
	}
}

func (e *llmEnv) dep(id string) appdeploy.Deployment {
	e.t.Helper()
	d, err := e.a.Apps.Get(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return d
}

// untilDeployHealthy 逐轮跑 Runner 直到部署 running 且健康检查通过。
func (e *llmEnv) untilDeployHealthy(id string) appdeploy.Deployment {
	e.t.Helper()
	d := e.dep(id)
	for i := 0; i < 120 && !(d.State == appdeploy.StateRunning && d.Health == appdeploy.HealthOK); i++ {
		wait := 50 * time.Millisecond
		if d.State == appdeploy.StateQueued || d.State == appdeploy.StateInstalling {
			wait = 300 * time.Millisecond // 安装脚本要真实时间（fake app.sh 等 1 秒）
		}
		e.appsRound(wait)
		d = e.dep(id)
	}
	if d.State != appdeploy.StateRunning || d.Health != appdeploy.HealthOK {
		e.t.Fatalf("deployment %s: state %s health %s error %q message %q\nlog:\n%s", id, d.State, d.Health, d.ErrorCode, d.Message, d.LogTail)
	}
	return d
}

func (e *llmEnv) node(id string) llm.Node {
	e.t.Helper()
	d, err := e.a.LLM.GetNode(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return d.Node.Node
}

// liveNodes 模型的非终态节点（按 ID 升序）。
func (e *llmEnv) liveNodes(modelID int64) []llm.Node {
	e.t.Helper()
	rows, err := e.a.LLM.ListNodes(e.ctx, llm.NodeFilter{ModelID: modelID})
	if err != nil {
		e.t.Fatal(err)
	}
	var out []llm.Node
	for _, r := range rows {
		if r.State != llm.NodeRemoved && r.State != llm.NodeFailed {
			out = append(out, r.Node)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// provision 走真实 Controller 路径把模型的节点推到 ready（PlaceSystem → Agent Create → appdeploy.Create(vllm-node) →
// Runner 推到 running + 健康 → 探测 → ready），返回全部 ready 节点（按 ID 升序）。
func (e *llmEnv) provision(modelID int64, want int) []llm.Node {
	e.t.Helper()
	for round := 0; round < 10; round++ {
		e.tick()
		e.agent()
		nodes := e.liveNodes(modelID)
		ready := 0
		for _, n := range nodes {
			switch {
			case n.State == llm.NodeReady:
				ready++
			case n.State == llm.NodeProvisioning && n.DeploymentID != nil:
				e.untilDeployHealthy(*n.DeploymentID)
			}
		}
		if ready == want && len(nodes) == want {
			e.refresh()
			return nodes
		}
	}
	e.t.Fatalf("nodes not ready: %+v", e.liveNodes(modelID))
	return nil
}

// createModel 后台服务直接建模型 qwen3-test 并上线（系统操作人）。
func (e *llmEnv) createModel(replicas int) llm.Model {
	e.t.Helper()
	m, err := e.a.LLM.CreateModel(e.ctx, audit.Actor{}, llm.ModelInput{Slug: llmSlug, Name: "Qwen3 测试", Kind: llm.KindChat,
		SourceID: "Qwen/Qwen3-8B", ImageID: e.img, ContextLen: 32768, InputPrice: llmInputPrice, OutputPrice: llmOutputPrice, Replicas: replicas})
	if err != nil {
		e.t.Fatal(err)
	}
	if m, err = e.a.LLM.SetModelStatus(e.ctx, audit.Actor{}, m.ID, llm.ModelOnline); err != nil {
		e.t.Fatal(err)
	}
	return m
}

func (e *llmEnv) assignPool(ids ...int64) {
	e.t.Helper()
	if err := e.a.Fleet.AssignMachines(e.ctx, audit.Actor{}, e.poolID, ids); err != nil {
		e.t.Fatal(err)
	}
}

// seedNode 与 /hooks/llm/node 同一路径：PlaceSystem + Agent 完成 Create + 直接插 ready 节点（无部署）。
func (e *llmEnv) seedNode(modelID, machineID int64) llm.Node {
	e.t.Helper()
	n, err := e2ehooks.SeedLLMNode(e.ctx, e.a, modelID, machineID, func(context.Context, string) error { e.agent(); return nil })
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

// ---- 用户与 /v1 调用 ----

type llmUser struct {
	id int64
	c  *client // 控制台 API（已登录）
}

// newUser 已实名、余额 balance 分的用户（行直接插入，余额经后台调账），并在控制台登录。
func (e *llmEnv) newUser(balance int64) *llmUser {
	e.t.Helper()
	phone := fmt.Sprintf("1370000%04d", e.phones.Add(1))
	uid := dbtest.CreateUser(e.t, e.pool, phone, true)
	if balance > 0 {
		e.adjust(uid, balance)
	}
	jar, _ := cookiejar.New(nil)
	c := &client{t: e.t, base: e.ts.URL + "/api/v1", http: &http.Client{Jar: jar}}
	c.do("POST", "/auth/sms-code", map[string]string{"phone": phone})
	c.do("POST", "/auth/login", map[string]string{"phone": phone, "code": "000000"})
	return &llmUser{id: uid, c: c}
}

var adjustSeq atomic.Int64

func (e *llmEnv) adjust(uid, amount int64) {
	e.t.Helper()
	if _, err := e.a.Wallet.AdminAdjust(e.ctx, 0, uid, amount, fmt.Sprintf("llm e2e %d", adjustSeq.Add(1)), "127.0.0.1"); err != nil {
		e.t.Fatal(err)
	}
}

func (e *llmEnv) balance(uid int64) int64 {
	e.t.Helper()
	b, err := e.a.Wallet.Balance(e.ctx, uid)
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

// createKey 经控制台 API 建 Key，返回明文与 ID。
func (u *llmUser) createKey(body map[string]any) (string, int64) {
	u.c.t.Helper()
	if body == nil {
		body = map[string]any{}
	}
	if body["name"] == nil {
		body["name"] = "e2e"
	}
	out := u.c.do("POST", "/llm/keys", body)
	return out["key"].(string), int64(out["item"].(map[string]any)["id"].(float64))
}

type v1Resp struct {
	status int
	header http.Header
	body   string
}

func (r v1Resp) code() string {
	var b struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(r.body), &b)
	return b.Error.Code
}

// chat POST /v1/chat/completions（extra 覆盖默认请求体字段）。
func (e *llmEnv) chat(key string, extra map[string]any) v1Resp {
	e.t.Helper()
	body := map[string]any{"model": llmSlug, "messages": []map[string]string{{"role": "user", "content": "你好"}}}
	for k, v := range extra {
		body[k] = v
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Error(err) // 可能在 goroutine 里调用：不 Fatal
		return v1Resp{}
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	return v1Resp{status: resp.StatusCode, header: resp.Header, body: string(rb)}
}

func (e *llmEnv) mustChat(key string, extra map[string]any) v1Resp {
	e.t.Helper()
	r := e.chat(key, extra)
	if r.status != http.StatusOK {
		e.t.Fatalf("chat = %d %s", r.status, r.body)
	}
	return r
}

// waitUsage 等用户的 llm_usage 行数达到 n（网关在写完响应之后记账，客户端可能先收到响应）。
func (e *llmEnv) waitUsage(uid int64, n int) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var got int
	for time.Now().Before(deadline) {
		e.scalar(&got, `SELECT count(*) FROM llm_usage WHERE user_id = $1`, uid)
		if got >= n {
			if got > n {
				e.t.Fatalf("usage rows = %d, want %d", got, n)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("usage rows = %d, want %d", got, n)
}

func (e *llmEnv) account(uid int64) llm.Account {
	e.t.Helper()
	var a llm.Account
	if err := e.pool.QueryRow(e.ctx, `SELECT unsettled_micro, carry_micro, debt FROM llm_accounts WHERE user_id = $1`, uid).
		Scan(&a.UnsettledMicro, &a.CarryMicro, &a.Debt); err != nil {
		e.t.Fatal(err)
	}
	return a
}

// ---- 后台 ----

// adminLogin 建一个 super 管理员、后台登录并绑定 TOTP；code() 每次推进 30 秒算一个新码。
func (e *llmEnv) adminLogin() (*client, func() string) {
	e.t.Helper()
	if _, err := e.a.Admins.Create(e.ctx, audit.Actor{}, "root", "correct horse battery", admin.RoleSuper); err != nil {
		e.t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	ac := &client{t: e.t, base: e.adm.URL + "/admin/api/v1", http: &http.Client{Jar: jar}}
	login := ac.do("POST", "/auth/login", map[string]string{"username": "root", "password": "correct horse battery"})
	u, _ := url.Parse(login["otpauth_uri"].(string))
	secret := u.Query().Get("secret")
	code := func() string {
		e.clk.Advance(30 * time.Second)
		c, _ := admin.TOTPCode(secret, admin.TOTPStep(e.clk.Now()))
		return c
	}
	ac.do("POST", "/auth/totp/setup", map[string]string{"code": code()})
	return ac, code
}

// rawCall 发请求，返回状态码与响应体（不因非 2xx 失败）。
func rawCall(c *client, method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// ==== 用例 ====

// 全链路（规格 §16.2 第 1 组）：后台（super）建模型、上线（TOTP）→ 池里加机器 → Controller：PlaceSystem → Agent 完成 Create →
// appdeploy.Create(vllm-node) → Runner 推到 running → 探测 → ready → 用户建 Key → 非流式 / 流式调用经网关 → 用量 →
// 推进 10 分钟 + 清扫 → 一条不扣钱的结算（25,000 微分 < 1 分，全进 carry）→ 再 100 次 → 结算 ≥ ¥1 → 钱包 llm_pay 流水。
func TestLLMFullChain(t *testing.T) {
	e := newLLMEnv(t, 1)
	mid := e.machines[0]
	ac, code := e.adminLogin()

	created := ac.do("POST", "/llm/models", map[string]any{"slug": llmSlug, "name": "Qwen3 测试", "kind": "chat", "source_id": "Qwen/Qwen3-8B",
		"image_id": e.img, "context_len": 32768, "input_price": llmInputPrice, "output_price": llmOutputPrice, "replicas": 1})
	modelID := int64(created["id"].(float64))
	if created["status"] != llm.ModelDraft || created["app_version_id"] != nil {
		t.Fatalf("created model = %v", created)
	}
	if st, out := rawCall(ac, "POST", fmt.Sprintf("/llm/models/%d/status", modelID), map[string]any{"status": "online"}); st == 200 {
		t.Fatalf("online without TOTP = %d %v", st, out)
	}
	if m := ac.do("POST", fmt.Sprintf("/llm/models/%d/status", modelID), map[string]any{"status": "online", "totp": code()}); m["status"] != llm.ModelOnline {
		t.Fatalf("online = %v", m)
	}
	poolView := ac.do("GET", "/llm/pool", nil)
	if int64(poolView["group_id"].(float64)) != e.poolID || poolView["total"].(float64) != 0 {
		t.Fatalf("pool = %v", poolView)
	}
	ac.do("POST", fmt.Sprintf("/groups/%d/machines", e.poolID), map[string]any{"machine_ids": []int64{mid}})
	if pv := ac.do("GET", "/llm/pool", nil); pv["total"].(float64) != 1 || pv["free"].(float64) != 1 {
		t.Fatalf("pool after assign = %v", pv)
	}

	// Controller 第 1 轮：PlaceSystem → 节点 provisioning、系统实例 creating
	if rep := e.tick(); rep.Provisioned != 1 {
		t.Fatalf("tick 1 = %+v", rep)
	}
	nodes := e.liveNodes(modelID)
	if len(nodes) != 1 || nodes[0].State != llm.NodeProvisioning || nodes[0].InstanceID == nil || nodes[0].MachineID != mid {
		t.Fatalf("nodes after tick 1 = %+v", nodes)
	}
	nodeID, instID := nodes[0].ID, *nodes[0].InstanceID
	inst, err := e.a.Rental.Get(e.ctx, instID)
	if err != nil || inst.State != rental.StateCreating || !inst.System || inst.ExpiresAt != nil {
		t.Fatalf("system instance = %+v, %v", inst, err)
	}
	// 这个节点的假 vLLM 只认它的节点密钥：探测与转发都必须带上
	key, err := e.a.LLM.NodeKey(e.ctx, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	fake := newFake(func(f *llmtest.FakeVLLM) { f.RequireKey = key })
	e.setNode(nodeID, fake)

	// fake Agent 完成 Create → 实例 running；第 2 轮：appdeploy.Create(vllm-node)
	e.agent()
	if inst, _ = e.a.Rental.Get(e.ctx, instID); inst.State != rental.StateRunning {
		t.Fatalf("instance after agent = %s", inst.State)
	}
	e.tick()
	n := e.node(nodeID)
	if n.State != llm.NodeProvisioning || n.DeploymentID == nil {
		t.Fatalf("node after tick 2 = %+v", n)
	}
	d := e.untilDeployHealthy(*n.DeploymentID)
	if d.UserID != dbtestSystemUserID(t, e.pool) || d.InstanceID != instID {
		t.Fatalf("deployment = %+v", d)
	}
	// 第 3 轮：部署 running + 健康 → 探测（GET /v1/models 带节点密钥）→ ready
	e.tick()
	if n = e.node(nodeID); n.State != llm.NodeReady || n.LastReadyAt == nil || n.ProbeFailCount != 0 {
		t.Fatalf("node after tick 3 = %+v", n)
	}
	probed := false
	for _, r := range fake.Recorded() {
		if r.Method == http.MethodGet && r.Path == "/v1/models" && r.Authorization == "Bearer "+key {
			probed = true
		}
	}
	if !probed {
		t.Fatalf("no authenticated probe recorded: %+v", fake.Recorded())
	}
	e.refresh()

	// 用户 A：已实名、余额 ¥10
	u := e.newUser(1000)
	plain, _ := u.createKey(nil)
	models := u.c.do("GET", "/llm/models", nil)["items"].([]any)
	if len(models) != 1 || models[0].(map[string]any)["slug"] != llmSlug || models[0].(map[string]any)["available"] != true {
		t.Fatalf("console models = %v", models)
	}

	// 非流式：usage 100 / 50 → cost_micro = 100×50 + 50×150 = 12,500
	r := e.mustChat(plain, nil)
	if !strings.HasPrefix(r.header.Get("X-Request-Id"), "req-") || !strings.Contains(r.body, `"prompt_tokens":100`) ||
		!strings.Contains(r.body, "你好，世界") {
		t.Fatalf("chat = %v %s", r.header, r.body)
	}
	e.waitUsage(u.id, 1)
	var cost int64
	var nodeOf string
	if err := e.pool.QueryRow(e.ctx, `SELECT cost_micro, node_id FROM llm_usage WHERE user_id = $1`, u.id).Scan(&cost, &nodeOf); err != nil ||
		cost != 12_500 || nodeOf != nodeID {
		t.Fatalf("usage = %d %s, %v", cost, nodeOf, err)
	}
	if a := e.account(u.id); a.UnsettledMicro != 12_500 {
		t.Fatalf("account = %+v", a)
	}

	// 流式：网关注入 stream_options.include_usage → SSE 带 usage 块，第二行用量
	r = e.mustChat(plain, map[string]any{"stream": true})
	if !strings.HasPrefix(r.header.Get("Content-Type"), "text/event-stream") || !strings.Contains(r.body, `"usage":{"completion_tokens":50`) ||
		!strings.HasSuffix(r.body, "data: [DONE]\n\n") {
		t.Fatalf("stream = %v\n%s", r.header, r.body)
	}
	last := fake.Recorded()[len(fake.Recorded())-1]
	if !strings.Contains(string(last.Body), `"include_usage":true`) {
		t.Fatalf("upstream stream body = %s", last.Body)
	}
	e.waitUsage(u.id, 2)
	var streamCost int64
	e.scalar(&streamCost, `SELECT cost_micro FROM llm_usage WHERE user_id = $1 AND stream`, u.id)
	if streamCost != 12_500 {
		t.Fatalf("stream usage cost = %d", streamCost)
	}

	// 推进 10 分钟 + 清扫 → 一条结算：25,000 微分不足 1 分 → usage_amount 0、charged 0、无钱包流水、carry 25,000
	e.clk.Advance(llm.SettleMaxAge + time.Second)
	e.sweep()
	var settlements, ledger int
	e.scalar(&settlements, `SELECT count(*) FROM llm_settlements WHERE user_id = $1 AND usage_amount = 0 AND charged = 0
		AND wallet_ledger_id IS NULL AND requests = 2 AND cost_micro = 25000`, u.id)
	e.scalar(&ledger, `SELECT count(*) FROM wallet_ledger WHERE user_id = $1 AND kind = 'llm_pay'`, u.id)
	if a := e.account(u.id); settlements != 1 || ledger != 0 || a.CarryMicro != 25_000 || a.UnsettledMicro != 0 {
		t.Fatalf("first settlement: settlements %d ledger %d account %+v", settlements, ledger, a)
	}
	if e.balance(u.id) != 1000 {
		t.Fatalf("balance = %d", e.balance(u.id))
	}

	// 再 100 次（每次 1000 / 10000 token = 1,550,000 微分）→ 未结算 ≥ ¥1 → 下一轮结算立即触发、扣 155 分
	// （默认 RPM 60 / TPM 20 万挡不住 100 次连调：后台调高该用户的限额）
	ac.do("PUT", fmt.Sprintf("/llm/accounts/%d", u.id), map[string]any{"rpm_limit": 1000, "tpm_limit": 10_000_000})
	e.setNode(nodeID, newFake(func(f *llmtest.FakeVLLM) { f.RequireKey, f.PromptTokens, f.CompletionTokens = key, 1000, 10_000 }))
	for i := 0; i < 100; i++ {
		e.mustChat(plain, nil)
	}
	e.waitUsage(u.id, 102)
	if a := e.account(u.id); a.UnsettledMicro != 155_000_000 {
		t.Fatalf("unsettled after 100 calls = %+v", a)
	}
	e.sweep()
	var usageAmount, charged int64
	e.scalar(&usageAmount, `SELECT usage_amount FROM llm_settlements WHERE user_id = $1 ORDER BY id DESC LIMIT 1`, u.id)
	e.scalar(&charged, `SELECT charged FROM llm_settlements WHERE user_id = $1 ORDER BY id DESC LIMIT 1`, u.id)
	if a := e.account(u.id); usageAmount != 155 || charged != 155 || a.CarryMicro != 25_000 || a.UnsettledMicro != 0 || a.Debt != 0 {
		t.Fatalf("second settlement: usage_amount %d charged %d account %+v", usageAmount, charged, a)
	}
	if e.balance(u.id) != 1000-155 {
		t.Fatalf("balance = %d", e.balance(u.id))
	}
	var pays []map[string]any
	for _, row := range u.c.do("GET", "/wallet/ledger", nil)["items"].([]any) {
		if row := row.(map[string]any); row["kind"] == "llm_pay" {
			pays = append(pays, row)
		}
	}
	if len(pays) != 1 || pays[0]["amount"].(float64) != -155 {
		t.Fatalf("llm_pay ledger = %v", pays)
	}
	if s := u.c.do("GET", "/llm/settlements", nil)["items"].([]any); len(s) != 2 {
		t.Fatalf("console settlements = %v", s)
	}
}

// dbtestSystemUserID 迁移写入的系统账号 ID。
func dbtestSystemUserID(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `SELECT id FROM users WHERE system AND phone = $1`, llm.SystemUserPhone).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// 余额打穿与欠费（规格 §16.2 第 2 组、计划 Review Focus #1）：余额 ¥0.02、50 个并发请求（每个预估 1 分，实际 3 分：假节点返回的
// completion_tokens 远超 max_tokens）→ 部分 402、部分 200；结算后 debt = Σ usage_amount − Σ charged > 0、余额 0、每条用量恰属于一次结算；
// 之后请求 402「欠费」；充值 ¥1 → 下一轮结算补扣欠费 → 请求恢复。
func TestLLMOverdraftSettlesIntoDebt(t *testing.T) {
	e := newLLMEnv(t, 1)
	m := e.createModel(1)
	e.assignPool(e.machines[0])
	e.seedNode(m.ID, e.machines[0])
	// 每次 100 / 20,000 token = 3,005,000 微分；延迟 300ms，让 50 个请求都在前几个完成之前到达预检
	e.setDefault(newFake(func(f *llmtest.FakeVLLM) { f.CompletionTokens, f.Delay = 20_000, 300*time.Millisecond }))
	u := e.newUser(2)
	plain, _ := u.createKey(nil)
	// 默认并发 8 会让大部分请求 429：调高并发，只让预算预检起作用
	if _, err := e.a.LLM.AdminUpdateAccount(e.ctx, audit.Actor{}, u.id, llm.AccountUpdate{ConcurrencyLimit: llm.OptInt{Set: true, Value: ptr(100)}}); err != nil {
		t.Fatal(err)
	}

	const n = 50
	results := make([]v1Resp, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Go(func() {
			<-start
			results[i] = e.chat(plain, map[string]any{"max_tokens": 1})
		})
	}
	close(start)
	wg.Wait()
	ok, paid := 0, 0
	for _, r := range results {
		switch {
		case r.status == http.StatusOK:
			ok++
		case r.status == http.StatusPaymentRequired && r.code() == "insufficient_quota":
			paid++
		default:
			t.Fatalf("unexpected response %d %s", r.status, r.body)
		}
	}
	if ok == 0 || paid == 0 {
		t.Fatalf("200 × %d, 402 × %d: want both", ok, paid)
	}
	e.waitUsage(u.id, ok)

	// 结算：欠费 = 应扣 − 实扣，余额扣到 0
	e.clk.Advance(llm.SettleMaxAge + time.Second)
	e.sweep()
	var sumUsage, sumCharged int64
	e.scalar(&sumUsage, `SELECT COALESCE(sum(usage_amount), 0) FROM llm_settlements WHERE user_id = $1`, u.id)
	e.scalar(&sumCharged, `SELECT COALESCE(sum(charged), 0) FROM llm_settlements WHERE user_id = $1`, u.id)
	a := e.account(u.id)
	t.Logf("50 concurrent: 200 × %d, 402 × %d; Σusage_amount %d, Σcharged %d, debt %d, carry %d", ok, paid, sumUsage, sumCharged, a.Debt, a.CarryMicro)
	if bal := e.balance(u.id); a.Debt <= 0 || a.Debt != sumUsage-sumCharged || bal != 0 || sumCharged != 2 {
		t.Fatalf("after settle: debt %d, Σusage %d, Σcharged %d, balance %d", a.Debt, sumUsage, sumCharged, bal)
	}
	if want := int64(ok) * 3_005_000; sumUsage != want/llm.MicroPerFen || a.CarryMicro != want%llm.MicroPerFen {
		t.Fatalf("Σusage_amount %d carry %d for %d requests", sumUsage, a.CarryMicro, ok)
	}
	var unsettled, orphan int
	e.scalar(&unsettled, `SELECT count(*) FROM llm_usage WHERE user_id = $1 AND settlement_id IS NULL`, u.id)
	e.scalar(&orphan, `SELECT count(*) FROM llm_usage g LEFT JOIN llm_settlements s ON s.id = g.settlement_id
		WHERE g.user_id = $1 AND (s.id IS NULL OR s.user_id <> g.user_id)`, u.id)
	if unsettled != 0 || orphan != 0 {
		t.Fatalf("usage rows: %d unsettled, %d without a matching settlement", unsettled, orphan)
	}
	if err := llm.CheckInvariants(e.ctx, e.pool); err != nil {
		t.Fatal(err)
	}

	// 欠费期间：402 且文案提示欠费（结算后鉴权缓存已失效）
	if r := e.chat(plain, nil); r.status != http.StatusPaymentRequired || r.code() != "insufficient_quota" || !strings.Contains(r.body, "欠费") {
		t.Fatalf("in debt = %d %s", r.status, r.body)
	}

	// 充值 ¥1 → 下一轮结算只扣欠费 → 欠费清零，请求恢复
	debt := a.Debt
	e.adjust(u.id, 100)
	e.sweep()
	if a := e.account(u.id); a.Debt != 0 || e.balance(u.id) != 100-debt {
		t.Fatalf("after recharge: account %+v balance %d (debt was %d)", a, e.balance(u.id), debt)
	}
	var debtOnly int
	e.scalar(&debtOnly, `SELECT count(*) FROM llm_settlements WHERE user_id = $1 AND requests = 0 AND debt_amount = $2 AND charged = $2`, u.id, debt)
	if debtOnly != 1 {
		t.Fatalf("debt-only settlement rows = %d", debtOnly)
	}
	// 网关的余额缓存是 2 秒（真实时间）：缓存里仍可能是打穿前的余额，两种情况都应放行
	e.setDefault(newFake(nil))
	deadline := time.Now().Add(3 * time.Second)
	r := e.chat(plain, nil)
	for r.status != http.StatusOK && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		r = e.chat(plain, nil)
	}
	if r.status != http.StatusOK {
		t.Fatalf("after repay = %d %s", r.status, r.body)
	}
	e.waitUsage(u.id, ok+1)
}

func ptr[T any](v T) *T { return &v }

// 限流（规格 §16.2 第 3 组）：RPM 2 → 第 3 次 429 且带 Retry-After；并发 1 + 假节点延迟 → 第二个并发 429；
// Key 月上限 1 分 → 用超之后（鉴权缓存过期）429 monthly_cap_exceeded，其它 Key 不受影响。
func TestLLMRateLimits(t *testing.T) {
	e := newLLMEnv(t, 1)
	m := e.createModel(1)
	e.assignPool(e.machines[0])
	e.seedNode(m.ID, e.machines[0])
	u := e.newUser(1000)
	plain, _ := u.createKey(nil)
	update := func(in llm.AccountUpdate) {
		t.Helper()
		if _, err := e.a.LLM.AdminUpdateAccount(e.ctx, audit.Actor{}, u.id, in); err != nil {
			t.Fatal(err)
		}
	}

	update(llm.AccountUpdate{RPMLimit: llm.OptInt{Set: true, Value: ptr(2)}})
	e.mustChat(plain, nil)
	e.mustChat(plain, nil)
	r := e.chat(plain, nil)
	ra, _ := strconv.Atoi(r.header.Get("Retry-After"))
	if r.status != http.StatusTooManyRequests || r.code() != "rate_limit_exceeded" || ra < 1 || ra > 60 {
		t.Fatalf("3rd request at RPM 2 = %d Retry-After %q %s", r.status, r.header.Get("Retry-After"), r.body)
	}

	// 并发 1：第一个请求挂在假节点上时，第二个 429
	e.clk.Advance(61 * time.Second)
	update(llm.AccountUpdate{RPMLimit: llm.OptInt{Set: true}, ConcurrencyLimit: llm.OptInt{Set: true, Value: ptr(1)}})
	slow := newFake(func(f *llmtest.FakeVLLM) { f.Delay = 500 * time.Millisecond })
	e.setDefault(slow)
	done := make(chan v1Resp, 1)
	go func() { done <- e.chat(plain, nil) }()
	deadline := time.Now().Add(5 * time.Second)
	for slow.Count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if r := e.chat(plain, nil); r.status != http.StatusTooManyRequests || r.code() != "rate_limit_exceeded" || !strings.Contains(r.body, "并发") {
		t.Fatalf("concurrent request at concurrency 1 = %d %s", r.status, r.body)
	}
	if r := <-done; r.status != http.StatusOK {
		t.Fatalf("first request = %d %s", r.status, r.body)
	}
	e.mustChat(plain, nil) // 在途释放之后又能调

	// Key 月上限 1 分：一次 1,505,000 微分的调用就用超；鉴权缓存（30 秒）过期后该 Key 429，另一个 Key 照常
	update(llm.AccountUpdate{ConcurrencyLimit: llm.OptInt{Set: true}})
	capped, _ := u.createKey(map[string]any{"name": "capped", "monthly_cap": 1})
	e.setDefault(newFake(func(f *llmtest.FakeVLLM) { f.CompletionTokens = 10_000 }))
	e.mustChat(capped, nil)
	e.waitUsage(u.id, 5)
	e.clk.Advance(llm.KeyCacheTTL + time.Second)
	if r := e.chat(capped, nil); r.status != http.StatusTooManyRequests || r.code() != "monthly_cap_exceeded" {
		t.Fatalf("over monthly cap = %d %s", r.status, r.body)
	}
	e.mustChat(plain, nil)
	e.waitUsage(u.id, 6)
}

// 故障转移与不健康（规格 §16.2 第 4 组）：两个节点（真实 Controller 路径，带部署）；A 对首个请求回 503 → 请求由 B 完成、
// A probe_fail_count 1；A 连续探测失败 → unhealthy、告警一条、路由只剩 B；A 恢复 → ready；再次不健康且 10 分钟不恢复 →
// Controller 自动 Redeploy（rebuild_count 1）→ 重新部署后回到 ready。
func TestLLMFailoverAndUnhealthy(t *testing.T) {
	e := newLLMEnv(t, 2)
	m := e.createModel(2)
	e.assignPool(e.machines...)
	nodes := e.provision(m.ID, 2)
	nodeA, nodeB := nodes[0].ID, nodes[1].ID // 路由在途相同时取 ID 小的：首个请求先到 A
	fakeA := newFake(func(f *llmtest.FakeVLLM) { f.FailFirstN = 1 })
	fakeB := newFake(nil)
	e.setNode(nodeA, fakeA)
	e.setNode(nodeB, fakeB)
	u := e.newUser(1000)
	plain, _ := u.createKey(nil)
	posts := func(f *llmtest.FakeVLLM) int {
		n := 0
		for _, r := range f.Recorded() {
			if r.Method == http.MethodPost {
				n++
			}
		}
		return n
	}

	e.mustChat(plain, nil)
	if posts(fakeA) != 1 || posts(fakeB) != 1 {
		t.Fatalf("failover: A got %d, B got %d POSTs", posts(fakeA), posts(fakeB))
	}
	e.waitUsage(u.id, 1)
	var servedBy string
	e.scalar(&servedBy, `SELECT node_id FROM llm_usage WHERE user_id = $1`, u.id)
	if a := e.node(nodeA); servedBy != nodeB || a.ProbeFailCount != 1 || a.State != llm.NodeReady {
		t.Fatalf("after failover: served by %s, A = %+v", servedBy, a)
	}

	// A 挂了：连续 3 次探测失败 → unhealthy + 一条告警；路由只剩 B
	e.setDown(nodeA, true)
	for i := 0; i < llm.ProbeFailThreshold; i++ {
		e.probe()
	}
	var alerts int
	e.scalar(&alerts, `SELECT count(*) FROM alerts WHERE kind = $1`, llm.AlertNodeUnhealthy)
	if a, b := e.node(nodeA), e.node(nodeB); a.State != llm.NodeUnhealthy || a.UnhealthySince == nil || b.State != llm.NodeReady || alerts != 1 {
		t.Fatalf("after probes: A = %+v, B = %s, alerts %d", a, b.State, alerts)
	}
	e.refresh()
	beforeA := posts(fakeA)
	for i := 0; i < 3; i++ {
		e.mustChat(plain, nil)
	}
	if posts(fakeA) != beforeA || posts(fakeB) != 4 {
		t.Fatalf("while A unhealthy: A got %d more, B total %d", posts(fakeA)-beforeA, posts(fakeB))
	}

	// A 恢复：一次成功探测 → ready
	e.setDown(nodeA, false)
	e.probe()
	if a := e.node(nodeA); a.State != llm.NodeReady || a.ProbeFailCount != 0 || a.UnhealthySince != nil {
		t.Fatalf("after recovery: A = %+v", a)
	}

	// 再次不健康、10 分钟不恢复 → Controller 自动重建（Redeploy），rebuild_count 1
	e.setDown(nodeA, true)
	for i := 0; i < llm.ProbeFailThreshold; i++ {
		e.probe()
	}
	if a := e.node(nodeA); a.State != llm.NodeUnhealthy {
		t.Fatalf("A = %+v", a)
	}
	e.clk.Advance(llm.UnhealthyRebuildAfter + time.Second)
	e.tick()
	a := e.node(nodeA)
	if a.State != llm.NodeProvisioning || a.RebuildCount != 1 || a.DeploymentID == nil {
		t.Fatalf("after rebuild tick: A = %+v", a)
	}
	if d := e.dep(*a.DeploymentID); d.State != appdeploy.StateQueued && d.State != appdeploy.StateInstalling {
		t.Fatalf("redeployed deployment = %s", d.State)
	}
	e.setDown(nodeA, false)
	nodes = e.provision(m.ID, 2)
	if nodes[0].ID != nodeA || e.node(nodeA).RebuildCount != 1 {
		t.Fatalf("after rebuild: nodes %+v", nodes)
	}
}

// gate 让请求在到达假节点后挂住，直到 release。
type gate struct {
	next    http.Handler
	arrived atomic.Int32
	ch      chan struct{}
}

func (g *gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		g.arrived.Add(1)
		<-g.ch
	}
	g.next.ServeHTTP(w, r)
}

// 排空（规格 §16.2 第 5 组）：模型下线（在线模型的 replicas 不能改成 0，下线即期望 0 个节点）→ 节点 draining，在途 1 个请求挂着时不释放 →
// 请求结束 → ReleaseSystem → Agent 洗机 → removed、机器 idle；机器仍在池里：普通用户下单拿不到它（no_capacity）；移出池后立即能租。
func TestLLMDrainReleasesMachine(t *testing.T) {
	e := newLLMEnv(t, 1)
	mid := e.machines[0]
	m := e.createModel(1)
	e.assignPool(mid)
	n := e.seedNode(m.ID, mid)
	g := &gate{next: newFake(nil), ch: make(chan struct{})}
	e.setDefault(g)
	u := e.newUser(1_000_000)
	plain, _ := u.createKey(nil)

	done := make(chan v1Resp, 1)
	go func() { done <- e.chat(plain, nil) }()
	deadline := time.Now().Add(5 * time.Second)
	for g.arrived.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := e.a.LLM.Router().Inflight(n.ID); got != 1 {
		t.Fatalf("inflight = %d", got)
	}

	if _, err := e.a.LLM.SetModelStatus(e.ctx, audit.Actor{}, m.ID, llm.ModelOffline); err != nil {
		t.Fatal(err)
	}
	if rep := e.tick(); rep.Drained != 1 {
		t.Fatalf("drain tick = %+v", rep)
	}
	e.clk.Advance(2*llm.RouterRefresh + time.Second)
	e.tick()
	if x := e.node(n.ID); x.State != llm.NodeDraining {
		t.Fatalf("node with a request in flight = %+v", x)
	}

	close(g.ch)
	if r := <-done; r.status != http.StatusOK {
		t.Fatalf("in-flight request = %d %s", r.status, r.body)
	}
	e.waitUsage(u.id, 1)
	e.tick() // 在途归零 → ReleaseSystem → removing
	if x := e.node(n.ID); x.State != llm.NodeRemoving {
		t.Fatalf("node after in-flight done = %+v", x)
	}
	inst, _ := e.a.Rental.Get(e.ctx, *n.InstanceID)
	if inst.State != rental.StateWiping {
		t.Fatalf("instance = %s", inst.State)
	}
	e.agent() // 洗机完成
	if rep := e.tick(); rep.Removed != 1 {
		t.Fatalf("remove tick = %+v", rep)
	}
	mach, err := e.a.Fleet.Get(e.ctx, e.pool, mid)
	if err != nil || mach.State != "idle" || mach.GroupID == nil || *mach.GroupID != e.poolID {
		t.Fatalf("machine = %+v, %v", mach, err)
	}

	// 仍在池里：不可租
	order := map[string]any{"product_id": dbtest.ProductID(t, e.pool), "duration": "1d", "image_id": e.img}
	if st, out := rawCall(u.c, "POST", "/orders", order); st != http.StatusConflict || out["code"] != apperr.ErrNoCapacity.Code {
		t.Fatalf("order while machine in pool = %d %v", st, out)
	}
	// 移出池：立即能租到这台机器
	if err := e.a.Fleet.UnassignMachines(e.ctx, audit.Actor{}, e.poolID, []int64{mid}); err != nil {
		t.Fatal(err)
	}
	inst2 := u.c.do("POST", "/orders", order)["instance"].(map[string]any)
	got, err := e.a.Rental.Get(e.ctx, inst2["id"].(string))
	if err != nil || got.MachineID != mid || got.System {
		t.Fatalf("ordered instance = %+v, %v", got, err)
	}
}

// 移出池被拒（规格 §16.2 第 6 组）：机器上有节点时后台 DELETE /groups/{pool}/machines → 409 llm_pool_machine_busy，
// 改到别的机组同样被拒；后台移除节点（TOTP）→ 洗机 → removed 之后可以移出。
func TestLLMUnassignBusyMachineRejected(t *testing.T) {
	e := newLLMEnv(t, 1)
	mid := e.machines[0]
	m := e.createModel(1)
	e.assignPool(mid)
	n := e.seedNode(m.ID, mid)
	ac, code := e.adminLogin()

	body := map[string]any{"machine_ids": []int64{mid}}
	if st, out := rawCall(ac, "DELETE", fmt.Sprintf("/groups/%d/machines", e.poolID), body); st != http.StatusConflict ||
		out["code"] != apperr.ErrLLMPoolMachineBusy.Code {
		t.Fatalf("unassign busy machine = %d %v", st, out)
	}
	other := ac.do("POST", "/groups", map[string]any{"name": "普通机组", "visible": true})
	if st, out := rawCall(ac, "POST", fmt.Sprintf("/groups/%d/machines", int64(other["id"].(float64))), body); st != http.StatusConflict ||
		out["code"] != apperr.ErrLLMPoolMachineBusy.Code {
		t.Fatalf("move busy machine = %d %v", st, out)
	}
	if mach, _ := e.a.Fleet.Get(e.ctx, e.pool, mid); mach.GroupID == nil || *mach.GroupID != e.poolID {
		t.Fatalf("machine group = %v", mach.GroupID)
	}

	// 后台移除节点 → removing（立即 ReleaseSystem）→ 洗机 → removed → 可以移出
	if st, out := rawCall(ac, "POST", "/llm/nodes/"+n.ID+"/remove", map[string]any{"reason": "e2e 移出池"}); st == 200 {
		t.Fatalf("remove without TOTP = %d %v", st, out)
	}
	ac.do("POST", "/llm/nodes/"+n.ID+"/remove", map[string]any{"reason": "e2e 移出池", "totp": code()})
	if st, _ := rawCall(ac, "DELETE", fmt.Sprintf("/groups/%d/machines", e.poolID), body); st != http.StatusConflict {
		t.Fatalf("unassign while wiping = %d", st) // 系统实例洗机中仍占机器
	}
	e.agent()
	e.tick()
	if x := e.node(n.ID); x.State != llm.NodeRemoved {
		t.Fatalf("node = %+v", x)
	}
	ac.do("DELETE", fmt.Sprintf("/groups/%d/machines", e.poolID), body)
	if mach, _ := e.a.Fleet.Get(e.ctx, e.pool, mid); mach.GroupID != nil {
		t.Fatalf("machine still in group %d", *mach.GroupID)
	}
}

// 封禁与撤销（规格 §16.2 第 7 组）：封禁用户 → 鉴权缓存（30 秒）过期后 403 account_banned；解封后恢复；
// 撤销 Key → 立即 401 invalid_api_key（本进程内缓存即时失效）。
func TestLLMBanAndRevoke(t *testing.T) {
	e := newLLMEnv(t, 1)
	m := e.createModel(1)
	e.assignPool(e.machines[0])
	e.seedNode(m.ID, e.machines[0])
	u := e.newUser(1000)
	plain, keyID := u.createKey(nil)
	e.mustChat(plain, nil)

	if err := e.a.Identity.Ban(e.ctx, audit.Actor{}, u.id, "e2e 封禁"); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(llm.KeyCacheTTL + time.Second)
	if r := e.chat(plain, nil); r.status != http.StatusForbidden || r.code() != "account_banned" {
		t.Fatalf("banned = %d %s", r.status, r.body)
	}
	if err := e.a.Identity.Unban(e.ctx, audit.Actor{}, u.id); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(llm.KeyCacheTTL + time.Second)
	e.mustChat(plain, nil)

	if err := e.a.LLM.RevokeKey(e.ctx, u.id, keyID, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if r := e.chat(plain, nil); r.status != http.StatusUnauthorized || r.code() != "invalid_api_key" {
		t.Fatalf("revoked = %d %s", r.status, r.body)
	}
	e.waitUsage(u.id, 2)
}
