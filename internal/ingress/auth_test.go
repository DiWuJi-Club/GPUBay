package ingress_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/ingress"
	"spark-cloud/internal/platform/clock"
)

func newAuth(t *testing.T, base, domain string, secure bool) *ingress.Auth {
	t.Helper()
	a, err := ingress.NewAuth(secret, base, domain, secure, clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestNewAuthRejectsBadBase(t *testing.T) {
	for _, base := range []string{"", "spark.test", "ftp://spark.test", "http://", "http://u:p@spark.test", "http://spark.test/x"} {
		if _, err := ingress.NewAuth(secret, base, "spark.test", false, clock.Real{}); err == nil {
			t.Fatalf("base %q accepted", base)
		}
	}
}

func TestParseTargetRejects(t *testing.T) {
	a := newAuth(t, "http://spark.test:18080", "spark.test", false)
	tg, err := a.ParseTarget("http://ins-a1b2c3d4-8888.spark.test:18080/lab/tree?x=1#frag")
	if err != nil || tg.InstanceID != "ins-a1b2c3d4" || tg.Port != 8888 || tg.Path != "/lab/tree?x=1" {
		t.Fatalf("target = %+v %v", tg, err)
	}
	if tg, _ := a.ParseTarget("http://ins-a1b2c3d4-8888.spark.test:18080"); tg.Path != "/" {
		t.Fatalf("empty path → %q", tg.Path)
	}
	if tg, err := a.ParseTarget("http://INS-A1B2C3D4-8888.Spark.Test:18080/x"); err != nil || tg.InstanceID != "ins-a1b2c3d4" {
		t.Fatalf("mixed case host = %+v %v", tg, err)
	}
	for _, bad := range []string{
		"", "lab", "/lab", "//ins-a1b2c3d4-8888.spark.test:18080/",
		"https://ins-a1b2c3d4-8888.spark.test:18080/", "http://ins-a1b2c3d4-8888.spark.test/", "http://ins-a1b2c3d4-8888.spark.test:1/",
		"http://ins-a1b2c3d4-8888.spark.test:018080/", "http://ins-a1b2c3d4-8888.spark.test:/",
		"http://user:pw@ins-a1b2c3d4-8888.spark.test:18080/", "http://user@ins-a1b2c3d4-8888.spark.test:18080/",
		"http://spark.test:18080/market", "http://ins-a1b2c3d4-8888.evil.test:18080/",
		"http://ins-a1b2c3d4-8888.spark.test.evil.test:18080/", "http://evil.test#@ins-a1b2c3d4-8888.spark.test:18080/",
		"http://evil.test\\@ins-a1b2c3d4-8888.spark.test:18080/", "http://ins-a1b2c3d4-22.spark.test:18080/",
		"http:ins-a1b2c3d4-8888.spark.test:18080/", "javascript:alert(1)", "http://ins-a1b2c3d4-8888.spark.test:18080/\n",
	} {
		if _, err := a.ParseTarget(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	prod := newAuth(t, "https://spark.example", "sparkapp.example", true)
	if tg, err := prod.ParseTarget("https://ins-a1b2c3d4-80.sparkapp.example/"); err != nil || tg.Port != 80 {
		t.Fatalf("prod target = %+v %v", tg, err)
	}
	if _, err := prod.ParseTarget("https://ins-a1b2c3d4-80.sparkapp.example:443/"); err != nil { // 显式写出默认端口等价
		t.Fatalf("explicit default port rejected: %v", err)
	}
	for _, bad := range []string{"https://ins-a1b2c3d4-80.sparkapp.example:8443/", "http://ins-a1b2c3d4-80.sparkapp.example/",
		"https://ins-a1b2c3d4-80.sparkapp.example:80/", "https://ins-a1b2c3d4-80.spark.example/"} {
		if _, err := prod.ParseTarget(bad); err == nil {
			t.Fatalf("prod %q accepted", bad)
		}
	}
}

func TestURLsAndSafeNext(t *testing.T) {
	a := newAuth(t, "http://spark.test:18080", "spark.test", false)
	if got := a.PortURL("ins-a1b2c3d4", 8888); got != "http://ins-a1b2c3d4-8888.spark.test:18080/" {
		t.Fatalf("port url = %s", got)
	}
	if got := a.AuthorizeURL("http://ins-a1b2c3d4-8888.spark.test:18080/lab?x=1"); got != "http://spark.test:18080/api/v1/ingress/authorize?target="+url.QueryEscape("http://ins-a1b2c3d4-8888.spark.test:18080/lab?x=1") {
		t.Fatalf("authorize url = %s", got)
	}
	cb := a.CallbackURL(ingress.Target{InstanceID: "ins-a1b2c3d4", Port: 8888, Path: "/lab?x=1"}, "T.S")
	u, _ := url.Parse(cb)
	if u.Host != "ins-a1b2c3d4-8888.spark.test:18080" || u.Path != "/.spark/callback" || u.Query().Get("ticket") != "T.S" || u.Query().Get("next") != "/lab?x=1" {
		t.Fatalf("callback url = %s", cb)
	}
	prod := newAuth(t, "https://spark.example", "sparkapp.example", true)
	if got := prod.PortURL("ins-a1b2c3d4", 80); got != "https://ins-a1b2c3d4-80.sparkapp.example/" {
		t.Fatalf("prod port url = %s", got)
	}
}

// 开放重定向矩阵：SafeNext 与控制台 safeRedirect 同规则，否则回 /。
func TestSafeNextMatrix(t *testing.T) {
	for in, want := range map[string]string{
		"/lab": "/lab", "/a?b=c": "/a?b=c", "/ok#frag": "/ok#frag", "/a/b/../c": "/a/b/../c", "/%2F%2Fevil.example": "/%2F%2Fevil.example",
		"": "/", "lab": "/", "//evil.example/x": "/", "///evil.example": "/", "/\\evil.example": "/", "\\\\evil.example": "/",
		"http://evil.example": "/", "https:evil.example": "/", "javascript:alert(1)": "/", " /lab": "/", "/\t/evil.example": "/",
		"/\n/evil.example": "/", "%2F%2Fevil.example": "/", "/lab\\..\\x": "/",
	} {
		if got := ingress.SafeNext(in); got != want {
			t.Fatalf("SafeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func cookieFrom(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == ingress.CookieName {
			return c
		}
	}
	return nil
}

func TestCallbackSetsCookieAndLogoutClearsIt(t *testing.T) {
	a := newAuth(t, "http://spark.test:18080", "spark.test", false)
	tk := a.Signer.Ticket(ingress.Claims{InstanceID: "ins-a1b2c3d4", Port: 8888, UserID: 7, SessionHash: "h"})
	cbURL := "http://ins-a1b2c3d4-8888.spark.test:18080/.spark/callback?ticket=" + url.QueryEscape(tk) + "&next=" + url.QueryEscape("//evil.example")
	rec := httptest.NewRecorder()
	a.HandleCallback(rec, httptest.NewRequest("GET", cbURL, nil))
	if rec.Code != 302 || rec.Header().Get("Location") != "/" {
		t.Fatalf("callback = %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if rec.Header().Get("Referrer-Policy") != "no-referrer" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("callback headers = %v", rec.Header())
	}
	ck := cookieFrom(rec)
	if ck == nil || !ck.HttpOnly || ck.Path != "/" || ck.Domain != "" || ck.SameSite != http.SameSiteLaxMode || ck.Secure || ck.MaxAge != int(ingress.CookieTTL/time.Second) {
		t.Fatalf("cookie = %+v", ck)
	}
	claims, err := a.Signer.VerifyCookie(ck.Value)
	if err != nil || claims.UserID != 7 || claims.Port != 8888 || claims.SessionHash != "h" {
		t.Fatalf("cookie claims = %+v %v", claims, err)
	}
	// 回放同一 callback URL：票据已用 → 403，不发 Cookie
	recReplay := httptest.NewRecorder()
	a.HandleCallback(recReplay, httptest.NewRequest("GET", cbURL, nil))
	if recReplay.Code != 403 || cookieFrom(recReplay) != nil {
		t.Fatalf("replayed callback = %d", recReplay.Code)
	}
	// 票据与当前 Host 的实例 / 端口不一致 → 403 页
	tk2 := a.Signer.Ticket(ingress.Claims{InstanceID: "ins-a1b2c3d4", Port: 7860, UserID: 7, SessionHash: "h"})
	req2 := httptest.NewRequest("GET", "http://ins-a1b2c3d4-8888.spark.test:18080/.spark/callback?ticket="+url.QueryEscape(tk2), nil)
	rec2 := httptest.NewRecorder()
	a.HandleCallback(rec2, req2)
	if rec2.Code != 403 || !strings.Contains(rec2.Body.String(), ingress.PageTicketInvalid) || cookieFrom(rec2) != nil {
		t.Fatalf("mismatched ticket = %d %s", rec2.Code, rec2.Body.String())
	}
	// ValidCookie 逐枚验证：第一枚是租户塞的垃圾，第二枚有效
	req3 := httptest.NewRequest("GET", "http://ins-a1b2c3d4-8888.spark.test:18080/lab", nil)
	req3.AddCookie(&http.Cookie{Name: ingress.CookieName, Value: "junk"})
	req3.AddCookie(&http.Cookie{Name: ingress.CookieName, Value: ck.Value})
	if got, ok := a.ValidCookie(req3, "ins-a1b2c3d4", 8888); !ok || got.UserID != 7 {
		t.Fatalf("valid cookie not found among multiple: %+v %v", got, ok)
	}
	if _, ok := a.ValidCookie(req3, "ins-a1b2c3d4", 7860); ok {
		t.Fatal("cookie for other port accepted")
	}
	if _, ok := a.ValidCookie(req3, "ins-zzzzzzzz", 8888); ok {
		t.Fatal("cookie for other instance accepted")
	}
	// logout：同源 POST 清 Cookie，GET 405（来源校验见 TestLogoutOriginCheck）
	rec4 := httptest.NewRecorder()
	logoutReq := httptest.NewRequest("POST", "http://ins-a1b2c3d4-8888.spark.test:18080/.spark/logout", nil)
	logoutReq.Header.Set("Sec-Fetch-Site", "same-origin")
	a.HandleLogout(rec4, logoutReq)
	if c := cookieFrom(rec4); rec4.Code != 200 || c == nil || c.MaxAge >= 0 || c.Path != "/" {
		t.Fatalf("logout = %d cookie=%+v", rec4.Code, c)
	}
	rec5 := httptest.NewRecorder()
	a.HandleLogout(rec5, httptest.NewRequest("GET", "http://ins-a1b2c3d4-8888.spark.test:18080/.spark/logout", nil))
	if rec5.Code != 405 || cookieFrom(rec5) != nil {
		t.Fatalf("GET logout = %d", rec5.Code)
	}
	// Secure 环境 Cookie 带 Secure
	sec := newAuth(t, "https://spark.example", "spark.example", true)
	tk3 := sec.Signer.Ticket(ingress.Claims{InstanceID: "ins-a1b2c3d4", Port: 80, UserID: 1, SessionHash: "h"})
	rec6 := httptest.NewRecorder()
	sec.HandleCallback(rec6, httptest.NewRequest("GET", "https://ins-a1b2c3d4-80.spark.example/.spark/callback?ticket="+url.QueryEscape(tk3)+"&next=/x", nil))
	if cs := rec6.Result().Cookies(); len(cs) != 1 || !cs[0].Secure || rec6.Header().Get("Location") != "/x" {
		t.Fatalf("secure callback = %+v %s", cs, rec6.Header().Get("Location"))
	}
}

// Host 伪造：callback 只信 Host 解析出的实例 / 端口，且票据必须与之一致；非实例 Host → 404。
func TestCallbackHostSpoofing(t *testing.T) {
	a := newAuth(t, "http://spark.test:18080", "spark.test", false)
	ticketA := func() string {
		return a.Signer.Ticket(ingress.Claims{InstanceID: "ins-a1b2c3d4", Port: 8888, UserID: 7, SessionHash: "h"})
	}
	for _, host := range []string{"ins-zzzzzzzz-8888.spark.test:18080", "ins-a1b2c3d4-8889.spark.test:18080"} {
		req := httptest.NewRequest("GET", "http://x/.spark/callback?ticket="+url.QueryEscape(ticketA()), nil)
		req.Host = host
		rec := httptest.NewRecorder()
		a.HandleCallback(rec, req)
		if rec.Code != 403 || cookieFrom(rec) != nil {
			t.Fatalf("host %s → %d", host, rec.Code)
		}
	}
	for _, host := range []string{"spark.test:18080", "evil.test", "ins-a1b2c3d4-8888.evil.test", "ins-a1b2c3d4-8888.spark.test.evil.test"} {
		req := httptest.NewRequest("GET", "http://x/.spark/callback?ticket="+url.QueryEscape(ticketA()), nil)
		req.Host = host
		rec := httptest.NewRecorder()
		a.HandleCallback(rec, req)
		if rec.Code != 404 || cookieFrom(rec) != nil || !strings.Contains(rec.Body.String(), ingress.PageNotFound) || strings.Contains(rec.Body.String(), ingress.PageNotOpen) {
			t.Fatalf("host %s → %d %s", host, rec.Code, rec.Body.String())
		}
	}
}

// Cookie 回放到别的实例 / 端口失败；过期 Cookie 失败；签名篡改失败；accept 回调拒绝的 Cookie 不遮住后面有效的那枚。
func TestValidCookieBinding(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	a, err := ingress.NewAuth(secret, "http://spark.test:18080", "spark.test", false, clk)
	if err != nil {
		t.Fatal(err)
	}
	ckA := a.Signer.Cookie(ingress.Claims{InstanceID: "ins-a1b2c3d4", Port: 8888, UserID: 7, SessionHash: "hA"})
	req := func(vals ...string) *http.Request {
		r := httptest.NewRequest("GET", "http://ins-a1b2c3d4-8888.spark.test:18080/", nil)
		for _, v := range vals {
			r.AddCookie(&http.Cookie{Name: ingress.CookieName, Value: v})
		}
		return r
	}
	if _, ok := a.ValidCookie(req(ckA), "ins-a1b2c3d4", 8888); !ok {
		t.Fatal("own cookie rejected")
	}
	if _, ok := a.ValidCookie(req(ckA), "ins-zzzzzzzz", 8888); ok {
		t.Fatal("A's cookie accepted on another instance")
	}
	if _, ok := a.ValidCookie(req(ckA), "ins-a1b2c3d4", 7860); ok {
		t.Fatal("A's cookie accepted on another port")
	}
	if _, ok := a.ValidCookie(req(flipSig(ckA)), "ins-a1b2c3d4", 8888); ok {
		t.Fatal("tampered cookie accepted")
	}
	if _, ok := a.ValidCookie(req(), "ins-a1b2c3d4", 8888); ok {
		t.Fatal("no cookie accepted")
	}
	// 另一个签名有效、但会话已失效的 Cookie 在前：accept 拒绝它后继续检查下一枚
	ckStale := a.Signer.Cookie(ingress.Claims{InstanceID: "ins-a1b2c3d4", Port: 8888, UserID: 7, SessionHash: "stale"})
	got, ok := a.ValidCookieWith(req(ckStale, ckA), "ins-a1b2c3d4", 8888, func(c ingress.Claims) bool { return c.SessionHash == "hA" })
	if !ok || got.SessionHash != "hA" {
		t.Fatalf("ValidCookieWith = %+v %v", got, ok)
	}
	if _, ok := a.ValidCookieWith(req(ckStale), "ins-a1b2c3d4", 8888, func(ingress.Claims) bool { return false }); ok {
		t.Fatal("accept=false still accepted")
	}
	clk.Advance(ingress.CookieTTL + time.Second)
	if _, ok := a.ValidCookie(req(ckA), "ins-a1b2c3d4", 8888); ok {
		t.Fatal("expired cookie accepted")
	}
}

// logout 来源校验：Sec-Fetch-Site 只放 same-origin / none；无该头时 Origin 必须等于本实例 origin；否则 403 且不动 Cookie。
func TestLogoutOriginCheck(t *testing.T) {
	a := newAuth(t, "http://spark.test:18080", "spark.test", false)
	const self = "http://ins-a1b2c3d4-8888.spark.test:18080"
	cases := []struct {
		name, sfs, origin string
		want              int
	}{
		{"sfs same-origin", "same-origin", "", 200},
		{"sfs none", "none", "", 200},
		{"sfs same-origin wins over bad origin", "same-origin", "http://evil.test", 200},
		{"sfs same-site (sibling instance)", "same-site", self, 403},
		{"sfs cross-site", "cross-site", self, 403},
		{"origin equal", "", self, 200},
		{"origin sibling instance", "", "http://ins-zzzzzzzz-8888.spark.test:18080", 403},
		{"origin other port of instance", "", "http://ins-a1b2c3d4-7860.spark.test:18080", 403},
		{"origin wrong scheme", "", "https://ins-a1b2c3d4-8888.spark.test:18080", 403},
		{"origin missing listen port", "", "http://ins-a1b2c3d4-8888.spark.test", 403},
		{"origin console", "", "http://spark.test:18080", 403},
		{"origin null", "", "null", 403},
		{"neither header", "", "", 403},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", self+"/.spark/logout", nil)
			if c.sfs != "" {
				req.Header.Set("Sec-Fetch-Site", c.sfs)
			}
			if c.origin != "" {
				req.Header.Set("Origin", c.origin)
			}
			rec := httptest.NewRecorder()
			a.HandleLogout(rec, req)
			ck := cookieFrom(rec)
			if rec.Code != c.want || (c.want == 200) != (ck != nil && ck.MaxAge < 0) {
				t.Fatalf("code = %d cookie = %+v, want %d", rec.Code, ck, c.want)
			}
		})
	}
	// 默认端口：prod origin 不带端口
	prod := newAuth(t, "https://spark.example", "sparkapp.example", true)
	req := httptest.NewRequest("POST", "https://ins-a1b2c3d4-80.sparkapp.example/.spark/logout", nil)
	req.Header.Set("Origin", "https://ins-a1b2c3d4-80.sparkapp.example")
	rec := httptest.NewRecorder()
	prod.HandleLogout(rec, req)
	if rec.Code != 200 {
		t.Fatalf("prod origin = %d", rec.Code)
	}
	// 非实例 Host → 404 页
	req = httptest.NewRequest("POST", "http://spark.test:18080/.spark/logout", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec = httptest.NewRecorder()
	a.HandleLogout(rec, req)
	if rec.Code != 404 || cookieFrom(rec) != nil || !strings.Contains(rec.Body.String(), ingress.PageNotFound) {
		t.Fatalf("non-instance host logout = %d", rec.Code)
	}
}
