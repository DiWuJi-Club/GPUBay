// 计划 12：AI 管家（控制台 /api/v1/butler，规格 §5.4 / §10.1）。

export type ButlerConversationStatus = 'idle' | 'running' | 'awaiting_confirm'
export type ButlerRole = 'user' | 'assistant' | 'tool'

/** GET /butler/status */
export interface ButlerStatus {
  enabled: boolean
  notice: string
  day_used: number
  day_limit: number
  /** 「平台模型」/「Claude」，不暴露模型名 */
  provider_label: string
  /** 实例域名父域，供 Markdown 链接白名单 */
  ingress_domain: string
}

/** GET /butler/conversations 的条目 */
export interface ConversationBrief {
  id: string
  title: string
  status: ButlerConversationStatus
  last_message_at: string | null
  created_at: string
}

/** 前端消息块（服务端已把落库的块转换：tool_use → tool，tool_result 只给 ≤ 2KB 摘要，context 不输出）。 */
export type ButlerBlock =
  | { type: 'text'; text: string }
  | { type: 'tool'; id: string; name: string; label: string; status: 'ok' | 'error'; summary?: string }
  | { type: 'tool_result'; tool_call_id: string; summary: string }

export interface ButlerMessage {
  id: number
  seq: number
  role: ButlerRole
  content: ButlerBlock[]
  /** 规格 §10.1 未列出，后端可能不返回 */
  created_at?: string
}

/** 待确认的写操作（SSE confirm 事件与 GET 详情的 pending_tool_call 同款）。 */
export interface PendingToolCall {
  id: string
  tool: string
  label: string
  /** 服务端渲染的确认卡文案（多行） */
  summary: string
  /** 报价（分）；无则 null */
  quote_amount: number | null
  args: Record<string, unknown>
  expires_at: string
  /** 服务器时钟下剩余整秒（倒计时以它为准；浏览器时钟与服务器不一致时 expires_at 不可靠）；缺省退回 expires_at */
  expires_in?: number
  /** release_instance / remove_app / cancel_booking：红色 + 需勾选「我已了解」 */
  danger: boolean
}

export interface ButlerConversation extends ConversationBrief {
  context?: { instance_id?: string }
}

/** GET /butler/conversations/{id} */
export interface ConversationDetail {
  conversation: ButlerConversation
  messages: ButlerMessage[]
  pending_tool_call: PendingToolCall | null
}

export type SSEEventName = 'start' | 'delta' | 'tool' | 'confirm' | 'usage' | 'done' | 'error'
export type ButlerDoneReason = 'end_turn' | 'awaiting_confirm' | 'max_tokens' | 'iteration_limit' | 'refusal'

/** 各 SSE 事件的 data 载荷 */
export interface ButlerStartEvent { conversation_id: string; message_id: number }
export interface ButlerDeltaEvent { text: string }
/** message：确认执行失败时的业务错误文案（「执行失败：<message>」） */
export interface ButlerToolEvent { id: string; name: string; label: string; status: 'ok' | 'error'; message?: string }
export interface ButlerConfirmEvent { tool_call: PendingToolCall }
export interface ButlerUsageEvent { input_tokens: number; output_tokens: number; day_used: number; day_limit: number }
export interface ButlerDoneEvent { reason: ButlerDoneReason }
export interface ButlerErrorEvent { code: string; message: string }

// ---- 后台（/admin/api/v1/butler，规格 §9 / §10.2，D8）。手机号为原文，页面显示前须脱敏；金额单位：分 ----

/** GET / PUT /butler/settings */
export interface ButlerSettingsView {
  enabled: boolean
  daily_token_limit: number
  max_turns: number
  notice: string
  updated_at: string
  /** admins.id；迁移写入的首行为 null */
  updated_by: number | null
}
/** PUT /butler/settings 的请求体（四项都要给） */
export type ButlerSettingsInput = Pick<ButlerSettingsView, 'enabled' | 'daily_token_limit' | 'max_turns' | 'notice'>

/** 用量计数（按日合计 / 按用户共用） */
export interface ButlerUsageCounts {
  requests: number
  input_tokens: number
  output_tokens: number
  tool_calls: number
  failures: number
  /** 估算费用（分）= round((输入 × 输入单价 + 输出 × 输出单价) / 1e6) */
  cost_cents: number
}
/** GET /butler/usage 的一行（只含有记录的日期，按日期升序） */
export interface ButlerDailyUsage extends ButlerUsageCounts {
  /** 北京时间日期 YYYY-MM-DD */
  day: string
  /** 当日活跃用户数 */
  users: number
}
/** GET /butler/usage?from=&to=（默认最近 30 天，两端含） */
export interface ButlerUsageReport {
  from: string
  to: string
  price_in_cents_per_m: number
  price_out_cents_per_m: number
  items: ButlerDailyUsage[]
  /** 各列合计（不含活跃用户：跨日不去重没有意义）；cost_cents 按合计 token 计算 */
  totals: ButlerUsageCounts
}
/** GET /butler/usage/users 的一行（当日按 token 降序） */
export interface ButlerUserUsage extends ButlerUsageCounts {
  user_id: number
  phone: string
}
/** GET /butler/usage/users?day=&page=&page_size= */
export interface ButlerUserUsagePage {
  day: string
  items: ButlerUserUsage[]
  total: number
}

/** GET /butler/conversations 的一行（ops） */
export interface AdminButlerConversation {
  id: string
  user_id: number
  phone: string
  title: string
  status: ButlerConversationStatus
  provider: string
  model: string
  /** 消息条数（含 assistant / tool） */
  messages: number
  user_turns: number
  input_tokens: number
  output_tokens: number
  /** 写工具调用数（任意状态） */
  write_calls: number
  last_message_at: string | null
  created_at: string
  updated_at: string
}

/** 后台对话详情的消息块：控制台前端块 + context 块 + tool_result 的可疑标记 */
export type AdminButlerBlock =
  | { type: 'text'; text: string }
  | { type: 'tool'; id: string; name: string; label: string; status: 'ok' | 'error' }
  | { type: 'tool_result'; tool_call_id: string; ok: boolean; summary: string; suspicious: boolean }
  | { type: 'context'; instance_id: string }

export interface AdminButlerMessage {
  id: number
  seq: number
  role: ButlerRole
  tokens: number
  created_at: string
  content: AdminButlerBlock[]
}

export type ButlerToolCallStatus = 'executed' | 'failed' | 'pending' | 'approved' | 'rejected' | 'expired'

/** 对话详情的工具调用表（args / summary 为落库的 *** 版本，从不含 secret 密文） */
export interface AdminButlerToolCall {
  id: string
  message_id: number
  tool: string
  label: string
  kind: 'read' | 'write'
  args: Record<string, unknown>
  summary: string
  quote_amount: number | null
  status: ButlerToolCallStatus
  /** 落库结果的文本摘要（去掉不可信包裹，≤ 2KB；无结果为 ''） */
  result_summary: string
  /** 结果为错误对象时的 code，否则 '' */
  error_code: string
  result_ref: string
  expires_at: string | null
  decided_at: string | null
  executed_at: string | null
  created_at: string
}

/** GET /butler/conversations/{id}（ops，只读） */
export interface AdminButlerConversationDetail {
  conversation: AdminButlerConversation & { context: { instance_id?: string } }
  messages: AdminButlerMessage[]
  tool_calls: AdminButlerToolCall[]
}

/** GET /users/{id} 的 butler 字段（平台未启用管家时缺省） */
export interface UserButlerStats {
  conversations: number
  tokens_30d: number
  write_calls_30d: number
}

/** GET /dashboard 增加的三个数字（平台未启用管家时缺省） */
export interface DashboardButler {
  butler_requests_today?: number
  /** 输入 + 输出 */
  butler_tokens_today?: number
  butler_failures_today?: number
}
