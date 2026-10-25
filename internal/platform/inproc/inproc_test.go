package inproc_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"spark-cloud/internal/platform/inproc"
)

const wait = 5 * time.Second

func client(h http.Handler) *http.Client {
	return &http.Client{Transport: &inproc.Transport{Handler: h, Host: "spark.test", Log: slog.New(slog.DiscardHandler)}}
}

func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(wait):
		t.Fatalf("timeout waiting for %s", what)
	}
	var zero T
	return zero
}

// 流式：处理器写一段 + Flush 后阻塞，调用方在处理器返回之前就读到这一段；请求字段与 net/http 服务端一致。
func TestStreamsIncrementally(t *testing.T) {
	release := make(chan struct{})
	finished := make(chan struct{})
	var got atomic.Value
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)
		b, _ := io.ReadAll(r.Body)
		got.Store([]string{r.Method, r.Host, r.RemoteAddr, r.URL.Path, r.URL.RawQuery, r.RequestURI, r.Header.Get("Authorization"), string(b), r.Proto})
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: one\n\n")
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush: %v", err)
		}
		<-release
		_, _ = io.WriteString(w, "data: two\n\n")
		w.Header().Set("X-Late", "1") // 写响应头之后改 Header 不影响已返回的响应
	})
	req, _ := http.NewRequest(http.MethodPost, "http://llmgw.internal/v1/chat/completions?x=1", strings.NewReader(`{"a":1}`))
	req.Header.Set("Authorization", "Bearer k")
	resp, err := client(h).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" || resp.ContentLength != -1 {
		t.Fatalf("resp = %d %v %d", resp.StatusCode, resp.Header, resp.ContentLength)
	}
	br := bufio.NewReader(resp.Body)
	lines := make(chan string, 4)
	go func() {
		for {
			l, err := br.ReadString('\n')
			if err != nil {
				close(lines)
				return
			}
			if l != "\n" {
				lines <- l
			}
		}
	}()
	if l := recv(t, lines, "first chunk"); l != "data: one\n" {
		t.Fatalf("first = %q", l)
	}
	select {
	case <-finished:
		t.Fatal("handler finished before the test released it")
	default:
	}
	close(release)
	if l := recv(t, lines, "second chunk"); l != "data: two\n" {
		t.Fatalf("second = %q", l)
	}
	if _, ok := <-lines; ok {
		t.Fatal("want EOF after handler returned")
	}
	recv(t, finished, "handler return")
	want := []string{"POST", "spark.test", "127.0.0.1:0", "/v1/chat/completions", "x=1", "/v1/chat/completions?x=1", "Bearer k", `{"a":1}`, "HTTP/1.1"}
	if g := got.Load().([]string); strings.Join(g, "|") != strings.Join(want, "|") {
		t.Fatalf("request = %q, want %q", g, want)
	}
	if resp.Header.Get("X-Late") != "" {
		t.Fatal("header changed after WriteHeader leaked into the response")
	}
}

// 调用方取消 ctx：处理器的请求 ctx 随之取消、处理器返回；调用方的读返回错误。
func TestCancelStopsHandler(t *testing.T) {
	stopped := make(chan error, 1)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "data: one\n\n")
		<-r.Context().Done()
		_, err := io.WriteString(w, "data: late\n\n")
		if err == nil {
			err = errors.New("write after cancel succeeded")
		} else {
			err = nil
		}
		stopped <- errors.Join(err, r.Context().Err())
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://x/stream", nil)
	resp, err := client(h).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 64)
	n, err := resp.Body.Read(buf)
	if err != nil || string(buf[:n]) != "data: one\n\n" {
		t.Fatalf("read = %q, %v", buf[:n], err)
	}
	cancel()
	if err := recv(t, stopped, "handler stop"); !errors.Is(err, context.Canceled) {
		t.Fatalf("handler saw %v, want only context.Canceled", err)
	}
	if _, err := io.ReadAll(resp.Body); !errors.Is(err, context.Canceled) {
		t.Fatalf("read after cancel = %v", err)
	}
}

// 响应头之前取消：RoundTrip 立即返回 ctx 错误，不等处理器。
func TestCancelBeforeHeader(t *testing.T) {
	stopped := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(stopped)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://x/", nil)
	start := time.Now()
	if _, err := client(h).Do(req); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("RoundTrip did not return promptly")
	}
	recv(t, stopped, "handler stop")
}

// 调用方提前关闭响应体 = 客户端断开：处理器 ctx 取消、之后的 Write 失败；请求体在处理器返回后被关闭。
func TestBodyCloseAborts(t *testing.T) {
	result := make(chan error, 1)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "x")
		for {
			if _, err := io.WriteString(w, "y"); err != nil {
				<-r.Context().Done()
				result <- r.Context().Err()
				return
			}
		}
	})
	rb := &closeTracker{Reader: strings.NewReader("body")}
	req, _ := http.NewRequest(http.MethodPost, "http://x/", rb)
	resp, err := client(h).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 1)
	if _, err := resp.Body.Read(b); err != nil || b[0] != 'x' {
		t.Fatalf("read = %q %v", b, err)
	}
	resp.Body.Close()
	if err := recv(t, result, "handler abort"); !errors.Is(err, context.Canceled) {
		t.Fatalf("handler ctx = %v", err)
	}
	deadline := time.Now().Add(wait)
	for !rb.closed.Load() {
		if time.Now().After(deadline) {
			t.Fatal("request body not closed")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type closeTracker struct {
	io.Reader
	closed atomic.Bool
}

func (c *closeTracker) Close() error { c.closed.Store(true); return nil }

// 不写响应头直接写 / 什么都不写：隐式 200；状态码与非流式响应体照常；读写期限不支持（网关只记调试日志）。
func TestImplicitHeaderAndStatus(t *testing.T) {
	deadlineErr := make(chan error, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/empty", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("/text", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "hello") })
	mux.HandleFunc("/err", func(w http.ResponseWriter, r *http.Request) {
		deadlineErr <- http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Second))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		w.WriteHeader(http.StatusOK) // 重复调用忽略
		_, _ = io.WriteString(w, `{"error":{"code":"insufficient_quota"}}`)
	})
	c := client(mux)
	for _, tc := range []struct {
		path, ct, body string
		status         int
	}{
		{"/empty", "", "", 200},
		{"/text", "text/plain; charset=utf-8", "hello", 200},
		{"/err", "application/json", `{"error":{"code":"insufficient_quota"}}`, 402},
	} {
		resp, err := c.Get("http://x" + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != tc.status || resp.Header.Get("Content-Type") != tc.ct || string(b) != tc.body ||
			resp.Status != fmt.Sprintf("%d %s", tc.status, http.StatusText(tc.status)) {
			t.Fatalf("%s: %d %q %q %v", tc.path, resp.StatusCode, resp.Header.Get("Content-Type"), b, err)
		}
	}
	if err := recv(t, deadlineErr, "deadline result"); !errors.Is(err, http.ErrNotSupported) {
		t.Fatalf("SetWriteDeadline = %v, want ErrNotSupported", err)
	}
}

// 处理器 panic 不会让进程崩溃：响应头之前 → RoundTrip 报错；之后 → 读响应体报错。
func TestHandlerPanic(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/early", func(http.ResponseWriter, *http.Request) { panic("boom") })
	mux.HandleFunc("/late", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "partial")
		panic(http.ErrAbortHandler)
	})
	c := client(mux)
	if _, err := c.Get("http://x/early"); !errors.Is(err, inproc.ErrHandlerPanic) {
		t.Fatalf("early panic err = %v", err)
	}
	resp, err := c.Get("http://x/late")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if string(b) != "partial" || !errors.Is(err, inproc.ErrHandlerPanic) {
		t.Fatalf("late panic = %q %v", b, err)
	}
}

func TestDefaults(t *testing.T) {
	var host, remote string
	h := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { host, remote = r.Host, r.RemoteAddr })
	c := &http.Client{Transport: &inproc.Transport{Handler: h}}
	resp, err := c.Get("http://llmgw.internal/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if host != "llmgw.internal" || remote != inproc.DefaultRemoteAddr {
		t.Fatalf("host %q remote %q", host, remote)
	}
	if _, err := (&http.Client{Transport: &inproc.Transport{}}).Get("http://x/"); err == nil {
		t.Fatal("nil handler should fail")
	}
}
