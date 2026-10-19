package httpx_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"spark-cloud/internal/platform/httpx"
)

func TestProxyRealIP(t *testing.T) {
	for _, tc := range []struct {
		name string
		hops int
		xff  []string
		want string
	}{
		{"no header", 1, nil, "192.0.2.1:1234"},
		{"empty header", 1, []string{""}, "192.0.2.1:1234"},
		{"invalid rightmost", 1, []string{"203.0.113.9, not-an-ip"}, "192.0.2.1:1234"},
		{"single", 1, []string{"203.0.113.9"}, "203.0.113.9:0"},
		{"rightmost of many", 1, []string{"6.6.6.6, 10.0.0.1, 203.0.113.9"}, "203.0.113.9:0"},
		{"multiple header lines", 1, []string{"6.6.6.6", "203.0.113.9"}, "203.0.113.9:0"},
		{"ipv6", 1, []string{"6.6.6.6, 2001:db8::1"}, "[2001:db8::1]:0"},
		{"hops=2", 2, []string{"6.6.6.6, 203.0.113.9, 10.0.0.2"}, "203.0.113.9:0"},
		{"hops=2 too few entries", 2, []string{"203.0.113.9"}, "192.0.2.1:1234"},
		{"hops=0 treated as 1", 0, []string{"6.6.6.6, 203.0.113.9"}, "203.0.113.9:0"},
		{"ipv4 with port", 1, []string{"6.6.6.6, 203.0.113.9:5678"}, "203.0.113.9:0"},
		{"ipv6 with port", 1, []string{"6.6.6.6, [2001:db8::1]:443"}, "[2001:db8::1]:0"},
		{"hostname with port", 1, []string{"6.6.6.6, proxy.local:80"}, "192.0.2.1:1234"},
		{"garbage port form", 1, []string{"1.2.3.4:x"}, "192.0.2.1:1234"},
	} {
		var got string
		h := httpx.ProxyRealIP(tc.hops)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.RemoteAddr }))
		req := httptest.NewRequest("GET", "http://x/", nil)
		req.RemoteAddr = "192.0.2.1:1234"
		for _, v := range tc.xff {
			req.Header.Add("X-Forwarded-For", v)
		}
		req.Header.Set("X-Real-IP", "7.7.7.7")
		req.Header.Set("True-Client-IP", "7.7.7.7")
		h.ServeHTTP(httptest.NewRecorder(), req)
		if got != tc.want {
			t.Errorf("%s: RemoteAddr = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// 最右项解析不了：保留连接地址，每个中间件实例只 warn 一次。
func TestProxyRealIPWarnsOnceOnUnparseable(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	h := httpx.ProxyRealIP(1)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest("GET", "http://x/", nil)
		req.Header.Set("X-Forwarded-For", "not-an-ip")
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	req := httptest.NewRequest("GET", "http://x/", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if n := strings.Count(buf.String(), "not an IP"); n != 1 {
		t.Fatalf("warn count = %d, log:\n%s", n, buf.String())
	}
}
