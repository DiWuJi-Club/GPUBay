import type { ConversationDetail, PendingToolCall, SSEEvent } from '@spark/shared'
import { describe, expect, it } from 'vitest'
import {
  SUGGESTIONS,
  appendError,
  appendUser,
  applyEvent,
  confirmAmountText,
  confirmSummaryLines,
  confirmTitle,
  emptyState,
  formatQuota,
  fromDetail,
  isDanger,
  markConfirm,
  secondsLeft,
  type ChatState,
} from '../butler'

const ev = (event: string, data: unknown): SSEEvent => ({ event, data: JSON.stringify(data) })
const run = (s: ChatState, events: SSEEvent[]) => events.reduce(applyEvent, s)

const card: PendingToolCall = {
  id: 'tc-renew1',
  tool: 'renew_instance',
  label: '续费实例',
  summary: '续费实例 ins-abc\n时长：1 天 × 1\n',
  quote_amount: 9900,
  args: { instance_id: 'ins-abc', duration: '1d', quantity: 1 },
  expires_at: '2026-10-25T10:10:00Z',
  danger: false,
}

describe('applyEvent', () => {
  it('完整一轮：start → delta → tool → delta → usage → done', () => {
    let s = appendUser(emptyState(), '我有几台实例？')
    expect(s.status).toBe('running')
    s = run(s, [
      ev('start', { conversation_id: 'cv-1', message_id: 1 }),
      ev('delta', { text: '我查' }),
      ev('delta', { text: '一下。' }),
      ev('tool', { id: 'tc-1', name: 'list_instances', label: '查询实例列表', status: 'ok' }),
      ev('delta', { text: '你有 **2** 台。' }),
      ev('usage', { input_tokens: 1200, output_tokens: 80, day_used: 5678, day_limit: 300000 }),
      ev('done', { reason: 'end_turn' }),
    ])
    expect(s).toEqual({
      conversationId: 'cv-1',
      status: 'idle',
      dayUsed: 5678,
      dayLimit: 300000,
      items: [
        { kind: 'user', text: '我有几台实例？' },
        { kind: 'assistant', text: '我查一下。', streaming: false },
        { kind: 'tool', id: 'tc-1', name: 'list_instances', label: '查询实例列表', status: 'ok' },
        { kind: 'assistant', text: '你有 **2** 台。', streaming: false },
      ],
    })
  })

  it('流式中最后一个助手气泡 streaming = true', () => {
    const s = run(emptyState(), [ev('start', { conversation_id: 'cv-1', message_id: 1 }), ev('delta', { text: 'a' })])
    expect(s.items).toEqual([{ kind: 'assistant', text: 'a', streaming: true }])
    expect(s.status).toBe('running')
  })

  it('写工具：confirm 追加卡片并等待确认；确认后 tool 事件把卡片变为 executed / failed', () => {
    let s = run(appendUser(emptyState(), '续费一天'), [
      ev('start', { conversation_id: 'cv-1', message_id: 3 }),
      ev('tool', { id: 'tc-0', name: 'get_instance', label: '查看实例详情', status: 'ok' }),
      ev('delta', { text: '请确认：' }),
      ev('confirm', { tool_call: card }),
      ev('done', { reason: 'awaiting_confirm' }),
    ])
    expect(s.status).toBe('awaiting_confirm')
    expect(s.items.at(-1)).toEqual({ kind: 'confirm', card, state: 'pending' })
    expect(s.items.at(-2)).toEqual({ kind: 'assistant', text: '请确认：', streaming: false })

    s = markConfirm(s, card.id, 'approved')
    expect(s.status).toBe('running')
    const ok = run(s, [
      ev('tool', { id: card.id, name: 'renew_instance', label: '续费实例', status: 'ok' }),
      ev('delta', { text: '已续费。' }),
      ev('done', { reason: 'end_turn' }),
    ])
    expect(ok.items.filter((i) => i.kind === 'tool').map((i) => i.kind === 'tool' && i.id)).toEqual(['tc-0']) // 不重复追加步骤
    expect(ok.items.find((i) => i.kind === 'confirm')).toMatchObject({ state: 'executed' })
    expect(ok.items.at(-1)).toEqual({ kind: 'assistant', text: '已续费。', streaming: false })
    expect(ok.status).toBe('idle')

    const bad = applyEvent(s, ev('tool', { id: card.id, name: 'renew_instance', label: '续费实例', status: 'error' }))
    expect(bad.items.find((i) => i.kind === 'confirm')).toMatchObject({ state: 'failed' })
  })

  it('同 id 的 tool 事件更新步骤而不是追加；同 id 的 confirm 替换卡片', () => {
    let s = run(emptyState(), [
      ev('tool', { id: 'tc-1', name: 'get_deployment_log', label: '读取部署日志', status: 'ok' }),
      ev('tool', { id: 'tc-1', name: 'get_deployment_log', label: '读取部署日志', status: 'error' }),
    ])
    expect(s.items).toEqual([{ kind: 'tool', id: 'tc-1', name: 'get_deployment_log', label: '读取部署日志', status: 'error' }])
    s = run(s, [ev('confirm', { tool_call: card }), ev('confirm', { tool_call: { ...card, summary: '新' } })])
    expect(s.items.filter((i) => i.kind === 'confirm')).toEqual([{ kind: 'confirm', card: { ...card, summary: '新' }, state: 'pending' }])
  })

  it('tool 事件缺 label 时用中文名表；缺 name 时显示「工具」', () => {
    const s = applyEvent(emptyState(), ev('tool', { id: 'tc-1', name: 'get_wallet', status: 'ok' }))
    expect(s.items[0]).toMatchObject({ label: '查询钱包' })
    const n = applyEvent(emptyState(), ev('tool', { id: 'tc-2', status: 'ok' }))
    expect(n.items[0]).toEqual({ kind: 'tool', id: 'tc-2', name: '', label: '工具', status: 'ok' })
  })

  it('tool 结果只改写待确认 / 已批准的卡片，已拒绝 / 已失效的不变', () => {
    const base = run(emptyState(), [ev('confirm', { tool_call: card }), ev('done', { reason: 'awaiting_confirm' })])
    const result = ev('tool', { id: card.id, name: 'renew_instance', label: '续费实例', status: 'ok' })
    expect(applyEvent(base, result).items[0]).toMatchObject({ state: 'executed' })
    expect(applyEvent(markConfirm(base, card.id, 'approved'), result).items[0]).toMatchObject({ state: 'executed' })
    for (const st of ['rejected', 'expired'] as const) {
      const s = markConfirm(base, card.id, st)
      expect(applyEvent(s, result)).toBe(s)
    }
  })

  it('done max_tokens → 最后一个助手气泡 truncated', () => {
    const s = run(emptyState(), [ev('delta', { text: '很长的回答' }), ev('done', { reason: 'max_tokens' })])
    expect(s.items).toEqual([{ kind: 'assistant', text: '很长的回答', streaming: false, truncated: true }])
    expect(s.status).toBe('idle')
  })

  it('done max_tokens 只标记本轮：本轮没有助手文本时不回头标记上一轮', () => {
    let s = run(appendUser(emptyState(), '一'), [ev('delta', { text: '上一轮' }), ev('done', { reason: 'end_turn' })])
    s = run(appendUser(s, '二'), [ev('start', { conversation_id: 'cv-1', message_id: 2 }), ev('done', { reason: 'max_tokens' })])
    expect(s.items).toEqual([
      { kind: 'user', text: '一' },
      { kind: 'assistant', text: '上一轮', streaming: false },
      { kind: 'user', text: '二' },
    ])
  })

  it('error 事件：追加错误项并带上最近的用户消息作为重试文本', () => {
    const s = run(appendUser(emptyState(), '看看日志'), [
      ev('start', { conversation_id: 'cv-1', message_id: 1 }),
      ev('delta', { text: '我' }),
      ev('error', { code: 'butler_llm_unavailable', message: 'AI 管家暂时不可用，请稍后再试' }),
    ])
    expect(s.items.slice(1)).toEqual([
      { kind: 'assistant', text: '我', streaming: false },
      { kind: 'error', code: 'butler_llm_unavailable', message: 'AI 管家暂时不可用，请稍后再试', retryText: '看看日志' },
    ])
    expect(s.status).toBe('idle')
  })

  it('不可变：不修改传入的状态与项', () => {
    const s0 = run(emptyState(), [ev('delta', { text: 'a' })])
    const snapshot = JSON.parse(JSON.stringify(s0))
    const s1 = run(s0, [ev('delta', { text: 'b' }), ev('confirm', { tool_call: card }), ev('done', { reason: 'awaiting_confirm' })])
    expect(s0).toEqual(snapshot)
    expect(s1.items).not.toBe(s0.items)
  })

  it('坏 JSON 与未知事件原样返回', () => {
    const s = emptyState()
    expect(applyEvent(s, { event: 'delta', data: '{oops' })).toBe(s)
    expect(applyEvent(s, { event: 'message', data: '{}' })).toBe(s)
    expect(applyEvent(s, { event: 'delta', data: 'null' })).toBe(s)
  })
})

describe('appendUser / appendError / markConfirm', () => {
  it('发新消息让仍待确认的卡片失效', () => {
    const s = appendUser(run(emptyState(), [ev('confirm', { tool_call: card })]), '算了')
    expect(s.items[0]).toMatchObject({ kind: 'confirm', state: 'expired' })
    expect(s.items[1]).toEqual({ kind: 'user', text: '算了' })
  })

  it('appendError 结束流式、回到 idle', () => {
    const s = appendError(run(emptyState(), [ev('delta', { text: 'x' })]), 'network', '网络异常，请稍后重试', '重发')
    expect(s.items).toEqual([
      { kind: 'assistant', text: 'x', streaming: false },
      { kind: 'error', code: 'network', message: '网络异常，请稍后重试', retryText: '重发' },
    ])
    expect(appendError(emptyState(), 'x', 'y').items[0]).toEqual({ kind: 'error', code: 'x', message: 'y' })
  })

  it('markConfirm：rejected → running；本地倒计时到 0 expired → idle；无此卡原样返回', () => {
    const s = run(emptyState(), [ev('confirm', { tool_call: card }), ev('done', { reason: 'awaiting_confirm' })])
    expect(markConfirm(s, card.id, 'rejected')).toMatchObject({ status: 'running', items: [{ state: 'rejected' }] })
    expect(markConfirm(s, card.id, 'expired')).toMatchObject({ status: 'idle', items: [{ state: 'expired' }] })
    expect(markConfirm(s, 'tc-none', 'expired')).toBe(s)
  })
})

describe('fromDetail', () => {
  const detail: ConversationDetail = {
    conversation: { id: 'cv-9', title: '续费', status: 'awaiting_confirm', last_message_at: '2026-10-25T10:00:00Z', created_at: '2026-10-25T09:59:00Z' },
    messages: [
      { id: 1, seq: 1, role: 'user', content: [{ type: 'text', text: '帮我续费 ins-abc' }], created_at: '' },
      {
        id: 2,
        seq: 2,
        role: 'assistant',
        content: [
          { type: 'text', text: '先看看。' },
          { type: 'tool', id: 'tc-1', name: 'get_instance', label: '查看实例详情', status: 'ok' },
        ],
        created_at: '',
      },
      { id: 3, seq: 3, role: 'tool', content: [{ type: 'tool_result', tool_call_id: 'tc-1', summary: '{"state":"running"}' }], created_at: '' },
      {
        id: 4,
        seq: 4,
        role: 'assistant',
        content: [
          { type: 'tool', id: 'tc-0', name: 'release_instance', label: '释放实例', status: 'ok' },
          { type: 'text', text: '请确认续费。' },
          { type: 'tool', id: card.id, name: 'renew_instance', label: '续费实例', status: 'ok' },
        ],
        created_at: '',
      },
      { id: 5, seq: 5, role: 'tool', content: [{ type: 'text', text: '用户拒绝了该操作' }], created_at: '' },
    ],
    pending_tool_call: card,
  }

  it('历史 → 聊天项；tool_result 摘要挂到步骤；pending → 末尾确认卡（去掉同 id 步骤）', () => {
    expect(fromDetail(detail, { dayUsed: 10, dayLimit: 100 })).toEqual({
      conversationId: 'cv-9',
      status: 'awaiting_confirm',
      dayUsed: 10,
      dayLimit: 100,
      items: [
        { kind: 'user', text: '帮我续费 ins-abc' },
        { kind: 'assistant', text: '先看看。', streaming: false },
        { kind: 'tool', id: 'tc-1', name: 'get_instance', label: '查看实例详情', status: 'ok', summary: '{"state":"running"}' },
        { kind: 'tool', id: 'tc-0', name: 'release_instance', label: '释放实例', status: 'ok' },
        { kind: 'assistant', text: '请确认续费。', streaming: false },
        { kind: 'confirm', card, state: 'pending' },
      ],
    })
  })

  it('无 pending、空消息', () => {
    const s = fromDetail({ conversation: { ...detail.conversation, status: 'idle' }, messages: [], pending_tool_call: null })
    expect(s).toEqual({ conversationId: 'cv-9', status: 'idle', items: [], dayUsed: 0, dayLimit: 0 })
  })

  it('相邻的助手文本块（跨消息）用空行连接', () => {
    const s = fromDetail({
      ...detail,
      pending_tool_call: null,
      messages: [
        { id: 1, seq: 1, role: 'assistant', content: [{ type: 'text', text: '第一段' }, { type: 'text', text: '第二段' }] },
        { id: 2, seq: 2, role: 'assistant', content: [{ type: 'text', text: '第三段' }] },
      ],
    })
    expect(s.items).toEqual([{ kind: 'assistant', text: '第一段\n\n第二段\n\n第三段', streaming: false }])
  })

  it('error 状态的步骤保留 error', () => {
    const s = fromDetail({
      ...detail,
      pending_tool_call: null,
      messages: [{ id: 1, seq: 1, role: 'assistant', content: [{ type: 'tool', id: 'tc-x', name: 'get_deployment_log', label: '', status: 'error' }], created_at: '' }],
    })
    expect(s.items).toEqual([{ kind: 'tool', id: 'tc-x', name: 'get_deployment_log', label: '读取部署日志', status: 'error' }])
  })
})

describe('formatQuota', () => {
  it('三档', () => {
    expect(formatQuota(36000, 300000)).toEqual({ text: '今日已用 12%', level: 'ok' })
    expect(formatQuota(269999, 300000)).toEqual({ text: '今日已用 89%', level: 'ok' })
    expect(formatQuota(270000, 300000)).toEqual({ text: '今日已用 90%', level: 'warn' })
    expect(formatQuota(299999, 300000)).toEqual({ text: '今日已用 99%', level: 'warn' })
    expect(formatQuota(300000, 300000)).toEqual({ text: '今日已用 100%', level: 'exhausted' })
    expect(formatQuota(450000, 300000)).toEqual({ text: '今日已用 100%', level: 'exhausted' })
  })
  it('零用量；限额未加载时不显示', () => {
    expect(formatQuota(0, 300000)).toEqual({ text: '今日已用 0%', level: 'ok' })
    expect(formatQuota(0, 0)).toEqual({ text: '', level: 'ok' }) // 0 = 额度尚未加载
    expect(formatQuota(500, -1)).toEqual({ text: '', level: 'ok' })
  })
})

describe('isDanger', () => {
  it('只有释放 / 卸载 / 取消预订', () => {
    expect(['release_instance', 'remove_app', 'cancel_booking'].every(isDanger)).toBe(true)
    expect(['place_order', 'renew_instance', 'deploy_app', 'redeploy_app', 'set_app_state', 'list_instances', ''].some(isDanger)).toBe(false)
  })
})

describe('确认卡辅助', () => {
  it('confirmTitle 用中文名表，未知工具退回 label', () => {
    expect(confirmTitle(card)).toBe('续费实例')
    expect(confirmTitle({ ...card, tool: 'unknown_tool', label: '某操作' })).toBe('某操作')
  })
  it('confirmSummaryLines 去首尾空行', () => {
    expect(confirmSummaryLines({ ...card, summary: '\n续费实例 ins-abc\r\n\n时长：1 天\n\n' })).toEqual(['续费实例 ins-abc', '', '时长：1 天'])
  })
  it('confirmAmountText：扣款 / 退款 / 无报价', () => {
    expect(confirmAmountText(card)).toBe('预计扣款 ¥99.00')
    expect(confirmAmountText({ ...card, tool: 'cancel_booking', quote_amount: 12345 })).toBe('退款 ¥123.45')
    expect(confirmAmountText({ ...card, quote_amount: null })).toBe('')
  })
})

describe('secondsLeft', () => {
  it('剩余整秒，过期为 0，非法时间为 0', () => {
    const now = new Date('2026-10-25T10:00:00Z')
    expect(secondsLeft('2026-10-25T10:10:00Z', now)).toBe(600)
    expect(secondsLeft('2026-10-25T10:00:00.999Z', now)).toBe(0)
    expect(secondsLeft('2026-10-25T10:00:01.5Z', now)).toBe(1)
    expect(secondsLeft('2026-10-25T09:59:00Z', now)).toBe(0)
    expect(secondsLeft('not a date', now)).toBe(0)
  })
})

it('SUGGESTIONS 4 条', () => {
  expect(SUGGESTIONS).toHaveLength(4)
})
