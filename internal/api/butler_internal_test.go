package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/butler"
	"spark-cloud/internal/platform/httpx"
)

// 流建立后 panic：连接被中断，事件流里不能出现 Recoverer 的 500 JSON；流建立前 panic：照常 500 JSON。
func TestStreamButlerPanic(t *testing.T) {
	s := &Server{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), ButlerPing: 5 * time.Millisecond}
	var afterStart bool
	h := httpx.Recoverer(s.Log)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.streamButler(w, r, func(sink butler.Sink) error {
			if afterStart {
				sink.Emit(butler.EvStart, butler.StartEvent{ConversationID: "cv-x"})
				time.Sleep(20 * time.Millisecond) // 让保活协程写几次 ping
			}
			panic("boom")
		})
	}))
	ts := httptest.NewServer(h)
	defer ts.Close()

	afterStart = true
	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "event: start\n") || strings.Contains(string(body), `"code"`) || readErr == nil {
		t.Fatalf("panic after start: %d readErr=%v body=%q", resp.StatusCode, readErr, body)
	}

	afterStart = false
	resp, err = http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 500 || !strings.Contains(string(body), `"code":"internal"`) {
		t.Fatalf("panic before start: %d %q", resp.StatusCode, body)
	}
}

// 客户端在流建立前断开（run 返回 context.Canceled 且请求 ctx 已取消）：什么都不写、不记 500 错误日志。
func TestStreamButlerClientGoneBeforeStart(t *testing.T) {
	var logs strings.Builder
	s := &Server{Log: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelError}))} // httptest.Recorder 不支持清期限（WARN），只看 ERROR
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/butler/conversations/new/messages", nil).WithContext(ctx)
	s.streamButler(rec, req, func(butler.Sink) error { return context.Canceled })
	if rec.Body.Len() != 0 || logs.Len() != 0 {
		t.Fatalf("body=%q logs=%q", rec.Body.String(), logs.String())
	}
}
