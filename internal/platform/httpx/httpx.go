// Package httpx 是用户 API 与后台 API 共用的 HTTP 辅助：JSON 读写、统一错误格式、panic 恢复、缓存控制与客户端 IP。
package httpx

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"

	"github.com/go-chi/chi/v5/middleware"

	"spark-cloud/internal/platform/apperr"
)

type ErrorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func OK(w http.ResponseWriter) { WriteJSON(w, http.StatusOK, map[string]bool{"ok": true}) }

// List 包装列表响应，保证空列表输出 [] 而不是 null。
func List[T any](items []T) map[string]any {
	if items == nil {
		items = []T{}
	}
	return map[string]any{"items": items}
}

// Page 包装分页列表响应 {items, total}。
func Page[T any](items []T, total int) map[string]any {
	if items == nil {
		items = []T{}
	}
	return map[string]any{"items": items, "total": total}
}

// WriteError 输出业务错误；未知错误记日志并统一返回 500，不暴露内部细节。
func WriteError(log *slog.Logger, w http.ResponseWriter, r *http.Request, err error) {
	rid := middleware.GetReqID(r.Context())
	if e := apperr.As(err); e != nil {
		WriteJSON(w, e.Status, ErrorBody{Code: e.Code, Message: e.Message, RequestID: rid})
		return
	}
	log.Error("internal error", "err", err, "request_id", rid, "method", r.Method, "path", r.URL.Path)
	WriteInternal(w, rid)
}

func WriteInternal(w http.ResponseWriter, rid string) {
	WriteJSON(w, http.StatusInternalServerError, ErrorBody{Code: "internal", Message: "服务器内部错误，请稍后重试", RequestID: rid})
}

// DecodeJSON 解析请求体（上限 1MB），拒绝未知字段。
func DecodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return apperr.BadRequest("请求参数格式不正确")
	}
	return nil
}

func ClientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// Recoverer 捕获处理器中的 panic：记录日志（含堆栈）并按统一错误格式返回 500。
func Recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				if rec == http.ErrAbortHandler { //nolint:errorlint // 与 net/http 约定一致：继续向上抛出以中断连接
					panic(rec)
				}
				rid := middleware.GetReqID(r.Context())
				log.Error("panic", "panic", fmt.Sprint(rec), "stack", string(debug.Stack()),
					"request_id", rid, "method", r.Method, "path", r.URL.Path)
				WriteInternal(w, rid)
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// NoStore 禁止缓存响应（含密码、余额、个人信息等敏感数据）。
func NoStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
