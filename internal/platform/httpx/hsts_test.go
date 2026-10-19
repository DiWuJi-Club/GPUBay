package httpx_test

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"

	"spark-cloud/internal/platform/httpx"
)

func TestHSTSOnlyOnTLS(t *testing.T) {
	h := httpx.HSTS(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	plain := httptest.NewRecorder()
	h.ServeHTTP(plain, httptest.NewRequest("GET", "http://x/", nil))
	if v := plain.Header().Get("Strict-Transport-Security"); v != "" {
		t.Fatalf("hsts on plain http = %q", v)
	}
	req := httptest.NewRequest("GET", "https://x/", nil)
	req.TLS = &tls.ConnectionState{}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if v := rec.Header().Get("Strict-Transport-Security"); v != "max-age=31536000" {
		t.Fatalf("hsts on tls = %q", v)
	}
}
