package llmgw

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"spark-cloud/internal/llm"
	"spark-cloud/internal/platform/httpx"
)

// keyRandLen 是 Key 前缀之后的 base62 位数（规格 T5）。
const keyRandLen = 40

// validKeyFormat 'sk-spark-' + 40 位 base62。格式不对不查库、不算哈希。
func validKeyFormat(k string) bool {
	if len(k) != len(llm.KeyPrefix)+keyRandLen || !strings.HasPrefix(k, llm.KeyPrefix) {
		return false
	}
	for i := len(llm.KeyPrefix); i < len(k); i++ {
		c := k[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

// bearer 取 Authorization: Bearer <token>（scheme 大小写不敏感）。
func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) < 7 || !strings.EqualFold(h[:7], "bearer ") {
		return ""
	}
	return strings.TrimSpace(h[7:])
}

// authenticate 规格 §6.1 第 2～3 步：401 的按 IP 限流（在鉴权之前）→ Key 格式 → llm.Authorize（Key / 用户 / 账户门槛）。
// 失败时已写好错误响应，返回 ok=false；401 计入该 IP 的失败次数。
func (g *Server) authenticate(w http.ResponseWriter, r *http.Request) (llm.AuthContext, bool) {
	ip := httpx.ClientIP(r)
	now := g.clk.Now()
	if blocked, wait := g.authLim.blocked(ip, now); blocked {
		setRetryAfter(w, wait)
		writeErr(w, http.StatusTooManyRequests, codeRateLimitExceeded, "认证失败次数过多，请稍后再试")
		return llm.AuthContext{}, false
	}
	key := bearer(r)
	if !validKeyFormat(key) {
		g.authLim.fail(ip, now)
		msg := "API Key 格式不正确"
		if key == "" {
			msg = "缺少 API Key：请在请求头加 Authorization: Bearer sk-spark-…"
		}
		writeErr(w, http.StatusUnauthorized, codeInvalidAPIKey, msg)
		return llm.AuthContext{}, false
	}
	ac, err := g.svc.Authorize(r.Context(), llm.HashKey(key), now)
	if err != nil {
		var ae *llm.AuthError
		if errors.As(err, &ae) {
			if ae.Status == http.StatusUnauthorized {
				g.authLim.fail(ip, now)
			}
			writeOpenAIError(w, ae.Status, ae.Type, ae.Code, ae.Message, "")
			return llm.AuthContext{}, false
		}
		g.log.Error("llmgw: authorize failed", "err", err, "request_id", w.Header().Get("X-Request-Id"))
		writeErr(w, http.StatusInternalServerError, codeInternal, "服务内部错误，请稍后再试")
		return llm.AuthContext{}, false
	}
	return ac, true
}

// parsedRequest 是请求体 JSON 顶层里网关关心的字段；其它字段不解析不校验。
type parsedRequest struct {
	top          map[string]json.RawMessage
	model        string
	stream       bool
	maxTokens    *int // max_completion_tokens 优先，其次 max_tokens
	includeUsage bool // stream_options.include_usage == true
}

// parseRequest 解析顶层字段：model 必须是字符串；stream 布尔；max_tokens / max_completion_tokens 整数；
// stream_options 对象（include_usage 布尔）。null 视为未给。失败返回给用户看的中文消息。
func parseRequest(body []byte) (parsedRequest, string) {
	var p parsedRequest
	if err := json.Unmarshal(body, &p.top); err != nil || p.top == nil {
		return p, "请求体不是合法的 JSON 对象"
	}
	raw, ok := p.top["model"]
	if !ok || isNull(raw) {
		return p, "缺少 model 字段"
	}
	if err := json.Unmarshal(raw, &p.model); err != nil {
		return p, "model 必须是字符串"
	}
	if p.model == "" {
		return p, "缺少 model 字段"
	}
	if raw, ok := p.top["stream"]; ok && !isNull(raw) {
		if err := json.Unmarshal(raw, &p.stream); err != nil {
			return p, "stream 必须是布尔值"
		}
	}
	for _, f := range []string{"max_tokens", "max_completion_tokens"} {
		raw, ok := p.top[f]
		if !ok || isNull(raw) {
			continue
		}
		var n int
		if err := json.Unmarshal(raw, &n); err != nil {
			return p, f + " 必须是整数"
		}
		if f == "max_completion_tokens" || p.maxTokens == nil {
			p.maxTokens = &n
		}
	}
	if raw, ok := p.top["stream_options"]; ok && !isNull(raw) {
		var so map[string]json.RawMessage
		if err := json.Unmarshal(raw, &so); err != nil {
			return p, "stream_options 必须是对象"
		}
		if iu, ok := so["include_usage"]; ok && !isNull(iu) {
			if err := json.Unmarshal(iu, &p.includeUsage); err != nil {
				return p, "stream_options.include_usage 必须是布尔值"
			}
		}
	}
	return p, ""
}

func isNull(raw json.RawMessage) bool { return strings.TrimSpace(string(raw)) == "null" }

// upstreamBody 返回转发给节点的请求体：流式且没带 stream_options.include_usage=true 时注入（规格 T11，
// 顶层 map[string]json.RawMessage 补丁后重新编码，字段顺序变化无害）；否则原样。
func upstreamBody(body []byte, p parsedRequest) ([]byte, error) {
	if !p.stream || p.includeUsage {
		return body, nil
	}
	so := map[string]json.RawMessage{}
	if raw, ok := p.top["stream_options"]; ok && !isNull(raw) {
		if err := json.Unmarshal(raw, &so); err != nil {
			return nil, err
		}
	}
	so["include_usage"] = json.RawMessage("true")
	b, err := json.Marshal(so)
	if err != nil {
		return nil, err
	}
	top := make(map[string]json.RawMessage, len(p.top)+1)
	for k, v := range p.top {
		top[k] = v
	}
	top["stream_options"] = b
	return json.Marshal(top)
}

// modelsResponse 是 GET /v1/models 的响应体。
type modelsResponse struct {
	Object string        `json:"object"`
	Data   []modelObject `json:"data"`
}

type modelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// handleModels 规格 §6.2：鉴权 + 门槛后列出该 Key 允许的在线模型（不要求有 ready 节点）。
func (g *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	ac, ok := g.authenticate(w, r)
	if !ok {
		return
	}
	list, _, err := g.onlineModels(r.Context())
	if err != nil {
		g.log.Error("llmgw: list online models failed", "err", err)
		writeErr(w, http.StatusInternalServerError, codeInternal, "服务内部错误，请稍后再试")
		return
	}
	out := modelsResponse{Object: "list", Data: []modelObject{}}
	for _, m := range list {
		if ac.Key.KeyAllowsModel(m.Slug) {
			out.Data = append(out.Data, modelObject{ID: m.Slug, Object: "model", Created: m.CreatedAt.Unix(), OwnedBy: "spark-cloud"})
		}
	}
	b, _ := json.Marshal(out)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}
