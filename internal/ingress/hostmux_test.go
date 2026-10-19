package ingress_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"spark-cloud/internal/ingress"
)

func TestHostMuxSplitsByHost(t *testing.T) {
	tag := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(name)) })
	}
	h := ingress.HostMux("spark.test", tag("ingress"), tag("console"))
	for host, want := range map[string]string{
		"spark.test": "console", "127.0.0.1:18080": "console", "spark.test:18080": "console", "ssh.spark.test": "console",
		"ins-a1b2c3d4-8888.spark.test": "ingress", "INS-a1b2c3d4-80.spark.test:18080": "ingress", "ins-a1b2c3d4-8888.other.test": "console",
	} {
		req := httptest.NewRequest("GET", "http://x/", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Body.String() != want {
			t.Fatalf("host %q → %q, want %q", host, rec.Body.String(), want)
		}
	}
}
