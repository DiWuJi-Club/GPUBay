package api_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/api"
	"spark-cloud/internal/butler"
	"spark-cloud/internal/butler/butlertest"
	"spark-cloud/internal/butler/llm"
	"spark-cloud/internal/butler/llm/mock"
	"spark-cloud/internal/butler/tools"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/secretbox"
)

// butlerEnv 是带 AI 管家（mock 模型 + 真实工具注册表，工具接真实的租用 / 钱包 / 目录 / 身份服务）的测试环境。
type butlerEnv struct {
	*testEnv
	svc  *butler.Service
	mock *mock.Mock
}

func newButlerEnv(t *testing.T, tweak func(*api.Server)) *butlerEnv {
	t.Helper()
	m := mock.New()
	var svc *butler.Service
	e := buildTestEnvPool(t, envPlain, func(srv *api.Server, pool *pgxpool.Pool) {
		box, err := secretbox.New(dbtest.Key)
		if err != nil {
			t.Fatal(err)
		}
		td := tools.Deps{Rental: srv.Rental, Catalog: srv.Catalog, Wallet: srv.Wallet, Users: srv.Identity, Pool: pool,
			PortURL: srv.Ingress.PortURL, SSHHost: srv.SSHHost, SSHPort: srv.SSHPort, Clock: srv.Clock, IngressDomain: srv.Ingress.IngressDomain, Box: box}
		if srv.Apps != nil {
			td.Apps = srv.Apps
		}
		svc = butler.NewService(butler.Deps{Pool: pool, Clock: srv.Clock, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Provider: m,
			Tools: tools.NewRegistry(td), ToolsEnabled: true, Users: srv.Identity, Wallet: srv.Wallet, Rental: srv.Rental, Fleet: srv.Fleet, Box: box})
		srv.Butler = svc
		if tweak != nil {
			tweak(srv)
		}
	})
	t.Cleanup(func() { butlertest.CheckInvariants(t, e.pool) })
	return &butlerEnv{testEnv: e, svc: svc, mock: m}
}

type sseEvent struct {
	name string
	data map[string]any
}

type sseResult struct {
	status   int
	ctype    string
	header   http.Header
	raw      string
	events   []sseEvent
	pings    int
	errJSON  map[string]any // 非 SSE 响应的 JSON 体
	comments []string
}

// parseSSE 按规格 §5.4 解析整段 SSE：帧之间空行分隔；注释帧（": …"）单独计数；事件帧必须恰为 event + data 两行且 data 是 JSON 对象。
func parseSSE(t *testing.T, raw string) (events []sseEvent, comments []string) {
	t.Helper()
	if raw != "" && !strings.HasSuffix(raw, "\n\n") {
		t.Fatalf("stream does not end on a frame boundary: %q", raw)
	}
	for _, frame := range strings.Split(strings.TrimSuffix(raw, "\n\n"), "\n\n") {
		if frame == "" {
			continue
		}
		lines := strings.Split(frame, "\n")
		if strings.HasPrefix(lines[0], ":") {
			if len(lines) != 1 {
				t.Fatalf("comment frame with extra lines: %q", frame)
			}
			comments = append(comments, strings.TrimSpace(strings.TrimPrefix(lines[0], ":")))
			continue
		}
		if len(lines) != 2 || !strings.HasPrefix(lines[0], "event: ") || !strings.HasPrefix(lines[1], "data: ") {
			t.Fatalf("malformed frame: %q", frame)
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &data); err != nil {
			t.Fatalf("frame data is not a JSON object: %q (%v)", frame, err)
		}
		events = append(events, sseEvent{name: strings.TrimPrefix(lines[0], "event: "), data: data})
	}
	return events, comments
}

// stream 发一个 SSE POST 并读完整个响应。
func (c *client) stream(path string, body any) sseResult {
	c.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", c.base+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	res := sseResult{status: resp.StatusCode, ctype: resp.Header.Get("Content-Type"), header: resp.Header, raw: string(raw)}
	if strings.HasPrefix(res.ctype, "text/event-stream") {
		res.events, res.comments = parseSSE(c.t, res.raw)
		for _, cm := range res.comments {
			if cm == "ping" {
				res.pings++
			}
		}
	} else if err := json.Unmarshal(raw, &res.errJSON); err != nil {
		c.t.Fatalf("non-SSE response is not JSON: %d %q", resp.StatusCode, raw)
	}
	return res
}

func (r sseResult) names() []string {
	out := make([]string, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.name)
	}
	return out
}

func (r sseResult) first(name string) map[string]any {
	for _, e := range r.events {
		if e.name == name {
			return e.data
		}
	}
	return nil
}

func TestButlerStatus(t *testing.T) {
	e := newButlerEnv(t, nil)
	c := e.client()
	if code, out := c.do("GET", "/butler/status", nil); code != 401 || out["code"] != "unauthorized" {
		t.Fatalf("anonymous status = %d %v", code, out)
	}
	c.login("13800001201")
	code, out := c.do("GET", "/butler/status", nil)
	if code != 200 || out["enabled"] != true || out["notice"] != "" || out["day_used"].(float64) != 0 || out["day_limit"].(float64) != 300000 ||
		out["provider_label"] != "平台模型" || out["ingress_domain"] != "spark.test" {
		t.Fatalf("status = %d %v", code, out)
	}
	// 发一条消息后今日用量反映 mock 的默认用量（1000 + 100）
	if res := c.stream("/butler/conversations/new/messages", map[string]any{"text": "你好"}); res.status != 200 {
		t.Fatalf("send = %d %s", res.status, res.raw)
	}
	if _, out := c.do("GET", "/butler/status", nil); out["day_used"].(float64) != 1100 {
		t.Fatalf("status after send = %v", out)
	}
	// 后台关闭：仍 200，enabled=false + notice
	e.exec(`UPDATE butler_settings SET enabled = false, notice = '系统维护中'`)
	e.svc.InvalidateSettings()
	if code, out := c.do("GET", "/butler/status", nil); code != 200 || out["enabled"] != false || out["notice"] != "系统维护中" {
		t.Fatalf("disabled status = %d %v", code, out)
	}
}

func TestButlerSendSSE(t *testing.T) {
	e := newButlerEnv(t, nil)
	c := e.client()
	c.login("13800001202")
	res := c.stream("/butler/conversations/new/messages", map[string]any{"text": "你好，介绍一下你自己"})
	if res.status != 200 || res.ctype != "text/event-stream; charset=utf-8" || res.header.Get("Cache-Control") != "no-store" ||
		res.header.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("send = %d %q %v", res.status, res.ctype, res.header)
	}
	names := strings.Join(res.names(), ",")
	if len(res.events) == 0 || res.events[0].name != "start" || !strings.Contains(names, "delta") || !strings.Contains(names, "usage") ||
		res.events[len(res.events)-1].name != "done" {
		t.Fatalf("events = %s\n%s", names, res.raw)
	}
	start := res.first("start")
	convID, _ := start["conversation_id"].(string)
	if !strings.HasPrefix(convID, "cv-") || start["message_id"].(float64) <= 0 {
		t.Fatalf("start = %v", start)
	}
	var text strings.Builder
	for _, ev := range res.events {
		if ev.name == "delta" {
			text.WriteString(ev.data["text"].(string))
		}
	}
	if text.String() != "我可以帮你选机器、部署应用、排查问题、查看余额。" {
		t.Fatalf("delta text = %q", text.String())
	}
	if d := res.first("done"); d["reason"] != "end_turn" {
		t.Fatalf("done = %v", d)
	}
	if u := res.first("usage"); u["input_tokens"].(float64) != 1000 || u["output_tokens"].(float64) != 100 || u["day_limit"].(float64) != 300000 {
		t.Fatalf("usage = %v", u)
	}

	code, out := c.do("GET", "/butler/conversations?limit=50", nil)
	items, _ := out["items"].([]any)
	if code != 200 || len(items) != 1 {
		t.Fatalf("list = %d %v", code, out)
	}
	it := items[0].(map[string]any)
	if it["id"] != convID || it["title"] != "你好，介绍一下你自己" || it["status"] != "idle" || it["created_at"] == nil {
		t.Fatalf("list item = %v", it)
	}
	if code, out := c.do("GET", "/butler/conversations?limit=x", nil); code != 400 || out["code"] != "bad_request" {
		t.Fatalf("bad limit = %d %v", code, out)
	}

	// 详情：user + assistant 两条，pending_tool_call 为 null
	code, out = c.do("GET", "/butler/conversations/"+convID, nil)
	msgs, _ := out["messages"].([]any)
	if code != 200 || out["conversation"].(map[string]any)["id"] != convID || len(msgs) != 2 || out["pending_tool_call"] != nil {
		t.Fatalf("detail = %d %v", code, out)
	}
	if v, ok := out["pending_tool_call"]; !ok || v != nil {
		t.Fatalf("pending_tool_call must be present and null: %v", out)
	}

	// 续聊同一对话
	res = c.stream("/butler/conversations/"+convID+"/messages", map[string]any{"text": "再说一次"})
	if res.status != 200 || res.first("start")["conversation_id"] != convID || res.first("done") == nil {
		t.Fatalf("follow-up = %d %s", res.status, res.raw)
	}
	if _, out := c.do("GET", "/butler/conversations", nil); len(out["items"].([]any)) != 1 {
		t.Fatalf("follow-up must not create a conversation: %v", out)
	}
}

func TestButlerSendValidationJSON(t *testing.T) {
	e := newButlerEnv(t, nil)
	c := e.client()
	c.login("13800001203")
	isJSONErr := func(res sseResult, status int, code string) {
		t.Helper()
		if res.status != status || !strings.HasPrefix(res.ctype, "application/json") || res.errJSON["code"] != code {
			t.Fatalf("want %d %s JSON, got %d %q %s", status, code, res.status, res.ctype, res.raw)
		}
	}
	isJSONErr(c.stream("/butler/conversations/new/messages", map[string]any{"text": "   "}), 400, "butler_message_invalid")
	isJSONErr(c.stream("/butler/conversations/new/messages", map[string]any{"text": strings.Repeat("字", 2001)}), 400, "butler_message_invalid")
	isJSONErr(c.stream("/butler/conversations/new/messages", map[string]any{"text": "hi", "extra": 1}), 400, "bad_request")
	isJSONErr(c.stream("/butler/conversations/new/messages", map[string]any{"text": "hi", "context": map[string]any{"instance_id": "ins-nope"}}), 404, "not_found")
	isJSONErr(c.stream("/butler/conversations/new/messages", map[string]any{"text": "hi", "context": map[string]any{"instance_id": "../x"}}), 404, "not_found")
	isJSONErr(c.stream("/butler/conversations/cv-doesnotexist/messages", map[string]any{"text": "hi"}), 404, "not_found")

	// Content-Type 不是 JSON → 400（requireJSONForWrites，兼作 CSRF 防护）
	req, _ := http.NewRequest("POST", c.base+"/butler/conversations/new/messages", strings.NewReader(`{"text":"hi"}`))
	req.Header.Set("Content-Type", "text/plain")
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("text/plain = %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	// 额度用完 → 429 JSON
	uid := userID(t, e.testEnv, "13800001203")
	if err := e.svc.AddUsage(context.Background(), e.pool, uid, e.svc.DayOf(e.clk.Now()), butler.Usage{Requests: 1, InputTokens: 300000}); err != nil {
		t.Fatal(err)
	}
	isJSONErr(c.stream("/butler/conversations/new/messages", map[string]any{"text": "hi"}), 429, "butler_quota_exceeded")

	// 后台关闭 → 503 JSON，消息带 notice
	e.exec(`UPDATE butler_settings SET enabled = false, notice = '升级中'`)
	e.svc.InvalidateSettings()
	res := c.stream("/butler/conversations/new/messages", map[string]any{"text": "hi"})
	isJSONErr(res, 503, "butler_disabled")
	if res.errJSON["message"] != "AI 管家暂未开放：升级中" {
		t.Fatalf("disabled message = %v", res.errJSON)
	}
	if e.mock.Calls() != 0 {
		t.Fatalf("model must not be called on validation errors, calls = %d", e.mock.Calls())
	}
	if n := e.countRows(`SELECT count(*) FROM butler_conversations`); n != 0 {
		t.Fatalf("validation errors must not create conversations, got %d", n)
	}
}

// userID 查手机号对应的用户 ID。
func userID(t *testing.T, e *testEnv, phone string) int64 {
	t.Helper()
	var id int64
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM users WHERE phone = $1`, phone).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// proposeRenew 让 mock 模型对实例 id 提议续费 1 天，返回确认卡（SSE confirm 事件的 tool_call）与对话 ID。
func proposeRenew(t *testing.T, e *butlerEnv, c *client, instanceID string) (card map[string]any, convID string) {
	t.Helper()
	e.mock.Enqueue(mock.Reply{Text: "好的，我来帮你续费。", ToolCalls: []llm.ToolCall{{Name: "renew_instance", Args: json.RawMessage(`{"instance_id":"` + instanceID + `","duration":"1d"}`)}}})
	res := c.stream("/butler/conversations/new/messages", map[string]any{"text": "帮我续费一天", "context": map[string]any{"instance_id": instanceID}})
	if res.status != 200 {
		t.Fatalf("send = %d %s", res.status, res.raw)
	}
	conf := res.first("confirm")
	if conf == nil || res.first("done")["reason"] != "awaiting_confirm" {
		t.Fatalf("events = %v\n%s", res.names(), res.raw)
	}
	return conf["tool_call"].(map[string]any), res.first("start")["conversation_id"].(string)
}

func TestButlerConfirmFlow(t *testing.T) {
	e := newButlerEnv(t, nil)
	c := e.client()
	id := c.runningInstance(t, e.testEnv, "13800001204")
	card, convID := proposeRenew(t, e, c, id)
	tcID, _ := card["id"].(string)
	if !strings.HasPrefix(tcID, "tc-") || card["tool"] != "renew_instance" || card["label"] == "" || card["summary"] == "" ||
		card["quote_amount"].(float64) != 6900 || card["danger"] != false || card["expires_in"].(float64) != 600 || card["expires_at"] == nil {
		t.Fatalf("confirm card = %v", card)
	}
	if args := card["args"].(map[string]any); args["instance_id"] != id || args["duration"] != "1d" {
		t.Fatalf("card args = %v", args)
	}
	// 详情里是同一张卡
	_, out := c.do("GET", "/butler/conversations/"+convID, nil)
	if p, _ := out["pending_tool_call"].(map[string]any); p == nil || p["id"] != tcID || out["conversation"].(map[string]any)["status"] != "awaiting_confirm" {
		t.Fatalf("detail while pending = %v", out)
	}
	// approve 字段缺失 → 400；不存在的调用 → 409 butler_confirm_invalid（JSON）
	if res := c.stream("/butler/conversations/"+convID+"/tool-calls/"+tcID+"/confirm", map[string]any{}); res.status != 400 || res.errJSON["code"] != "bad_request" {
		t.Fatalf("missing approve = %d %s", res.status, res.raw)
	}
	if res := c.stream("/butler/conversations/"+convID+"/tool-calls/tc-nope/confirm", map[string]any{"approve": true}); res.status != 409 || res.errJSON["code"] != "butler_confirm_invalid" {
		t.Fatalf("unknown tc = %d %s", res.status, res.raw)
	}

	res := c.stream("/butler/conversations/"+convID+"/tool-calls/"+tcID+"/confirm", map[string]any{"approve": true})
	if res.status != 200 || res.ctype != "text/event-stream; charset=utf-8" {
		t.Fatalf("confirm = %d %s", res.status, res.raw)
	}
	var tool map[string]any
	for _, ev := range res.events {
		if ev.name == "tool" {
			tool = ev.data
			break
		}
	}
	if tool == nil || tool["id"] != tcID || tool["name"] != "renew_instance" || tool["status"] != "ok" || res.first("done")["reason"] != "end_turn" {
		t.Fatalf("confirm events = %v\n%s", res.names(), res.raw)
	}
	_, out = c.do("GET", "/butler/conversations/"+convID, nil)
	if out["pending_tool_call"] != nil || out["conversation"].(map[string]any)["status"] != "idle" {
		t.Fatalf("detail after confirm = %v", out)
	}
	if !strings.Contains(rawJSON(out), `"status":"ok"`) {
		t.Fatalf("executed write call must show as an ok tool step: %v", out)
	}
	if code, out := c.do("GET", "/orders", nil); code != 200 || len(out["items"].([]any)) != 2 {
		t.Fatalf("orders after renew = %d %v", code, out)
	}
	// 第二次确认 → 409 JSON（不是流），不产生第二个订单
	if res := c.stream("/butler/conversations/"+convID+"/tool-calls/"+tcID+"/confirm", map[string]any{"approve": true}); res.status != 409 || res.errJSON["code"] != "butler_confirm_invalid" {
		t.Fatalf("second confirm = %d %s", res.status, res.raw)
	}
	if _, out := c.do("GET", "/orders", nil); len(out["items"].([]any)) != 2 {
		t.Fatalf("second confirm created an order: %v", out)
	}

	// 拒绝：无 tool 事件要求，模型收尾，卡片在详情里为 rejected
	card, convID = proposeRenew(t, e, c, id)
	res = c.stream("/butler/conversations/"+convID+"/tool-calls/"+card["id"].(string)+"/confirm", map[string]any{"approve": false})
	if res.status != 200 || res.first("done") == nil {
		t.Fatalf("reject = %d %s", res.status, res.raw)
	}
	_, out = c.do("GET", "/butler/conversations/"+convID, nil)
	if out["pending_tool_call"] != nil || !strings.Contains(rawJSON(out), `"status":"rejected"`) {
		t.Fatalf("detail after reject = %v", out)
	}
	if _, out := c.do("GET", "/orders", nil); len(out["items"].([]any)) != 2 {
		t.Fatalf("reject created an order: %v", out)
	}
}

func TestButlerOtherUsersConversation404(t *testing.T) {
	e := newButlerEnv(t, nil)
	owner := e.client()
	id := owner.runningInstance(t, e.testEnv, "13800001205")
	card, convID := proposeRenew(t, e, owner, id)

	other := e.client()
	other.login("13800001206")
	if code, out := other.do("GET", "/butler/conversations/"+convID, nil); code != 404 || out["code"] != "not_found" {
		t.Fatalf("other GET = %d %v", code, out)
	}
	if code, out := other.do("DELETE", "/butler/conversations/"+convID, nil); code != 404 || out["code"] != "not_found" {
		t.Fatalf("other DELETE = %d %v", code, out)
	}
	if res := other.stream("/butler/conversations/"+convID+"/messages", map[string]any{"text": "hi"}); res.status != 404 || res.errJSON["code"] != "not_found" {
		t.Fatalf("other send = %d %s", res.status, res.raw)
	}
	if res := other.stream("/butler/conversations/"+convID+"/tool-calls/"+card["id"].(string)+"/confirm", map[string]any{"approve": true}); res.status != 404 || res.errJSON["code"] != "not_found" {
		t.Fatalf("other confirm = %d %s", res.status, res.raw)
	}
	if res := other.stream("/butler/conversations/new/messages", map[string]any{"text": "hi", "context": map[string]any{"instance_id": id}}); res.status != 404 || res.errJSON["code"] != "not_found" {
		t.Fatalf("other context instance = %d %s", res.status, res.raw)
	}
	if _, out := other.do("GET", "/butler/conversations", nil); len(out["items"].([]any)) != 0 {
		t.Fatalf("other list = %v", out)
	}
	// 主人的卡不受影响
	if _, out := owner.do("GET", "/butler/conversations/"+convID, nil); out["pending_tool_call"] == nil {
		t.Fatalf("owner card lost: %v", out)
	}
	// 收尾：拒绝掉，保持不变量检查干净
	owner.stream("/butler/conversations/"+convID+"/tool-calls/"+card["id"].(string)+"/confirm", map[string]any{"approve": false})
}

func TestButlerRateLimit(t *testing.T) {
	e := newButlerEnv(t, func(srv *api.Server) { srv.ButlerLimit = 2 })
	c := e.client()
	c.login("13800001207")
	for i := 0; i < 2; i++ {
		if res := c.stream("/butler/conversations/new/messages", map[string]any{"text": "你好"}); res.status != 200 {
			t.Fatalf("send %d = %d %s", i, res.status, res.raw)
		}
	}
	res := c.stream("/butler/conversations/new/messages", map[string]any{"text": "你好"})
	if res.status != 429 || res.errJSON["code"] != "rate_limited" {
		t.Fatalf("third send = %d %s", res.status, res.raw)
	}
	// confirm 共用同一个计数
	if res := c.stream("/butler/conversations/cv-x/tool-calls/tc-x/confirm", map[string]any{"approve": true}); res.status != 429 {
		t.Fatalf("confirm over limit = %d %s", res.status, res.raw)
	}
	// 读接口不限
	for i := 0; i < 3; i++ {
		if code, _ := c.do("GET", "/butler/conversations", nil); code != 200 {
			t.Fatalf("list %d = %d", i, code)
		}
	}
	// 按用户计数
	other := e.client()
	other.login("13800001208")
	if res := other.stream("/butler/conversations/new/messages", map[string]any{"text": "你好"}); res.status != 200 {
		t.Fatalf("other user send = %d %s", res.status, res.raw)
	}
	// 窗口滑过后恢复
	e.clk.Advance(time.Minute)
	if res := c.stream("/butler/conversations/new/messages", map[string]any{"text": "你好"}); res.status != 200 {
		t.Fatalf("send after window = %d %s", res.status, res.raw)
	}
}

func TestButlerDelete(t *testing.T) {
	e := newButlerEnv(t, nil)
	c := e.client()
	c.login("13800001209")
	res := c.stream("/butler/conversations/new/messages", map[string]any{"text": "你好"})
	convID := res.first("start")["conversation_id"].(string)
	if code, out := c.do("DELETE", "/butler/conversations/"+convID, nil); code != 200 || out["ok"] != true {
		t.Fatalf("delete = %d %v", code, out)
	}
	if _, out := c.do("GET", "/butler/conversations", nil); len(out["items"].([]any)) != 0 {
		t.Fatalf("list after delete = %v", out)
	}
	if code, _ := c.do("GET", "/butler/conversations/"+convID, nil); code != 404 {
		t.Fatalf("GET after delete = %d", code)
	}
	if code, out := c.do("DELETE", "/butler/conversations/"+convID, nil); code != 404 || out["code"] != "not_found" {
		t.Fatalf("delete twice = %d %v", code, out)
	}
	if n := e.countRows(`SELECT count(*) FROM butler_messages`); n != 0 {
		t.Fatalf("messages left after delete: %d", n)
	}
	// 运行中的对话不可删 → butler_busy
	res = c.stream("/butler/conversations/new/messages", map[string]any{"text": "你好"})
	convID = res.first("start")["conversation_id"].(string)
	e.exec(`UPDATE butler_conversations SET status = 'running', run_token = 'x' WHERE id = $1`, convID)
	if code, out := c.do("DELETE", "/butler/conversations/"+convID, nil); code != 409 || out["code"] != "butler_busy" {
		t.Fatalf("delete running = %d %v", code, out)
	}
	e.exec(`UPDATE butler_conversations SET status = 'idle', run_token = '' WHERE id = $1`, convID)
}

func TestButlerDisabledHidden(t *testing.T) {
	e := newTestEnv(t) // Butler == nil
	c := e.client()
	c.login("13800001210")
	for _, p := range []string{"/butler/status", "/butler/conversations", "/butler/conversations/cv-x"} {
		if code, out := c.do("GET", p, nil); code != 404 || out["code"] != "not_found" {
			t.Fatalf("GET %s = %d %v", p, code, out)
		}
	}
	if code, _ := c.do("POST", "/butler/conversations/new/messages", map[string]any{"text": "hi"}); code != 404 {
		t.Fatalf("POST messages = %d", code)
	}
	if code, _ := c.do("DELETE", "/butler/conversations/cv-x", nil); code != 404 {
		t.Fatalf("DELETE = %d", code)
	}
}

// 客户端在流中途断开：服务端轮次继续跑完并落库（Review Focus 3）。
func TestButlerClientDisconnectKeepsRunning(t *testing.T) {
	e := newButlerEnv(t, nil)
	c := e.client()
	c.login("13800001211")
	long := strings.Repeat("这是一段很长的回答。", 12) // 120 字 → 15 段，每段 40ms ≈ 0.6s
	e.mock.Enqueue(mock.Reply{Text: long, ChunkDelay: 40 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b, _ := json.Marshal(map[string]any{"text": "说点长的"})
	req, _ := http.NewRequestWithContext(ctx, "POST", c.base+"/butler/conversations/new/messages", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("send = %d", resp.StatusCode)
	}
	// 读到 start 和第一段 delta 后断开
	rd := bufio.NewReader(resp.Body)
	var convID string
	sawDelta := false
	for !sawDelta {
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if strings.HasPrefix(line, "data: ") && convID == "" {
			var d map[string]any
			_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &d)
			convID, _ = d["conversation_id"].(string)
		}
		if line == "event: delta\n" {
			sawDelta = true
		}
	}
	cancel()
	resp.Body.Close()
	if convID == "" {
		t.Fatal("no conversation id from start")
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		_, out := c.do("GET", "/butler/conversations/"+convID, nil)
		conv := out["conversation"].(map[string]any)
		if conv["status"] == "idle" {
			msgs := out["messages"].([]any)
			if len(msgs) != 2 || !strings.Contains(rawJSON(msgs[1]), long) {
				t.Fatalf("assistant reply not fully stored after disconnect: %v", out)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run did not finish after client disconnect: %v", out)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// 流持续期间按 ButlerPing 间隔写保活注释；流建立前不写（JSON 错误不会被 200 抢先）。
func TestButlerSSEPing(t *testing.T) {
	e := newButlerEnv(t, func(srv *api.Server) { srv.ButlerPing = 20 * time.Millisecond })
	c := e.client()
	c.login("13800001212")
	e.mock.Enqueue(mock.Reply{Text: strings.Repeat("慢慢地说。", 8), ChunkDelay: 50 * time.Millisecond}) // 40 字 → 5 段 ≈ 250ms
	res := c.stream("/butler/conversations/new/messages", map[string]any{"text": "慢一点"})
	if res.status != 200 || res.pings == 0 {
		t.Fatalf("pings = %d, status %d\n%s", res.pings, res.status, res.raw)
	}
	if res.events[0].name != "start" || res.events[len(res.events)-1].name != "done" {
		t.Fatalf("events = %v", res.names())
	}
	if !strings.HasPrefix(res.raw, ": ok\n\nevent: start\n") {
		t.Fatalf("stream must open with the ok comment then start: %q", res.raw[:min(len(res.raw), 60)])
	}
	// 校验错误在慢 ping 下仍是纯 JSON
	if res := c.stream("/butler/conversations/new/messages", map[string]any{"text": ""}); res.status != 400 || res.errJSON["code"] != "butler_message_invalid" {
		t.Fatalf("validation = %d %s", res.status, res.raw)
	}
}

// 流建立后的模型错误只以 SSE error 事件报告（状态 200）。
func TestButlerLLMErrorAsSSEEvent(t *testing.T) {
	e := newButlerEnv(t, nil)
	c := e.client()
	c.login("13800001213")
	e.mock.Enqueue(mock.Reply{Err: &llm.UnavailableError{Cause: io.ErrUnexpectedEOF}})
	res := c.stream("/butler/conversations/new/messages", map[string]any{"text": "你好"})
	if res.status != 200 || res.events[0].name != "start" {
		t.Fatalf("send = %d %s", res.status, res.raw)
	}
	if ev := res.first("error"); ev == nil || ev["code"] != "butler_llm_unavailable" || ev["message"] == "" {
		t.Fatalf("events = %v\n%s", res.names(), res.raw)
	}
}

// 服务器设了 WriteTimeout（生产 60 秒）时，SSE 流不受它截断：handler 清除了本请求的读写期限。
func TestButlerStreamOutlivesWriteTimeout(t *testing.T) {
	e := newButlerEnv(t, nil)
	ts := httptest.NewUnstartedServer(e.ts.Config.Handler)
	ts.Config.WriteTimeout = 200 * time.Millisecond
	ts.Start()
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	c := &client{t: t, base: ts.URL + "/api/v1", http: &http.Client{Jar: jar}}
	c.login("13800001214")
	e.mock.Enqueue(mock.Reply{Text: strings.Repeat("慢慢地说。", 8), ChunkDelay: 100 * time.Millisecond}) // 5 段 ≈ 500ms > WriteTimeout
	res := c.stream("/butler/conversations/new/messages", map[string]any{"text": "慢一点"})
	if res.status != 200 || len(res.events) == 0 || res.events[len(res.events)-1].name != "done" {
		t.Fatalf("stream cut by WriteTimeout: %d %v\n%s", res.status, res.names(), res.raw)
	}
}

// 流建立前的其它 JSON 错误：未登录 401、busy 409、轮数上限 409、确认时已关闭 503。
func TestButlerPreStreamErrorsJSON(t *testing.T) {
	e := newButlerEnv(t, nil)
	anon := e.client()
	for _, p := range []string{"/butler/conversations/new/messages", "/butler/conversations/cv-x/tool-calls/tc-x/confirm"} {
		body := map[string]any{"text": "hi"}
		if strings.HasSuffix(p, "/confirm") {
			body = map[string]any{"approve": true}
		}
		if res := anon.stream(p, body); res.status != 401 || res.errJSON["code"] != "unauthorized" {
			t.Fatalf("anonymous %s = %d %s", p, res.status, res.raw)
		}
	}

	c := e.client()
	id := c.runningInstance(t, e.testEnv, "13800001215")
	res := c.stream("/butler/conversations/new/messages", map[string]any{"text": "你好"})
	convID := res.first("start")["conversation_id"].(string)

	e.exec(`UPDATE butler_conversations SET status = 'running', run_token = 'x' WHERE id = $1`, convID)
	if res := c.stream("/butler/conversations/"+convID+"/messages", map[string]any{"text": "再来"}); res.status != 409 || res.errJSON["code"] != "butler_busy" {
		t.Fatalf("busy = %d %s", res.status, res.raw)
	}
	e.exec(`UPDATE butler_conversations SET status = 'idle', run_token = '', user_turns = 60 WHERE id = $1`, convID)
	if res := c.stream("/butler/conversations/"+convID+"/messages", map[string]any{"text": "再来"}); res.status != 409 || res.errJSON["code"] != "butler_turn_limit" {
		t.Fatalf("turn limit = %d %s", res.status, res.raw)
	}

	card, convID := proposeRenew(t, e, c, id)
	e.exec(`UPDATE butler_settings SET enabled = false, notice = '维护'`)
	e.svc.InvalidateSettings()
	confirm := "/butler/conversations/" + convID + "/tool-calls/" + card["id"].(string) + "/confirm"
	if res := c.stream(confirm, map[string]any{"approve": true}); res.status != 503 || res.errJSON["code"] != "butler_disabled" || res.errJSON["message"] != "AI 管家暂未开放：维护" {
		t.Fatalf("confirm while disabled = %d %s", res.status, res.raw)
	}
	if _, out := c.do("GET", "/orders", nil); len(out["items"].([]any)) != 1 {
		t.Fatalf("disabled confirm must not execute: %v", out)
	}
	// 收尾：重新打开并拒绝，保持不变量检查干净
	e.exec(`UPDATE butler_settings SET enabled = true, notice = ''`)
	e.svc.InvalidateSettings()
	if res := c.stream(confirm, map[string]any{"approve": false}); res.status != 200 {
		t.Fatalf("reject = %d %s", res.status, res.raw)
	}
}
