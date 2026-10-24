package butler

import "time"

// 限额与超时（规格 §7）。
const (
	MaxUserChars           = 2000  // 单条用户消息最多字符数（rune）
	MaxIterations          = 8     // 每轮最多调用模型次数（工具循环）
	MaxToolResultBytes     = 16384 // 日志类工具结果上限
	MaxSmallResultBytes    = 4096  // 其它工具结果上限
	MaxHistoryMessages     = 40    // 历史窗口（另加第一条 user 消息）
	MaxPromptTokens        = 60000 // 历史估算 token 上限
	MaxConcurrentRuns      = 32    // 本副本同时进行的轮次
	RetentionDays          = 90    // 对话保留天数（按 updated_at）
	UsageRetentionDays     = 400   // butler_usage 保留天数
	SweepDeleteBatch       = 200   // 清扫器每批删除的对话数
	DefaultMaxOutputTokens = 2048  // 单次输出 token（SPARK_LLM_MAX_OUTPUT_TOKENS 的默认值）
	SendRatePerMinute      = 20    // 每用户每分钟发消息次数
	ListLimit              = 50    // 对话列表默认条数
	TitleRunes             = 30    // 对话标题取首条用户消息的前 N 个字
	LLMFailAlertAfter      = 5     // 连续 LLM 失败次数达到后告警 butler_llm_unavailable

	LLMTimeout       = 60 * time.Second  // 单次模型请求
	RunTimeout       = 120 * time.Second // 整轮（脱离请求 ctx）
	ReadToolTimeout  = 15 * time.Second
	WriteToolTimeout = 30 * time.Second
	ConfirmTTL       = 10 * time.Minute // 确认卡有效期
	StaleRunAfter    = 3 * time.Minute  // running 超过该时长视为副本崩溃，清扫器接管
)

// butler_settings 的默认值与取值范围（与迁移 00025 的 DEFAULT / CHECK 一致）。
const (
	DefaultEnabled         = true
	DefaultDailyTokenLimit = 300000
	DefaultMaxTurns        = 60

	MinDailyTokenLimit = 10000
	MaxDailyTokenLimit = 100000000
	MinMaxTurns        = 5
	MaxMaxTurns        = 1000
	MaxNoticeRunes     = 200
)

// 对话状态（butler_conversations.status）。
const (
	ConvIdle            = "idle"
	ConvRunning         = "running"
	ConvAwaitingConfirm = "awaiting_confirm"
)

// 工具调用状态（butler_tool_calls.status）：读工具直接 executed / failed；写工具 pending → approved → executed / failed，或 rejected / expired。
const (
	CallExecuted = "executed"
	CallFailed   = "failed"
	CallPending  = "pending"
	CallApproved = "approved"
	CallRejected = "rejected"
	CallExpired  = "expired"
)

// 工具种类（butler_tool_calls.kind）。
const (
	KindRead  = "read"
	KindWrite = "write"
)

// 审计动作与告警种类。
const (
	AuditToolExecute = "butler.tool_execute" // user actor；target butler_tool_call
	AuditSettings    = "butler.settings"     // admin；diff {old, new}
	AuditTargetCall  = "butler_tool_call"
	AlertLLMDown     = "butler_llm_unavailable"
)
