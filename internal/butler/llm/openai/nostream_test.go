package openai

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"spark-cloud/internal/butler/llm"
)

// jsonServer 返回一次性的 chat.completion（非流式），并记下请求体与 Accept 头。
func jsonServer(t *testing.T, status int, resp string, body *map[string]any, accept *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, body)
		*accept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func noStreamProvider(base string) *Provider {
	return New(Config{BaseURL: base + "/v1", APIKey: "test-key", Model: "qwen3-4b", MaxTokens: 1024, NoStream: true})
}

func TestOpenAINoStreamTextAndTools(t *testing.T) {
	var body map[string]any
	var accept string
	srv := jsonServer(t, 200, `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant",
		"content":"\n\n我查一下","reasoning_content":"想一想","tool_calls":[
		{"id":"call_a","type":"function","function":{"name":"get_instance","arguments":"{\"instance_id\": \"ins-x\"}"}},
		{"id":"","type":"function","function":{"name":"list_products","arguments":""}}]},"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":5000,"completion_tokens":300,"total_tokens":5300}}`, &body, &accept)
	evs, err := collect(t, noStreamProvider(srv.URL), llm.Request{System: "sys", Tools: sampleTools,
		Messages: []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "hi"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(types(evs), ","); got != "text_delta,tool_use,tool_use,usage,done" {
		t.Fatalf("events = %s", got)
	}
	if evs[0].Text != "\n\n我查一下" {
		t.Fatalf("text = %q（推理内容不应出现在正文里）", evs[0].Text)
	}
	a, b := evs[1].ToolCall, evs[2].ToolCall
	if a.ProviderCallID != "call_a" || a.Name != "get_instance" || string(a.Args) != `{"instance_id": "ins-x"}` {
		t.Fatalf("call a = %+v %s", a, a.Args)
	}
	if b.ProviderCallID != "call_1" || b.Name != "list_products" || string(b.Args) != `{}` {
		t.Fatalf("call b = %+v %s", b, b.Args)
	}
	if u := evs[3].Usage; u.InputTokens != 5000 || u.OutputTokens != 300 {
		t.Fatalf("usage = %+v", u)
	}
	if evs[4].StopReason != llm.StopToolUse {
		t.Fatalf("stop = %s", evs[4].StopReason)
	}
	if body["stream"] != false || body["max_tokens"] != float64(1024) || body["tool_choice"] != "auto" {
		t.Fatalf("body = %v", body)
	}
	if _, ok := body["stream_options"]; ok {
		t.Fatalf("非流式不能带 stream_options（OpenAI 会 400）: %v", body["stream_options"])
	}
	if accept != "application/json" {
		t.Fatalf("Accept = %q", accept)
	}
}

func TestOpenAINoStreamStopReasons(t *testing.T) {
	for _, tc := range []struct {
		name, resp, types string
		stop              string
	}{
		{"stop", `{"choices":[{"message":{"content":"好的"},"finish_reason":"stop"}]}`, "text_delta,done", llm.StopEndTurn},
		{"length 丢弃半截调用", `{"choices":[{"message":{"content":"好","tool_calls":[{"id":"c1","function":{"name":"deploy_app","arguments":"{\"instance_id\":\"ins"}}]},"finish_reason":"length"}]}`, "text_delta,done", llm.StopMaxTokens},
		{"content_filter", `{"choices":[{"message":{"content":""},"finish_reason":"content_filter"}]}`, "done", llm.StopRefusal},
		{"有调用但没有 finish_reason", `{"choices":[{"message":{"tool_calls":[{"id":"c1","function":{"name":"list_instances","arguments":"{}"}}]}}]}`, "tool_use,done", llm.StopToolUse},
		{"参数不是 JSON", `{"choices":[{"message":{"tool_calls":[{"id":"c1","function":{"name":"list_instances","arguments":"{oops"}}]},"finish_reason":"tool_calls"}]}`, "tool_use,done", llm.StopToolUse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			var accept string
			srv := jsonServer(t, 200, tc.resp, &body, &accept)
			evs, err := collect(t, noStreamProvider(srv.URL), llm.Request{Tools: sampleTools})
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(types(evs), ","); got != tc.types || evs[len(evs)-1].StopReason != tc.stop {
				t.Fatalf("events = %s / %s", got, evs[len(evs)-1].StopReason)
			}
			if tc.name == "参数不是 JSON" && !strings.Contains(string(evs[0].ToolCall.Args), "_invalid_json") {
				t.Fatalf("args = %s", evs[0].ToolCall.Args)
			}
		})
	}
}

func TestOpenAINoStreamErrors(t *testing.T) {
	for _, tc := range []struct {
		name, resp string
		status     int
		unavail    bool
	}{
		{"vLLM 错误体", `{"object":"error","message":"overloaded"}`, 200, true},
		{"error 字段", `{"error":{"message":"overloaded"}}`, 200, true},
		{"不是 JSON", `<html>`, 200, true},
		{"没有 choices", `{"choices":[]}`, 200, true},
		{"500", `{"error":{"message":"bad"}}`, 500, true},
		{"400", `{"error":{"message":"bad"}}`, 400, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			var accept string
			srv := jsonServer(t, tc.status, tc.resp, &body, &accept)
			_, err := collect(t, noStreamProvider(srv.URL), llm.Request{})
			var ue *llm.UnavailableError
			var re *llm.RequestError
			if tc.unavail && !errors.As(err, &ue) || !tc.unavail && !errors.As(err, &re) {
				t.Fatalf("err = %#v", err)
			}
		})
	}
}
