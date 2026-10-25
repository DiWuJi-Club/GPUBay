package llmtest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFakeVLLMBehaves(t *testing.T) {
	f := &FakeVLLM{Served: []string{"m1"}, RequireKey: "k", PromptTokens: 4, CompletionTokens: 3, Chunks: []string{"a", "b", "c"}, FailFirstN: 1}
	srv := httptest.NewServer(f)
	defer srv.Close()
	do := func(method, path, key, body string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	if r, _ := do("GET", "/v1/models", "wrong", ""); r.StatusCode != 401 {
		t.Fatalf("wrong key: %d", r.StatusCode)
	}
	if r, b := do("GET", "/v1/models", "k", ""); r.StatusCode != 200 || !strings.Contains(b, `"id":"m1"`) {
		t.Fatalf("models: %d %s", r.StatusCode, b)
	}
	// FailFirstN：第一个 POST 503
	if r, _ := do("POST", "/v1/chat/completions", "k", `{"model":"m1"}`); r.StatusCode != 503 {
		t.Fatalf("first POST: %d", r.StatusCode)
	}
	r, b := do("POST", "/v1/chat/completions", "k", `{"model":"m1"}`)
	var nonStream struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(b), &nonStream); err != nil || r.StatusCode != 200 ||
		nonStream.Choices[0].Message.Content != "abc" || nonStream.Usage.PromptTokens != 4 || nonStream.Usage.CompletionTokens != 3 {
		t.Fatalf("non-stream: %d %s", r.StatusCode, b)
	}
	if r, _ := do("POST", "/v1/chat/completions", "k", `{"model":"other"}`); r.StatusCode != 404 {
		t.Fatalf("unserved model: %d", r.StatusCode)
	}
	// 流式：不带 include_usage 没有 usage 块
	_, b = do("POST", "/v1/chat/completions", "k", `{"model":"m1","stream":true}`)
	if strings.Count(b, `"content":"`) != 4 || strings.Contains(b, `"usage"`) || !strings.HasSuffix(b, "data: [DONE]\n\n") {
		t.Fatalf("stream without usage: %q", b)
	}
	_, b = do("POST", "/v1/chat/completions", "k", `{"model":"m1","stream":true,"stream_options":{"include_usage":true}}`)
	if !strings.Contains(b, `"usage":{"completion_tokens":3,"prompt_tokens":4,"total_tokens":7}`) {
		t.Fatalf("stream with usage: %q", b)
	}
	rec := f.Recorded()
	last := rec[len(rec)-1]
	if last.Path != "/v1/chat/completions" || last.Authorization != "Bearer k" || !strings.Contains(string(last.Body), "include_usage") ||
		string(last.Sent) != b {
		t.Fatalf("recorded %+v", last)
	}
	// completions / embeddings
	if _, b := do("POST", "/v1/completions", "k", `{"model":"m1","prompt":"x"}`); !strings.Contains(b, `"text":"abc"`) {
		t.Fatalf("completions: %s", b)
	}
	if _, b := do("POST", "/v1/embeddings", "k", `{"model":"m1","input":"x"}`); !strings.Contains(b, `"embedding"`) ||
		!strings.Contains(b, `"prompt_tokens":4`) {
		t.Fatalf("embeddings: %s", b)
	}

	// DropAfterChunks：发完 N 块后断开（客户端读到意外 EOF）
	d := &FakeVLLM{Chunks: []string{"a", "b", "c"}, DropAfterChunks: 1}
	ds := httptest.NewServer(d)
	defer ds.Close()
	resp, err := http.Post(ds.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"x","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err == nil || strings.Count(string(body), `"content":"a"`) != 1 || strings.Contains(string(body), `"content":"b"`) {
		t.Fatalf("drop: err %v body %q", err, body)
	}
	// ErrorStatus
	es := httptest.NewServer(&FakeVLLM{ErrorStatus: 400})
	defer es.Close()
	resp, _ = http.Post(es.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"x"}`))
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("ErrorStatus: %d", resp.StatusCode)
	}
}
