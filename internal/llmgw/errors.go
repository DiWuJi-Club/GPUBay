package llmgw

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// OpenAI 错误类型（规格 §6.3）。
const (
	typeInvalidRequest = "invalid_request_error"
	typeAuthentication = "authentication_error"
	typePermission     = "permission_error"
	typeQuota          = "insufficient_quota"
	typeRateLimit      = "rate_limit_error"
	typeServer         = "server_error"
)

// 网关自己产生的 error.code（鉴权 / 门槛的 code 来自 llm.AuthError，限流的来自 llm.LimitError）。
const (
	codeInvalidRequest     = "invalid_request_error"
	codeInvalidAPIKey      = "invalid_api_key"
	codeRequestTooLarge    = "request_too_large"
	codeModelNotFound      = "model_not_found"
	codeModelNotAllowed    = "model_not_allowed"
	codeMonthlyCapExceeded = "monthly_cap_exceeded"
	codeRateLimitExceeded  = "rate_limit_exceeded"
	codeInsufficientQuota  = "insufficient_quota"
	codeModelUnavailable   = "model_unavailable"
	codeServerOverloaded   = "server_overloaded"
	codeUpstreamError      = "upstream_error"
	codeUpstreamTimeout    = "upstream_timeout"
	codeNotFound           = "not_found"
	codeMethodNotAllowed   = "method_not_allowed"
	codeInternal           = "internal_error"
)

// codeType 是 code → type 映射表（writeErr 用；llm.AuthError 自带 type，直接用它的）。
var codeType = map[string]string{
	codeInvalidRequest:     typeInvalidRequest,
	codeRequestTooLarge:    typeInvalidRequest,
	codeModelNotFound:      typeInvalidRequest,
	codeNotFound:           typeInvalidRequest,
	codeMethodNotAllowed:   typeInvalidRequest,
	codeInvalidAPIKey:      typeAuthentication,
	codeModelNotAllowed:    typePermission,
	"account_banned":       typePermission,
	"kyc_required":         typePermission,
	"account_blocked":      typePermission,
	codeInsufficientQuota:  typeQuota,
	codeMonthlyCapExceeded: typeRateLimit,
	codeRateLimitExceeded:  typeRateLimit,
	codeModelUnavailable:   typeServer,
	codeServerOverloaded:   typeServer,
	codeUpstreamError:      typeServer,
	codeUpstreamTimeout:    typeServer,
	codeInternal:           typeServer,
}

// errorBody 是 OpenAI 错误体 {"error":{"message","type","code"}}。
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

func errorJSON(typ, code, msg string) []byte {
	b, _ := json.Marshal(errorBody{Error: errorDetail{Message: msg, Type: typ, Code: code}})
	return b
}

// writeOpenAIError 写 OpenAI 格式错误体；reqID 非空时补 X-Request-Id 头（入口中间件已设置的不覆盖）。
func writeOpenAIError(w http.ResponseWriter, status int, typ, code, msg string, reqID string) {
	h := w.Header()
	if reqID != "" && h.Get("X-Request-Id") == "" {
		h.Set("X-Request-Id", reqID)
	}
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(errorJSON(typ, code, msg))
}

// writeErr 按映射表取 type 写错误体。
func writeErr(w http.ResponseWriter, status int, code, msg string) {
	typ, ok := codeType[code]
	if !ok {
		typ = typeServer
	}
	writeOpenAIError(w, status, typ, code, msg, "")
}

// setRetryAfter 写 Retry-After（整秒，至少 1）。
func setRetryAfter(w http.ResponseWriter, d time.Duration) {
	s := int((d + time.Second - 1) / time.Second)
	if s < 1 {
		s = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(s))
}
