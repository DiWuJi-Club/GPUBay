package httpx_test

import (
	"net/http"
	"net/http/httptest"
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
