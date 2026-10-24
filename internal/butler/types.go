// Package butler 实现 AI 管家：对话轮次、确认流程、用量与设置、清扫。
package butler

import (
	"encoding/json"
	"time"

	"spark-cloud/internal/butler/llm"
)

// Block 是消息内容块；定义在叶子包 llm（避免 butler ↔ llm 的导入环），这里只做别名。
type Block = llm.Block

// 内容块类型与消息角色，转出 llm 的同名常量。
const (
	BlockText       = llm.BlockText
	BlockToolUse    = llm.BlockToolUse
	BlockToolResult = llm.BlockToolResult
	BlockContext    = llm.BlockContext

	RoleUser      = llm.RoleUser
	RoleAssistant = llm.RoleAssistant
	RoleTool      = llm.RoleTool
)

// Conversation 对应 butler_conversations 一行。
type Conversation struct {
	ID            string
	UserID        int64
	Title         string
	Status        string // ConvIdle / ConvRunning / ConvAwaitingConfirm
	RunToken      string // running 时非空；轮次内所有回写都比对它
	RunStartedAt  *time.Time
	Context       Context
	Provider      string
	Model         string
	UserTurns     int
	InputTokens   int64
	OutputTokens  int64
	LastMessageAt *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Context 是对话最近一次消息带的上下文（butler_conversations.context）。
type Context struct {
	InstanceID string `json:"instance_id,omitempty"`
}

// Message 对应 butler_messages 一行。
type Message struct {
	ID             int64
	ConversationID string
	Seq            int
	Role           string // RoleUser / RoleAssistant / RoleTool
	Content        []Block
	Tokens         int
	CreatedAt      time.Time
}

// ToolCall 对应 butler_tool_calls 一行。Args 是已校验、secret 值已替换为 *** 的参数；
// SecretEnvEnc 是 secret 真实值（JSON 的 secretbox 密文），执行 / 拒绝 / 过期后清空。
type ToolCall struct {
	ID             string
	ConversationID string
	MessageID      int64
	UserID         int64
	ProviderCallID string
	Tool           string
	Kind           string // KindRead / KindWrite
	Args           json.RawMessage
	Summary        string
	QuoteAmount    *int64 // 分
	Status         string // Call*
	Result         json.RawMessage
	ResultRef      string
	SecretEnvEnc   []byte
	ExpiresAt      *time.Time
	DecidedAt      *time.Time
	ExecutedAt     *time.Time
	CreatedAt      time.Time
}

// Settings 对应 butler_settings 唯一一行（后台可改）。
type Settings struct {
	Enabled         bool
	DailyTokenLimit int64
	MaxTurns        int
	Notice          string
	UpdatedAt       time.Time
	UpdatedBy       *int64 // admins.id；迁移写入的首行为 nil
}

// Usage 对应 butler_usage 一行（每用户每个东八区自然日）。
type Usage struct {
	UserID       int64
	Day          time.Time
	Requests     int
	InputTokens  int64
	OutputTokens int64
	ToolCalls    int
	Failures     int
}

// DefaultSettings 返回与迁移首行一致的默认设置。
func DefaultSettings() Settings {
	return Settings{Enabled: DefaultEnabled, DailyTokenLimit: DefaultDailyTokenLimit, MaxTurns: DefaultMaxTurns}
}
