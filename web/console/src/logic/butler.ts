// AI 管家对话的纯逻辑（规格 §11.3）：SSE 事件 → 聊天状态机、历史详情 → 聊天项、额度 / 确认卡辅助。
// 全部不可变更新：每次返回新的 ChatState 与新的 items 数组，被改动的项也是新对象。
import {
  butlerToolLabel,
  fmtYuan,
  labelOf,
  type ButlerConfirmEvent,
  type ButlerDeltaEvent,
  type ButlerDoneEvent,
  type ButlerErrorEvent,
  type ButlerStartEvent,
  type ButlerToolEvent,
  type ButlerUsageEvent,
  type ConversationDetail,
  type PendingToolCall,
  type SSEEvent,
} from '@spark/shared'

export type ConfirmState = 'pending' | 'approved' | 'rejected' | 'expired' | 'executed' | 'failed'

export type ChatItem =
  | { kind: 'user'; text: string }
  | { kind: 'assistant'; text: string; streaming: boolean; truncated?: boolean }
  | { kind: 'tool'; id: string; name: string; label: string; status: 'ok' | 'error' | 'running'; summary?: string }
  | { kind: 'confirm'; card: PendingToolCall; state: ConfirmState }
  | { kind: 'error'; code: string; message: string; retryText?: string }

export interface ChatState {
  conversationId: string | null
  items: ChatItem[]
  status: 'idle' | 'running' | 'awaiting_confirm'
  dayUsed: number
  dayLimit: number
}

export function emptyState(): ChatState {
  return { conversationId: null, items: [], status: 'idle', dayUsed: 0, dayLimit: 0 }
}

/** 危险写操作：确认卡红色边框 + 需勾选「我已了解」。 */
const DANGER_TOOLS = new Set(['release_instance', 'remove_app', 'cancel_booking'])
export function isDanger(tool: string): boolean {
  return DANGER_TOOLS.has(tool)
}

/** 结束所有流式中的助手气泡（新的步骤 / 卡片 / 错误出现时，之前的文本就不会再续写）。 */
function endStreaming(items: ChatItem[]): ChatItem[] {
  return items.map((it) => (it.kind === 'assistant' && it.streaming ? { ...it, streaming: false } : it))
}

/** 步骤的中文名：没有工具名时显示「工具」。 */
function toolLabel(name: string): string {
  return name ? labelOf(butlerToolLabel, name) : '工具'
}

function lastUserText(items: ChatItem[]): string | undefined {
  for (let i = items.length - 1; i >= 0; i--) {
    const it = items[i]
    if (it.kind === 'user') return it.text
  }
  return undefined
}

function parseData<T>(data: string): T | null {
  try {
    const v = JSON.parse(data) as unknown
    return v && typeof v === 'object' ? (v as T) : null
  } catch {
    return null
  }
}

/**
 * 应用一个 SSE 事件（规格 §5.4）。无法解析的 data 或未知事件名原样返回 s。
 * - start：记下 conversation_id，进入 running；
 * - delta：追加到末尾的流式助手气泡，没有则新建；
 * - tool：同 id 的确认卡 → executed / failed（确认执行的结果）；同 id 的步骤 → 更新；否则追加步骤；
 * - confirm：追加确认卡（同 id 则替换），status = awaiting_confirm；
 * - usage：更新今日额度；
 * - done：结束流式，max_tokens → 本轮最后一个助手气泡 truncated；awaiting_confirm 保持待确认，其余回到 idle；
 * - error：追加错误项（retryText = 最近一条用户消息，供「重试」），回到 idle。
 */
export function applyEvent(s: ChatState, e: SSEEvent): ChatState {
  switch (e.event) {
    case 'start': {
      const d = parseData<ButlerStartEvent>(e.data)
      if (!d) return s
      return { ...s, conversationId: d.conversation_id || s.conversationId, status: 'running' }
    }
    case 'delta': {
      const d = parseData<ButlerDeltaEvent>(e.data)
      if (!d || typeof d.text !== 'string') return s
      const last = s.items[s.items.length - 1]
      if (last?.kind === 'assistant' && last.streaming) {
        return { ...s, items: [...s.items.slice(0, -1), { ...last, text: last.text + d.text }] }
      }
      return { ...s, items: [...s.items, { kind: 'assistant', text: d.text, streaming: true }] }
    }
    case 'tool': {
      const d = parseData<ButlerToolEvent>(e.data)
      if (!d || !d.id) return s
      const status = d.status === 'error' ? 'error' : 'ok'
      const ci = s.items.findIndex((it) => it.kind === 'confirm' && it.card.id === d.id)
      if (ci >= 0) {
        const items = endStreaming(s.items)
        const card = items[ci] as Extract<ChatItem, { kind: 'confirm' }>
        // 只有待确认 / 已批准的卡片才会有执行结果；已拒绝 / 已失效的卡片不被改写
        if (card.state !== 'pending' && card.state !== 'approved') return s
        items[ci] = { ...card, state: status === 'ok' ? 'executed' : 'failed' }
        return { ...s, items }
      }
      const ti = s.items.findIndex((it) => it.kind === 'tool' && it.id === d.id)
      const name = typeof d.name === 'string' ? d.name : ''
      const step: ChatItem = { kind: 'tool', id: d.id, name, label: d.label || toolLabel(name), status }
      if (ti >= 0) {
        const items = [...s.items]
        items[ti] = { ...(items[ti] as Extract<ChatItem, { kind: 'tool' }>), ...step }
        return { ...s, items }
      }
      return { ...s, items: [...endStreaming(s.items), step] }
    }
    case 'confirm': {
      const d = parseData<ButlerConfirmEvent>(e.data)
      if (!d?.tool_call?.id) return s
      const card = d.tool_call
      const items = endStreaming(s.items).filter((it) => !(it.kind === 'confirm' && it.card.id === card.id))
      return { ...s, items: [...items, { kind: 'confirm', card, state: 'pending' }], status: 'awaiting_confirm' }
    }
    case 'usage': {
      const d = parseData<ButlerUsageEvent>(e.data)
      if (!d) return s
      return { ...s, dayUsed: Number(d.day_used) || 0, dayLimit: Number(d.day_limit) || 0 }
    }
    case 'done': {
      const d = parseData<ButlerDoneEvent>(e.data)
      if (!d) return s
      const items = endStreaming(s.items)
      if (d.reason === 'max_tokens') {
        // 只标记本轮（最近一条用户消息之后）的助手气泡，不回头改上一轮的
        for (let i = items.length - 1; i >= 0 && items[i].kind !== 'user'; i--) {
          const it = items[i]
          if (it.kind === 'assistant') {
            items[i] = { ...it, truncated: true }
            break
          }
        }
      }
      return { ...s, items, status: d.reason === 'awaiting_confirm' ? 'awaiting_confirm' : 'idle' }
    }
    case 'error': {
      const d = parseData<ButlerErrorEvent>(e.data)
      if (!d) return s
      return appendError(s, d.code || 'butler_llm_unavailable', d.message || 'AI 管家暂时不可用，请稍后再试', lastUserText(s.items))
    }
    default:
      return s
  }
}

/** 发送前在本地追加用户气泡并进入 running；仍待确认的卡片随之失效（服务端收到新消息会把 pending 标 expired）。 */
export function appendUser(s: ChatState, text: string): ChatState {
  const items: ChatItem[] = s.items.map((it) => (it.kind === 'confirm' && it.state === 'pending' ? { ...it, state: 'expired' } : it))
  return { ...s, items: [...items, { kind: 'user', text }], status: 'running' }
}

/** 追加错误行（SSE error 事件，或请求在建立流之前 / 读流时失败）；回到 idle。 */
export function appendError(s: ChatState, code: string, message: string, retryText?: string): ChatState {
  const item: ChatItem = retryText !== undefined ? { kind: 'error', code, message, retryText } : { kind: 'error', code, message }
  return { ...s, items: [...endStreaming(s.items), item], status: 'idle' }
}

/** 改确认卡状态（approve 发出后 approved、reject → rejected、倒计时到 0 → expired）。 */
export function markConfirm(s: ChatState, id: string, state: ConfirmState): ChatState {
  let changed = false
  const items = s.items.map((it) => {
    if (it.kind !== 'confirm' || it.card.id !== id || it.state === state) return it
    changed = true
    return { ...it, state }
  })
  if (!changed) return s
  const stillPending = items.some((it) => it.kind === 'confirm' && it.state === 'pending')
  const status = s.status === 'awaiting_confirm' && !stillPending ? (state === 'expired' ? 'idle' : 'running') : s.status
  return { ...s, items, status }
}

/**
 * 历史详情 → 聊天状态。user 的 text 块 → 用户气泡；assistant 的 text 块 → 助手气泡（相邻的用空行连接）；tool 块 → 步骤（同 id 去重）；
 * tool_result → 挂到同 id 步骤的 summary；tool 角色的文本（「用户拒绝了该操作」等给模型看的回填）不展示。
 * pending_tool_call → 末尾的待确认卡（同 id 的步骤去掉，避免重复）。额度不在详情里，沿用 prev。
 */
export function fromDetail(d: ConversationDetail, prev?: Pick<ChatState, 'dayUsed' | 'dayLimit'>): ChatState {
  const items: ChatItem[] = []
  const stepIndex = new Map<string, number>()
  const summaries = new Map<string, string>()
  for (const m of d.messages ?? []) {
    let userText = ''
    for (const b of m.content ?? []) {
      if (b.type === 'text') {
        if (m.role === 'user') userText += b.text
        else if (m.role === 'assistant' && b.text) {
          const last = items[items.length - 1]
          if (last?.kind === 'assistant') items[items.length - 1] = { ...last, text: `${last.text}\n\n${b.text}` }
          else items.push({ kind: 'assistant', text: b.text, streaming: false })
        }
      } else if (b.type === 'tool') {
        const name = typeof b.name === 'string' ? b.name : ''
        const step: ChatItem = { kind: 'tool', id: b.id, name, label: b.label || toolLabel(name), status: b.status === 'error' ? 'error' : 'ok' }
        if (b.summary) step.summary = b.summary
        const at = stepIndex.get(b.id)
        if (at !== undefined) items[at] = { ...(items[at] as Extract<ChatItem, { kind: 'tool' }>), ...step }
        else {
          stepIndex.set(b.id, items.length)
          items.push(step)
        }
      } else if (b.type === 'tool_result') {
        summaries.set(b.tool_call_id, b.summary)
      }
    }
    if (m.role === 'user' && userText) items.push({ kind: 'user', text: userText })
  }
  for (const [id, summary] of summaries) {
    const at = stepIndex.get(id)
    if (at !== undefined && summary) items[at] = { ...(items[at] as Extract<ChatItem, { kind: 'tool' }>), summary }
  }
  let out = items
  const pending = d.pending_tool_call
  if (pending) {
    out = items.filter((it) => !(it.kind === 'tool' && it.id === pending.id))
    out.push({ kind: 'confirm', card: pending, state: 'pending' })
  }
  return {
    conversationId: d.conversation.id,
    items: out,
    status: d.conversation.status,
    dayUsed: prev?.dayUsed ?? 0,
    dayLimit: prev?.dayLimit ?? 0,
  }
}

/** 额度提示「今日已用 12%」：≥ 90% warn（橙），≥ 100% exhausted；限额 ≤ 0 表示还没拿到额度，返回空文案。 */
export function formatQuota(used: number, limit: number): { text: string; level: 'ok' | 'warn' | 'exhausted' } {
  if (!(limit > 0)) return { text: '', level: 'ok' } // 额度未加载（0）：不显示
  const u = Math.max(0, used || 0)
  if (u >= limit) return { text: '今日已用 100%', level: 'exhausted' }
  const pct = Math.floor((u * 100) / limit)
  return { text: `今日已用 ${pct}%`, level: u * 10 >= limit * 9 ? 'warn' : 'ok' }
}

/** 确认卡标题：工具中文名（未知工具退回服务端给的 label / 工具名）。 */
export function confirmTitle(card: PendingToolCall): string {
  return butlerToolLabel[card.tool] ?? (card.label || card.tool)
}

/** 确认卡 summary 按行拆分（去掉首尾空行）。 */
export function confirmSummaryLines(card: PendingToolCall): string[] {
  const lines = (card.summary ?? '').replace(/\r\n?/g, '\n').split('\n')
  while (lines.length && !lines[0].trim()) lines.shift()
  while (lines.length && !lines[lines.length - 1].trim()) lines.pop()
  return lines
}

/** 金额行：有报价时「预计扣款 ¥X」，cancel_booking 为「退款 ¥X」；无报价返回空串。 */
export function confirmAmountText(card: PendingToolCall): string {
  if (card.quote_amount == null) return ''
  return `${card.tool === 'cancel_booking' ? '退款' : '预计扣款'} ${fmtYuan(card.quote_amount)}`
}

/** 距 expires_at 的剩余整秒（≥ 0；无法解析的时间视为已过期）。 */
export function secondsLeft(expiresAt: string, now: Date): number {
  const t = Date.parse(expiresAt)
  if (Number.isNaN(t)) return 0
  return Math.max(0, Math.floor((t - now.getTime()) / 1000))
}

/** 空状态的 4 个建议 chip（规格 §11.2）。 */
export const SUGGESTIONS: readonly string[] = ['帮我选一台机器', '在我的实例上部署 vLLM', '我的应用为什么失败了', '我的余额还能用多久']
