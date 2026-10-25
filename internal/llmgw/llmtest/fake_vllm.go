// Package llmtest 提供 Token 网关测试用的假 vLLM（OpenAI 兼容接口），供 llmgw 单测与 e2e 钩子挂载。
package llmtest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// RecordedRequest 是 FakeVLLM 收到的一次请求。Sent 是假节点写给网关的响应体字节（逐块追加，写出之前先记），
// 供断言「客户端收到的字节 == 上游发出的字节」。
type RecordedRequest struct {
	Method        string
	Path          string
	Authorization string
	Header        http.Header
	Body          []byte
	Sent          []byte
}

// FakeVLLM 是一个假 vLLM 节点（http.Handler）：
//   - GET /health：200（不鉴权、不记录；appdeploy Runner 的 HTTP 健康检查打它）；
//   - GET /v1/models：返回 Served 名单；
//   - POST /v1/chat/completions、/v1/completions：非流式返回一个完整响应（content = Chunks 拼接）与 usage；
//     流式先发角色块，再按 Chunks 逐块发 data:，然后是 finish 块，请求带 stream_options.include_usage=true 时发 usage 块，最后 [DONE]；
//   - POST /v1/embeddings：返回一个 3 维向量与 usage（只有 prompt）。
//
// 可配置：RequireKey（Authorization 必须是 Bearer RequireKey，否则 401）；Delay（非流式在响应前、流式在每块前等待）；
// HeaderDelay（POST 在写响应头之前等待，模拟首字节慢）；
// FailFirstN（前 N 个 POST 返回 503）；DropAfterChunks（>0：流式发完 N 个内容块后直接断开连接）；
// ErrorStatus（≠0：POST 一律返回该状态码与 vLLM 风格错误体）。Served 非空时，请求的 model 不在其中 → 404。
// 字段在开始服务前设置；运行中只读（Requests 由 mu 保护）。
type FakeVLLM struct {
	Served           []string
	RequireKey       string
	PromptTokens     int
	CompletionTokens int
	Chunks           []string
	Delay            time.Duration
	HeaderDelay      time.Duration
	FailFirstN       int32
	DropAfterChunks  int
	ErrorStatus      int

	failed   atomic.Int32
	mu       sync.Mutex
	Requests []RecordedRequest
}

// Recorded 返回已收到请求的副本。
func (f *FakeVLLM) Recorded() []RecordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]RecordedRequest, len(f.Requests))
	for i, r := range f.Requests {
		r.Body = slices.Clone(r.Body)
		r.Sent = slices.Clone(r.Sent)
		out[i] = r
	}
	return out
}

// Count 返回已收到的请求数。
func (f *FakeVLLM) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Requests)
}

func (f *FakeVLLM) record(r *http.Request, body []byte) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Requests = append(f.Requests, RecordedRequest{Method: r.Method, Path: r.URL.Path, Authorization: r.Header.Get("Authorization"),
		Header: r.Header.Clone(), Body: body})
	return len(f.Requests) - 1
}

// sentWriter 把写出的字节同时记进 Requests[idx].Sent（先记再写）。
type sentWriter struct {
	f   *FakeVLLM
	idx int
	w   http.ResponseWriter
}

func (s *sentWriter) write(b []byte) error {
	s.f.mu.Lock()
	s.f.Requests[s.idx].Sent = append(s.f.Requests[s.idx].Sent, b...)
	s.f.mu.Unlock()
	_, err := s.w.Write(b)
	if fl, ok := s.w.(http.Flusher); ok {
		fl.Flush()
	}
	return err
}

// writeJSON 写一个 JSON 响应（字节同样记进 Sent）。
func (s *sentWriter) writeJSON(status int, v any) {
	b, _ := json.Marshal(v)
	s.w.Header().Set("Content-Type", "application/json")
	s.w.WriteHeader(status)
	_ = s.write(b)
}

// vllmError 是 vLLM 的错误体形态。
func vllmError(msg string, status int) map[string]any {
	return map[string]any{"object": "error", "message": msg, "type": http.StatusText(status), "param": nil, "code": status}
}

func (f *FakeVLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/health" { // 与 vLLM 相同：健康检查不鉴权、空响应体；不记进 Requests
		w.WriteHeader(http.StatusOK)
		return
	}
	body, _ := io.ReadAll(r.Body)
	idx := f.record(r, body)
	sw := &sentWriter{f: f, idx: idx, w: w}
	if f.RequireKey != "" && r.Header.Get("Authorization") != "Bearer "+f.RequireKey {
		sw.writeJSON(http.StatusUnauthorized, map[string]any{"error": "Unauthorized"})
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		data := make([]map[string]any, 0, len(f.Served))
		for _, s := range f.Served {
			data = append(data, map[string]any{"id": s, "object": "model", "created": 0, "owned_by": "vllm"})
		}
		sw.writeJSON(http.StatusOK, map[string]any{"object": "list", "data": data})
		return
	case r.Method == http.MethodPost && (r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/v1/completions" || r.URL.Path == "/v1/embeddings"):
	default:
		sw.writeJSON(http.StatusNotFound, vllmError("Not Found", http.StatusNotFound))
		return
	}

	if !sleepFor(r, f.HeaderDelay) {
		return
	}
	if f.failed.Add(1) <= f.FailFirstN {
		sw.writeJSON(http.StatusServiceUnavailable, vllmError("Service Unavailable", http.StatusServiceUnavailable))
		return
	}
	if f.ErrorStatus != 0 {
		sw.writeJSON(f.ErrorStatus, vllmError("fake upstream error", f.ErrorStatus))
		return
	}
	var req struct {
		Model         string `json:"model"`
		Stream        bool   `json:"stream"`
		StreamOptions *struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		sw.writeJSON(http.StatusBadRequest, vllmError("invalid json", http.StatusBadRequest))
		return
	}
	if len(f.Served) > 0 && !slices.Contains(f.Served, req.Model) {
		sw.writeJSON(http.StatusNotFound, vllmError(fmt.Sprintf("The model `%s` does not exist.", req.Model), http.StatusNotFound))
		return
	}
	usage := map[string]any{"prompt_tokens": f.PromptTokens, "completion_tokens": f.CompletionTokens,
		"total_tokens": f.PromptTokens + f.CompletionTokens}

	if r.URL.Path == "/v1/embeddings" {
		f.sleep(r)
		b, _ := json.Marshal(map[string]any{"object": "list", "model": req.Model,
			"data":  []map[string]any{{"object": "embedding", "index": 0, "embedding": []float64{0.1, 0.2, 0.3}}},
			"usage": map[string]any{"prompt_tokens": f.PromptTokens, "total_tokens": f.PromptTokens}})
		w.Header().Set("Content-Type", "application/json")
		_ = sw.write(b)
		return
	}
	chat := r.URL.Path == "/v1/chat/completions"
	if !req.Stream {
		f.sleep(r)
		text := strings.Join(f.Chunks, "")
		var choice map[string]any
		object := "text_completion"
		if chat {
			object = "chat.completion"
			choice = map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": text}, "finish_reason": "stop"}
		} else {
			choice = map[string]any{"index": 0, "text": text, "finish_reason": "stop"}
		}
		b, _ := json.Marshal(map[string]any{"id": "cmpl-fake", "object": object, "created": 0, "model": req.Model,
			"choices": []any{choice}, "usage": usage})
		w.Header().Set("Content-Type", "application/json")
		_ = sw.write(b)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
	object := "text_completion"
	if chat {
		object = "chat.completion.chunk"
	}
	event := func(choices []any, extra map[string]any) error {
		m := map[string]any{"id": "cmpl-fake", "object": object, "created": 0, "model": req.Model, "choices": choices}
		for k, v := range extra {
			m[k] = v
		}
		b, _ := json.Marshal(m)
		return sw.write([]byte("data: " + string(b) + "\n\n"))
	}
	choice := func(text string, finish any) map[string]any {
		if chat {
			delta := map[string]any{}
			if text != "" {
				delta["content"] = text
			}
			return map[string]any{"index": 0, "delta": delta, "finish_reason": finish}
		}
		return map[string]any{"index": 0, "text": text, "finish_reason": finish}
	}
	if chat {
		// 角色块：content 为空，不应被网关计数
		if err := event([]any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": ""}, "finish_reason": nil}}, nil); err != nil {
			return
		}
	}
	for i, c := range f.Chunks {
		if !f.sleep(r) {
			return
		}
		if err := event([]any{choice(c, nil)}, nil); err != nil {
			return
		}
		if f.DropAfterChunks > 0 && i+1 >= f.DropAfterChunks {
			panic(http.ErrAbortHandler) // 直接断开连接（不发 chunked 结束块）
		}
	}
	if err := event([]any{choice("", "stop")}, nil); err != nil {
		return
	}
	if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
		if err := event([]any{}, map[string]any{"usage": usage}); err != nil {
			return
		}
	}
	_ = sw.write([]byte("data: [DONE]\n\n"))
}

// sleep 等 Delay；请求被取消返回 false。
func (f *FakeVLLM) sleep(r *http.Request) bool { return sleepFor(r, f.Delay) }

func sleepFor(r *http.Request, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-r.Context().Done():
		return false
	}
}
