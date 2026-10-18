package ingress_test

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/ingress"
	"spark-cloud/internal/rental"
)

// 公网完整中间件栈（HSTS → HostMux → [RealIP] → ingress.Server），与 app.Build 组装的 Handler 相同。
func newFrontEnv(t *testing.T, trustProxy bool) *env {
	console := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("console")) })
	return newEnvFull(t, nil, func(s *ingress.Server) http.Handler { return ingress.Front("spark.test", trustProxy, s, console) })
}

// 经完整栈的 WebSocket 与 SSE 都不受 http.Server 1 秒 WriteTimeout 约束：栈上没有不带 Unwrap 的 ResponseWriter 包装。
func TestFrontStackLongStreamsSurviveWriteTimeout(t *testing.T) {
	for _, trust := range []bool{false, true} {
		e := newFrontEnv(t, trust)
		e.route("ins-a1b2c3d4", 7, 8888, "public", rental.StateRunning, http.HandlerFunc(wsEchoHandler))
		wsRoundTrip(t, e.host, "ins-a1b2c3d4-8888.spark.test", 1200*time.Millisecond)

		e.route("ins-b1b2c3d4", 7, 9000, "public", rental.StateRunning, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			for i := 0; i < 5; i++ {
				fmt.Fprintf(w, "data: %d\n\n", i)
				w.(http.Flusher).Flush()
				time.Sleep(400 * time.Millisecond)
			}
		}))
		resp := e.get("ins-b1b2c3d4-9000.spark.test", "/events")
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || strings.Count(string(b), "data:") != 5 {
			t.Fatalf("trust=%v sse = %q %v", trust, b, err)
		}
		if got := body(e.get("spark.test", "/")); got != "console" {
			t.Fatalf("console via front = %q", got)
		}
	}
}

// 不信任代理：客户端自带的 X-Forwarded-For / X-Real-IP 被忽略；信任代理：取代理写入的 X-Forwarded-For 最左项作为客户端 IP。
func TestFrontStackForwardedFor(t *testing.T) {
	for _, tc := range []struct {
		trust bool
		want  string
	}{{false, "xff=127.0.0.1 "}, {true, "xff=203.0.113.9 "}} {
		e := newFrontEnv(t, tc.trust)
		e.route("ins-a1b2c3d4", 7, 8888, "public", rental.StateRunning, http.HandlerFunc(echo))
		req := e.req("GET", "ins-a1b2c3d4-8888.spark.test", "/")
		req.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
		req.Header.Set("X-Real-IP", "6.6.6.6")
		req.Header.Set("True-Client-IP", "6.6.6.6")
		b := body(e.do(req))
		if !strings.Contains(b, tc.want) || strings.Contains(b, "6.6.6.6") || strings.Contains(b, "10.0.0.1") {
			t.Fatalf("trust=%v forwarded = %q", tc.trust, b)
		}
	}
}

// Shutdown 撤销所有活动流，之后新进来的转发请求返回 503，http.Server.Shutdown 因而不会被长连接拖到超时。
func TestShutdownRevokesStreams(t *testing.T) {
	e := newEnv(t)
	e.route("ins-a1b2c3d4", 7, 8888, "public", rental.StateRunning, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for {
			fmt.Fprint(w, "data: x\n\n")
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}))
	resp := e.get("ins-a1b2c3d4-8888.spark.test", "/events")
	br := bufio.NewReader(resp.Body)
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	e.srv.Shutdown()
	done := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, br); done <- err }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream survived Shutdown")
	}
	resp.Body.Close()
	waitUntil(t, "streams drained", func() bool { return e.srv.ActiveStreams() == 0 })
	r2 := e.get("ins-a1b2c3d4-8888.spark.test", "/events")
	r2.Body.Close()
	if r2.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("new request after Shutdown = %d", r2.StatusCode)
	}
}
