package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"spark-cloud/internal/butler/llm"
)

// fakeServer 记录收到的请求并按脚本回放 SSE 行。
type fakeServer struct {
	mu     sync.Mutex
	body   map[string]any
	header http.Header
	path   string
}

func (f *fakeServer) start(t *testing.T, status int, lines []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.header = r.Header.Clone()
		f.path = r.URL.Path
		f.body = nil
		_ = json.Unmarshal(raw, &f.body)
		f.mu.Unlock()
		if status != 200 {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":{"message":"bad"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, l := range lines {
			_, _ = io.WriteString(w, l+"\n\n")
			fl.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newProvider(base string) *Provider {
	return New(Config{BaseURL: base + "/v1", APIKey: "test-key", Model: "qwen3-32b", MaxTokens: 2048})
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

var sampleTools = []llm.ToolDef{{Name: "list_instances", Description: "查询实例", Schema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)}}

func TestOpenAIStreamText(t *testing.T) {
	f := &fakeServer{}
	srv := f.start(t, 200, []string{
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"你好"}}]}`,
		`data: {"choices":[{"index":0,"delta":{"content":"，我是"}}]}`,
		`data: {"choices":[{"index":0,"delta":{"content":"管家"},"finish_reason":"stop"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":120,"completion_tokens":7,"total_tokens":127}}`,
		`data: [DONE]`,
	})
	p := newProvider(srv.URL)
	evs, err := collect(t, p, llm.Request{System: "sys", Messages: []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "hi"}}}}, Tools: sampleTools, MaxTokens: 1000})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(types(evs), ",")
	if got != "text_delta,text_delta,text_delta,usage,done" {
		t.Fatalf("events = %s", got)
	}
	if evs[0].Text+evs[1].Text+evs[2].Text != "你好，我是管家" {
		t.Fatalf("text = %q", evs[0].Text+evs[1].Text+evs[2].Text)
	}
	if u := evs[3].Usage; u.InputTokens != 120 || u.OutputTokens != 7 {
		t.Fatalf("usage = %+v", u)
	}
	if evs[4].StopReason != llm.StopEndTurn {
		t.Fatalf("stop = %s", evs[4].StopReason)
	}
	// 请求快照
	if f.path != "/v1/chat/completions" {
		t.Fatalf("path = %s", f.path)
	}
	if f.header.Get("Authorization") != "Bearer test-key" || f.header.Get("Content-Type") != "application/json" {
		t.Fatalf("headers = %v", f.header)
	}
	b := f.body
	if b["model"] != "qwen3-32b" || b["stream"] != true || b["tool_choice"] != "auto" || b["max_tokens"] != float64(1000) || b["temperature"] != 0.2 {
		t.Fatalf("body = %v", b)
	}
	if so, _ := b["stream_options"].(map[string]any); so["include_usage"] != true {
		t.Fatalf("stream_options = %v", b["stream_options"])
	}
	tools := b["tools"].([]any)
	fn := tools[0].(map[string]any)
	if fn["type"] != "function" || fn["function"].(map[string]any)["name"] != "list_instances" {
		t.Fatalf("tools = %v", tools)
	}
	if params := fn["function"].(map[string]any)["parameters"].(map[string]any); params["additionalProperties"] != false {
		t.Fatalf("parameters = %v", params)
	}
	msgs := b["messages"].([]any)
	if m0 := msgs[0].(map[string]any); m0["role"] != "system" || m0["content"] != "sys" {
		t.Fatalf("messages[0] = %v", m0)
	}
	if m1 := msgs[1].(map[string]any); m1["role"] != "user" || m1["content"] != "hi" {
		t.Fatalf("messages[1] = %v", m1)
	}
}

func TestOpenAIDefaultMaxTokens(t *testing.T) {
	f := &fakeServer{}
	srv := f.start(t, 200, []string{`data: {"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"length"}]}`, `data: [DONE]`})
	p := New(Config{BaseURL: srv.URL + "/v1", Model: "m", MaxTokens: 512})
	evs, err := collect(t, p, llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "q"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if f.body["max_tokens"] != float64(512) {
		t.Fatalf("max_tokens = %v", f.body["max_tokens"])
	}
	if f.header.Get("Authorization") != "" {
		t.Fatal("no key → no Authorization header")
	}
	if last := evs[len(evs)-1]; last.Type != llm.EvDone || last.StopReason != llm.StopMaxTokens {
		t.Fatalf("last = %+v", last)
	}
	if _, ok := f.body["messages"].([]any)[0].(map[string]any)["role"]; !ok || f.body["messages"].([]any)[0].(map[string]any)["role"] != "user" {
		t.Fatal("empty System → no system message")
	}
}

func TestOpenAIToolCallsAssembled(t *testing.T) {
	f := &fakeServer{}
	srv := f.start(t, 200, []string{
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"我查一下"}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"get_instance","arguments":""}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"get_app","arguments":"{\"sl"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"instance_"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"ug\":\"vllm\"}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"id\":\"ins-x\"}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":20}}`,
		`data: [DONE]`,
	})
	evs, err := collect(t, newProvider(srv.URL), llm.Request{Tools: sampleTools})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(types(evs), ","); got != "text_delta,tool_use,tool_use,usage,done" {
		t.Fatalf("events = %s", got)
	}
	a, b := evs[1].ToolCall, evs[2].ToolCall
	if a.ProviderCallID != "call_a" || a.Name != "get_instance" || string(a.Args) != `{"instance_id":"ins-x"}` {
		t.Fatalf("call a = %+v %s", a, a.Args)
	}
	if b.ProviderCallID != "call_b" || b.Name != "get_app" || string(b.Args) != `{"slug":"vllm"}` {
		t.Fatalf("call b = %+v %s", b, b.Args)
	}
	if evs[4].StopReason != llm.StopToolUse {
		t.Fatalf("stop = %s", evs[4].StopReason)
	}
}

func TestOpenAIToolCallsWithoutFinishReason(t *testing.T) {
	f := &fakeServer{}
	srv := f.start(t, 200, []string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"list_instances","arguments":""}}]}}]}`,
		`data: [DONE]`,
	})
	evs, err := collect(t, newProvider(srv.URL), llm.Request{Tools: sampleTools})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(types(evs), ","); got != "tool_use,done" {
		t.Fatalf("events = %s", got)
	}
	if string(evs[0].ToolCall.Args) != `{}` || evs[1].StopReason != llm.StopToolUse {
		t.Fatalf("empty args → {}; got %s / %s", evs[0].ToolCall.Args, evs[1].StopReason)
	}
}

func TestOpenAIInvalidToolJSON(t *testing.T) {
	f := &fakeServer{}
	long := strings.Repeat("坏", 300)
	srv := f.start(t, 200, []string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"get_app","arguments":"{\"slug\": vllm"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"c2","function":{"name":"get_app","arguments":"[1,2]"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":2,"id":"c3","function":{"name":"get_app","arguments":"` + long + `"}}]},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	})
	evs, err := collect(t, newProvider(srv.URL), llm.Request{Tools: sampleTools})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(types(evs), ","); got != "tool_use,tool_use,tool_use,done" {
		t.Fatalf("events = %s", got)
	}
	for i, want := range []string{`{"slug": vllm`, `[1,2]`, strings.Repeat("坏", 200)} {
		var m map[string]string
		if err := json.Unmarshal(evs[i].ToolCall.Args, &m); err != nil {
			t.Fatalf("args %d not object: %s", i, evs[i].ToolCall.Args)
		}
		if m["_invalid_json"] != want {
			t.Fatalf("args %d marker = %q", i, m["_invalid_json"])
		}
	}
}

func TestOpenAIErrors(t *testing.T) {
	req := llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "q"}}}}}

	t.Run("500", func(t *testing.T) {
		srv := (&fakeServer{}).start(t, 500, nil)
		_, err := collect(t, newProvider(srv.URL), req)
		var ue *llm.UnavailableError
		if !errors.As(err, &ue) || ue.Status != 500 {
			t.Fatalf("err = %#v", err)
		}
	})
	t.Run("429", func(t *testing.T) {
		srv := (&fakeServer{}).start(t, 429, nil)
		_, err := collect(t, newProvider(srv.URL), req)
		var ue *llm.UnavailableError
		if !errors.As(err, &ue) || ue.Status != 429 {
			t.Fatalf("err = %#v", err)
		}
	})
	t.Run("401", func(t *testing.T) {
		srv := (&fakeServer{}).start(t, 401, nil)
		_, err := collect(t, newProvider(srv.URL), req)
		var re *llm.RequestError
		if !errors.As(err, &re) || re.Status != 401 || !strings.Contains(re.Body, "bad") {
			t.Fatalf("err = %#v", err)
		}
	})
	t.Run("connection refused", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		ln.Close()
		_, err = collect(t, newProvider("http://"+addr), req)
		var ue *llm.UnavailableError
		if !errors.As(err, &ue) || ue.Status != 0 {
			t.Fatalf("err = %#v", err)
		}
	})
	t.Run("ctx timeout", func(t *testing.T) {
		stop := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"content":"a"}}]}`+"\n\n")
			w.(http.Flusher).Flush()
			_, _ = io.Copy(io.Discard, r.Body)
			select {
			case <-r.Context().Done():
			case <-stop:
			}
		}))
		defer srv.Close()
		defer close(stop)
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		err := newProvider(srv.URL).Chat(ctx, req, func(llm.Event) {})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %#v", err)
		}
	})
	t.Run("ctx timeout before headers", func(t *testing.T) {
		stop := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			select {
			case <-r.Context().Done():
			case <-stop:
			}
		}))
		defer srv.Close()
		defer close(stop)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		err := newProvider(srv.URL).Chat(ctx, req, func(llm.Event) {})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %#v", err)
		}
	})
	t.Run("line too long", func(t *testing.T) {
		big := `data: {"choices":[{"index":0,"delta":{"content":"` + strings.Repeat("a", 1<<20+10) + `"}}]}`
		srv := (&fakeServer{}).start(t, 200, []string{big, "data: [DONE]"})
		_, err := collect(t, newProvider(srv.URL), req)
		var ue *llm.UnavailableError
		if !errors.As(err, &ue) {
			t.Fatalf("err = %#v", err)
		}
	})
	t.Run("stream cut", func(t *testing.T) {
		srv := (&fakeServer{}).start(t, 200, []string{`data: {"choices":[{"index":0,"delta":{"content":"半"}}]}`})
		_, err := collect(t, newProvider(srv.URL), req)
		var ue *llm.UnavailableError
		if !errors.As(err, &ue) {
			t.Fatalf("err = %#v", err)
		}
	})
	t.Run("error chunk", func(t *testing.T) {
		srv := (&fakeServer{}).start(t, 200, []string{`data: {"error":{"message":"overloaded"}}`})
		_, err := collect(t, newProvider(srv.URL), req)
		var ue *llm.UnavailableError
		if !errors.As(err, &ue) || !strings.Contains(err.Error(), "overloaded") {
			t.Fatalf("err = %#v", err)
		}
	})
}

func TestOpenAINoToolsField(t *testing.T) {
	f := &fakeServer{}
	srv := f.start(t, 200, []string{`data: {"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"stop"}]}`, `data: [DONE]`})
	if _, err := collect(t, newProvider(srv.URL), llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "q"}}}}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.body["tools"]; ok {
		t.Fatal("tools present")
	}
	if _, ok := f.body["tool_choice"]; ok {
		t.Fatal("tool_choice present")
	}
}

func TestOpenAIMessageMapping(t *testing.T) {
	f := &fakeServer{}
	srv := f.start(t, 200, []string{`data: {"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`, `data: [DONE]`})
	req := llm.Request{
		System: "sys",
		Tools:  sampleTools,
		Messages: []llm.Message{
			{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockContext, InstanceID: "ins-x"}, {Type: llm.BlockText, Text: "部署 vLLM"}}},
			{Role: llm.RoleAssistant, Blocks: []llm.Block{
				{Type: llm.BlockText, Text: "好的，"},
				{Type: llm.BlockText, Text: "先查一下"},
				{Type: llm.BlockToolUse, ToolCallID: "tc-1", ProviderCallID: "call_1", Name: "list_instances", Args: json.RawMessage(`{}`)},
				{Type: llm.BlockToolUse, ToolCallID: "tc-2", ProviderCallID: "call_2", Name: "get_app", Args: json.RawMessage(`{"slug":"vllm"}`)},
			}},
			{Role: llm.RoleTool, Blocks: []llm.Block{
				{Type: llm.BlockToolResult, ToolCallID: "tc-1", ProviderCallID: "call_1", OK: true, Content: "[]"},
				{Type: llm.BlockToolResult, ToolCallID: "tc-2", ProviderCallID: "call_2", OK: true, Content: `{"slug":"vllm"}`},
			}},
			{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockToolUse, ToolCallID: "tc-3", Name: "list_apps"}}},
			{Role: llm.RoleTool, Blocks: []llm.Block{{Type: llm.BlockToolResult, ToolCallID: "tc-3", Content: "x"}}},
		},
	}
	if _, err := collect(t, newProvider(srv.URL), req); err != nil {
		t.Fatal(err)
	}
	msgs := f.body["messages"].([]any)
	if len(msgs) != 7 {
		t.Fatalf("len = %d: %v", len(msgs), msgs)
	}
	m := func(i int) map[string]any { return msgs[i].(map[string]any) }
	if m(1)["role"] != "user" || m(1)["content"] != "部署 vLLM" {
		t.Fatalf("user = %v", m(1))
	}
	if m(2)["role"] != "assistant" || m(2)["content"] != "好的，先查一下" {
		t.Fatalf("assistant = %v", m(2))
	}
	tcs := m(2)["tool_calls"].([]any)
	tc1 := tcs[1].(map[string]any)
	fn := tc1["function"].(map[string]any)
	if tc1["id"] != "call_2" || tc1["type"] != "function" || fn["name"] != "get_app" || fn["arguments"] != `{"slug":"vllm"}` {
		t.Fatalf("tool_call = %v", tc1)
	}
	if m(3)["role"] != "tool" || m(3)["tool_call_id"] != "call_1" || m(3)["content"] != "[]" {
		t.Fatalf("tool 1 = %v", m(3))
	}
	if m(4)["role"] != "tool" || m(4)["tool_call_id"] != "call_2" {
		t.Fatalf("tool 2 = %v", m(4))
	}
	// 没有 ProviderCallID 时退回 ToolCallID；Args 为空 → "{}"；无文本的 assistant 不带 content
	if _, has := m(5)["content"]; has {
		t.Fatalf("assistant without text has content: %v", m(5))
	}
	tc := m(5)["tool_calls"].([]any)[0].(map[string]any)
	if tc["id"] != "tc-3" || tc["function"].(map[string]any)["arguments"] != "{}" {
		t.Fatalf("fallback id = %v", tc)
	}
	if m(6)["tool_call_id"] != "tc-3" {
		t.Fatalf("tool fallback id = %v", m(6))
	}
}
