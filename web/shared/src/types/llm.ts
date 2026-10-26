import type { FabricKind, LLMNodeMember } from './cluster'

// 计划 9：Token 商城（规格 §10）。金额单位：价格「分 / 百万 token」，费用 cost_micro「微分」（1 分 = 10^6 微分），其余金额「分」。

export type LLMModelKind = 'chat' | 'embedding'
export type LLMModelSource = 'modelscope' | 'hf-mirror'
export type LLMModelStatus = 'draft' | 'online' | 'offline'
export type LLMNodeState = 'provisioning' | 'ready' | 'unhealthy' | 'draining' | 'removing' | 'removed' | 'failed'
export type LLMKeyStatus = 'active' | 'disabled' | 'revoked'
export type LLMUsageStatus = 'ok' | 'client_abort' | 'upstream_error' | 'upstream_timeout' | 'no_node'
export type LLMEndpoint = 'chat' | 'completions' | 'embeddings'

// ---- 控制台（/api/v1/llm/*）----

/** GET /llm/models 的条目（只含 online 模型）。 */
export interface LLMModelPublic {
  slug: string
  name: string
  description: string
  kind: LLMModelKind
  context_len: number
  max_output_tokens: number
  input_price: number
  output_price: number
  features: string[]
  /** 有 ready 节点 */
  available: boolean
}
export interface LLMModelsResponse { items: LLMModelPublic[]; base_url: string }

/** GET /llm/account */
export interface LLMAccount {
  debt: number
  /** ceil(unsettled_micro / 10^6) */
  unsettled: number
  today_cost: number
  month_cost: number
  month_tokens: number
  limits: { rpm: number; tpm: number; concurrency: number }
  blocked: boolean
  blocked_reason: string
  kyc_verified: boolean
  base_url: string
}

/** GET /llm/keys 的条目（只有 key_hint，没有明文）。 */
export interface LLMKey {
  id: number
  name: string
  key_hint: string
  status: LLMKeyStatus
  /** 空 = 全部在线模型 */
  model_slugs: string[]
  monthly_cap: number | null
  month_cost: number
  expires_at: string | null
  last_used_at: string | null
  created_at: string
}
export interface LLMKeyInput {
  name: string
  model_slugs?: string[]
  monthly_cap?: number | null
  expires_at?: string | null
}
export interface LLMKeyUpdate {
  name?: string
  status?: 'active' | 'disabled'
  model_slugs?: string[]
  monthly_cap?: number | null
}
/** POST /llm/keys → 201：明文 key 只返回这一次。 */
export interface LLMKeyCreated { key: string; item: LLMKey }

/** GET /llm/usage 的条目。 */
export interface LLMUsageRow {
  request_id: string
  created_at: string
  model: string
  key_name: string
  /** Key 提示（sk-spark-…末 4 位）；后端可能不返回 */
  key_hint?: string
  endpoint: LLMEndpoint
  stream: boolean
  prompt_tokens: number
  completion_tokens: number
  estimated: boolean
  cost_micro: number
  status: LLMUsageStatus
  http_status: number
  latency_ms: number
  ttft_ms: number | null
}

/** GET /llm/usage/daily 的条目（day 为北京时间 YYYY-MM-DD）。 */
export interface LLMDailyRow {
  day: string
  model: string
  requests: number
  prompt_tokens: number
  completion_tokens: number
  cost_micro: number
}

/** GET /llm/settlements 的条目。 */
export interface LLMSettlement {
  id: number
  created_at: string
  requests: number
  prompt_tokens: number
  completion_tokens: number
  usage_amount: number
  debt_amount: number
  charged: number
  wallet_ledger_id: number | null
  period_from: string | null
  period_to: string | null
}

// ---- 后台（/admin/api/v1/llm/*）----

/** GET /llm/models 的条目：全部字段 + 节点计数。 */
export interface LLMModelAdmin {
  id: number
  slug: string
  name: string
  description: string
  kind: LLMModelKind
  source: LLMModelSource
  source_id: string
  image_id: number
  app_version_id: number | null
  context_len: number
  max_output_tokens: number
  default_max_tokens: number
  input_price: number
  output_price: number
  node_env: Record<string, string>
  replicas: number
  max_concurrency: number
  features: string[]
  status: LLMModelStatus
  sort: number
  created_at: string
  updated_at: string
  nodes: { ready: number; total: number }
  /** 计划 13：每副本机器数（1 / 2 / 4）与并行切分（tensor_parallel × pipeline_parallel = nodes_per_replica）；旧后端缺省 = 1 / 1 / 1 */
  nodes_per_replica?: number
  tensor_parallel?: number
  pipeline_parallel?: number
}
/** POST /llm/models（PUT 不含 slug）。 */
export type LLMModelInput = Omit<LLMModelAdmin, 'id' | 'status' | 'created_at' | 'updated_at' | 'nodes'>

/** GET /llm/nodes 的条目。 */
export interface LLMNode {
  id: string
  model_id: number
  model_slug: string
  machine_id: number
  machine_name: string
  instance_id: string | null
  deployment_id: string | null
  state: LLMNodeState
  deployment: { state: string; health: string; phase: string; progress: number } | null
  inflight: number
  probe_fail_count: number
  rebuild_count: number
  unhealthy_since: string | null
  drain_since: string | null
  last_probe_at: string | null
  last_ready_at: string | null
  error: string
  created_at: string
  updated_at: string
  /** 计划 13：每副本机器数（1 = 单机节点）；组节点的 machine / instance / deployment 指向 rank0（head） */
  size?: number
  cluster_id?: string | null
  app_cluster_id?: string | null
  /** 组节点的成员（按 rank，含 rank0）；单机节点为空数组 */
  members?: LLMNodeMember[]
  /** 组节点的集群部署聚合；读不到 / 还没建为 null */
  app_cluster?: { id: string; status: string; health: string; progress: number } | null
}
export interface LLMNodeEvent {
  id: number
  node_id: string
  at: string
  from_state: string
  to_state: string
  detail: string
}
/** GET /llm/nodes/{id}：详情 + 最近 50 条节点事件 + 部署事件。 */
export interface LLMNodeDetail {
  node: LLMNode
  events: LLMNodeEvent[]
  deployment_events: { id: number; at: string; from_state: string; to_state: string; detail: string }[]
}

/** GET /llm/pool 的机器行。 */
export interface LLMPoolMachine {
  machine: { id: number; serial: string; display_name: string; state: string; agent_online: boolean }
  /** 计划 13：组节点时 rank 为本机的 rank、size 为组的台数（单机节点 0 / 1） */
  node?: { id: string; model_slug: string; state: LLMNodeState; rank?: number; size?: number }
  /** 机器上仍有租约时的到期时间（「等待租约结束」） */
  lease_until?: string
  /** 计划 13：所属集群网络（无 → null / ''） */
  fabric_id?: number | null
  fabric_name?: string
}
/** 计划 13：机器池按集群网络汇总；groups["2"] / groups["4"] = 用空闲且集群就绪的机器能组出的副本数。 */
export interface LLMPoolFabric { fabric_id: number; name: string; kind: FabricKind; rdma: boolean; total: number; free: number; groups: Partial<Record<'2' | '4', number>> }
export interface LLMPool { group_id: number; machines: LLMPoolMachine[]; free: number; total: number; fabrics?: LLMPoolFabric[] }

export interface LLMLimits { rpm: number; tpm: number; concurrency: number }

/**
 * GET /llm/accounts 的条目：llm_accounts 一行 + 手机号、Key 数、本月消费（分）、未结算（分，向上取整）、生效限额。
 * *_limit 为 null = 用默认值（limits 是合并默认值后的生效值）。
 */
export interface LLMAccountAdmin {
  user_id: number
  unsettled_micro: number
  carry_micro: number
  debt: number
  rpm_limit: number | null
  tpm_limit: number | null
  concurrency_limit: number | null
  blocked: boolean
  blocked_reason: string
  created_at: string
  updated_at: string
  phone: string
  key_count: number
  month_cost: number
  unsettled: number
  limits: LLMLimits
}

/** PUT /llm/accounts/{user_id}：省略 = 不改，null = 恢复默认；停用要原因，解除停用清空原因。 */
export interface LLMAccountUpdate {
  rpm_limit?: number | null
  tpm_limit?: number | null
  concurrency_limit?: number | null
  blocked?: boolean
  blocked_reason?: string
}

/** 后台 GET /llm/keys?user_id= 的条目（只有 key_hint）。month_cost_micro 只对 month_ym 那个月有效。 */
export interface LLMKeyAdmin {
  id: number
  user_id: number
  name: string
  key_hint: string
  status: LLMKeyStatus
  model_slugs: string[]
  monthly_cap: number | null
  month_ym: string
  month_cost_micro: number
  /** 当前（北京时间）月的消费，分；后端按当前月计算，旧后端缺省 */
  month_cost?: number
  expires_at: string | null
  last_used_at: string | null
  created_at: string
  revoked_at: string | null
}

/** 后台 GET /llm/usage 的条目。 */
export interface LLMUsageAdminRow {
  request_id: string
  created_at: string
  user_id: number
  user_phone: string
  model: string
  key_name: string
  key_hint: string
  endpoint: LLMEndpoint
  stream: boolean
  prompt_tokens: number
  completion_tokens: number
  estimated: boolean
  input_price: number
  output_price: number
  cost_micro: number
  status: LLMUsageStatus
  http_status: number
  latency_ms: number
  ttft_ms: number | null
  node_id: string | null
  client_ip: string
}

/** 后台 GET /llm/usage/daily?group_by=model|user 的条目（day 为北京时间 YYYY-MM-DD；按用户时带 user_id / phone，按模型时带 model）。 */
export interface LLMDailyAdminRow {
  day: string
  user_id?: number
  phone?: string
  model?: string
  requests: number
  prompt_tokens: number
  completion_tokens: number
  cost_micro: number
}

/** 后台 GET /llm/settlements 的条目。 */
export interface LLMSettlementAdmin extends LLMSettlement {
  user_id: number
  cost_micro: number
}

/** GET /llm/summary（仪表盘卡片）。 */
export interface LLMSummary {
  nodes: { ready: number; total: number; unhealthy: number }
  today: { requests: number; prompt_tokens: number; completion_tokens: number; settled_amount: number; unsettled_estimate: number }
  debt_total: number
  debt_users: number
}
