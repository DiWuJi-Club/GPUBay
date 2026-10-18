package ingress_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/access/upstream"
	"spark-cloud/internal/ingress"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/rental"
)

type fakeDialer struct {
	mu      sync.Mutex
	targets map[string]string        // instance → httptest 地址
	errs    map[string]error         // instance → 固定返回的错误
	block   map[string]chan struct{} // instance → 拨号挂起直到关闭，然后返回 DialError
	dials   []string
	done    int // 已返回的拨号次数
}

func (d *fakeDialer) Dial(_ context.Context, t upstream.Target, port int) (net.Conn, error) {
	d.mu.Lock()
	addr, ok := d.targets[t.InstanceID]
	err := d.errs[t.InstanceID]
	blk := d.block[t.InstanceID]
	d.dials = append(d.dials, fmt.Sprintf("%s:%d", t.InstanceID, port))
	d.mu.Unlock()
	defer func() { d.mu.Lock(); d.done++; d.mu.Unlock() }()
	if blk != nil {
		<-blk
		return nil, &upstream.DialError{InstanceID: t.InstanceID, Err: errors.New("dial tcp: i/o timeout")}
	}
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, &upstream.DialError{InstanceID: t.InstanceID, Err: errors.New("dial tcp: connection refused")}
	}
	return net.Dial("tcp", addr)
}

func (d *fakeDialer) dialCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.dials)
}

type fakeSessions struct {
	mu    sync.Mutex
	users map[string]int64
	calls int
	fail  bool // 模拟数据库故障
	hang  bool // 模拟数据库卡死
}

func (f *fakeSessions) SessionUserID(ctx context.Context, hash string) (int64, error) {
	f.mu.Lock()
	f.calls++
	hang := f.hang
	f.mu.Unlock()
	if hang { // 模拟数据库卡死：直到 ctx 结束
		<-ctx.Done()
		return 0, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return 0, errors.New("db down")
	}
	if id, ok := f.users[hash]; ok {
		return id, nil
	}
	return 0, apperr.ErrUnauthorized
}

type fakeAlerts struct {
	mu    sync.Mutex
	raise []string
}

func (f *fakeAlerts) RaiseUpstreamUnreachable(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.raise = append(f.raise, id)
	return nil
}

func (f *fakeAlerts) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.raise)
}

type env struct {
	t      *testing.T
	clk    *clock.Fake
	auth   *ingress.Auth
	src    *memSource
	table  *ingress.Table
	dialer *fakeDialer
	sess   *fakeSessions
	alerts *fakeAlerts
	logs   *syncBuf
	ts     *httptest.Server
	srv    *ingress.Server
	host   string // ingress 监听的 host:port
}

// newEnvWith 用给定 Dialer 起一个 ingress（nil → fakeDialer）。
func newEnvWith(t *testing.T, dialer ingress.Dialer) *env { return newEnvFull(t, dialer, nil) }

// newEnvFull 同 newEnvWith；wrap 非 nil 时 http.Server 的 Handler 是 wrap(ingress.Server)（例如公网完整中间件栈）。
func newEnvFull(t *testing.T, dialer ingress.Dialer, wrap func(*ingress.Server) http.Handler) *env {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	ts := httptest.NewUnstartedServer(nil)
	ts.Config.WriteTimeout = time.Second
	ts.Config.ReadTimeout = 2 * time.Second
	host := ts.Listener.Addr().String()
	_, port, _ := net.SplitHostPort(host)
	auth, err := ingress.NewAuth(secret, "http://spark.test:"+port, "spark.test", false, clk)
	if err != nil {
		t.Fatal(err)
	}
	src := &memSource{}
	table := ingress.NewTable(src, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e := &env{t: t, clk: clk, auth: auth, src: src, table: table, dialer: &fakeDialer{targets: map[string]string{}, errs: map[string]error{}, block: map[string]chan struct{}{}},
		sess: &fakeSessions{users: map[string]int64{"sess-7": 7, "sess-8": 8}}, alerts: &fakeAlerts{}, logs: &syncBuf{}, ts: ts, host: host}
	if dialer == nil {
		dialer = e.dialer
	}
	log := slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e.srv = ingress.NewServer(auth, table, e.sess, dialer, e.alerts, clk, log)
	ts.Config.Handler = e.srv
	if wrap != nil {
		ts.Config.Handler = wrap(e.srv)
	}
	ts.Start()
	t.Cleanup(ts.Close)
	return e
}

func newEnv(t *testing.T) *env { return newEnvWith(t, nil) }

// addRoute 登记一条路由并刷新路由表。
func (e *env) addRoute(r rental.IngressRoute) {
	e.t.Helper()
	e.src.mu.Lock()
	e.src.routes = append(e.src.routes, r)
	e.src.mu.Unlock()
	if err := e.table.Refresh(context.Background()); err != nil {
		e.t.Fatal(err)
	}
}

// route 登记一个实例端口并指向 handler。
func (e *env) route(id string, userID int64, port int, visibility, state string, h http.Handler) {
	e.t.Helper()
	up := httptest.NewServer(h)
	e.t.Cleanup(up.Close)
	e.dialer.mu.Lock()
	e.dialer.targets[id] = strings.TrimPrefix(up.URL, "http://")
	e.dialer.mu.Unlock()
	e.addRoute(rental.IngressRoute{InstanceID: id, UserID: userID, Port: port, Visibility: visibility, State: state, LanIP: "127.0.0.1", HostPubkey: "k"})
}

func (e *env) do(req *http.Request) *http.Response {
	e.t.Helper()
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp
}

func (e *env) req(method, hostHeader, path string, cookies ...*http.Cookie) *http.Request {
	req, _ := http.NewRequest(method, "http://"+e.host+path, nil)
	req.Host = hostHeader
	for _, c := range cookies {
		req.AddCookie(c)
	}
	return req
}

// get 以给定 Host 头请求 ingress，不跟随重定向。
func (e *env) get(hostHeader, path string, cookies ...*http.Cookie) *http.Response {
	e.t.Helper()
	return e.do(e.req("GET", hostHeader, path, cookies...))
}

func body(resp *http.Response) string {
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func (e *env) cookie(id string, port int, userID int64, sess string) *http.Cookie {
	return &http.Cookie{Name: ingress.CookieName, Value: e.auth.Signer.Cookie(ingress.Claims{InstanceID: id, Port: port, UserID: userID, SessionHash: sess})}
}

func echo(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "method=%s host=%s path=%s cookie=%s xff=%s xfp=%s xfh=%s xport=%s",
		r.Method, r.Host, r.URL.RequestURI(), r.Header.Get("Cookie"), strings.Join(r.Header.Values("X-Forwarded-For"), ","),
		r.Header.Get("X-Forwarded-Proto"), r.Header.Get("X-Forwarded-Host"), r.Header.Get("X-Forwarded-Port"))
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPublicPortProxiesAndStripsIngressCookie(t *testing.T) {
	e := newEnv(t)
	e.route("ins-a1b2c3d4", 7, 8888, "public", rental.StateRunning, http.HandlerFunc(echo))
	h := "ins-a1b2c3d4-8888.spark.test"
	resp := e.get(h, "/lab?x=1", &http.Cookie{Name: "sc_ingress", Value: "secret"}, &http.Cookie{Name: "app", Value: "keep"}, &http.Cookie{Name: "sc_ingress", Value: "secret2"}, &http.Cookie{Name: "sc_ingress_x", Value: "other"})
	b := body(resp)
	if resp.StatusCode != 200 || !strings.Contains(b, "host="+h) || !strings.Contains(b, "path=/lab?x=1") || !strings.Contains(b, "cookie=app=keep; sc_ingress_x=other ") || strings.Contains(b, "secret") || !strings.Contains(b, "xff=127.0.0.1 ") {
		t.Fatalf("proxied = %d %q", resp.StatusCode, b)
	}
	if r := e.get("INS-A1B2C3D4-8888.SPARK.TEST.", "/"); r.StatusCode != 200 {
		t.Fatalf("normalized host = %d", r.StatusCode)
	}
	if r := e.get("ins-a1b2c3d4-9999.spark.test", "/"); r.StatusCode != 404 || !strings.Contains(body(r), ingress.PageNotOpen) {
		t.Fatalf("unknown port = %d", r.StatusCode)
	}
	if r := e.get("other.test", "/"); r.StatusCode != 404 {
		t.Fatalf("unknown host = %d", r.StatusCode)
	}
	if r := e.get(h, "/.spark/healthz"); r.StatusCode != 200 || body(r) != "ok" {
		t.Fatal("healthz")
	}
	if r := e.get(h, "/.spark/unknown"); r.StatusCode != 404 {
		t.Fatal("reserved path leaked to tenant")
	}
	// 只有 Cookie 头里只有 sc_ingress 时：转发请求不带 Cookie 头
	if b := body(e.get(h, "/", &http.Cookie{Name: "sc_ingress", Value: "secret"})); !strings.Contains(b, "cookie= ") {
		t.Fatalf("only-ingress cookie = %q", b)
	}
}

func TestForwardedHeadersReplaceClientSupplied(t *testing.T) {
	e := newEnv(t)
	e.route("ins-a1b2c3d4", 7, 8888, "public", rental.StateRunning, http.HandlerFunc(echo))
	h := "ins-a1b2c3d4-8888.spark.test"
	req := e.req("GET", h, "/")
	req.Header.Set("X-Forwarded-For", "6.6.6.6")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "evil.test")
	req.Header.Set("X-Forwarded-Port", "443")
	req.Header.Set("Forwarded", "for=6.6.6.6")
	b := body(e.do(req))
	if !strings.Contains(b, "xff=127.0.0.1 ") || !strings.Contains(b, "xfp=http ") || !strings.Contains(b, "xfh="+h+" ") || !strings.Contains(b, "xport=") || strings.Contains(b, "6.6.6.6") || strings.Contains(b, "evil") || strings.Contains(b, "xport=443") {
		t.Fatalf("forwarded headers = %q", b)
	}
}

func TestPrivatePortRequiresCookieBoundToOwnerAndSession(t *testing.T) {
	e := newEnv(t)
	e.route("ins-a1b2c3d4", 7, 8888, "private", rental.StateRunning, http.HandlerFunc(echo))
	h := "ins-a1b2c3d4-8888.spark.test"
	resp := e.get(h, "/lab?x=1")
	loc := resp.Header.Get("Location")
	if resp.StatusCode != 302 || loc != e.auth.AuthorizeURL("http://"+h+":"+e.auth.BaseURL.Port()+"/lab?x=1") {
		t.Fatalf("no cookie = %d %s", resp.StatusCode, loc)
	}
	if e.dialer.dialCount() != 0 {
		t.Fatal("unauthenticated request reached upstream")
	}
	if r := e.get(h, "/lab", e.cookie("ins-a1b2c3d4", 8888, 7, "sess-7")); r.StatusCode != 200 {
		t.Fatalf("owner cookie = %d", r.StatusCode)
	}
	if r := e.get(h, "/lab", e.cookie("ins-a1b2c3d4", 8888, 8, "sess-8")); r.StatusCode != 302 {
		t.Fatalf("other user's cookie = %d", r.StatusCode)
	}
	if r := e.get(h, "/lab", e.cookie("ins-a1b2c3d4", 7860, 7, "sess-7")); r.StatusCode != 302 {
		t.Fatalf("cookie for other port = %d", r.StatusCode)
	}
	if r := e.get(h, "/lab", &http.Cookie{Name: ingress.CookieName, Value: "junk"}, e.cookie("ins-a1b2c3d4", 8888, 7, "sess-7")); r.StatusCode != 200 {
		t.Fatalf("second cookie ignored = %d", r.StatusCode)
	}
	// 一枚验签通过但会话已失效的 Cookie 不能遮住后面那枚有效的（逐枚验证）
	if r := e.get(h, "/lab", e.cookie("ins-a1b2c3d4", 8888, 7, "sess-gone"), e.cookie("ins-a1b2c3d4", 8888, 7, "sess-7")); r.StatusCode != 200 {
		t.Fatalf("stale-session cookie shadowed valid one = %d", r.StatusCode)
	}
	// 控制台登出：会话失效 → 2 秒缓存过后 302
	e.sess.mu.Lock()
	delete(e.sess.users, "sess-7")
	e.sess.mu.Unlock()
	if r := e.get(h, "/lab", e.cookie("ins-a1b2c3d4", 8888, 7, "sess-7")); r.StatusCode != 200 {
		t.Fatalf("within cache window = %d", r.StatusCode)
	}
	e.clk.Advance(3 * time.Second)
	if r := e.get(h, "/lab", e.cookie("ins-a1b2c3d4", 8888, 7, "sess-7")); r.StatusCode != 302 {
		t.Fatalf("after logout = %d", r.StatusCode)
	}
	// 会话查询有缓存：2 秒内同一会话只查一次
	e.sess.mu.Lock()
	e.sess.users["sess-7"] = 7
	before := e.sess.calls
	e.sess.mu.Unlock()
	e.clk.Advance(3 * time.Second)
	for i := 0; i < 3; i++ {
		if r := e.get(h, "/lab", e.cookie("ins-a1b2c3d4", 8888, 7, "sess-7")); r.StatusCode != 200 {
			t.Fatalf("re-login %d = %d", i, r.StatusCode)
		}
	}
	e.sess.mu.Lock()
	calls := e.sess.calls - before
	e.sess.mu.Unlock()
	if calls != 1 {
		t.Fatalf("session lookups within cache window = %d", calls)
	}
}

func TestPrivatePortRejectsCrossSiteWrites(t *testing.T) {
	e := newEnv(t)
	e.route("ins-a1b2c3d4", 7, 8888, "private", rental.StateRunning, http.HandlerFunc(echo))
	e.route("ins-b2c3d4e5", 7, 80, "public", rental.StateRunning, http.HandlerFunc(echo))
	priv, pub := "ins-a1b2c3d4-8888.spark.test", "ins-b2c3d4e5-80.spark.test"
	ck := e.cookie("ins-a1b2c3d4", 8888, 7, "sess-7")
	for _, tc := range []struct {
		method, host, sfs string
		want              int
	}{
		{"POST", priv, "same-site", 403}, // 兄弟租户页面发起的写请求
		{"POST", priv, "cross-site", 403},
		{"DELETE", priv, "same-site", 403},
		{"PUT", priv, "cross-site", 403},
		{"POST", priv, "same-origin", 200},
		{"POST", priv, "none", 200},
		{"POST", priv, "", 200}, // 非浏览器客户端没有该头
		{"GET", priv, "same-site", 200},
		{"HEAD", priv, "cross-site", 200},
		{"OPTIONS", priv, "cross-site", 200},
		{"POST", pub, "cross-site", 200}, // 公开端口不管
	} {
		req := e.req(tc.method, tc.host, "/api")
		if tc.host == priv {
			req.AddCookie(ck)
		}
		if tc.sfs != "" {
			req.Header.Set("Sec-Fetch-Site", tc.sfs)
		}
		resp := e.do(req)
		b := body(resp)
		if resp.StatusCode != tc.want {
			t.Fatalf("%s %s sfs=%q = %d %q", tc.method, tc.host, tc.sfs, resp.StatusCode, b)
		}
		if tc.want == 403 && strings.Contains(b, "method=") {
			t.Fatalf("%s %s sfs=%q reached upstream", tc.method, tc.host, tc.sfs)
		}
	}
	// 跨站写请求即使没有 Cookie 也是 403（不是 302 去授权）
	req := e.req("POST", priv, "/api")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if r := e.do(req); r.StatusCode != 403 {
		t.Fatalf("cross-site without cookie = %d", r.StatusCode)
	}
}

func TestPrivatePortRejectsCrossSiteWebSocket(t *testing.T) {
	e := newEnv(t)
	e.route("ins-a1b2c3d4", 7, 8888, "private", rental.StateRunning, http.HandlerFunc(echo))
	h := "ins-a1b2c3d4-8888.spark.test"
	ck := e.cookie("ins-a1b2c3d4", 8888, 7, "sess-7")
	for _, tc := range []struct {
		sfs, origin string
		want        int
	}{
		{"same-site", "", 403},
		{"cross-site", "", 403},
		{"", "http://ins-evil0000-80.spark.test:" + e.auth.BaseURL.Port(), 403},
		{"same-origin", "", 200}, // echo 不升级，回 200 即视为放行
		{"", "http://" + h + ":" + e.auth.BaseURL.Port(), 200},
		{"", "", 200},
	} {
		req := e.req("GET", h, "/ws", ck)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		if tc.sfs != "" {
			req.Header.Set("Sec-Fetch-Site", tc.sfs)
		}
		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}
		if r := e.do(req); r.StatusCode != tc.want {
			t.Fatalf("ws sfs=%q origin=%q = %d", tc.sfs, tc.origin, r.StatusCode)
		}
	}
}

func TestCallbackHandledLocallyAndTicketNeverForwardedOrLogged(t *testing.T) {
	e := newEnv(t)
	e.route("ins-a1b2c3d4", 7, 8888, "private", rental.StateRunning, http.HandlerFunc(echo))
	h := "ins-a1b2c3d4-8888.spark.test"
	ticket := e.auth.Signer.Ticket(ingress.Claims{InstanceID: "ins-a1b2c3d4", Port: 8888, UserID: 7, SessionHash: "sess-7"})
	resp := e.get(h, "/.spark/callback?"+url.Values{"ticket": {ticket}, "next": {"/lab"}}.Encode())
	body(resp)
	if resp.StatusCode != 302 || resp.Header.Get("Location") != "/lab" {
		t.Fatalf("callback = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	var ck *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == ingress.CookieName {
			ck = c
		}
	}
	if ck == nil {
		t.Fatal("no cookie set")
	}
	if e.dialer.dialCount() != 0 {
		t.Fatal("callback reached upstream")
	}
	// 拿到的 Cookie 可用，且不会被转发给租户
	if b := body(e.get(h, "/lab", ck)); !strings.Contains(b, "path=/lab") || strings.Contains(b, ck.Value) {
		t.Fatalf("with issued cookie = %q", b)
	}
	// 票据已用：再访问回调 403；无论成功失败，日志里都不出现票据
	if r := e.get(h, "/.spark/callback?ticket="+url.QueryEscape(ticket)); r.StatusCode != 403 {
		t.Fatalf("replayed ticket = %d", r.StatusCode)
	}
	if strings.Contains(e.logs.String(), ticket) || strings.Contains(e.logs.String(), ck.Value) {
		t.Fatalf("ticket or cookie logged: %s", e.logs.String())
	}
	// logout 只接受 POST，同样不转发
	if r := e.get(h, "/.spark/logout"); r.StatusCode != 405 || e.dialer.dialCount() != 1 {
		t.Fatalf("logout GET = %d dials=%d", r.StatusCode, e.dialer.dialCount())
	}
}

func TestStatePagesAndUpstreamErrors(t *testing.T) {
	e := newEnv(t)
	e.route("ins-a1b2c3d4", 7, 8888, "public", rental.StateStopped, http.HandlerFunc(echo))
	r := e.get("ins-a1b2c3d4-8888.spark.test", "/")
	if b := body(r); r.StatusCode != 503 || !strings.Contains(b, ingress.PageStopped) || !strings.Contains(b, `http-equiv="refresh" content="10"`) {
		t.Fatalf("stopped = %d %q", r.StatusCode, b)
	}
	e.src.mu.Lock()
	e.src.routes[0].State, e.src.routes[0].Suspended = rental.StateRunning, true
	e.src.mu.Unlock()
	e.table.Refresh(context.Background())
	if r := e.get("ins-a1b2c3d4-8888.spark.test", "/"); r.StatusCode != 503 || !strings.Contains(body(r), ingress.PageSuspended) {
		t.Fatalf("suspended = %d", r.StatusCode)
	}
	if e.dialer.dialCount() != 0 {
		t.Fatal("not-running instance dialed")
	}
	// 上游拨不通：502 + 告警一次（5 分钟节流）
	e.src.mu.Lock()
	e.src.routes[0].Suspended = false
	e.src.mu.Unlock()
	e.addRoute(rental.IngressRoute{InstanceID: "ins-dead0000", UserID: 7, Port: 80, Visibility: "public", State: rental.StateRunning, LanIP: "127.0.0.1"})
	for i := 0; i < 2; i++ {
		if r := e.get("ins-dead0000-80.spark.test", "/"); r.StatusCode != 502 || !strings.Contains(body(r), "无法连接到实例端口") {
			t.Fatalf("unreachable = %d", r.StatusCode)
		}
	}
	if n := e.alerts.count(); n != 1 {
		t.Fatalf("alerts = %d", n)
	}
	e.clk.Advance(5*time.Minute + time.Second)
	e.get("ins-dead0000-80.spark.test", "/").Body.Close()
	if n := e.alerts.count(); n != 2 {
		t.Fatalf("alerts after throttle window = %d", n)
	}
}

func TestNonDialErrorsDoNotAlert(t *testing.T) {
	e := newEnv(t)
	notListening := fmt.Errorf("容器内端口 %d 未监听: %w", 80, &ssh.OpenChannelError{Reason: ssh.ConnectionFailed, Message: "connect refused"})
	e.dialer.errs["ins-nolsn000"] = notListening
	e.dialer.errs["ins-closed00"] = upstream.ErrClosed
	for _, id := range []string{"ins-nolsn000", "ins-closed00"} {
		e.addRoute(rental.IngressRoute{InstanceID: id, UserID: 7, Port: 80, Visibility: "public", State: rental.StateRunning, LanIP: "127.0.0.1"})
		r := e.get(id+"-80.spark.test", "/")
		if b := body(r); r.StatusCode != 502 || !strings.Contains(b, fmt.Sprintf(ingress.PageUpstreamFmt, 80)) {
			t.Fatalf("%s = %d %q", id, r.StatusCode, b)
		}
	}
	// 客户端放弃（请求 ctx 取消）：不告警；之后拨号才失败也无人告警
	blk := make(chan struct{})
	e.dialer.block["ins-slow0000"] = blk
	e.addRoute(rental.IngressRoute{InstanceID: "ins-slow0000", UserID: 7, Port: 80, Visibility: "public", State: rental.StateRunning, LanIP: "127.0.0.1"})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	req := e.req("GET", "ins-slow0000-80.spark.test", "/").WithContext(ctx)
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("expected client timeout")
	}
	cancel()
	waitUntil(t, "slow dial started", func() bool { return e.dialer.dialCount() == 3 })
	// 服务端已处理完被取消的请求（ErrorHandler 记录了 client gone）、请求已注销
	waitUntil(t, "cancelled request handled", func() bool {
		return strings.Contains(e.logs.String(), "ingress client gone") && strings.Contains(e.logs.String(), "instance=ins-slow0000") && e.srv.ActiveStreams() == 0
	})
	close(blk)
	waitUntil(t, "slow dial returned", func() bool { e.dialer.mu.Lock(); defer e.dialer.mu.Unlock(); return e.dialer.done == 3 })
	if n := e.alerts.count(); n != 0 {
		t.Fatalf("alerts for non-dial errors = %d", n)
	}
}

func TestUpstreamCloseMidResponseReturns502OrTruncates(t *testing.T) {
	e := newEnv(t)
	e.route("ins-a1b2c3d4", 7, 8888, "public", rental.StateRunning, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\npartial"))
		conn.Close()
	}))
	resp := e.get("ins-a1b2c3d4-8888.spark.test", "/")
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	// 上游已发 200 头再断开：状态码改不了，但客户端必须看到读错误（unexpected EOF）且字节数不足，不能被当成完整响应
	if resp.StatusCode != 200 || err == nil || len(b) >= 100 {
		t.Fatalf("truncated upstream response: status=%d err=%v len=%d", resp.StatusCode, err, len(b))
	}
	// 之后的请求照常
	e.route("ins-b2c3d4e5", 7, 80, "public", rental.StateRunning, http.HandlerFunc(echo))
	if r := e.get("ins-b2c3d4e5-80.spark.test", "/"); r.StatusCode != 200 {
		t.Fatalf("next request = %d", r.StatusCode)
	}
}

func TestTenantsDoNotShareConnections(t *testing.T) {
	e := newEnv(t)
	for _, id := range []string{"ins-aaaaaaaa", "ins-bbbbbbbb"} {
		id := id
		e.route(id, 7, 8888, "public", rental.StateRunning, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, id) }))
	}
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for i := 0; i < 100; i++ {
		for _, id := range []string{"ins-aaaaaaaa", "ins-bbbbbbbb"} {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				req, _ := http.NewRequest("GET", "http://"+e.host+"/", nil)
				req.Host = id + "-8888.spark.test"
				r, err := http.DefaultClient.Do(req)
				if err != nil {
					errs <- err
					return
				}
				if b := body(r); b != id {
					errs <- fmt.Errorf("%s got %q", id, b)
				}
			}(id)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestStreamingNotCutByWriteTimeout(t *testing.T) {
	e := newEnv(t) // ingress http.Server 的 WriteTimeout 是 1 秒
	e.route("ins-a1b2c3d4", 7, 8888, "public", rental.StateRunning, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < 5; i++ {
			fmt.Fprintf(w, "data: %d\n\n", i)
			fl.Flush()
			time.Sleep(400 * time.Millisecond)
		}
	}))
	resp := e.get("ins-a1b2c3d4-8888.spark.test", "/events")
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || strings.Count(string(b), "data:") != 5 {
		t.Fatalf("stream = %q %v", b, err)
	}
}

func wsEchoHandler(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Upgrade") != "websocket" {
		fmt.Fprint(w, "plain")
		return
	}
	hj, _ := w.(http.Hijacker)
	conn, rw, _ := hj.Hijack()
	rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
	rw.Flush()
	io.Copy(conn, rw) // 原样回显
	conn.Close()
}

// wsRoundTrip 发起升级，等待超过 ingress 的 Read/WriteTimeout 后再收发，确认期限已清除。
func wsRoundTrip(t *testing.T, addr, host string, wait time.Duration) {
	t.Helper()
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	fmt.Fprintf(nc, "GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: x\r\nSec-WebSocket-Version: 13\r\n\r\n", host)
	br := bufio.NewReader(nc)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 101 {
		t.Fatalf("upgrade = %v %v", resp, err)
	}
	for _, msg := range []string{"hello-ws", "second-m"} {
		time.Sleep(wait)
		fmt.Fprint(nc, msg)
		buf := make([]byte, len(msg))
		nc.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err := io.ReadFull(br, buf); err != nil || string(buf) != msg {
			t.Fatalf("echo = %q %v", buf, err)
		}
	}
}

func TestWebSocketUpgradePassthrough(t *testing.T) {
	e := newEnv(t)
	e.route("ins-a1b2c3d4", 7, 8888, "public", rental.StateRunning, http.HandlerFunc(wsEchoHandler))
	wsRoundTrip(t, e.host, "ins-a1b2c3d4-8888.spark.test", 1200*time.Millisecond)
}

func TestSlowUploadNotCutByReadTimeout(t *testing.T) {
	e := newEnv(t) // ingress http.Server 的 ReadTimeout 是 2 秒
	e.route("ins-a1b2c3d4", 7, 8888, "public", rental.StateRunning, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		fmt.Fprintf(w, "got=%d err=%v", len(b), err)
	}))
	pr, pw := io.Pipe()
	go func() {
		for i := 0; i < 6; i++ {
			pw.Write([]byte("chunk"))
			time.Sleep(450 * time.Millisecond)
		}
		pw.Close()
	}()
	req, _ := http.NewRequest("POST", "http://"+e.host+"/upload", pr)
	req.Host = "ins-a1b2c3d4-8888.spark.test"
	resp := e.do(req)
	if b := body(resp); resp.StatusCode != 200 || b != "got=30 err=<nil>" {
		t.Fatalf("slow upload = %d %q", resp.StatusCode, b)
	}
}
