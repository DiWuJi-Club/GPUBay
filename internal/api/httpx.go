package api

import (
	"encoding/json"
	"io"
	"net"
	"net/http"

	"github.com/go-chi/chi/v5/middleware"

	"spark-cloud/internal/platform/apperr"
)

type errorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func ok(w http.ResponseWriter) { writeJSON(w, http.StatusOK, map[string]bool{"ok": true}) }

// list 包装列表响应，保证空列表输出 [] 而不是 null。
func list[T any](items []T) map[string]any {
	if items == nil {
		items = []T{}
	}
	return map[string]any{"items": items}
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	rid := middleware.GetReqID(r.Context())
	if e := apperr.As(err); e != nil {
		writeJSON(w, e.Status, errorBody{Code: e.Code, Message: e.Message, RequestID: rid})
		return
	}
	s.Log.Error("internal error", "err", err, "request_id", rid, "method", r.Method, "path", r.URL.Path)
	writeInternal(w, rid)
}

// writeInternal 输出未知错误的统一响应，不暴露内部细节。
func writeInternal(w http.ResponseWriter, rid string) {
	writeJSON(w, http.StatusInternalServerError, errorBody{Code: "internal", Message: "服务器内部错误，请稍后重试", RequestID: rid})
}

// decodeJSON 解析请求体（上限 1MB），拒绝未知字段。
func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return apperr.BadRequest("请求参数格式不正确")
	}
	return nil
}

func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
