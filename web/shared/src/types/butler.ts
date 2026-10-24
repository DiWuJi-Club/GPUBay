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
  created_at: string
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
export interface ButlerToolEvent { id: string; name: string; label: string; status: 'ok' | 'error' }
export interface ButlerConfirmEvent { tool_call: PendingToolCall }
export interface ButlerUsageEvent { input_tokens: number; output_tokens: number; day_used: number; day_limit: number }
export interface ButlerDoneEvent { reason: ButlerDoneReason }
export interface ButlerErrorEvent { code: string; message: string }
