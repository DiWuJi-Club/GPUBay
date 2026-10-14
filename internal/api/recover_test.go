package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5/middleware"
)

func TestRecovererWritesJSONErrorAndLogs(t *testing.T) {
	var logBuf bytes.Buffer
	s := &Server{Log: slog.New(slog.NewTextHandler(&logBuf, nil))}
	h := middleware.RequestID(s.recoverer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/x", nil))

	if rec.Code != http.StatusInternalServerError || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("status = %d, content-type = %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	if body.Code != "internal" || body.Message != "服务器内部错误，请稍后重试" || body.RequestID == "" {
		t.Fatalf("body = %+v", body)
	}
	if log := logBuf.String(); !strings.Contains(log, "boom") || !strings.Contains(log, "goroutine") {
		t.Fatalf("log missing panic value or stack: %s", log)
	}
}
