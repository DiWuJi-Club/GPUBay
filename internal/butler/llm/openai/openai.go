// Package openai 是 OpenAI 兼容 Chat Completions 接口（流式）的 llm.Provider 适配器（规格 §3.2）。
// 只用 net/http，不引入 SDK：默认端点是计划 9 的内网模型网关（vLLM 等）。
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"spark-cloud/internal/butler/llm"
)

const (
	maxLineBytes    = 1 << 20 // SSE 单行上限 1MB
	maxErrBodyBytes = 4096
	defaultMaxToken = 2048
	temperature     = 0.2
	invalidArgRunes = 200
)

// Config 是适配器配置；BaseURL 含 /v1（如 http://host:8000/v1），请求发到 BaseURL + "/chat/completions"。
type Config struct {
	BaseURL   string
	APIKey    string
	Model     string
	MaxTokens int
	HTTP      *http.Client // nil → 无超时的 Client（超时由 ctx 控制）
}

// Provider 实现 llm.Provider。
type Provider struct {
	cfg  Config
	http *http.Client
}

func New(cfg Config) *Provider {
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 0}
	}
	return &Provider{cfg: cfg, http: hc}
}

func (p *Provider) Name() string  { return "openai" }
func (p *Provider) Model() string { return p.cfg.Model }

// ---- 请求体 ----

type reqBody struct {
	Model         string         `json:"model"`
	Messages      []reqMessage   `json:"messages"`
	Tools         []reqTool      `json:"tools,omitempty"`
	ToolChoice    string         `json:"tool_choice,omitempty"`
	Stream        bool           `json:"stream"`
	StreamOptions map[string]any `json:"stream_options"`
	MaxTokens     int            `json:"max_tokens"`
	Temperature   float64        `json:"temperature"`
}

type reqMessage struct {
	Role       string        `json:"role"`
	Content    *string       `json:"content,omitempty"`
	ToolCalls  []reqToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
}

type reqToolCall struct {
	ID       string      `json:"id"`
	Type     string      `json:"type"`
	Function reqFunction `json:"function"`
}

type reqFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type reqTool struct {
	Type     string     `json:"type"`
	Function reqToolDef `json:"function"`
}

type reqToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

func strp(s string) *string { return &s }

// callID 取模型给的调用 ID；历史里缺失时退回本系统的 tool_call_id（两端一致即可配对）。
func callID(b llm.Block) string { return llm.CallID(b) }

func (p *Provider) buildBody(req llm.Request) ([]byte, error) {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = p.cfg.MaxTokens
	}
	if maxTokens <= 0 {
		maxTokens = defaultMaxToken
	}
	body := reqBody{
		Model:         p.cfg.Model,
		Stream:        true,
		StreamOptions: map[string]any{"include_usage": true},
		MaxTokens:     maxTokens,
		Temperature:   temperature,
		Messages:      []reqMessage{},
	}
	// 固定前缀 + 动态快照段拼成一条 system 消息（OpenAI 兼容端点只认一条 system）
	sys := req.System
	if req.SystemDynamic != "" {
		if sys != "" {
			sys += "\n\n"
		}
		sys += req.SystemDynamic
	}
	if sys != "" {
		body.Messages = append(body.Messages, reqMessage{Role: "system", Content: strp(sys)})
	}
	// 防御：每个 tool_call 必须紧跟其结果，孤立结果丢弃（否则端点 400）
	for _, m := range llm.PairToolCalls(req.Messages) {
		body.Messages = append(body.Messages, mapMessage(m)...)
	}
	// 裁定：Tools 为空（SPARK_LLM_TOOLS=off）时不发 tools / tool_choice
	if len(req.Tools) > 0 {
		for _, t := range req.Tools {
			params := t.Schema
			if len(params) == 0 {
				params = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			body.Tools = append(body.Tools, reqTool{Type: "function", Function: reqToolDef{Name: t.Name, Description: t.Description, Parameters: params}})
		}
		body.ToolChoice = "auto"
	}
	// 只有工具 Schema / 历史 Args 不是合法 JSON 时会失败：属于本地配置 / 数据错误，不是模型不可用
	return json.Marshal(body)
}

// mapMessage 把一条内部消息映射为 0..n 条 OpenAI 消息：user / assistant 一条，tool 每个结果一条。
func mapMessage(m llm.Message) []reqMessage {
	switch m.Role {
	case llm.RoleUser:
		var parts []string
		for _, b := range m.Blocks {
			if b.Type == llm.BlockText { // context 块不作为用户文本
				parts = append(parts, b.Text)
			}
		}
		return []reqMessage{{Role: "user", Content: strp(strings.Join(parts, "\n"))}}
	case llm.RoleAssistant:
		var text strings.Builder
		out := reqMessage{Role: "assistant"}
		for _, b := range m.Blocks {
			switch b.Type {
			case llm.BlockText:
				text.WriteString(b.Text)
			case llm.BlockToolUse:
				args := string(b.Args)
				if strings.TrimSpace(args) == "" {
					args = "{}"
				}
				out.ToolCalls = append(out.ToolCalls, reqToolCall{ID: callID(b), Type: "function", Function: reqFunction{Name: b.Name, Arguments: args}})
			}
		}
		if text.Len() > 0 || len(out.ToolCalls) == 0 {
			out.Content = strp(text.String())
		}
		return []reqMessage{out}
	case llm.RoleTool:
		var out []reqMessage
		for _, b := range m.Blocks {
			if b.Type == llm.BlockToolResult {
				out = append(out, reqMessage{Role: "tool", ToolCallID: callID(b), Content: strp(b.Content)})
			}
		}
		return out
	}
	return nil
}

// ---- 流式响应 ----

type chunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage"`
	// 有的实现用 {"error": {...}} 或 {"error": "…"}，vLLM 用 {"object": "error", "message": "…"}
	Object  string          `json:"object"`
	Message string          `json:"message"`
	Error   json.RawMessage `json:"error"`
}

// streamError 返回 chunk 里携带的错误描述；没有错误 → ""。
func (c *chunk) streamError() string {
	if c.Object == "error" {
		if c.Message != "" {
			return c.Message
		}
		return "模型返回错误"
	}
	raw := strings.TrimSpace(string(c.Error))
	if raw == "" || raw == "null" {
		return ""
	}
	var str string
	if json.Unmarshal(c.Error, &str) == nil {
		if str == "" {
			return "模型返回错误"
		}
		return str
	}
	var obj struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(c.Error, &obj) == nil && obj.Message != "" {
		return obj.Message
	}
	return raw
}

type partialCall struct {
	id, name string
	args     strings.Builder
}

// validOrMarker：参数是合法 JSON 对象 → 原文；空 → {}；否则 {"_invalid_json": "<前 200 rune>"}（Run 层据此回填错误）。
func validOrMarker(s string) json.RawMessage {
	t := strings.TrimSpace(s)
	if t == "" {
		return json.RawMessage(`{}`)
	}
	if json.Valid([]byte(t)) && strings.HasPrefix(t, "{") {
		return json.RawMessage(t)
	}
	r := []rune(t)
	if len(r) > invalidArgRunes {
		r = r[:invalidArgRunes]
	}
	b, _ := json.Marshal(map[string]string{"_invalid_json": string(r)})
	return b
}

func (p *Provider) Chat(ctx context.Context, req llm.Request, emit func(llm.Event)) error {
	body, err := p.buildBody(req)
	if err != nil {
		return &llm.RequestError{Status: 0, Body: "请求体序列化失败: " + err.Error()}
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return &llm.UnavailableError{Cause: err}
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "text/event-stream")
	if p.cfg.APIKey != "" {
		hreq.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}
	resp, err := p.http.Do(hreq)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &llm.UnavailableError{Cause: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBodyBytes))
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			return &llm.UnavailableError{Status: resp.StatusCode, Cause: errors.New(string(b))}
		}
		return &llm.RequestError{Status: resp.StatusCode, Body: string(b)}
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	// 调用按出现顺序放进 calls；cur 把增量里的 index 映射到当前累积中的调用。
	// 有的实现并行调用都不带 index（默认 0）：同一 index 上出现另一个非空 id → 视为新调用。
	var calls []*partialCall
	cur := map[int]int{}
	var usage *llm.Usage
	stop := llm.StopEndTurn
	finished := false // 收到 [DONE] 或 finish_reason；都没有就断流 → 不可用
	flushCalls := func() {
		for i, c := range calls {
			id := c.id
			if id == "" {
				id = fmt.Sprintf("call_%d", i)
			}
			emit(llm.Event{Type: llm.EvTool, ToolCall: &llm.ToolCall{ProviderCallID: id, Name: c.name, Args: validOrMarker(c.args.String())}})
		}
		calls = nil
		cur = map[int]int{}
	}
	for sc.Scan() {
		line := sc.Text() // ScanLines 已去掉行尾 \r（CRLF）
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(line[len("data:"):])
		if data == "[DONE]" {
			finished = true
			break
		}
		var ch chunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			continue
		}
		if msg := ch.streamError(); msg != "" {
			return &llm.UnavailableError{Cause: errors.New(msg)}
		}
		if ch.Usage != nil {
			usage = &llm.Usage{InputTokens: ch.Usage.PromptTokens, OutputTokens: ch.Usage.CompletionTokens}
		}
		for _, c := range ch.Choices {
			if c.Delta.Content != "" {
				emit(llm.Event{Type: llm.EvText, Text: c.Delta.Content})
			}
			for _, tc := range c.Delta.ToolCalls {
				pos, ok := cur[tc.Index]
				if ok && tc.ID != "" && calls[pos].id != "" && calls[pos].id != tc.ID {
					ok = false
				}
				if !ok {
					calls = append(calls, &partialCall{})
					pos = len(calls) - 1
					cur[tc.Index] = pos
				}
				pc := calls[pos]
				if tc.ID != "" {
					pc.id = tc.ID
				}
				if tc.Function.Name != "" {
					pc.name = tc.Function.Name
				}
				pc.args.WriteString(tc.Function.Arguments)
			}
			switch c.FinishReason {
			case "":
			case "tool_calls", "function_call":
				stop = llm.StopToolUse
				finished = true
				flushCalls()
			case "length":
				// 输出被 max_tokens 截断：半截的工具调用直接丢弃（参数不完整，不能执行），停止原因保持 max_tokens
				stop = llm.StopMaxTokens
				finished = true
				calls = nil
				cur = map[int]int{}
			case "content_filter":
				stop = llm.StopRefusal
				finished = true
			default: // stop 及未知值
				stop = llm.StopEndTurn
				finished = true
			}
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// finish_reason 已到达后尾部读错误（连接被对端提前关闭等）不影响结果，照常收尾
	if err := sc.Err(); err != nil && !finished {
		return &llm.UnavailableError{Cause: err}
	}
	if !finished {
		return &llm.UnavailableError{Cause: errors.New("模型响应流意外结束")}
	}
	if len(calls) > 0 { // 某些实现不发 finish_reason=tool_calls
		stop = llm.StopToolUse
		flushCalls()
	}
	if usage != nil {
		emit(llm.Event{Type: llm.EvUsage, Usage: usage})
	}
	emit(llm.Event{Type: llm.EvDone, StopReason: stop})
	return nil
}
