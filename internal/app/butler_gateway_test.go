package app_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/app"
	"spark-cloud/internal/butler"
	"spark-cloud/internal/llm"
	"spark-cloud/internal/platform/config"
	"spark-cloud/internal/platform/dbtest"
)

// AI 管家默认端点 = 本进程的 Token 商城网关（计划 9 的 /v1，进程内调用）：真实 app.Build（SPARK_LLM_PROVIDER=openai、
// SPARK_LLM_BASE_URL 空、SPARK_LLM_GATEWAY on）+ 嵌入式 PG + 假容器上的模型节点。一轮对话经网关鉴权、选节点、转发、
// 逐块流式回传，用量按「平台管家账号」的 API Key 记账（llm_usage），与外部调用 /v1 完全相同。

// gatedNode 是假的 vLLM 节点：流式回复先发「你好」，等测试放行后再发「，世界」、finish、usage 与 [DONE]。
type gatedNode struct {
	release chan struct{}
	mu      sync.Mutex
	bodies  []map[string]any
	auth    []string
}

func (g *gatedNode) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/health" {
		w.WriteHeader(http.StatusOK)
		return
	}
	b, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(b, &body)
	g.mu.Lock()
	g.bodies = append(g.bodies, body)
	g.auth = append(g.auth, r.Header.Get("Authorization"))
	g.mu.Unlock()
	if body["stream"] != true { // 非流式（SPARK_LLM_STREAM=off，openai 的默认）：放行后一次回完整段 chat.completion
		select {
		case <-g.release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"`+llmSlug+
			`","choices":[{"index":0,"message":{"role":"assistant","content":"你好，世界"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":1200,"completion_tokens":30,"total_tokens":1230}}`)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	send := func(data string) {
		_, _ = io.WriteString(w, "data: "+data+"\n\n")
		w.(http.Flusher).Flush()
	}
	chunk := func(delta, finish string) string {
		fr := "null"
		if finish != "" {
			fr = `"` + finish + `"`
		}
		return `{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"` + llmSlug +
			`","choices":[{"index":0,"delta":` + delta + `,"finish_reason":` + fr + `}]}`
	}
	send(chunk(`{"role":"assistant","content":""}`, ""))
	send(chunk(`{"content":"你好"}`, ""))
	select {
	case <-g.release:
	case <-r.Context().Done():
		return
	}
	send(chunk(`{"content":"，世界"}`, ""))
	send(chunk(`{}`, "stop"))
	send(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"` + llmSlug +
		`","choices":[],"usage":{"prompt_tokens":1200,"completion_tokens":30,"total_tokens":1230}}`)
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	w.(http.Flusher).Flush()
}

// chanSink 把管家的 SSE 事件转进通道（Send 在调用方 goroutine 里同步跑完一轮）。
type chanSink struct{ ch chan sinkEvent }

type sinkEvent struct {
	name string
	v    any
}

func (s chanSink) Emit(name string, v any) { s.ch <- sinkEvent{name, v} }

// butlerGW 是 AI 管家走进程内网关的测试环境：平台管家账号（有 Key、有余额）、上线的模型与一个假节点。
type butlerGW struct {
	*llmEnv
	butlerUID, keyID int64
	model            llm.Model
	node             *gatedNode
}

func newButlerGW(t *testing.T, stream bool) *butlerGW {
	t.Helper()
	var butlerUID, keyID int64
	e := newLLMEnvWith(t, 1, func(pool *pgxpool.Pool, cfg *config.Config, _ *app.Options) {
		// 运维事先准备的「平台管家账号」：已实名、有 Token 商城账户与一把 API Key（这里直接写库，等价于控制台创建）
		butlerUID = dbtest.CreateUser(t, pool, "13900009999", true)
		plain, hash, hint := llm.GenerateKey()
		if _, err := pool.Exec(context.Background(), `INSERT INTO llm_accounts (user_id, created_at, updated_at) VALUES ($1, now(), now())`, butlerUID); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(context.Background(), `INSERT INTO llm_api_keys (user_id, name, key_hash, key_hint, created_at, updated_at)
			VALUES ($1, 'butler', $2, $3, now(), now()) RETURNING id`, butlerUID, hash, hint).Scan(&keyID); err != nil {
			t.Fatal(err)
		}
		cfg.Butler = true
		cfg.ButlerLLM = config.ButlerLLMConfig{Provider: config.LLMOpenAI, APIKey: plain, Model: llmSlug, MaxOutputTokens: 512, Tools: true, Stream: stream}
	})
	if e.a.Butler == nil || e.a.ButlerLLM == nil || e.a.ButlerLLM.Name() != config.LLMOpenAI || e.a.ButlerLLM.Model() != llmSlug {
		t.Fatalf("butler = %v provider = %v", e.a.Butler, e.a.ButlerLLM)
	}
	e.adjust(butlerUID, 10_000)
	m := e.createModel(1)
	e.assignPool(e.machines[0])
	node := &gatedNode{release: make(chan struct{})}
	e.setDefault(node)
	e.seedNode(m.ID, e.machines[0])
	e.refresh()
	return &butlerGW{llmEnv: e, butlerUID: butlerUID, keyID: keyID, model: m, node: node}
}

func TestButlerUsesInProcessGateway(t *testing.T) {
	g := newButlerGW(t, true) // SPARK_LLM_STREAM=on：逐块流式经网关回传
	e, node, m, butlerUID, keyID := g.llmEnv, g.node, g.model, g.butlerUID, g.keyID
	endUser := dbtest.CreateUser(t, e.pool, "13900008888", true)

	sink := chanSink{ch: make(chan sinkEvent, 64)}
	ctx, cancel := context.WithTimeout(e.ctx, 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := e.a.Butler.Send(ctx, butler.SendRequest{UserID: endUser, IP: "127.0.0.1", ConversationID: "new", Text: "你好"}, sink)
		done <- err
	}()
	var events []sinkEvent
	next := func(what string) sinkEvent {
		t.Helper()
		select {
		case ev := <-sink.ch:
			events = append(events, ev)
			return ev
		case err := <-done:
			t.Fatalf("turn ended (%v) while waiting for %s; events %+v", err, what, events)
		case <-time.After(15 * time.Second):
			t.Fatalf("timeout waiting for %s; events %+v", what, events)
		}
		return sinkEvent{}
	}
	// 节点只发了第一块、还卡着：「你好」已经经网关与进程内传输流到管家的 SSE（流式，而不是整段缓冲）
	for {
		ev := next("first delta")
		if d, ok := ev.v.(butler.DeltaEvent); ok && ev.name == butler.EvDelta {
			if !strings.Contains(d.Text, "你好") {
				t.Fatalf("first delta = %q", d.Text)
			}
			break
		}
		if ev.name == butler.EvError || ev.name == butler.EvDone {
			t.Fatalf("unexpected %s before the first delta: %+v", ev.name, ev.v)
		}
	}
	select {
	case err := <-done:
		t.Fatalf("turn finished before the node was released: %v", err)
	default:
	}
	close(node.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	close(sink.ch)
	for ev := range sink.ch {
		events = append(events, ev)
	}
	var text strings.Builder
	var usage *butler.UsageEvent
	var reason string
	for _, ev := range events {
		switch v := ev.v.(type) {
		case butler.DeltaEvent:
			text.WriteString(v.Text)
		case butler.UsageEvent:
			usage = &v
		case butler.DoneEvent:
			reason = v.Reason
		case butler.ErrorEvent:
			t.Fatalf("error event %+v", v)
		}
	}
	if text.String() != "你好，世界" || usage == nil || usage.InputTokens != 1200 || usage.OutputTokens != 30 || reason == "" {
		t.Fatalf("text %q usage %+v done %q; events %+v", text.String(), usage, reason, events)
	}

	// 节点收到的是管家的原始请求（模型 slug、工具、流式），鉴权用的是节点密钥而不是管家的 API Key
	node.mu.Lock()
	if len(node.bodies) != 1 || node.bodies[0]["model"] != llmSlug || node.bodies[0]["stream"] != true || node.bodies[0]["tools"] == nil ||
		node.bodies[0]["max_tokens"] != float64(512) || strings.HasPrefix(strings.TrimPrefix(node.auth[0], "Bearer "), llm.KeyPrefix) {
		t.Fatalf("node got %d requests: %v auth %v", len(node.bodies), node.bodies, node.auth)
	}
	node.mu.Unlock()

	// 用量按管家账号的 Key 记账（与外部 /v1 调用同一张表、同一价格），终端用户不付 Token 费
	var n, prompt, completion int
	var uid, kid, cost int64
	var status, ip string
	var stream bool
	// 管家读到 [DONE] 就关闭响应体（与真实 HTTP 一样不等网关收尾），网关随后记账：轮询等这一行
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if e.scalar(&n, `SELECT count(*) FROM llm_usage`); n > 0 || time.Now().After(deadline) {
			break
		}
	}
	if err := e.pool.QueryRow(e.ctx, `SELECT user_id, key_id, prompt_tokens, completion_tokens, cost_micro, status, stream, client_ip
		FROM llm_usage`).Scan(&uid, &kid, &prompt, &completion, &cost, &status, &stream, &ip); err != nil {
		t.Fatal(err)
	}
	wantCost := llm.Cost(m, 1200, 30)
	if n != 1 || uid != butlerUID || kid != keyID || prompt != 1200 || completion != 30 || cost != wantCost || cost <= 0 ||
		status != llm.UsageOK || !stream || ip != "127.0.0.1" {
		t.Fatalf("llm_usage: n=%d user %d key %d tokens %d/%d cost %d (want %d) status %s stream %v ip %s",
			n, uid, kid, prompt, completion, cost, wantCost, status, stream, ip)
	}
	// 管家自己的每日用量照常累计到终端用户名下（每日额度按用户算）
	var bIn, bOut int64
	if err := e.pool.QueryRow(e.ctx, `SELECT input_tokens, output_tokens FROM butler_usage WHERE user_id = $1`, endUser).Scan(&bIn, &bOut); err != nil {
		t.Fatal(err)
	}
	if bIn != 1200 || bOut != 30 {
		t.Fatalf("butler_usage = %d / %d", bIn, bOut)
	}
}

// 回归（真机验收发现）：经控制台 HTTP 接口 POST /api/v1/butler/conversations/new/messages 发一轮。轮次在控制台 chi 处理器里跑，
// 外层的 chi.RouteContext 曾经随请求 ctx 漏进进程内网关的 chi 路由 → 网关按外层路径匹配、每轮都 404（直接调 Butler.Send 测不出来）；
// 控制台处理器返回后 chi 回收这个 RouteContext，而网关还在收尾记账 → 数据竞争（-race 下报错）。
func TestButlerHTTPTurnThroughInProcessGateway(t *testing.T) {
	g := newButlerGW(t, false) // openai 默认 SPARK_LLM_STREAM=off：非流式经网关，usage 取自整段响应
	close(g.node.release)      // 节点一口气回完
	u := g.newUser(0)
	for i, text := range []string{"你好", "再说一次"} {
		path := "/butler/conversations/new/messages"
		if i > 0 {
			// 第二轮在同一对话里：路径参数 {id} 是真实的对话 ID
			var conv string
			g.scalar(&conv, `SELECT id FROM butler_conversations WHERE user_id = $1`, u.id)
			path = "/butler/conversations/" + conv + "/messages"
		}
		code, raw := u.c.sse(path, map[string]any{"text": text})
		if code != http.StatusOK {
			t.Fatalf("turn %d = %d %s", i, code, raw)
		}
		evs, err := parseSSE(raw)
		if err != nil {
			t.Fatal(err)
		}
		var deltas strings.Builder
		var done bool
		for _, ev := range evs {
			switch ev.name {
			case butler.EvDelta:
				deltas.WriteString(ev.data["text"].(string))
			case butler.EvError:
				t.Fatalf("turn %d: error event %v", i, ev.data)
			case butler.EvDone:
				done = true
			}
		}
		if deltas.String() != "你好，世界" || !done {
			t.Fatalf("turn %d: text %q done %v; raw %s", i, deltas.String(), done, raw)
		}
		g.waitUsage(g.butlerUID, i+1) // 网关照常鉴权、转发、记账（管家账号的 Key）
	}
	var ok int
	g.scalar(&ok, `SELECT count(*) FROM llm_usage WHERE user_id = $1 AND key_id = $2 AND status = $3`, g.butlerUID, g.keyID, llm.UsageOK)
	if ok != 2 {
		t.Fatalf("ok usage rows = %d", ok)
	}
	var streamed int
	g.scalar(&streamed, `SELECT count(*) FROM llm_usage WHERE user_id = $1 AND stream`, g.butlerUID)
	g.node.mu.Lock()
	if streamed != 0 || len(g.node.bodies) != 2 || g.node.bodies[0]["stream"] != false || g.node.bodies[0]["stream_options"] != nil {
		t.Fatalf("non-stream: streamed rows %d, node bodies %v", streamed, g.node.bodies)
	}
	g.node.mu.Unlock()
	var bOut int64
	g.scalar(&bOut, `SELECT output_tokens FROM butler_usage WHERE user_id = $1`, u.id)
	if bOut != 60 {
		t.Fatalf("butler_usage output = %d", bOut)
	}
}
