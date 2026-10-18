package adminapi_test

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/adminapi"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/web"
)

func TestLoginAndTOTPSetupOverHTTP(t *testing.T) {
	e := newEnv(t)
	if _, err := e.admins.Create(e.ctx, audit.Actor{}, "root", adminPassword, admin.RoleSuper); err != nil {
		t.Fatal(err)
	}
	c := e.anon()
	c.admin.Username = "root"
	if code, out := c.do("GET", "/me", nil); code != 401 || codeOf(out) != "unauthorized" {
		t.Fatalf("anonymous me = %d %v", code, out)
	}
	if code, out := c.do("POST", "/auth/login", map[string]string{"username": "root", "password": "wrong password!"}); code != 401 || codeOf(out) != "invalid_credentials" {
		t.Fatalf("wrong password = %d %v", code, out)
	}
	resp, out := c.send("POST", "/auth/login", map[string]string{"username": "root", "password": adminPassword})
	cookie := resp.Header.Get("Set-Cookie")
	if resp.StatusCode != 200 || out["next"] != "totp_setup" || !strings.Contains(cookie, "sc_admin=") || !strings.Contains(cookie, "HttpOnly") ||
		!strings.Contains(cookie, "SameSite=Strict") || !strings.Contains(cookie, "Path=/admin/") {
		t.Fatalf("login = %d %v cookie=%q", resp.StatusCode, out, cookie)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("admin responses must not be cached")
	}
	u, _ := url.Parse(out["otpauth_uri"].(string))
	c.secret = u.Query().Get("secret")
	if code, _ := c.do("GET", "/me", nil); code != 401 {
		t.Fatalf("me before totp setup = %d", code)
	}
	if code, out := c.do("POST", "/auth/totp/setup", map[string]string{"code": "000000"}); code != 400 || codeOf(out) != "invalid_totp" {
		t.Fatalf("wrong setup code = %d %v", code, out)
	}
	code, out := c.do("POST", "/auth/totp/setup", map[string]string{"code": c.totp()})
	if code != 200 || len(out["recovery_codes"].([]any)) != admin.RecoveryCodeCount || out["admin"].(map[string]any)["totp_confirmed"] != true {
		t.Fatalf("setup = %d %v", code, out)
	}
	codes := out["recovery_codes"].([]any)
	if code, out := c.do("GET", "/me", nil); code != 200 || out["username"] != "root" || out["role"] != "super" {
		t.Fatalf("me = %d %v", code, out)
	}
	if code, _ := c.do("POST", "/auth/logout", map[string]any{}); code != 200 {
		t.Fatal("logout failed")
	}
	if code, _ := c.do("GET", "/me", nil); code != 401 {
		t.Fatalf("me after logout = %d", code)
	}

	if code, out := c.do("POST", "/auth/login", map[string]string{"username": "root", "password": adminPassword}); code != 200 || out["next"] != "totp" {
		t.Fatalf("second login = %d %v", code, out)
	}
	for _, body := range []map[string]string{{}, {"code": "123456", "recovery_code": "abcde-12345"}} {
		if code, out := c.do("POST", "/auth/totp", body); code != 400 || codeOf(out) != "bad_request" {
			t.Fatalf("totp body %v = %d %v", body, code, out)
		}
	}
	if code, out := c.do("POST", "/auth/totp", map[string]string{"recovery_code": codes[0].(string)}); code != 200 {
		t.Fatalf("recovery code login = %d %v", code, out)
	}
	if code, _ := c.do("GET", "/me", nil); code != 200 {
		t.Fatalf("me after recovery login = %d", code)
	}
}

func TestLoginRateLimitPerIP(t *testing.T) {
	e := newEnv(t)
	c := e.anon()
	body := map[string]string{"username": "nobody", "password": "whatever password"}
	for i := 0; i < 20; i++ {
		if code, out := c.do("POST", "/auth/login", body); code != 401 {
			t.Fatalf("attempt %d = %d %v", i+1, code, out)
		}
	}
	if code, out := c.do("POST", "/auth/login", body); code != 429 || codeOf(out) != "rate_limited" {
		t.Fatalf("21st attempt = %d %v", code, out)
	}
	e.clk.Advance(time.Minute)
	if code, _ := c.do("POST", "/auth/login", body); code != 401 {
		t.Fatalf("after a minute = %d", code)
	}
}

func TestAdminManagementRequiresSuperAndStepUp(t *testing.T) {
	e := newEnv(t)
	root := e.loginAs("root", admin.RoleSuper)
	newAdmin := map[string]any{"username": "alice", "password": adminPassword, "role": "ops"}
	if code, out := root.do("POST", "/admins", newAdmin); code != 403 || codeOf(out) != "totp_required" {
		t.Fatalf("create without totp = %d %v", code, out)
	}
	newAdmin["totp"] = root.totp()
	code, out := root.do("POST", "/admins", newAdmin)
	if code != 201 || out["role"] != "ops" {
		t.Fatalf("create = %d %v", code, out)
	}
	aliceID := int64(out["id"].(float64))
	// 5 分钟内的后续高风险操作可以不带动态码
	if code, out := root.do("PUT", fmt.Sprintf("/admins/%d", aliceID), map[string]any{"status": "disabled"}); code != 200 || out["status"] != "disabled" {
		t.Fatalf("disable = %d %v", code, out)
	}
	if code, out := root.do("PUT", fmt.Sprintf("/admins/%d", root.admin.ID), map[string]any{"role": "ops"}); code != 409 || codeOf(out) != "last_super" {
		t.Fatalf("demote last super = %d %v", code, out)
	}
	if code, out := root.do("POST", fmt.Sprintf("/admins/%d/reset-totp", aliceID), map[string]any{}); code != 200 {
		t.Fatalf("reset totp = %d %v", code, out)
	}
	if code, out := root.do("GET", "/admins", nil); code != 200 || len(out["items"].([]any)) != 2 {
		t.Fatalf("list = %d %v", code, out)
	}
	if code, out := root.do("PUT", "/admins/abc", map[string]any{"role": "ops"}); code != 400 {
		t.Fatalf("bad id = %d %v", code, out)
	}

	ops := e.loginAs("bob", admin.RoleOps)
	if code, out := ops.do("GET", "/admins", nil); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("ops list admins = %d %v", code, out)
	}
	if code, out := ops.do("GET", "/me", nil); code != 200 || out["role"] != "ops" {
		t.Fatalf("ops me = %d %v", code, out)
	}
	e.clk.Advance(admin.StepUpWindow)
	if code, out := root.do("PUT", fmt.Sprintf("/admins/%d", aliceID), map[string]any{"status": "active"}); code != 403 || codeOf(out) != "totp_required" {
		t.Fatalf("after step-up window = %d %v", code, out)
	}
}

func TestChangeOwnPassword(t *testing.T) {
	e := newEnv(t)
	root := e.loginAs("root", admin.RoleSuper)
	if code, out := root.do("POST", "/me/password", map[string]string{"old": "wrong password!", "new": "a brand new password"}); code != 400 || codeOf(out) != "wrong_password" {
		t.Fatalf("wrong old = %d %v", code, out)
	}
	if code, out := root.do("POST", "/me/password", map[string]string{"old": adminPassword, "new": "a brand new password"}); code != 200 {
		t.Fatalf("change = %d %v", code, out)
	}
	if code, _ := e.anon().do("POST", "/auth/login", map[string]string{"username": "root", "password": "a brand new password"}); code != 200 {
		t.Fatalf("login with new password = %d", code)
	}
}

func TestAdminWritesRequireJSON(t *testing.T) {
	e := newEnv(t)
	resp, err := http.Post(e.ts.URL+"/admin/api/v1/auth/login", "text/plain", strings.NewReader(`{"username":"root"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("text/plain login = %d", resp.StatusCode)
	}
}

// 后台端口不信任 X-Forwarded-For：轮换转发头也不能绕过按 IP 的登录限流。
func TestLoginRateLimitIgnoresForwardedFor(t *testing.T) {
	e := newEnv(t)
	body := []byte(`{"username":"nobody","password":"whatever password"}`)
	login := func(i int) int {
		req, _ := http.NewRequest("POST", e.ts.URL+"/admin/api/v1/auth/login", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("10.9.%d.%d", i/250, i%250+1))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for i := 0; i < 20; i++ {
		if code := login(i); code != 401 {
			t.Fatalf("attempt %d = %d", i+1, code)
		}
	}
	if code := login(20); code != 429 {
		t.Fatalf("21st attempt with a new X-Forwarded-For = %d, want 429", code)
	}
}

func TestLoginLimitConfigurable(t *testing.T) {
	e := newEnvWith(t, func(s *adminapi.Server) { s.LoginLimit = 2 })
	c := e.anon()
	for i := 0; i < 2; i++ {
		if code, _ := c.do("POST", "/auth/login", map[string]string{"username": "nobody", "password": "wrong-password-123"}); code != 401 {
			t.Fatalf("attempt %d = %d, want 401", i, code)
		}
	}
	if code, out := c.do("POST", "/auth/login", map[string]string{"username": "nobody", "password": "wrong-password-123"}); code != 429 || codeOf(out) != "rate_limited" {
		t.Fatalf("third attempt = %d %v, want 429 rate_limited", code, out)
	}
}

func TestAdminRouterServesSPAAndJSON404(t *testing.T) {
	index := fstest.MapFS{"index.html": {Data: []byte("<!doctype html><title>admin</title>")}}
	e := newEnvWith(t, func(s *adminapi.Server) { s.Static = web.NewHandler(index, "/admin/") })
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(path string) (*http.Response, string) {
		resp, err := noRedirect.Get(e.ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, string(body)
	}
	for _, p := range []string{"/", "/admin"} {
		if resp, _ := get(p); resp.StatusCode != 302 || resp.Header.Get("Location") != "/admin/" {
			t.Fatalf("%s: %d -> %q, want 302 /admin/", p, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	for _, p := range []string{"/admin/", "/admin/machines/12", "/admin/login"} {
		resp, body := get(p)
		if resp.StatusCode != 200 || !strings.Contains(body, "admin") || resp.Header.Get("Cache-Control") != "no-cache" ||
			resp.Header.Get("Content-Security-Policy") == "" {
			t.Fatalf("%s: %d %q cache=%q", p, resp.StatusCode, body, resp.Header.Get("Cache-Control"))
		}
	}
	for _, p := range []string{"/admin/api/v1/nope", "/admin/api/", "/nope", "/api/v1/products"} {
		resp, body := get(p)
		if resp.StatusCode != 404 || !strings.Contains(body, `"not_found"`) {
			t.Fatalf("%s: %d %q, want JSON 404", p, resp.StatusCode, body)
		}
	}
	if resp, _ := get("/admin/api/v1/dashboard"); resp.StatusCode != 401 || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("anonymous dashboard = %d cache=%q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
}

func TestAdminSameSiteGuard(t *testing.T) {
	e := newEnvWith(t, func(s *adminapi.Server) { s.PublicBaseURL = "https://admin.spark.test" })
	req, _ := http.NewRequest("POST", e.ts.URL+"/admin/api/v1/auth/login", strings.NewReader(`{"username":"x","password":"y"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-site")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("same-site admin login = %d", resp.StatusCode)
	}
	req2, _ := http.NewRequest("POST", e.ts.URL+"/admin/api/v1/auth/login", strings.NewReader(`{"username":"x","password":"y"}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Origin", "https://admin.spark.test")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode == 403 {
		t.Fatal("matching origin rejected")
	}
}
