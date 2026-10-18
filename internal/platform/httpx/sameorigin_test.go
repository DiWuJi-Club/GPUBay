package httpx_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"spark-cloud/internal/platform/httpx"
)

func TestSameSiteGuardMatrix(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	exempt := func(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, "/api/v1/pay/notify/") }
	h := httpx.SameSiteGuard("https://spark.example", exempt, slog.New(slog.NewTextHandler(io.Discard, nil)))(ok)
	cases := []struct {
		name    string
		method  string
		path    string
		headers map[string]string
		want    int
	}{
		{"get never checked", "GET", "/api/v1/me", map[string]string{"Sec-Fetch-Site": "cross-site"}, 204},
		{"same-origin", "POST", "/api/v1/orders", map[string]string{"Sec-Fetch-Site": "same-origin"}, 204},
		{"none (typed url)", "POST", "/api/v1/orders", map[string]string{"Sec-Fetch-Site": "none"}, 204},
		{"same-site sibling subdomain", "POST", "/api/v1/uploads", map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "https://ins-a1b2c3d4-80.spark.example"}, 403},
		{"cross-site", "POST", "/api/v1/orders", map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"no sfs, origin matches", "POST", "/api/v1/orders", map[string]string{"Origin": "https://spark.example"}, 204},
		{"no sfs, origin matches with trailing slash", "PUT", "/api/v1/x", map[string]string{"Origin": "https://spark.example/"}, 204},
		{"no sfs, origin differs", "POST", "/api/v1/orders", map[string]string{"Origin": "https://evil.example"}, 403},
		{"no sfs, origin scheme differs", "POST", "/api/v1/orders", map[string]string{"Origin": "http://spark.example"}, 403},
		{"neither header (curl / go client)", "POST", "/api/v1/orders", nil, 204},
		{"payment callback exempt", "POST", "/api/v1/pay/notify/wechat", map[string]string{"Sec-Fetch-Site": "cross-site"}, 204},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		for k, v := range c.headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Fatalf("%s: %d, want %d (body %s)", c.name, rec.Code, c.want, rec.Body.String())
		}
		if c.want == 403 && !strings.Contains(rec.Body.String(), `"forbidden"`) {
			t.Fatalf("%s: body = %s", c.name, rec.Body.String())
		}
	}
	// 没有对外地址（后台未配置 SPARK_ADMIN_PUBLIC_BASE_URL）：有 Origin 一律拒绝，没有则放行
	h2 := httpx.SameSiteGuard("", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))(ok)
	for origin, want := range map[string]int{"https://admin.spark.example": 403, "": 204} {
		req := httptest.NewRequest("POST", "/admin/api/v1/x", nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		h2.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("no base url, origin %q: %d", origin, rec.Code)
		}
	}
}

func TestSameSiteGuardDefaultPortBaseURL(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cases := []struct {
		base, origin string
		want         int
	}{
		{"https://spark.example:443", "https://spark.example", 204},
		{"http://spark.example:80", "http://spark.example", 204},
		{"https://Spark.Example:443/", "https://spark.example", 204},
		{"https://spark.example:8443", "https://spark.example:8443", 204},
		{"https://spark.example:8443", "https://spark.example", 403},
		{"http://spark.example:443", "http://spark.example", 403}, // 443 只对 https 是默认端口
	}
	for _, c := range cases {
		h := httpx.SameSiteGuard(c.base, nil, log)(ok)
		req := httptest.NewRequest("POST", "/api/v1/orders", nil)
		req.Header.Set("Origin", c.origin)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Fatalf("base %q origin %q: %d, want %d", c.base, c.origin, rec.Code, c.want)
		}
	}
}
