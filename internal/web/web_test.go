package web_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"spark-cloud/internal/web"
)

func serve(h http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestSPAHandler(t *testing.T) {
	fsys := fstest.MapFS{
		"index.html":           {Data: []byte("<!doctype html><title>admin</title>")},
		"assets/app.abc123.js": {Data: []byte("console.log(1)")},
		"brand/logo.svg":       {Data: []byte("<svg xmlns=\"http://www.w3.org/2000/svg\"/>")},
	}
	h := web.NewHandler(fsys, "/admin/")
	cases := []struct{ path, cache, ctype, body string }{
		{"/admin/", "no-cache", "text/html", "<!doctype"},
		{"/admin/index.html", "no-cache", "text/html", "<!doctype"},
		{"/admin/instances/ins-abc12345", "no-cache", "text/html", "<!doctype"},
		{"/admin/assets/", "no-cache", "text/html", "<!doctype"}, // 目录不直接列出，回退 index
		{"/admin/../etc/passwd", "no-cache", "text/html", "<!doctype"},
		{"/admin/assets/app.abc123.js", "public, max-age=31536000, immutable", "javascript", "console.log"},
		{"/admin/brand/logo.svg", "public, max-age=3600", "image/svg+xml", "<svg"},
	}
	for _, c := range cases {
		rec := serve(h, c.path)
		if rec.Code != 200 || rec.Header().Get("Cache-Control") != c.cache ||
			!strings.Contains(rec.Header().Get("Content-Type"), c.ctype) || !strings.Contains(rec.Body.String(), c.body) {
			t.Errorf("%s: %d cache=%q type=%q body=%q", c.path, rec.Code, rec.Header().Get("Cache-Control"), rec.Header().Get("Content-Type"), rec.Body.String())
		}
		for k, want := range map[string]string{"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Referrer-Policy": "same-origin"} {
			if rec.Header().Get(k) != want {
				t.Errorf("%s: %s = %q, want %q", c.path, k, rec.Header().Get(k), want)
			}
		}
		if csp := rec.Header().Get("Content-Security-Policy"); !strings.HasPrefix(csp, "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline';") ||
			!strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "form-action 'self'") {
			t.Errorf("%s: csp = %q", c.path, csp)
		}
	}
	if rec := httptest.NewRecorder(); true {
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST to spa = %d, want 405", rec.Code)
		}
	}
}

func TestSPAHandlerWithoutBuild(t *testing.T) {
	rec := serve(web.NewHandler(fstest.MapFS{}, "/"), "/market")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "前端尚未构建，请执行 make web") ||
		rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatalf("missing build = %d %q", rec.Code, rec.Body.String())
	}
}

// 无论产物是否已构建，Console / Admin 都能构造并响应（200 或 503），不会 panic。
func TestConsoleAndAdminHandlers(t *testing.T) {
	for name, h := range map[string]http.Handler{"console": web.Console(), "admin": web.Admin()} {
		rec := serve(h, "/admin/x")
		if rec.Code != 200 && rec.Code != 503 {
			t.Fatalf("%s: %d", name, rec.Code)
		}
	}
}
