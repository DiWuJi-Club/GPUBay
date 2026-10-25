import { ApiError, type ButlerStatus, type ConversationDetail, type PendingToolCall, type SSEEvent } from '@spark/shared'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import ElementPlus from 'element-plus'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { nextTick } from 'vue'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'

type Handler = (e: SSEEvent) => void
const mocks = vi.hoisted(() => ({
  get: vi.fn(),
  stream: vi.fn(),
  unauthorized: vi.fn(),
}))
vi.mock('../../api', () => ({ api: { get: mocks.get }, onUnauthorized: vi.fn() }))
vi.mock('../../store/session', () => ({ handleUnauthorized: mocks.unauthorized }))
vi.mock('@spark/shared', async (orig) => ({ ...(await orig<typeof import('@spark/shared')>()), streamPost: mocks.stream }))

import ButlerChat from '../ButlerChat.vue'

const ev = (event: string, data: unknown): SSEEvent => ({ event, data: JSON.stringify(data) })
const card: PendingToolCall = {
  id: 'tc-1',
  tool: 'renew_instance',
  label: '续费实例',
  summary: '续费实例 ins-abc\n时长：1 天',
  quote_amount: 9900,
  args: { instance_id: 'ins-abc' },
  expires_at: '2000-01-01T00:00:00Z', // 浏览器时钟早已过期：倒计时以 expires_in 为准
  expires_in: 600,
  danger: false,
}

let status: ButlerStatus
let detail: ConversationDetail
/** 每次 streamPost 依次取一个脚本：事件数组，或要抛出的错误 */
let scripts: (SSEEvent[] | Error)[]
let wrapper: VueWrapper | null = null
let router: Router

async function render(props: Record<string, unknown> = {}) {
  router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/:p(.*)*', component: { template: '<div />' } }] })
  await router.push('/market')
  wrapper = mount(ButlerChat, { props, attachTo: document.body, global: { plugins: [ElementPlus, router] } })
  await flushPromises()
  return wrapper
}
const $ = (id: string) => document.querySelector<HTMLElement>(`[data-testid="${id}"]`)
const $$ = (id: string) => [...document.querySelectorAll<HTMLElement>(`[data-testid="${id}"]`)]
async function click(id: string) {
  $(id)!.click()
  await flushPromises()
}
async function type(text: string) {
  const ta = $('butler-input') as HTMLTextAreaElement
  ta.value = text
  ta.dispatchEvent(new Event('input'))
  await flushPromises()
}

/** 确认卡的倒计时由 Date.now() 与 useTicker 的 setInterval 驱动：冻结时钟、手动推进，避免负载下毫秒级漂移让 10:00 变成 10:01 / 09:59 */
const FAKE_NOW = new Date('2026-10-25T08:00:00Z')
async function tick(ms: number) {
  await vi.advanceTimersByTimeAsync(ms)
  await nextTick()
}

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['Date', 'setInterval', 'clearInterval'] })
  vi.setSystemTime(FAKE_NOW)
  status = { enabled: true, notice: '', day_used: 1000, day_limit: 300000, provider_label: '平台模型', ingress_domain: 'spark.test' }
  detail = { conversation: { id: 'cv-1', title: '帮我选一台机器', status: 'idle', last_message_at: null, created_at: '2026-10-25T00:00:00Z' }, messages: [], pending_tool_call: null }
  scripts = []
  mocks.get.mockReset()
  mocks.get.mockImplementation((path: string) => {
    if (path === '/butler/status') return Promise.resolve(status)
    if (path.startsWith('/butler/conversations/')) return Promise.resolve(detail)
    return Promise.reject(new Error(`unexpected ${path}`))
  })
  mocks.stream.mockReset()
  mocks.stream.mockImplementation(async (_base: string, _path: string, _body: unknown, onEvent: Handler) => {
    const s = scripts.shift()
    if (s instanceof Error) throw s
    for (const e of s ?? []) onEvent(e)
  })
  mocks.unauthorized.mockReset()
})
afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  document.body.innerHTML = ''
  vi.useRealTimers()
})

describe('ButlerChat', () => {
  it('空状态建议 chip → 流式回答、步骤行、额度；新对话 id 向上同步', async () => {
    const w = await render()
    expect(mocks.get).toHaveBeenCalledWith('/butler/status')
    expect($$('butler-suggest-0').length + $$('butler-suggest-3').length).toBe(2)
    expect($('butler-quota')!.textContent).toBe('今日已用 0%')
    scripts.push([
      ev('start', { conversation_id: 'cv-1', message_id: 1 }),
      ev('tool', { id: 'tc-r', name: 'list_products', label: '查询套餐', status: 'ok' }),
      ev('delta', { text: '推荐 **GB10 单机**，详见 [实例](/instances/ins-abc) 与 [这里](https://evil.example/)' }),
      ev('usage', { input_tokens: 10, output_tokens: 5, day_used: 285000, day_limit: 300000 }),
      ev('done', { reason: 'end_turn' }),
    ])
    await click('butler-suggest-0')
    expect(mocks.stream).toHaveBeenCalledWith('/api/v1', '/butler/conversations/new/messages', { text: '帮我选一台机器' }, expect.any(Function), expect.anything())
    expect($('butler-msg-user')!.textContent).toBe('帮我选一台机器')
    expect($('butler-step-list_products')!.textContent).toBe('✓ 查询套餐')
    const a = $('butler-msg-assistant')!
    expect(a.querySelector('strong')!.textContent).toBe('GB10 单机')
    expect(a.querySelector('a')!.getAttribute('href')).toBe('/instances/ins-abc')
    expect(a.querySelectorAll('a')).toHaveLength(1) // 站外链接为纯文本
    expect($('butler-quota')!.textContent).toBe('今日已用 95%')
    expect($('butler-quota')!.className).toContain('warn')
    expect(w.emitted('update:conversationId')).toEqual([['cv-1']])
    expect(w.emitted('settled')).toHaveLength(1)
  })

  it('Enter 发送（带实例上下文，收到 start 后 emit context-used）；Shift+Enter 不发送', async () => {
    const w = await render({ context: { instance_id: 'ins-abc' }, initialDraft: '这个实例上的应用为什么失败了？' })
    expect(($('butler-input') as HTMLTextAreaElement).value).toBe('这个实例上的应用为什么失败了？')
    $('butler-input')!.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', shiftKey: true, bubbles: true }))
    await flushPromises()
    expect(mocks.stream).not.toHaveBeenCalled()
    scripts.push([ev('start', { conversation_id: 'cv-1', message_id: 1 }), ev('delta', { text: '日志显示' }), ev('done', { reason: 'end_turn' })])
    $('butler-input')!.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }))
    await flushPromises()
    expect(mocks.stream.mock.calls[0][2]).toEqual({ text: '这个实例上的应用为什么失败了？', context: { instance_id: 'ins-abc' } })
    expect(w.emitted('context-used')).toHaveLength(1)
    expect(($('butler-input') as HTMLTextAreaElement).value).toBe('')
    expect(w.emitted('update:draft')!.at(-1)).toEqual([''])
  })

  it('额度用完：横幅显示服务端文案，用户气泡撤回、原文放回输入框', async () => {
    await render()
    scripts.push(new ApiError(429, 'butler_quota_exceeded', '今日 AI 管家额度已用完，明天再来', ''))
    await type('你好')
    await click('butler-send')
    expect($('butler-banner')!.textContent).toContain('今日 AI 管家额度已用完，明天再来')
    expect($('butler-msg-user')).toBeNull()
    expect(($('butler-input') as HTMLTextAreaElement).value).toBe('你好')
  })

  it('管家关闭：横幅带 notice，输入框禁用', async () => {
    status = { ...status, enabled: false, notice: '系统维护中' }
    await render()
    expect($('butler-banner')!.textContent).toContain('AI 管家暂未开放：系统维护中')
    expect(($('butler-input') as HTMLTextAreaElement).disabled).toBe(true)
  })

  it('SSE error（模型不可用）→ 错误行，重试重发同一条文本', async () => {
    await render()
    scripts.push([ev('start', { conversation_id: 'cv-1', message_id: 1 }), ev('error', { code: 'butler_llm_unavailable', message: 'AI 管家暂时不可用，请稍后再试' })])
    await type('我的余额还能用多久')
    await click('butler-send')
    expect($('butler-error')!.textContent).toContain('AI 管家暂时不可用')
    scripts.push([ev('start', { conversation_id: 'cv-1', message_id: 3 }), ev('delta', { text: '余额 ¥100' }), ev('done', { reason: 'end_turn' })])
    await click('butler-retry')
    expect(mocks.stream.mock.calls[1][1]).toBe('/butler/conversations/cv-1/messages')
    expect(mocks.stream.mock.calls[1][2]).toEqual({ text: '我的余额还能用多久' })
    expect($('butler-error')).toBeNull()
    expect($('butler-msg-assistant')!.textContent).toContain('余额 ¥100')
  })

  it('busy（建立流之前）→ 错误行可重试，气泡撤回', async () => {
    await render()
    scripts.push(new ApiError(409, 'butler_busy', '上一条消息还在处理中，请稍候', ''))
    await type('你好')
    await click('butler-send')
    expect($('butler-error')!.textContent).toContain('上一条消息还在处理中')
    expect($('butler-retry')).not.toBeNull()
    expect($('butler-msg-user')).toBeNull()
  })

  it('401 → 交给会话的 unauthorized 处理（跳登录）', async () => {
    await render()
    scripts.push(new ApiError(401, 'unauthorized', '请先登录', ''))
    await type('你好')
    await click('butler-send')
    expect(mocks.unauthorized).toHaveBeenCalledOnce()
    expect($('butler-error')).toBeNull()
  })

  it('流在 done 之前断开 → 拉详情刷新；running 时继续轮询直到结束', async () => {
    vi.useFakeTimers({ toFake: ['Date', 'setInterval', 'clearInterval', 'setTimeout', 'clearTimeout'] })
    vi.setSystemTime(FAKE_NOW)
    try {
      await render()
      detail = { ...detail, conversation: { ...detail.conversation, status: 'running' } }
      scripts.push([ev('start', { conversation_id: 'cv-1', message_id: 1 }), ev('delta', { text: '我查' })])
      await type('帮我选一台机器')
      await click('butler-send')
      expect(mocks.get).toHaveBeenCalledWith('/butler/conversations/cv-1')
      expect($('butler-waiting')).not.toBeNull()
      const calls = () => mocks.get.mock.calls.filter((c) => c[0] === '/butler/conversations/cv-1').length
      expect(calls()).toBe(1)
      detail = {
        ...detail,
        conversation: { ...detail.conversation, status: 'idle' },
        messages: [
          { id: 1, seq: 1, role: 'user', content: [{ type: 'text', text: '帮我选一台机器' }] },
          { id: 2, seq: 2, role: 'assistant', content: [{ type: 'text', text: '推荐 GB10' }] },
        ],
      }
      await vi.advanceTimersByTimeAsync(3_000)
      await flushPromises()
      expect(calls()).toBe(2)
      expect($('butler-msg-assistant')!.textContent).toContain('推荐 GB10')
      expect($('butler-waiting')).toBeNull()
      await vi.advanceTimersByTimeAsync(6_000)
      expect(calls()).toBe(2) // idle 后停止轮询
    } finally {
      vi.useRealTimers()
    }
  })

  it('确认卡：倒计时按 expires_in；确认 → confirm 流 {approve: true}；执行失败显示服务端文案', async () => {
    await render()
    scripts.push([ev('start', { conversation_id: 'cv-1', message_id: 1 }), ev('confirm', { tool_call: card }), ev('done', { reason: 'awaiting_confirm' })])
    await type('续费')
    await click('butler-send')
    expect($('butler-confirm-title')!.textContent).toBe('续费实例')
    expect($('butler-confirm-amount')!.textContent).toBe('预计扣款 ¥99.00')
    expect($('butler-confirm-countdown')!.textContent).toBe('10:00 内有效')
    await tick(1_000)
    expect($('butler-confirm-countdown')!.textContent).toBe('09:59 内有效')
    expect($('butler-confirm-danger')).toBeNull()
    scripts.push([ev('tool', { id: 'tc-1', name: 'renew_instance', label: '续费实例', status: 'error', message: '余额不足' }), ev('delta', { text: '余额不足，请先充值' }), ev('done', { reason: 'end_turn' })])
    await click('butler-confirm-ok')
    expect(mocks.stream.mock.calls[1][1]).toBe('/butler/conversations/cv-1/tool-calls/tc-1/confirm')
    expect(mocks.stream.mock.calls[1][2]).toEqual({ approve: true })
    expect($('butler-confirm-result')!.textContent).toBe('执行失败：余额不足')
    expect($('butler-confirm-ok')).toBeNull()
  })

  it('危险卡需勾选「我已了解」；取消 → {approve: false}、已取消', async () => {
    detail = {
      ...detail,
      conversation: { ...detail.conversation, status: 'awaiting_confirm' },
      pending_tool_call: { ...card, id: 'tc-2', tool: 'release_instance', label: '释放实例', quote_amount: null, danger: true },
    }
    await render({ conversationId: 'cv-1' })
    expect($('butler-confirm-danger')!.textContent).toBe('此操作不可恢复')
    expect(($('butler-confirm-ok') as HTMLButtonElement).disabled).toBe(true)
    $('butler-confirm-ack')!.querySelector('input')!.click()
    await flushPromises()
    expect(($('butler-confirm-ok') as HTMLButtonElement).disabled).toBe(false)
    scripts.push([ev('delta', { text: '好的，已取消' }), ev('done', { reason: 'end_turn' })])
    await click('butler-confirm-cancel')
    expect(mocks.stream.mock.calls[0][2]).toEqual({ approve: false })
    expect($('butler-confirm-result')!.textContent).toBe('已取消')
  })

  it('确认已失效（服务端 butler_confirm_invalid）→ 卡片「已失效」、按钮禁用', async () => {
    detail = { ...detail, conversation: { ...detail.conversation, status: 'awaiting_confirm' }, pending_tool_call: card }
    await render({ conversationId: 'cv-1' })
    scripts.push(new ApiError(409, 'butler_confirm_invalid', '该操作已失效，请重新发起', ''))
    await click('butler-confirm-ok')
    expect($('butler-confirm-result')!.textContent).toBe('已失效')
    expect(($('butler-confirm-ok') as HTMLButtonElement).disabled).toBe(true)
    expect(($('butler-confirm-cancel') as HTMLButtonElement).disabled).toBe(true)
  })

  it('倒计时走到 0 → 卡片已失效、按钮禁用，不发 confirm', async () => {
    detail = { ...detail, conversation: { ...detail.conversation, status: 'awaiting_confirm' }, pending_tool_call: { ...card, expires_in: 3 } }
    await render({ conversationId: 'cv-1' })
    expect($('butler-confirm-countdown')!.textContent).toBe('00:03 内有效')
    await tick(2_000)
    expect($('butler-confirm-countdown')!.textContent).toBe('00:01 内有效')
    expect(($('butler-confirm-ok') as HTMLButtonElement).disabled).toBe(false)
    await tick(1_000)
    await flushPromises()
    expect($('butler-confirm-countdown')).toBeNull()
    expect($('butler-confirm-result')!.textContent).toBe('已失效')
    expect(($('butler-confirm-ok') as HTMLButtonElement).disabled).toBe(true)
    expect(mocks.stream).not.toHaveBeenCalled()
  })

  it('expires_in = 0 的卡片直接显示已失效', async () => {
    detail = { ...detail, conversation: { ...detail.conversation, status: 'awaiting_confirm' }, pending_tool_call: { ...card, expires_in: 0 } }
    await render({ conversationId: 'cv-1' })
    expect($('butler-confirm-result')!.textContent).toBe('已失效')
    expect(($('butler-confirm-ok') as HTMLButtonElement).disabled).toBe(true)
  })

  it('max_tokens → 截断提示；新对话按钮清空并 emit null', async () => {
    const w = await render()
    scripts.push([ev('start', { conversation_id: 'cv-1', message_id: 1 }), ev('delta', { text: '很长' }), ev('done', { reason: 'max_tokens' })])
    await type('写一篇长文')
    await click('butler-send')
    expect($('butler-truncated')).not.toBeNull()
    await click('butler-new')
    expect($('butler-msg-user')).toBeNull()
    expect($('butler-suggest-0')).not.toBeNull()
    expect(w.emitted('update:conversationId')!.at(-1)).toEqual([null])
  })

  it('确认已失效后回到 idle：可以直接发新消息', async () => {
    detail = { ...detail, conversation: { ...detail.conversation, status: 'awaiting_confirm' }, pending_tool_call: card }
    await render({ conversationId: 'cv-1' })
    scripts.push(new ApiError(409, 'butler_confirm_invalid', '该操作已失效，请重新发起', ''))
    await click('butler-confirm-ok')
    scripts.push([ev('start', { conversation_id: 'cv-1', message_id: 5 }), ev('delta', { text: '好的' }), ev('done', { reason: 'end_turn' })])
    await type('那算了')
    expect(($('butler-send') as HTMLButtonElement).disabled).toBe(false)
    await click('butler-send')
    expect(mocks.stream.mock.calls[1][1]).toBe('/butler/conversations/cv-1/messages')
    expect($('butler-msg-assistant')!.textContent).toContain('好的')
  })

  it('确认流里的 SSE error 不带重试（重试不会重发上一条用户消息）', async () => {
    detail = { ...detail, conversation: { ...detail.conversation, status: 'awaiting_confirm' }, pending_tool_call: card, messages: [{ id: 1, seq: 1, role: 'user', content: [{ type: 'text', text: '续费' }] }] }
    await render({ conversationId: 'cv-1' })
    scripts.push([ev('tool', { id: 'tc-1', name: 'renew_instance', label: '续费实例', status: 'ok' }), ev('error', { code: 'butler_llm_unavailable', message: 'AI 管家暂时不可用，请稍后再试' })])
    await click('butler-confirm-ok')
    expect($('butler-confirm-result')!.textContent).toBe('已执行')
    expect($('butler-error')!.textContent).toContain('AI 管家暂时不可用')
    expect($('butler-retry')).toBeNull()
  })

  it('发送在服务端收下之前失败：恢复发送前的状态（待确认卡不被本地判为失效）', async () => {
    detail = { ...detail, conversation: { ...detail.conversation, status: 'awaiting_confirm' }, pending_tool_call: card }
    await render({ conversationId: 'cv-1' })
    scripts.push(new ApiError(409, 'butler_busy', '上一条消息还在处理中，请稍候', ''))
    await type('再说一句')
    await click('butler-send')
    expect($('butler-confirm-card')!.dataset.state).toBe('pending')
    expect($('butler-confirm-countdown')).not.toBeNull()
    expect($('butler-error')).not.toBeNull()
  })

  it('站内链接走前端路由，实例域名链接不拦截', async () => {
    await render()
    scripts.push([
      ev('start', { conversation_id: 'cv-1', message_id: 1 }),
      ev('delta', { text: '看 [实例](/instances/ins-abc) 或 [文档](https://ins-abc-8000.spark.test/docs)' }),
      ev('done', { reason: 'end_turn' }),
    ])
    await type('看看')
    await click('butler-send')
    const links = [...$('butler-msg-assistant')!.querySelectorAll('a')]
    expect(links).toHaveLength(2)
    const inSite = new MouseEvent('click', { bubbles: true, cancelable: true, button: 0 })
    links[0].dispatchEvent(inSite)
    await flushPromises()
    expect(inSite.defaultPrevented).toBe(true)
    expect(router.currentRoute.value.fullPath).toBe('/instances/ins-abc')
    const external = new MouseEvent('click', { bubbles: true, cancelable: true, button: 0 })
    links[1].dispatchEvent(external)
    expect(external.defaultPrevented).toBe(false)
    expect(links[1].getAttribute('target')).toBe('_blank')
  })

  it('流式中切换对话：旧流的迟到事件被丢弃', async () => {
    let late: Handler | null = null
    let release: () => void = () => {}
    mocks.stream.mockImplementationOnce(async (_b: string, _p: string, _body: unknown, onEvent: Handler) => {
      onEvent(ev('start', { conversation_id: 'cv-1', message_id: 1 }))
      onEvent(ev('delta', { text: '旧对话的回答' }))
      late = onEvent
      await new Promise<void>((r) => (release = r))
    })
    const w = await render()
    await type('第一问')
    await click('butler-send')
    expect($('butler-msg-assistant')!.textContent).toContain('旧对话的回答')
    detail = { ...detail, conversation: { ...detail.conversation, id: 'cv-2', title: '另一段' }, messages: [{ id: 9, seq: 1, role: 'user', content: [{ type: 'text', text: '另一段的问题' }] }] }
    await w.setProps({ conversationId: 'cv-2' })
    await flushPromises()
    expect(mocks.get).toHaveBeenCalledWith('/butler/conversations/cv-2')
    late!(ev('delta', { text: '迟到的文字' }))
    late!(ev('done', { reason: 'end_turn' }))
    release()
    await flushPromises()
    expect($('butler-msg-user')!.textContent).toBe('另一段的问题')
    expect(document.body.textContent).not.toContain('迟到的文字')
    expect(document.body.textContent).not.toContain('旧对话的回答')
  })

  it('字数按字符计；超过 2000 字发送按钮禁用', async () => {
    await render()
    await type('😀'.repeat(1000))
    expect($('butler-count')!.textContent).toBe('1000/2000')
    expect(($('butler-send') as HTMLButtonElement).disabled).toBe(false)
    await type('字'.repeat(2001))
    expect($('butler-count')!.textContent).toBe('2001/2000')
    expect(($('butler-send') as HTMLButtonElement).disabled).toBe(true)
  })
})
