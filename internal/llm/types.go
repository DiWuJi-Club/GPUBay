package llm

import "time"

// Model 是 llm_models 一行。价格单位：分 / 百万 token。
type Model struct {
	ID               int64             `json:"id"`
	Slug             string            `json:"slug"`
	Name             string            `json:"name"`
	Description      string            `json:"description"`
	Kind             string            `json:"kind"`
	Source           string            `json:"source"`
	SourceID         string            `json:"source_id"`
	ImageID          int64             `json:"image_id"`
	AppVersionID     *int64            `json:"app_version_id"`
	ContextLen       int               `json:"context_len"`
	MaxOutputTokens  int               `json:"max_output_tokens"`
	DefaultMaxTokens int               `json:"default_max_tokens"`
	InputPrice       int64             `json:"input_price"`
	OutputPrice      int64             `json:"output_price"`
	NodeEnv          map[string]string `json:"node_env"`
	Replicas         int               `json:"replicas"`
	MaxConcurrency   int               `json:"max_concurrency"`
	Features         []string          `json:"features"`
	Status           string            `json:"status"`
	Sort             int               `json:"sort"`
	CreatedAt        time.Time         `json:"created_at"`
	UpdatedAt        time.Time         `json:"updated_at"`
}

// Node 是 llm_nodes 一行。APIKeyEnc 是节点 vLLM --api-key 的 secretbox 密文，永不序列化。
type Node struct {
	ID             string     `json:"id"`
	ModelID        int64      `json:"model_id"`
	MachineID      int64      `json:"machine_id"`
	InstanceID     *string    `json:"instance_id"`
	DeploymentID   *string    `json:"deployment_id"`
	State          string     `json:"state"`
	APIKeyEnc      []byte     `json:"-"`
	ProbeFailCount int        `json:"probe_fail_count"`
	RebuildCount   int        `json:"rebuild_count"`
	UnhealthySince *time.Time `json:"unhealthy_since"`
	DrainSince     *time.Time `json:"drain_since"`
	LastProbeAt    *time.Time `json:"last_probe_at"`
	LastReadyAt    *time.Time `json:"last_ready_at"`
	Error          string     `json:"error"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// NodeEvent 是 llm_node_events 一行（每次节点状态变化一条）。
type NodeEvent struct {
	ID        int64     `json:"id"`
	NodeID    string    `json:"node_id"`
	At        time.Time `json:"at"`
	FromState string    `json:"from_state"`
	ToState   string    `json:"to_state"`
	Detail    string    `json:"detail"`
}

// Account 是 llm_accounts 一行。金额：UnsettledMicro / CarryMicro 微分，Debt 分；限额 nil = 默认值。
type Account struct {
	UserID           int64     `json:"user_id"`
	UnsettledMicro   int64     `json:"unsettled_micro"`
	CarryMicro       int64     `json:"carry_micro"`
	Debt             int64     `json:"debt"`
	RPMLimit         *int      `json:"rpm_limit"`
	TPMLimit         *int      `json:"tpm_limit"`
	ConcurrencyLimit *int      `json:"concurrency_limit"`
	Blocked          bool      `json:"blocked"`
	BlockedReason    string    `json:"blocked_reason"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// Key 是 llm_api_keys 一行。KeyHash 不序列化（对外只有 KeyHint）。
type Key struct {
	ID             int64      `json:"id"`
	UserID         int64      `json:"user_id"`
	Name           string     `json:"name"`
	KeyHash        string     `json:"-"`
	KeyHint        string     `json:"key_hint"`
	Status         string     `json:"status"`
	ModelSlugs     []string   `json:"model_slugs"`
	MonthlyCap     *int64     `json:"monthly_cap"`
	MonthYM        string     `json:"month_ym"`
	MonthCostMicro int64      `json:"month_cost_micro"`
	ExpiresAt      *time.Time `json:"expires_at"`
	LastUsedAt     *time.Time `json:"last_used_at"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	RevokedAt      *time.Time `json:"revoked_at"`
}

// Usage 是 llm_usage 一行（不含请求 / 补全内容）。CostMicro 单位微分；价格是当时快照。
// 也是 RecordUsage 的输入：ID / SettlementID 忽略；CreatedAt 即请求时间（零值 = 当前时钟），日界 / 月界按它算。
type Usage struct {
	ID               int64     `json:"id"`
	RequestID        string    `json:"request_id"`
	UserID           int64     `json:"user_id"`
	KeyID            int64     `json:"key_id"`
	ModelID          int64     `json:"model_id"`
	NodeID           *string   `json:"node_id"`
	Endpoint         string    `json:"endpoint"`
	Stream           bool      `json:"stream"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	Estimated        bool      `json:"estimated"`
	InputPrice       int64     `json:"input_price"`
	OutputPrice      int64     `json:"output_price"`
	CostMicro        int64     `json:"cost_micro"`
	Status           string    `json:"status"`
	HTTPStatus       int       `json:"http_status"`
	LatencyMS        int       `json:"latency_ms"`
	TTFTMS           *int      `json:"ttft_ms"`
	ClientIP         string    `json:"client_ip"`
	SettlementID     *int64    `json:"settlement_id"`
	CreatedAt        time.Time `json:"created_at"`
}

// Settlement 是 llm_settlements 一行。UsageAmount / DebtAmount / Charged 单位分。
type Settlement struct {
	ID               int64      `json:"id"`
	UserID           int64      `json:"user_id"`
	Requests         int        `json:"requests"`
	PromptTokens     int64      `json:"prompt_tokens"`
	CompletionTokens int64      `json:"completion_tokens"`
	CostMicro        int64      `json:"cost_micro"`
	UsageAmount      int64      `json:"usage_amount"`
	DebtAmount       int64      `json:"debt_amount"`
	Charged          int64      `json:"charged"`
	WalletLedgerID   *int64     `json:"wallet_ledger_id"`
	PeriodFrom       *time.Time `json:"period_from"`
	PeriodTo         *time.Time `json:"period_to"`
	CreatedAt        time.Time  `json:"created_at"`
}

// DailyRow 是 llm_usage_daily 一行。Day 为 Asia/Shanghai 自然日（DATE，扫描为当天 00:00 UTC 的 time.Time；对外视图格式化为 YYYY-MM-DD）。
type DailyRow struct {
	Day              time.Time `json:"day"`
	UserID           int64     `json:"user_id"`
	ModelID          int64     `json:"model_id"`
	ModelSlug        string    `json:"model,omitempty"` // 不是表列：DailyUsage 联表带出，方便视图
	Requests         int64     `json:"requests"`
	PromptTokens     int64     `json:"prompt_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
	CostMicro        int64     `json:"cost_micro"`
}
