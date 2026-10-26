package llm

import (
	"time"

	"spark-cloud/internal/appdeploy"
)

// 规格 §13。价格、限额、副本数是数据库里的运营参数，这里只有默认值与机制常量。
const (
	DefaultRPM             = 60      // 每用户每分钟请求数（llm_accounts.rpm_limit NULL 时）
	DefaultTPM             = 200_000 // 每用户每分钟 token 数
	DefaultConcurrency     = 8       // 每用户并发
	DefaultNodeConcurrency = 32      // llm_models.max_concurrency 默认

	MaxBodyBytes         = 2 << 20 // 请求体上限
	MaxUpstreamBodyBytes = 8 << 20 // 上游非流式响应上限
	TotalTimeout         = 10 * time.Minute
	TTFTTimeout          = 120 * time.Second // 首字节超时（http.Transport.ResponseHeaderTimeout）

	KeyCacheTTL        = 30 * time.Second
	BalanceCacheTTL    = 2 * time.Second
	AuthFailLimitPerIP = 60 // 401 按 IP 每分钟次数
	MaxKeysPerUser     = 20 // 每用户有效（非 revoked）Key 上限

	SettleMinMicro = 100_000_000 // 未结算 ≥ ¥1（微分）即结算
	SettleMaxAge   = 10 * time.Minute
	UsageRetention = 90 * 24 * time.Hour // 已结算明细保留期
	PurgeBatch     = 5000                // 每轮清理行数

	ControllerInterval    = 10 * time.Second
	ProbeInterval         = 10 * time.Second
	ProbeTimeout          = 5 * time.Second
	ProbeFailThreshold    = 3
	UnhealthyRebuildAfter = 10 * time.Minute
	MaxRebuilds           = 3
	ProvisionTimeout      = 150 * time.Minute // vllm-node 1.0.0（120 分钟部署超时）的建节点超时；版本相关的取值见 ProvisionTimeoutFor
	DrainTimeout          = 60 * time.Second
	MaxProvisionPerRound  = 4
	SystemLeaseHorizon    = 10 * 365 * 24 * time.Hour // 系统实例 lease [now, now + 10 年)
	RouterRefresh         = 2 * time.Second

	NodePort        = 8000 // 容器内 vLLM 端口
	NodeAppSlug     = "vllm-node"
	SystemUserPhone = "system:llm"
	MicroPerFen     = 1_000_000 // 1 分 = 10^6 微分
)

// 计划 13：多机节点组（规格 §7、裁定 M12 / M20）。
const (
	GroupRebuildAfter     = 2 * time.Minute  // 组 unhealthy（成员进程崩溃 / 不健康）持续这么久 → 整组 RedeployCluster
	GroupSuspendFailAfter = 10 * time.Minute // 组有成员暂停（机器失联）持续这么久仍未恢复 → failed
	provisionSlack        = 30 * time.Minute // ProvisionTimeoutFor = 版本 timeout_minutes + 它
)

// NodesPerReplicaOptions 是模型 nodes_per_replica 的取值（M11）。
var NodesPerReplicaOptions = []int{1, 2, 4}

// ProvisionTimeoutFor 建节点（单机与组）的超时 = 该 vllm-node 版本的 timeout_minutes + 30 分钟（0 → appdeploy.DefaultTimeoutMinutes）。
func ProvisionTimeoutFor(v appdeploy.Version) time.Duration {
	m := v.Spec.TimeoutMinutes
	if m <= 0 {
		m = appdeploy.DefaultTimeoutMinutes
	}
	return time.Duration(m)*time.Minute + provisionSlack
}

// 节点状态（llm_nodes.state）。
const (
	NodeProvisioning = "provisioning"
	NodeReady        = "ready"
	NodeUnhealthy    = "unhealthy"
	NodeDraining     = "draining"
	NodeRemoving     = "removing"
	NodeRemoved      = "removed"
	NodeFailed       = "failed"
)

// NodeTerminalStates 不占 llm_nodes_machine_live_uniq。
var NodeTerminalStates = []string{NodeRemoved, NodeFailed}

// 模型状态（llm_models.status）。
const (
	ModelDraft   = "draft"
	ModelOnline  = "online"
	ModelOffline = "offline"
)

// 模型类型（llm_models.kind）。
const (
	KindChat      = "chat"
	KindEmbedding = "embedding"
)

// Key 状态（llm_api_keys.status）。
const (
	KeyActive   = "active"
	KeyDisabled = "disabled"
	KeyRevoked  = "revoked"
)

// 用量状态（llm_usage.status）。
const (
	UsageOK              = "ok"
	UsageClientAbort     = "client_abort"
	UsageUpstreamError   = "upstream_error"
	UsageUpstreamTimeout = "upstream_timeout"
	UsageNoNode          = "no_node"
)

// 端点（llm_usage.endpoint）。
const (
	EndpointChat        = "chat"
	EndpointCompletions = "completions"
	EndpointEmbeddings  = "embeddings"
)

// 告警种类（文案稳定，不含数值）。
const (
	AlertPoolShort         = "llm_pool_short"
	AlertNodeUnhealthy     = "llm_node_unhealthy"
	AlertNodeFailed        = "llm_node_failed"
	AlertUsageRecordFailed = "llm_usage_record_failed"
)

// LedgerRefTypeSettlement 结算流水的 wallet_ledger.ref_type（ref_id = 结算 ID）。
const LedgerRefTypeSettlement = "llm_settlement"
