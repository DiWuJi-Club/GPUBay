package butler

import (
	"context"
	"encoding/json"
	"time"
	"unicode/utf8"

	"spark-cloud/internal/butler/guard"
	"spark-cloud/internal/butler/tools"
)

// 控制台 / 后台渲染对话用的视图（JSON 字段即 web/shared/src/types/butler.ts 的约定）。api 与 adminapi 直接序列化它们。

// SummaryBytes 是 tool_result 摘要的上限（去掉 <untrusted_data> 包裹后按 rune 截断）。
const SummaryBytes = 2048

// ConversationBrief 是对话列表项。
type ConversationBrief struct {
	ID            string     `json:"id"`
	Title         string     `json:"title"`
	Status        string     `json:"status"`
	LastMessageAt *time.Time `json:"last_message_at"`
	CreatedAt     time.Time  `json:"created_at"`
}

// ConversationView 是会话详情里的 conversation。
type ConversationView struct {
	ConversationBrief
	Context   Context   `json:"context"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (c Conversation) Brief() ConversationBrief {
	return ConversationBrief{ID: c.ID, Title: c.Title, Status: c.Status, LastMessageAt: c.LastMessageAt, CreatedAt: c.CreatedAt}
}

func (c Conversation) View() ConversationView {
	return ConversationView{ConversationBrief: c.Brief(), Context: c.Context, UpdatedAt: c.UpdatedAt}
}

// ViewBlock 是前端消息块：text {text} / tool {id, name, label, status} / tool_result {tool_call_id, summary}。
type ViewBlock struct {
	Type       string `json:"type"`
	Text       string `json:"text,omitempty"`
	ID         string `json:"id,omitempty"`
	Name       string `json:"name,omitempty"`
	Label      string `json:"label,omitempty"`
	Status     string `json:"status,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	Summary    string `json:"summary,omitempty"`
}

// MessageView 是控制台会话详情的一条消息。
type MessageView struct {
	ID        int64       `json:"id"`
	Seq       int         `json:"seq"`
	Role      string      `json:"role"`
	Content   []ViewBlock `json:"content"`
	CreatedAt time.Time   `json:"created_at"`
}

// AdminBlock 是后台会话详情的消息块：控制台的块 + context {instance_id} + tool_result 的 ok / suspicious。
type AdminBlock struct {
	Type       string `json:"type"`
	Text       string `json:"text,omitempty"`
	ID         string `json:"id,omitempty"`
	Name       string `json:"name,omitempty"`
	Label      string `json:"label,omitempty"`
	Status     string `json:"status,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	OK         *bool  `json:"ok,omitempty"`
	Summary    string `json:"summary,omitempty"`
	Suspicious *bool  `json:"suspicious,omitempty"`
	InstanceID string `json:"instance_id,omitempty"`
}

// AdminMessageView 是后台会话详情的一条消息（多一个 tokens）。
type AdminMessageView struct {
	ID        int64        `json:"id"`
	Seq       int          `json:"seq"`
	Role      string       `json:"role"`
	Tokens    int          `json:"tokens"`
	Content   []AdminBlock `json:"content"`
	CreatedAt time.Time    `json:"created_at"`
}

// ResultSummary 是工具结果的展示摘要：去掉最外层 <untrusted_data> 包裹，截到 SummaryBytes 字节（不切断 UTF-8）。
func ResultSummary(content string) string {
	s := StripWrapper(content)
	if len(s) <= SummaryBytes {
		return s
	}
	cut := SummaryBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// toolStatuses 计算每个 tool_use 的前端状态（ok | error | rejected | expired | pending | running）：
// 写调用按 butler_tool_calls.status（executed → ok，failed → error，rejected / expired 原样，pending / approved → pending）；
// 读调用（及没有行的调用）按对应 tool_result.ok；还没有结果时对话 running → running，否则 error。
func toolStatuses(msgs []Message, calls []ToolCall, convStatus string) map[string]string {
	results := map[string]bool{}
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == BlockToolResult {
				results[b.ToolCallID] = b.OK
			}
		}
	}
	rows := map[string]ToolCall{}
	for _, c := range calls {
		rows[c.ID] = c
	}
	out := map[string]string{}
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type != BlockToolUse {
				continue
			}
			id := b.ToolCallID
			if row, ok := rows[id]; ok && row.Kind == KindWrite {
				switch row.Status {
				case CallExecuted:
					out[id] = "ok"
				case CallFailed:
					out[id] = "error"
				case CallRejected, CallExpired:
					out[id] = row.Status
				default: // pending / approved
					out[id] = "pending"
				}
				continue
			}
			ok, has := results[id]
			switch {
			case has && ok:
				out[id] = "ok"
			case !has && convStatus == ConvRunning:
				out[id] = "running"
			default:
				out[id] = "error"
			}
		}
	}
	return out
}

// MessageViews 把落库消息转为控制台块：tool_use → tool（status 见 toolStatuses；calls 为该对话的工具调用行），
// tool_result → 摘要，context 块不输出。
func MessageViews(msgs []Message, calls []ToolCall, convStatus string) []MessageView {
	st := toolStatuses(msgs, calls, convStatus)
	out := make([]MessageView, 0, len(msgs))
	for _, m := range msgs {
		v := MessageView{ID: m.ID, Seq: m.Seq, Role: m.Role, Content: []ViewBlock{}, CreatedAt: m.CreatedAt}
		for _, b := range m.Content {
			switch b.Type {
			case BlockText:
				v.Content = append(v.Content, ViewBlock{Type: BlockText, Text: b.Text})
			case BlockToolUse:
				v.Content = append(v.Content, ViewBlock{Type: "tool", ID: b.ToolCallID, Name: b.Name, Label: tools.Label(b.Name), Status: st[b.ToolCallID]})
			case BlockToolResult:
				v.Content = append(v.Content, ViewBlock{Type: BlockToolResult, ToolCallID: b.ToolCallID, Summary: ResultSummary(b.Content)})
			}
		}
		out = append(out, v)
	}
	return out
}

// AdminMessageViews 把落库消息转为后台块（全部消息、全文文本、context 块、tool_result 的 ok 与可疑标记）。
func AdminMessageViews(msgs []Message, calls []ToolCall, convStatus string) []AdminMessageView {
	st := toolStatuses(msgs, calls, convStatus)
	out := make([]AdminMessageView, 0, len(msgs))
	for _, m := range msgs {
		v := AdminMessageView{ID: m.ID, Seq: m.Seq, Role: m.Role, Tokens: m.Tokens, Content: []AdminBlock{}, CreatedAt: m.CreatedAt}
		for _, b := range m.Content {
			switch b.Type {
			case BlockText:
				v.Content = append(v.Content, AdminBlock{Type: BlockText, Text: b.Text})
			case BlockToolUse:
				v.Content = append(v.Content, AdminBlock{Type: "tool", ID: b.ToolCallID, Name: b.Name, Label: tools.Label(b.Name), Status: st[b.ToolCallID]})
			case BlockToolResult:
				ok, sus := b.OK, guard.Suspicious(b.Content)
				v.Content = append(v.Content, AdminBlock{Type: BlockToolResult, ToolCallID: b.ToolCallID, OK: &ok, Summary: ResultSummary(b.Content), Suspicious: &sus})
			case BlockContext:
				v.Content = append(v.Content, AdminBlock{Type: BlockContext, InstanceID: b.InstanceID})
			}
		}
		out = append(out, v)
	}
	return out
}

// CallResultSummary 是后台工具调用表的 result_summary（result 列紧凑 JSON 去包裹、≤ 2KB；NULL → ""）；
// CallErrorCode 是 result 为错误对象 {code, message, ok:false} 时的 code。
func CallResultSummary(result json.RawMessage) string {
	if len(result) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(result, &s) == nil {
		return ResultSummary(s)
	}
	return ResultSummary(string(result))
}

func CallErrorCode(result json.RawMessage) string {
	code, _ := resultError(string(result))
	return code
}

// Detail 是控制台 GET /butler/conversations/{id} 的完整响应体。
type Detail struct {
	Conversation    ConversationView `json:"conversation"`
	Messages        []MessageView    `json:"messages"`
	PendingToolCall *ConfirmView     `json:"pending_tool_call"`
}

// Detail 组装用户自己的会话详情（消息块、待确认卡含真实 secret 文案与 expires_in）；他人 / 不存在 → not_found。
func (s *Service) Detail(ctx context.Context, userID int64, id string) (Detail, error) {
	conv, msgs, pending, err := s.GetConversation(ctx, userID, id)
	if err != nil {
		return Detail{}, err
	}
	calls, err := s.toolCalls(ctx, id)
	if err != nil {
		return Detail{}, err
	}
	d := Detail{Conversation: conv.View(), Messages: MessageViews(msgs, calls, conv.Status)}
	if pending != nil {
		v := s.ConfirmViewOf(*pending)
		d.PendingToolCall = &v
	}
	return d, nil
}
