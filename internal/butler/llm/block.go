// Package llm 是 AI 管家与模型后端之间的接口层（与具体提供方无关的消息表示、Provider 接口）。
// 本包是叶子包：只依赖标准库，butler / tools / 各适配器都导入它，它不导入任何 butler 子包。
package llm

import "encoding/json"

// 消息角色（butler_messages.role）。
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// 消息内容块类型（规格 §2.1）。
const (
	BlockText       = "text"        // user / assistant 文本
	BlockToolUse    = "tool_use"    // assistant 发起的工具调用（可多块）
	BlockToolResult = "tool_result" // tool 消息：工具结果（content 为 <untrusted_data> 包裹后的文本）
	BlockContext    = "context"     // user 消息的可选上下文（{instance_id}），不作为用户文本给模型
)

// Block 是 butler_messages.content 数组的一项，与 LLM 适配器无关；适配器负责双向转换。
// tool_use 块的 ToolCallID 即 butler_tool_calls.id（JSON 键 tool_call_id），ProviderCallID 是模型给的调用 ID。
type Block struct {
	Type           string          `json:"type"`
	Text           string          `json:"text,omitempty"`
	ToolCallID     string          `json:"tool_call_id,omitempty"`
	ProviderCallID string          `json:"provider_call_id,omitempty"`
	Name           string          `json:"name,omitempty"`
	Args           json.RawMessage `json:"args,omitempty"`
	OK             bool            `json:"ok,omitempty"`
	Content        string          `json:"content,omitempty"`
	InstanceID     string          `json:"instance_id,omitempty"`
}
