package ingress_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/rental"
)

func TestUpstreamSetCookieMadeHostOnly(t *testing.T) {
	e := newEnv(t)
	e.route("ins-a1b2c3d4", 7, 8888, "public", rental.StateRunning, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		h := w.Header()
		h.Add("Set-Cookie", "a=1; Domain=spark.test; Path=/; HttpOnly")
		h.Add("Set-Cookie", "b=2; Path=/x; domain=.spark.test; Secure; SameSite=Strict")
		h.Add("Set-Cookie", "c=3; DOMAIN=ins-a1b2c3d4-8888.spark.test; Max-Age=60")
		h.Add("Set-Cookie", "sc_ingress=forged; Path=/; HttpOnly")
		h.Add("Set-Cookie", " sc_ingress =x")
		h.Add("Set-Cookie", "plain=keep; Path=/; Max-Age=3600; HttpOnly; Secure; SameSite=Lax")
		h.Add("Set-Cookie", "bad cookie value; DoMaIn = spark.test; Path=/")
		h.Add("Set-Cookie", "sc_ingress_x=ok; Path=/")
		fmt.Fprint(w, "ok")
	}))
	resp := e.get("ins-a1b2c3d4-8888.spark.test", "/")
	body(resp)
	got := resp.Header.Values("Set-Cookie")
	want := []string{
		"a=1; Path=/; HttpOnly",
		"b=2; Path=/x; Secure; SameSite=Strict",
		"c=3; Max-Age=60",
		"plain=keep; Path=/; Max-Age=3600; HttpOnly; Secure; SameSite=Lax",
		"bad cookie value; Path=/",
		"sc_ingress_x=ok; Path=/",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("set-cookie =\n%q\nwant\n%q", got, want)
	}
}

func TestPrivatePortWriteOriginFallback(t *testing.T) {
	e := newEnv(t)
	e.route("ins-a1b2c3d4", 7, 8888, "private", rental.StateRunning, http.HandlerFunc(echo))
	h := "ins-a1b2c3d4-8888.spark.test"
	self := "http://" + h + ":" + e.auth.BaseURL.Port()
	ck := e.cookie("ins-a1b2c3d4", 8888, 7, "sess-7")
	for _, tc := range []struct {
		method, sfs, origin string
		want                int
	}{
		{"POST", "", "http://ins-evil0000-80.spark.test:" + e.auth.BaseURL.Port(), 403},
		{"POST", "", "https://evil.example", 403},
		{"POST", "", "null", 403},
		{"DELETE", "", "http://spark.test:" + e.auth.BaseURL.Port(), 403},
		{"POST", "", self, 200},
		{"POST", "", "", 200},
		{"POST", "same-origin", "https://evil.example", 200}, // 有 Sec-Fetch-Site 时以它为准
		{"GET", "", "https://evil.example", 200},
	} {
		req := e.req(tc.method, h, "/api", ck)
		if tc.sfs != "" {
			req.Header.Set("Sec-Fetch-Site", tc.sfs)
		}
		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}
		r := e.do(req)
		if b := body(r); r.StatusCode != tc.want || (tc.want == 403 && strings.Contains(b, "method=")) {
			t.Fatalf("%s sfs=%q origin=%q = %d", tc.method, tc.sfs, tc.origin, r.StatusCode)
		}
	}
}

func TestPrivatePortSafeMethodsWithoutCookieRedirect(t *testing.T) {
	e := newEnv(t)
	e.route("ins-a1b2c3d4", 7, 8888, "private", rental.StateRunning, http.HandlerFunc(echo))
	for _, m := range []string{"OPTIONS", "HEAD"} {
		r := e.do(e.req(m, "ins-a1b2c3d4-8888.spark.test", "/x"))
		r.Body.Close()
		if r.StatusCode != 302 || !strings.Contains(r.Header.Get("Location"), "/api/v1/ingress/authorize") {
			t.Fatalf("%s without cookie = %d %s", m, r.StatusCode, r.Header.Get("Location"))
		}
	}
	if n := e.dialer.dialCount(); n != 0 {
		t.Fatalf("upstream dialed %d times", n)
	}
}

// startSweeper 以较短间隔运行撤销扫描（生产用 ≤ 10 秒）。
func startSweeper(t *testing.T, e *env) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.srv.Run(ctx, 50*time.Millisecond); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

func (e *env) setRoute(id string, port int, mut func(*rental.IngressRoute)) {
	e.t.Helper()
	e.src.mu.Lock()
	out := e.src.routes[:0]
	for _, r := range e.src.routes {
		if r.InstanceID == id && r.Port == port {
			if mut == nil {
				continue // 删除
			}
			mut(&r)
		}
		out = append(out, r)
	}
	e.src.routes = out
	e.src.mu.Unlock()
	if err := e.table.Refresh(context.Background()); err != nil {
		e.t.Fatal(err)
	}
}

// openWS 经 ingress 建立 WebSocket 并确认回显可用。
func openWS(t *testing.T, e *env, host string, cookies ...*http.Cookie) (net.Conn, *bufio.Reader) {
	t.Helper()
	nc, err := net.Dial("tcp", e.host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nc.Close() })
	var ck string
	for _, c := range cookies {
		ck += "Cookie: " + c.String() + "\r\n"
	}
	fmt.Fprintf(nc, "GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n%s\r\n", host, ck)
	br := bufio.NewReader(nc)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 101 {
		t.Fatalf("upgrade = %v %v", resp, err)
	}
	wsEcho(t, nc, br, "ping")
	return nc, br
}

func wsEcho(t *testing.T, nc net.Conn, br *bufio.Reader, msg string) {
	t.Helper()
	fmt.Fprint(nc, msg)
	buf := make([]byte, len(msg))
	nc.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != msg {
		t.Fatalf("echo = %q %v", buf, err)
	}
}

// expectClosed：连接在 bound 内被服务端关闭（读到 EOF / 连接重置，而不是超时）。
func expectClosed(t *testing.T, nc net.Conn, br *bufio.Reader, bound time.Duration) {
	t.Helper()
	nc.SetReadDeadline(time.Now().Add(bound))
	_, err := br.ReadByte()
	if err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("connection not closed within %v: %v", bound, err)
	}
}

func TestWebSocketRevokedWhenAccessChanges(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*rental.IngressRoute) // nil = 删除端口
	}{
		{"port turned private", func(r *rental.IngressRoute) { r.Visibility = "private" }},
		{"port deleted", nil},
		{"instance stopped", func(r *rental.IngressRoute) { r.State = rental.StateStopped }},
		{"instance suspended", func(r *rental.IngressRoute) { r.Suspended = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			startSweeper(t, e)
			e.route("ins-a1b2c3d4", 7, 8888, "public", rental.StateRunning, http.HandlerFunc(wsEchoHandler))
			nc, br := openWS(t, e, "ins-a1b2c3d4-8888.spark.test")
			time.Sleep(200 * time.Millisecond) // 几轮扫描：权限没变时不断开
			wsEcho(t, nc, br, "still-open")
			e.setRoute("ins-a1b2c3d4", 8888, tc.mut)
			expectClosed(t, nc, br, time.Second)
			waitUntil(t, "stream unregistered", func() bool { return e.srv.ActiveStreams() == 0 })
		})
	}
}

func TestAuthenticatedWebSocketSurvivesPublicFlip(t *testing.T) {
	e := newEnv(t)
	startSweeper(t, e)
	e.route("ins-a1b2c3d4", 7, 8888, "private", rental.StateRunning, http.HandlerFunc(wsEchoHandler))
	nc, br := openWS(t, e, "ins-a1b2c3d4-8888.spark.test", e.cookie("ins-a1b2c3d4", 8888, 7, "sess-7"))
	// 私有 → 公开：已认证的连接照常
	e.setRoute("ins-a1b2c3d4", 8888, func(r *rental.IngressRoute) { r.Visibility = "public" })
	time.Sleep(200 * time.Millisecond)
	wsEcho(t, nc, br, "after-public")
	// 主人变了（换租户不会发生，但规则如此）：断开
	e.setRoute("ins-a1b2c3d4", 8888, func(r *rental.IngressRoute) { r.Visibility, r.UserID = "private", 8 })
	expectClosed(t, nc, br, time.Second)
}

func TestSSERevokedOnConsoleLogout(t *testing.T) {
	e := newEnv(t)
	startSweeper(t, e)
	upstreamDone := make(chan struct{})
	e.route("ins-a1b2c3d4", 7, 8888, "private", rental.StateRunning, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamDone)
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; ; i++ {
			fmt.Fprintf(w, "data: %d\n\n", i)
			fl.Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}))
	resp := e.get("ins-a1b2c3d4-8888.spark.test", "/events", e.cookie("ins-a1b2c3d4", 8888, 7, "sess-7"))
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("sse = %d", resp.StatusCode)
	}
	br := bufio.NewReader(resp.Body)
	readEvent := func() error {
		_, err := br.ReadString('\n')
		return err
	}
	if err := readEvent(); err != nil {
		t.Fatal(err)
	}
	// 会话查询故障（数据库抖动）：不撤销
	e.sess.mu.Lock()
	e.sess.fail = true
	e.sess.mu.Unlock()
	e.clk.Advance(3 * time.Second)
	time.Sleep(300 * time.Millisecond)
	for i := 0; i < 4; i++ {
		if err := readEvent(); err != nil {
			t.Fatalf("stream cut on transient session error: %v", err)
		}
	}
	// 控制台登出：会话失效 → 缓存过期后撤销
	e.sess.mu.Lock()
	e.sess.fail = false
	delete(e.sess.users, "sess-7")
	e.sess.mu.Unlock()
	e.clk.Advance(3 * time.Second)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := readEvent(); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sse not revoked after logout")
		}
	}
	select {
	case <-upstreamDone:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request not cancelled")
	}
}
