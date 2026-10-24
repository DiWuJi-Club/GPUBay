package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/option"

	"spark-cloud/internal/butler/llm"
)

// fakeServer 是假的 Messages API：记录请求，按脚本回放 SSE 事件。
type fakeServer struct {
	mu     sync.Mutex
	body   map[string]any
	header http.Header
	path   string
	calls  int
}

// sse 生成一帧 `event: <type>\ndata: <json>`。
func sse(typ, data string) string { return "event: " + typ + "\ndata: " + data }

func (f *fakeServer) start(t *testing.T, status int, frames []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls++
		f.header = r.Header.Clone()
		f.path = r.URL.Path
		f.body = nil
		_ = json.Unmarshal(raw, &f.body)
		f.mu.Unlock()
		if status != 200 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"bad"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, fr := range frames {
			_, _ = io.WriteString(w, fr+"\n\n")
			fl.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newProvider(base string) *Provider {
	return New(Config{BaseURL: base, APIKey: "test-key", Model: "claude-opus-5-5", MaxTokens: 2048, opts: []option.RequestOption{option.WithMaxRetries(0)}})
}

func collect(t *testing.T, p *Provider, req llm.Request) ([]llm.Event, error) {
	t.Helper()
	var evs []llm.Event
	err := p.Chat(context.Background(), req, func(e llm.Event) { evs = append(evs, e) })
	return evs, err
}

func types(evs []llm.Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Type
	}
	return out
}

const toolSchema = `{"type":"object","properties":{"state":{"type":"string","enum":["running","stopped"]}},"required":["state"],"additionalProperties":false}`

var sampleTools = []llm.ToolDef{{Name: "list_instances", Description: "查询实例", Schema: json.RawMessage(toolSchema)}}

// 计划 Step 2 的脚本：文本 + 一个 tool_use，stop_reason = tool_use。
var toolUseScript = []string{
	sse("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":50,"output_tokens":1}}}`),
	sse("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
	sse("ping", `{"type":"ping"}`),
	sse("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你好"}}`),
	sse("content_block_stop", `{"type":"content_block_stop","index":0}`),
	sse("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"list_instances","input":{}}}`),
	sse("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{}"}}`),
	sse("content_block_stop", `{"type":"content_block_stop","index":1}`),
	sse("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":20}}`),
	sse("message_stop", `{"type":"message_stop"}`),
}

func TestAnthropicStreamAndRequest(t *testing.T) {
	f := &fakeServer{}
	srv := f.start(t, 200, toolUseScript)
	p := newProvider(srv.URL)
	if p.Name() != "anthropic" || p.Model() != "claude-opus-5-5" {
		t.Fatalf("name/model = %s/%s", p.Name(), p.Model())
	}
	req := llm.Request{
		System: "你是 AI 管家",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "我的实例"}, {Type: llm.BlockContext, InstanceID: "ins-1"}}},
			{Role: llm.RoleAssistant, Blocks: []llm.Block{
				{Type: llm.BlockText, Text: "我查一下"},
				{Type: llm.BlockToolUse, ToolCallID: "tc-a", ProviderCallID: "toolu_a", Name: "list_instances", Args: json.RawMessage(`{"state":"running"}`)},
				{Type: llm.BlockToolUse, ToolCallID: "tc-b", Name: "get_wallet"}, // 无 ProviderCallID → 退回 ToolCallID；Args 空 → {}
			}},
			{Role: llm.RoleTool, Blocks: []llm.Block{{Type: llm.BlockToolResult, ToolCallID: "tc-a", ProviderCallID: "toolu_a", OK: true, Content: "[]"}}},
			{Role: llm.RoleTool, Blocks: []llm.Block{{Type: llm.BlockToolResult, ToolCallID: "tc-b", OK: false, Content: "boom"}}},
		},
		Tools: sampleTools,
	}
	evs, err := collect(t, p, req)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := types(evs), []string{llm.EvText, llm.EvTool, llm.EvUsage, llm.EvDone}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if evs[0].Text != "你好" {
		t.Fatalf("text = %q", evs[0].Text)
	}
	tc := evs[1].ToolCall
	if tc.ProviderCallID != "toolu_1" || tc.Name != "list_instances" || string(tc.Args) != "{}" {
		t.Fatalf("tool = %+v (%s)", tc, tc.Args)
	}
	if u := evs[2].Usage; u.InputTokens != 50 || u.OutputTokens != 20 {
		t.Fatalf("usage = %+v", u)
	}
	if evs[3].StopReason != llm.StopToolUse {
		t.Fatalf("stop = %s", evs[3].StopReason)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.path != "/v1/messages" {
		t.Fatalf("path = %s", f.path)
	}
	if f.header.Get("X-Api-Key") != "test-key" {
		t.Fatalf("x-api-key = %q", f.header.Get("X-Api-Key"))
	}
	if f.header.Get("Anthropic-Version") == "" {
		t.Fatal("缺少 anthropic-version 头")
	}
	if f.header.Get("Authorization") != "" {
		t.Fatal("不应带 Authorization 头")
	}
	b := f.body
	if b["model"] != "claude-opus-5-5" || b["max_tokens"] != float64(2048) || b["stream"] != true {
		t.Fatalf("model/max_tokens/stream = %v/%v/%v", b["model"], b["max_tokens"], b["stream"])
	}
	for _, k := range []string{"thinking", "tool_choice", "temperature"} {
		if _, ok := b[k]; ok {
			t.Fatalf("不应发送 %s", k)
		}
	}
	sys := b["system"].([]any)[0].(map[string]any)
	if sys["text"] != "你是 AI 管家" || sys["cache_control"].(map[string]any)["type"] != "ephemeral" {
		t.Fatalf("system = %v", sys)
	}
	if oc := b["output_config"].(map[string]any); oc["effort"] != "low" {
		t.Fatalf("output_config = %v", oc)
	}
	tool := b["tools"].([]any)[0].(map[string]any)
	if tool["name"] != "list_instances" || tool["description"] != "查询实例" || tool["strict"] != true {
		t.Fatalf("tool = %v", tool)
	}
	var wantSchema, gotSchema any
	_ = json.Unmarshal([]byte(toolSchema), &wantSchema)
	gotSchema = tool["input_schema"]
	if !reflect.DeepEqual(gotSchema, wantSchema) {
		gs, _ := json.Marshal(gotSchema)
		t.Fatalf("input_schema = %s, want %s", gs, toolSchema)
	}

	msgs := b["messages"].([]any)
	if len(msgs) != 3 {
		raw, _ := json.Marshal(msgs)
		t.Fatalf("messages = %d: %s", len(msgs), raw)
	}
	m0 := msgs[0].(map[string]any)
	c0 := m0["content"].([]any)
	if m0["role"] != "user" || len(c0) != 1 || c0[0].(map[string]any)["text"] != "我的实例" {
		t.Fatalf("msg0 = %v", m0)
	}
	m1 := msgs[1].(map[string]any)
	c1 := m1["content"].([]any)
	if m1["role"] != "assistant" || len(c1) != 3 {
		t.Fatalf("msg1 = %v", m1)
	}
	if c1[0].(map[string]any)["type"] != "text" || c1[0].(map[string]any)["text"] != "我查一下" {
		t.Fatalf("msg1 text = %v", c1[0])
	}
	tu := c1[1].(map[string]any)
	if tu["type"] != "tool_use" || tu["id"] != "toolu_a" || tu["name"] != "list_instances" || tu["input"].(map[string]any)["state"] != "running" {
		t.Fatalf("tool_use a = %v", tu)
	}
	tu2 := c1[2].(map[string]any)
	if tu2["id"] != "tc-b" || tu2["name"] != "get_wallet" || len(tu2["input"].(map[string]any)) != 0 {
		t.Fatalf("tool_use b = %v", tu2)
	}
	// 相邻两条 tool 消息合并进同一个 user 消息
	m2 := msgs[2].(map[string]any)
	c2 := m2["content"].([]any)
	if m2["role"] != "user" || len(c2) != 2 {
		t.Fatalf("msg2 = %v", m2)
	}
	r0, r1 := c2[0].(map[string]any), c2[1].(map[string]any)
	if r0["type"] != "tool_result" || r0["tool_use_id"] != "toolu_a" || r0["is_error"] != false {
		t.Fatalf("result a = %v", r0)
	}
	if r0["content"].([]any)[0].(map[string]any)["text"] != "[]" {
		t.Fatalf("result a content = %v", r0["content"])
	}
	if r1["tool_use_id"] != "tc-b" || r1["is_error"] != true || r1["content"].([]any)[0].(map[string]any)["text"] != "boom" {
		t.Fatalf("result b = %v", r1)
	}
}

func TestAnthropicNoToolsNoSystem(t *testing.T) {
	f := &fakeServer{}
	srv := f.start(t, 200, toolUseScript)
	p := New(Config{BaseURL: srv.URL, APIKey: "test-key", Model: "claude-opus-5-5", opts: []option.RequestOption{option.WithMaxRetries(0)}})
	if _, err := collect(t, p, llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "hi"}}}}}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	b := f.body
	f.mu.Unlock()
	for _, k := range []string{"tools", "tool_choice", "system", "thinking"} {
		if _, ok := b[k]; ok {
			t.Fatalf("不应发送 %s: %v", k, b[k])
		}
	}
	if b["max_tokens"] != float64(defaultMaxTokens) {
		t.Fatalf("max_tokens = %v", b["max_tokens"])
	}
	// Request.MaxTokens 优先于配置
	if _, err := collect(t, p, llm.Request{MaxTokens: 77, Messages: []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "hi"}}}}}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.body["max_tokens"] != float64(77) {
		t.Fatalf("max_tokens = %v", f.body["max_tokens"])
	}
}

func TestAnthropicMessageMerging(t *testing.T) {
	// 连续同角色消息合并；合并后的 user 消息里 tool_result 块排在最前；空文本、context 块不发送；空结果不发空文本块
	msgs := []llm.Message{
		{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "a"}}},
		{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockContext, InstanceID: "ins-1"}, {Type: llm.BlockText, Text: ""}}},
		{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockToolUse, ProviderCallID: "toolu_x", Name: "get_wallet", Args: json.RawMessage(`not json`)}}},
		{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "还在吗"}}},
		{Role: llm.RoleTool, Blocks: []llm.Block{{Type: llm.BlockToolResult, ProviderCallID: "toolu_x", OK: true, Content: ""}}},
	}
	out := mapMessages(msgs)
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	_ = json.Unmarshal(raw, &got)
	if len(got) != 3 || got[0]["role"] != "user" || got[1]["role"] != "assistant" || got[2]["role"] != "user" {
		t.Fatalf("messages = %s", raw)
	}
	if c := got[0]["content"].([]any); len(c) != 1 {
		t.Fatalf("msg0 = %s", raw)
	}
	if in := got[1]["content"].([]any)[0].(map[string]any)["input"].(map[string]any); len(in) != 0 {
		t.Fatalf("非法 Args 应发送 {}: %s", raw)
	}
	c2 := got[2]["content"].([]any)
	if len(c2) != 2 || c2[0].(map[string]any)["type"] != "tool_result" || c2[1].(map[string]any)["type"] != "text" {
		t.Fatalf("msg2 = %s", raw)
	}
	if _, ok := c2[0].(map[string]any)["content"]; ok {
		t.Fatalf("空结果不应带 content: %s", raw)
	}
}

func TestAnthropicRefusal(t *testing.T) {
	f := &fakeServer{}
	srv := f.start(t, 200, []string{
		sse("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"usage":{"input_tokens":10,"cache_read_input_tokens":30,"cache_creation_input_tokens":5,"output_tokens":0}}}`),
		sse("message_delta", `{"type":"message_delta","delta":{"stop_reason":"refusal"},"usage":{"output_tokens":3}}`),
		sse("message_stop", `{"type":"message_stop"}`),
	})
	evs, err := collect(t, newProvider(srv.URL), llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "x"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := types(evs); !reflect.DeepEqual(got, []string{llm.EvUsage, llm.EvDone}) {
		t.Fatalf("events = %v", got)
	}
	// 输入 token 计入缓存读写（总输入 = input + cache_read + cache_creation）
	if u := evs[0].Usage; u.InputTokens != 45 || u.OutputTokens != 3 {
		t.Fatalf("usage = %+v", u)
	}
	if evs[1].StopReason != llm.StopRefusal {
		t.Fatalf("stop = %s", evs[1].StopReason)
	}
}

func TestAnthropicStopReasons(t *testing.T) {
	cases := map[string]string{
		"end_turn":                      llm.StopEndTurn,
		"stop_sequence":                 llm.StopEndTurn,
		"pause_turn":                    llm.StopEndTurn,
		"max_tokens":                    llm.StopMaxTokens,
		"model_context_window_exceeded": llm.StopMaxTokens,
		"tool_use":                      llm.StopToolUse,
		"refusal":                       llm.StopRefusal,
		"something_new":                 llm.StopEndTurn,
	}
	for in, want := range cases {
		if got := mapStop(in); got != want {
			t.Errorf("mapStop(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestAnthropicInvalidToolJSON(t *testing.T) {
	f := &fakeServer{}
	srv := f.start(t, 200, []string{
		sse("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`),
		sse("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_instance","input":{}}}`),
		sse("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"id\":"}}`),
		sse("content_block_stop", `{"type":"content_block_stop","index":0}`),
		sse("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_2","name":"get_wallet","input":{}}}`),
		sse("content_block_stop", `{"type":"content_block_stop","index":1}`),
		sse("content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_3","name":"get_instance","input":{}}}`),
		sse("content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"id\":"}}`),
		sse("content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"ins-1\"}"}}`),
		sse("content_block_stop", `{"type":"content_block_stop","index":2}`),
		sse("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`),
		sse("message_stop", `{"type":"message_stop"}`),
	})
	evs, err := collect(t, newProvider(srv.URL), llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "x"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := types(evs); !reflect.DeepEqual(got, []string{llm.EvTool, llm.EvTool, llm.EvTool, llm.EvUsage, llm.EvDone}) {
		t.Fatalf("events = %v", got)
	}
	var m map[string]string
	if err := json.Unmarshal(evs[0].ToolCall.Args, &m); err != nil || m["_invalid_json"] != `{"id":` {
		t.Fatalf("args0 = %s", evs[0].ToolCall.Args)
	}
	if string(evs[1].ToolCall.Args) != "{}" {
		t.Fatalf("args1 = %s", evs[1].ToolCall.Args)
	}
	if string(evs[2].ToolCall.Args) != `{"id":"ins-1"}` || evs[2].ToolCall.ProviderCallID != "toolu_3" {
		t.Fatalf("tool2 = %+v %s", evs[2].ToolCall, evs[2].ToolCall.Args)
	}
}

func TestAnthropicErrors(t *testing.T) {
	req := llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "x"}}}}}

	for _, st := range []int{529, 500, 429} {
		f := &fakeServer{}
		srv := f.start(t, st, nil)
		_, err := collect(t, newProvider(srv.URL), req)
		var ue *llm.UnavailableError
		if !errors.As(err, &ue) || ue.Status != st {
			t.Fatalf("%d → %T %v", st, err, err)
		}
	}
	for _, st := range []int{400, 401, 404} {
		f := &fakeServer{}
		srv := f.start(t, st, nil)
		_, err := collect(t, newProvider(srv.URL), req)
		var re *llm.RequestError
		if !errors.As(err, &re) || re.Status != st || !strings.Contains(re.Body, "overloaded_error") {
			t.Fatalf("%d → %T %v", st, err, err)
		}
		if strings.Contains(err.Error(), "test-key") {
			t.Fatal("错误信息不得包含密钥")
		}
	}

	// 连接失败 → Unavailable（Status 0）
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	_ = ln.Close()
	_, err := collect(t, newProvider("http://"+addr), req)
	var ue *llm.UnavailableError
	if !errors.As(err, &ue) || ue.Status != 0 {
		t.Fatalf("conn refused → %T %v", err, err)
	}

	// 流中 error 事件（HTTP 200）→ Unavailable
	f := &fakeServer{}
	srv := f.start(t, 200, []string{
		sse("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`),
		sse("error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`),
	})
	_, err = collect(t, newProvider(srv.URL), req)
	if !errors.As(err, &ue) {
		t.Fatalf("stream error → %T %v", err, err)
	}

	// 流在 message_stop 前断开 → Unavailable，且不发 done
	f = &fakeServer{}
	srv = f.start(t, 200, toolUseScript[:4])
	evs, err := collect(t, newProvider(srv.URL), req)
	if !errors.As(err, &ue) {
		t.Fatalf("truncated → %T %v", err, err)
	}
	for _, e := range evs {
		if e.Type == llm.EvDone || e.Type == llm.EvUsage {
			t.Fatalf("断流不应发 %s", e.Type)
		}
	}

	// ctx 取消 → ctx.Err()
	block := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(block); slow.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err = newProvider(slow.URL).Chat(ctx, req, func(llm.Event) {})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ctx → %T %v", err, err)
	}
}

func TestAnthropicIgnoresEnvironmentCredentials(t *testing.T) {
	// 只用 SPARK_LLM_* 配置：环境里的 ANTHROPIC_* 不得覆盖 BaseURL / 密钥
	f := &fakeServer{}
	srv := f.start(t, 200, toolUseScript)
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:1")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "env-token")
	if _, err := collect(t, newProvider(srv.URL), llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "x"}}}}}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls != 1 || f.header.Get("Authorization") != "" || f.header.Get("X-Api-Key") != "test-key" {
		t.Fatalf("calls=%d auth=%q key=%q", f.calls, f.header.Get("Authorization"), f.header.Get("X-Api-Key"))
	}
}
