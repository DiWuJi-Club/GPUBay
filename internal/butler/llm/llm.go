package llm

import (
	"context"
	"encoding/json"
	"fmt"
)

// Message 是送给模型的一条历史消息（规格 §2.1 的 Go 表示）。Role ∈ RoleUser / RoleAssistant / RoleTool。
type Message struct {
	Role   string
	Blocks []Block
}

// ToolDef 是一个工具的声明；Schema 为 JSON Schema（object，additionalProperties = false）。
type ToolDef struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

// Request 是一次模型调用。Tools 为空表示不开放工具（SPARK_LLM_TOOLS=off）。
// System 是固定前缀（可被提供方缓存）；SystemDynamic 是每轮重算的用户快照段（D10），不进缓存前缀。
type Request struct {
	System        string
	SystemDynamic string
	Messages      []Message
	Tools         []ToolDef
	MaxTokens     int
}

// ToolCall 是模型发起的一次工具调用；Args 已由适配器拼接为完整 JSON。
type ToolCall struct {
	ProviderCallID string
	Name           string
	Args           json.RawMessage
}

// Usage 是一次调用的 token 用量。
type Usage struct {
	InputTokens  int64
	OutputTokens int64
}

// Event 是流式事件；emit 顺序：EvText* / EvTool* / EvUsage / EvDone。
type Event struct {
	Type       string
	Text       string    // EvText
	ToolCall   *ToolCall // EvTool
	Usage      *Usage    // EvUsage
	StopReason string    // EvDone：StopEndTurn / StopToolUse / StopMaxTokens / StopRefusal
	Err        error
}

// 事件类型与停止原因。
const (
	EvText  = "text_delta"
	EvTool  = "tool_use"
	EvUsage = "usage"
	EvDone  = "done"

	StopEndTurn   = "end_turn"
	StopToolUse   = "tool_use"
	StopMaxTokens = "max_tokens"
	StopRefusal   = "refusal"
)

// Provider 是模型后端。Chat 阻塞直到流结束；返回的 error 为 *UnavailableError、*RequestError 或 ctx.Err()。
type Provider interface {
	Name() string
	Model() string
	Chat(ctx context.Context, req Request, emit func(Event)) error
}

// UnavailableError：连接失败 / 5xx / 429 / 流中断（规格 D16：对用户显示「暂时不可用」）。
type UnavailableError struct {
	Cause  error
	Status int // HTTP 状态码；连接错误时为 0
}

func (e *UnavailableError) Error() string {
	switch {
	case e.Status != 0 && e.Cause != nil:
		return fmt.Sprintf("LLM 不可用: HTTP %d: %v", e.Status, e.Cause)
	case e.Status != 0:
		return fmt.Sprintf("LLM 不可用: HTTP %d", e.Status)
	case e.Cause != nil:
		return "LLM 不可用: " + e.Cause.Error()
	}
	return "LLM 不可用"
}

func (e *UnavailableError) Unwrap() error { return e.Cause }

// RequestError：4xx（除 429），通常是配置错误（模型名、密钥）；Run 层记日志并按不可用处理。
type RequestError struct {
	Status int
	Body   string // 错误体前 ≤ 4KB
}

func (e *RequestError) Error() string {
	b := []rune(e.Body)
	if len(b) > 200 {
		b = append(b[:200], '…')
	}
	return fmt.Sprintf("LLM 请求错误: HTTP %d: %s", e.Status, string(b))
}
