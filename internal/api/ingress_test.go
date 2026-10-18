package api_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"spark-cloud/internal/api"
	"spark-cloud/internal/platform/dbtest"
)

// ingressFixture：一个已下单用户（实例 id，端口 8888 私有、7860 公开）。
type ingressFixture struct {
	e      *testEnv
	owner  *client
	userID int64
	id     string
}

func newIngressFixture(t *testing.T, tweak func(*api.Server)) *ingressFixture {
	t.Helper()
	e := newTestEnvWith(t, tweak)
	dbtest.CreateMachine(t, e.pool, "SN001")
	c := e.client()
	user := c.login("13800000031")
	c.do("POST", "/kyc/personal", map[string]string{"name": "张三", "id_number": "11010119900101123X"})
	uid := int64(user["id"].(float64))
	if _, err := e.wallet.AdminAdjust(t.Context(), 1, uid, 100000, "fund", ""); err != nil {
		t.Fatal(err)
	}
	_, out := c.do("POST", "/orders", map[string]any{"product_id": dbtest.ProductID(t, e.pool), "duration": "1d", "image_id": dbtest.ImageID(t, e.pool)})
	id := out["instance"].(map[string]any)["id"].(string)
	if _, err := e.rental.AddPort(t.Context(), uid, id, 8888, "private", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.rental.AddPort(t.Context(), uid, id, 7860, "public", "", ""); err != nil {
		t.Fatal(err)
	}
	return &ingressFixture{e: e, owner: c, userID: uid, id: id}
}

func (f *ingressFixture) target(port, path string) string {
	return "http://" + f.id + "-" + port + ".spark.test" + path
}

// authorize 以 c 的会话（nil = 未登录）请求 authorize，不跟随跳转。
func (f *ingressFixture) authorize(t *testing.T, c *client, rawTarget string, host string) (*http.Response, string) {
	t.Helper()
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest("GET", f.e.ts.URL+"/api/v1/ingress/authorize?target="+url.QueryEscape(rawTarget), nil)
	if c != nil { // 显式带上会话 Cookie：改了 req.Host 时 Jar 不一定按 URL 取 Cookie
		for _, ck := range c.http.Jar.Cookies(req.URL) {
			req.AddCookie(ck)
		}
	}
	if host != "" {
		req.Host = host
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestIngressAuthorizeFlow(t *testing.T) {
	f := newIngressFixture(t, nil)
	e, id := f.e, f.id
	target := f.target("8888", "/lab?x=1")
	// 主人 → 302 到 callback，票据可验
	resp, _ := f.authorize(t, f.owner, target, "")
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != 302 || loc.Scheme != "http" || loc.Host != id+"-8888.spark.test" || loc.Path != "/.spark/callback" || loc.Query().Get("next") != "/lab?x=1" {
		t.Fatalf("owner = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("cache-control = %q", resp.Header.Get("Cache-Control"))
	}
	claims, err := e.ingress.Signer.VerifyTicket(loc.Query().Get("ticket"))
	if err != nil || claims.InstanceID != id || claims.Port != 8888 || claims.UserID != f.userID || claims.SessionHash == "" {
		t.Fatalf("ticket = %+v %v", claims, err)
	}
	// 非法 target → 400 页
	for _, bad := range []string{"http://evil.test/", "https://" + id + "-8888.spark.test/", "http://u:p@" + id + "-8888.spark.test/", "//" + id + "-8888.spark.test/", ""} {
		if resp, _ := f.authorize(t, f.owner, bad, ""); resp.StatusCode != 400 || resp.Header.Get("Location") != "" {
			t.Fatalf("bad target %q = %d", bad, resp.StatusCode)
		}
	}
	// 另一个用户 → 403 页，无票据
	other := e.client()
	other.login("13800000032")
	r2, body := f.authorize(t, other, target, "")
	if r2.StatusCode != 403 || !strings.HasPrefix(r2.Header.Get("Content-Type"), "text/html") || r2.Header.Get("Location") != "" || strings.Contains(body, "ticket") {
		t.Fatalf("non-owner = %d %s", r2.StatusCode, r2.Header.Get("Content-Type"))
	}
	// 未登录 → 302 到登录页，redirect 回本端点
	r3, _ := f.authorize(t, nil, target, "")
	want := "http://spark.test/login?redirect=" + url.QueryEscape("/api/v1/ingress/authorize?target="+url.QueryEscape(target))
	if r3.StatusCode != 302 || r3.Header.Get("Location") != want {
		t.Fatalf("anon = %d %s, want %s", r3.StatusCode, r3.Header.Get("Location"), want)
	}
	// 登出后的旧会话 → 当作未登录
	f.owner.do("POST", "/auth/logout", map[string]any{})
	if r4, _ := f.authorize(t, f.owner, target, ""); r4.StatusCode != 302 || !strings.HasPrefix(r4.Header.Get("Location"), "http://spark.test/login?") {
		t.Fatalf("logged-out = %d %s", r4.StatusCode, r4.Header.Get("Location"))
	}
}

func TestIngressAuthorizePortAndState(t *testing.T) {
	f := newIngressFixture(t, nil)
	// 未登记的端口 → 404 页，无票据
	if resp, body := f.authorize(t, f.owner, f.target("9999", "/"), ""); resp.StatusCode != 404 || resp.Header.Get("Location") != "" || !strings.Contains(body, "该端口未开放") {
		t.Fatalf("unregistered port = %d", resp.StatusCode)
	}
	// 公开端口：不需要票据，直接回到目标地址（主人与其他用户都一样）
	other := f.e.client()
	other.login("13800000033")
	for _, c := range []*client{f.owner, other} {
		resp, _ := f.authorize(t, c, f.target("7860", "/app?q=1"), "")
		if resp.StatusCode != 302 || resp.Header.Get("Location") != f.target("7860", "/app?q=1") {
			t.Fatalf("public port = %d %s", resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	// 其他用户访问私有端口 / 不存在的端口 / 不存在的实例：一律 403（不泄露端口是否存在）
	for _, tg := range []string{f.target("8888", "/"), f.target("9999", "/"), "http://ins-zzzzzzzz-8888.spark.test/"} {
		if resp, _ := f.authorize(t, other, tg, ""); resp.StatusCode != 403 {
			t.Fatalf("other %s = %d", tg, resp.StatusCode)
		}
	}
	// 实例进入终态 → 不再签发票据
	for _, state := range []string{"wiping", "released", "failed"} {
		if _, err := f.e.pool.Exec(t.Context(), `UPDATE instances SET state = $2 WHERE id = $1`, f.id, state); err != nil {
			t.Fatal(err)
		}
		if resp, body := f.authorize(t, f.owner, f.target("8888", "/"), ""); resp.StatusCode != 410 || resp.Header.Get("Location") != "" || !strings.Contains(body, "实例已释放") {
			t.Fatalf("state %s = %d", state, resp.StatusCode)
		}
	}
	// stopped / running 仍可签发（入口按状态显示页面；续费后不用重新授权）
	for _, state := range []string{"stopped", "running"} {
		if _, err := f.e.pool.Exec(t.Context(), `UPDATE instances SET state = $2 WHERE id = $1`, f.id, state); err != nil {
			t.Fatal(err)
		}
		if resp, _ := f.authorize(t, f.owner, f.target("8888", "/"), ""); resp.StatusCode != 302 || !strings.Contains(resp.Header.Get("Location"), "/.spark/callback?") {
			t.Fatalf("state %s = %d", state, resp.StatusCode)
		}
	}
}

// Host 伪造：callback 地址只从配置推导，不受请求 Host / X-Forwarded-Host 影响。
func TestIngressAuthorizeIgnoresRequestHost(t *testing.T) {
	f := newIngressFixture(t, nil)
	for _, host := range []string{"evil.test", f.id + "-8888.evil.test"} {
		resp, _ := f.authorize(t, f.owner, f.target("8888", "/"), host)
		loc, _ := url.Parse(resp.Header.Get("Location"))
		if resp.StatusCode != 302 || loc.Host != f.id+"-8888.spark.test" || loc.Scheme != "http" {
			t.Fatalf("host %s → %d %s", host, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	r, _ := f.authorize(t, nil, f.target("8888", "/"), "evil.test")
	if !strings.HasPrefix(r.Header.Get("Location"), "http://spark.test/login?") {
		t.Fatalf("anon login redirect with spoofed host = %s", r.Header.Get("Location"))
	}
}

func TestIngressAuthorizeRateLimit(t *testing.T) {
	f := newIngressFixture(t, func(s *api.Server) { s.AuthorizeLimit = 2 })
	for i := 0; i < 2; i++ {
		if resp, _ := f.authorize(t, f.owner, f.target("8888", "/"), ""); resp.StatusCode != 302 {
			t.Fatalf("call %d = %d", i, resp.StatusCode)
		}
	}
	if resp, _ := f.authorize(t, f.owner, f.target("8888", "/"), ""); resp.StatusCode != 429 || resp.Header.Get("Location") != "" {
		t.Fatalf("over limit = %d", resp.StatusCode)
	}
	f.e.clk.Advance(61e9)
	if resp, _ := f.authorize(t, f.owner, f.target("8888", "/"), ""); resp.StatusCode != 302 {
		t.Fatalf("after window = %d", resp.StatusCode)
	}
}
