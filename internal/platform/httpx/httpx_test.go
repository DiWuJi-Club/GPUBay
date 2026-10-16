package httpx_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5/middleware"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/httpx"
)

func TestRecovererWritesJSONErrorAndLogs(t *testing.T) {
	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, nil))
	h := middleware.RequestID(httpx.Recoverer(log)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Code != http.StatusInternalServerError || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("status = %d, content-type = %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	var body httpx.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	if body.Code != "internal" || body.Message != "服务器内部错误，请稍后重试" || body.RequestID == "" {
		t.Fatalf("body = %+v", body)
	}
	if out := logBuf.String(); !strings.Contains(out, "boom") || !strings.Contains(out, "goroutine") {
		t.Fatalf("log missing panic value or stack: %s", out)
	}
}

func TestWriteErrorMapsBusinessErrors(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := httptest.NewRecorder()
	httpx.WriteError(log, rec, httptest.NewRequest("GET", "/", nil), apperr.ErrNotFound)
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), `"code":"not_found"`) {
		t.Fatalf("business error = %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	httpx.WriteError(log, rec, httptest.NewRequest("GET", "/", nil), errors.New("db down"))
	if rec.Code != 500 || strings.Contains(rec.Body.String(), "db down") {
		t.Fatalf("internal error = %d %s", rec.Code, rec.Body.String())
	}
}

func TestPageAndListNeverNull(t *testing.T) {
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(httpx.Page[int](nil, 0))
	if strings.TrimSpace(buf.String()) != `{"items":[],"total":0}` {
		t.Fatalf("page = %s", buf.String())
	}
	buf.Reset()
	_ = json.NewEncoder(&buf).Encode(httpx.List[string](nil))
	if strings.TrimSpace(buf.String()) != `{"items":[]}` {
		t.Fatalf("list = %s", buf.String())
	}
}

func TestDecodeJSONRejectsUnknownFields(t *testing.T) {
	var v struct {
		A int `json:"a"`
	}
	req := httptest.NewRequest("POST", "/", strings.NewReader(`{"a":1,"b":2}`))
	if err := httpx.DecodeJSON(req, &v); apperr.As(err) == nil || apperr.As(err).Code != "bad_request" {
		t.Fatalf("unknown field = %v", err)
	}
}
