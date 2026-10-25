package butler

// Sink 接收一轮对话推给前端的事件（规格 §5.4）。HTTP 的 SSE 实现在 api 包（sseWriter）：写失败后静默丢弃，
// 轮次照常跑完并落库。Emit 不能阻塞太久，也不能 panic；v 由实现方 json.Marshal。
type Sink interface {
	Emit(event string, v any)
}

// NopSink 丢弃所有事件（后台任务 / 测试用）。
type NopSink struct{}

func (NopSink) Emit(string, any) {}

// SSE 事件名。
const (
	EvStart   = "start"
	EvDelta   = "delta"
	EvTool    = "tool"
	EvConfirm = "confirm"
	EvUsage   = "usage"
	EvDone    = "done"
	EvError   = "error"
)

// done 事件的 reason。
const (
	DoneEndTurn         = "end_turn"
	DoneAwaitingConfirm = "awaiting_confirm"
	DoneMaxTokens       = "max_tokens"
	DoneIterationLimit  = "iteration_limit"
	DoneRefusal         = "refusal"
)

// 各事件的载荷（字段名即前端 web/shared/src/types/butler.ts 的约定）。

// StartEvent：Send 时 message_id = 本条 user 消息 ID；Confirm 时为 0（不输出）。
type StartEvent struct {
	ConversationID string `json:"conversation_id"`
	MessageID      int64  `json:"message_id,omitempty"`
}

type DeltaEvent struct {
	Text string `json:"text"`
}

// ToolEvent：读工具执行完、写工具确认执行完（以及写工具未能生成确认卡）各一条；status = ok | error，失败时带 message。
type ToolEvent struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Label   string `json:"label"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

type ConfirmEvent struct {
	ToolCall ConfirmView `json:"tool_call"`
}

// UsageEvent：每次模型请求后一条；day_used = 今日已用（含本次），day_limit = 每日限额。
type UsageEvent struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	DayUsed      int64 `json:"day_used"`
	DayLimit     int64 `json:"day_limit"`
}

type DoneEvent struct {
	Reason string `json:"reason"`
}

type ErrorEvent struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
