package ingress_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/ingress"
	"spark-cloud/internal/rental"
)

// 公网完整中间件栈（HSTS → HostMux → [ProxyRealIP] → ingress.Server），与 app.Build 组装的 Handler 相同。proxyHops 0 = 不信任代理。
func newFrontEnv(t *testing.T, proxyHops int, useTLS bool) *env {
	console := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("console")) })
	return newEnvFull(t, nil, func(s *ingress.Server) http.Handler { return ingress.Front("spark.test", proxyHops, s, console) }, useTLS)
}

// 经完整栈的 WebSocket 与 SSE 都不受 http.Server 1 秒 WriteTimeout 约束：栈上没有不带 Unwrap 的 ResponseWriter 包装。
func TestFrontStackLongStreamsSurviveWriteTimeout(t *testing.T) {
	for _, hops := range []int{0, 1} {
		e := newFrontEnv(t, hops, false)
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
			t.Fatalf("hops=%d sse = %q %v", hops, b, err)
		}
		if got := body(e.get("spark.test", "/")); got != "console" {
			t.Fatalf("console via front = %q", got)
		}
	}
}

// TLS 监听器（与生产一样协商 ALPN，WebSocket 客户端走 http/1.1）上的 WebSocket 经完整栈同样不受 WriteTimeout 约束，HSTS 只加在 TLS 上。
func TestFrontStackWebSocketOverTLS(t *testing.T) {
	e := newFrontEnv(t, 0, true)
	e.route("ins-a1b2c3d4", 7, 8888, "public", rental.StateRunning, http.HandlerFunc(wsEchoHandler))
	nc, err := tls.Dial("tcp", e.host, &tls.Config{InsecureSkipVerify: true, ServerName: "ins-a1b2c3d4-8888.spark.test", NextProtos: []string{"http/1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	wsRoundTripConn(t, nc, "ins-a1b2c3d4-8888.spark.test", 1200*time.Millisecond)

	e.route("ins-b1b2c3d4", 7, 9000, "public", rental.StateRunning, http.HandlerFunc(echo))
	req := e.req("GET", "ins-b1b2c3d4-9000.spark.test", "/")
	req.URL.Scheme = "https"
	resp, err := e.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Strict-Transport-Security") != "max-age=31536000" {
		t.Fatalf("https via front = %d hsts=%q", resp.StatusCode, resp.Header.Get("Strict-Transport-Security"))
	}
}

// 不信任代理：客户端自带的 X-Forwarded-For 被忽略；信任代理：取 X-Forwarded-For 自右数第 hops 项。伪造的 X-Real-IP / True-Client-IP 都不起作用、也不转给租户。
func TestFrontStackForwardedFor(t *testing.T) {
	for _, tc := range []struct {
		hops int
		xff  string
		want string
	}{
		{0, "6.6.6.6, 203.0.113.9", "xff=127.0.0.1 "},
		{1, "6.6.6.6, 203.0.113.9", "xff=203.0.113.9 "},
		{2, "6.6.6.6, 203.0.113.9, 10.0.0.1", "xff=203.0.113.9 "},
		{1, "6.6.6.6, bogus", "xff=127.0.0.1 "},
	} {
		e := newFrontEnv(t, tc.hops, false)
		e.route("ins-a1b2c3d4", 7, 8888, "public", rental.StateRunning, http.HandlerFunc(echo))
		req := e.req("GET", "ins-a1b2c3d4-8888.spark.test", "/")
		req.Header.Set("X-Forwarded-For", tc.xff)
		req.Header.Set("X-Real-IP", "6.6.6.6")
		req.Header.Set("True-Client-IP", "6.6.6.6")
		b := body(e.do(req))
		if !strings.Contains(b, tc.want) || strings.Contains(b, "6.6.6.6") || strings.Contains(b, "10.0.0.1") {
			t.Fatalf("hops=%d xff=%q forwarded = %q", tc.hops, tc.xff, b)
		}
	}
}

// Shutdown：SSE / WebSocket 立即撤销；进行中的普通请求在宽限期内照常完成；超过宽限期的被撤销；之后的转发请求返回 503。
func TestShutdownGraceful(t *testing.T) {
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
	e.route("ins-b1b2c3d4", 7, 9000, "public", rental.StateRunning, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d, _ := time.ParseDuration(r.URL.Query().Get("sleep"))
		select {
		case <-time.After(d):
			w.Write([]byte("done"))
		case <-r.Context().Done():
		}
	}))
	e.route("ins-c1b2c3d4", 7, 9001, "public", rental.StateRunning, http.HandlerFunc(wsEchoHandler))

	resp := e.get("ins-a1b2c3d4-8888.spark.test", "/events")
	br := bufio.NewReader(resp.Body)
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	wsConn, wsBR := openWS(t, e, "ins-c1b2c3d4-9001.spark.test")
	type result struct {
		code int
		body string
		err  error
	}
	slow := func(sleep string) chan result {
		ch := make(chan result, 1)
		go func() {
			req := e.req("GET", "ins-b1b2c3d4-9000.spark.test", "/?sleep="+sleep)
			r, err := http.DefaultClient.Do(req)
			if err != nil {
				ch <- result{err: err}
				return
			}
			b, err := io.ReadAll(r.Body)
			r.Body.Close()
			ch <- result{r.StatusCode, string(b), err}
		}()
		return ch
	}
	short, long := slow("600ms"), slow("30s")
	waitUntil(t, "requests in flight", func() bool { return e.srv.ActiveStreams() == 4 })

	start := time.Now()
	done := make(chan struct{})
	go func() { e.srv.Shutdown(context.Background(), 1500*time.Millisecond); close(done) }()

	sseDone := make(chan struct{})
	go func() { io.Copy(io.Discard, br); close(sseDone) }()
	select {
	case <-sseDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("sse not revoked immediately on Shutdown")
	}
	expectClosed(t, wsConn, wsBR, 500*time.Millisecond)
	if r := <-short; r.err != nil || r.code != 200 || r.body != "done" {
		t.Fatalf("short request during grace = %+v", r)
	}
	if after := e.get("ins-b1b2c3d4-9000.spark.test", "/?sleep=1ms"); body(after) == "" || after.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("new request after Shutdown = %d", after.StatusCode)
	}
	r := <-long
	if el := time.Since(start); el < 1400*time.Millisecond || el > 3*time.Second || r.body == "done" {
		t.Fatalf("long request after grace: %+v after %v", r, el)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not return")
	}
}
