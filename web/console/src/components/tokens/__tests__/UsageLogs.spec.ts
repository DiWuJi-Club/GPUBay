import { flushPromises, mount } from '@vue/test-utils'
import type { LLMAccount, LLMDailyRow, LLMKey, LLMModelPublic, LLMSettlement, LLMUsageRow, User } from '@spark/shared'
import ElementPlus from 'element-plus'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter, RouterView, type Router } from 'vue-router'

const { get } = vi.hoisted(() => ({ get: vi.fn() }))
vi.mock('../../../api', () => ({ api: { get, post: vi.fn(), put: vi.fn(), del: vi.fn() }, onUnauthorized: vi.fn() }))

import { addDays, beijingToday } from '../../../logic/tokens'
import { session } from '../../../store/session'
import TokensView from '../../../views/TokensView.vue'

const model = (slug: string): LLMModelPublic => ({
  slug, name: slug, description: '', kind: 'chat', context_len: 32768, max_output_tokens: 8192, input_price: 100, output_price: 200, features: [], available: true,
})
const account = (over: Partial<LLMAccount> = {}): LLMAccount => ({
  debt: 0, unsettled: 3, today_cost: 120, month_cost: 4567, month_tokens: 1_234_000, limits: { rpm: 120, tpm: 500_000, concurrency: 16 },
  blocked: false, blocked_reason: '', kyc_verified: true, base_url: 'https://x/v1', ...over,
})
const usage = (id: string, over: Partial<LLMUsageRow> = {}): LLMUsageRow => ({
  request_id: id, created_at: '2026-10-24T02:30:05Z', model: 'qwen3-4b', key_name: '生产', endpoint: 'chat', stream: true, prompt_tokens: 1200,
  completion_tokens: 345, estimated: false, cost_micro: 1_250_000, status: 'ok', http_status: 200, latency_ms: 2345, ttft_ms: 180, ...over,
})
const settlement = (id: number, over: Partial<LLMSettlement> = {}): LLMSettlement => ({
  id, created_at: '2026-10-24T02:40:00Z', requests: 12, prompt_tokens: 12_000, completion_tokens: 3400, usage_amount: 120, debt_amount: 0, charged: 120,
  wallet_ledger_id: 900 + id, period_from: null, period_to: null, ...over,
})
const key = (id: number, name: string): LLMKey => ({
  id, name, key_hint: 'sk-spark-…abcd', status: 'active', model_slugs: [], monthly_cap: null, month_cost: 0, expires_at: null, last_used_at: null, created_at: '2026-10-01T00:00:00Z',
})

let acct: LLMAccount
let daily: LLMDailyRow[]
let settlements: { items: LLMSettlement[]; total: number }
let logs: { items: LLMUsageRow[]; total: number }
const writeText = vi.fn()

let router: Router
let wrapper: ReturnType<typeof mount> | undefined
async function mountAt(url: string) {
  router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/tokens', component: TokensView },
      { path: '/wallet', component: { template: '<div data-testid="wallet-stub" />' } },
      { path: '/wallet/recharge', component: { template: '<div data-testid="recharge-stub" />' } },
    ],
  })
  await router.push(url)
  await router.isReady()
  wrapper = mount(RouterView, { global: { plugins: [router, ElementPlus] }, attachTo: document.body })
  await flushPromises()
}
const byId = (id: string) => document.querySelector<HTMLElement>(`[data-testid="${id}"]`)
const allById = (id: string) => [...document.querySelectorAll<HTMLElement>(`[data-testid="${id}"]`)]
const calls = (path: string) => get.mock.calls.filter(([p]) => p === path)
async function setDate(id: string, v: string) {
  const el = byId(id) as HTMLInputElement
  el.value = v
  el.dispatchEvent(new Event('input'))
  await flushPromises()
}

beforeEach(() => {
  vi.stubGlobal('matchMedia', (q: string) => ({ matches: false, media: q, addEventListener: () => {}, removeEventListener: () => {} }))
  writeText.mockReset().mockResolvedValue(undefined)
  vi.stubGlobal('navigator', { ...navigator, clipboard: { writeText } })
  get.mockReset()
  acct = account()
  daily = [
    { day: '2026-10-23', model: 'qwen3-4b', requests: 3, prompt_tokens: 1000, completion_tokens: 200, cost_micro: 500_000 },
    { day: '2026-10-24', model: 'qwen3-4b', requests: 1234, prompt_tokens: 1_234_567, completion_tokens: 89, cost_micro: 123_456_789 },
  ]
  settlements = {
    items: [
      settlement(2, { usage_amount: 120, debt_amount: 50, charged: 100 }),
      settlement(1, { charged: 0, wallet_ledger_id: null, usage_amount: 0, debt_amount: 30, requests: 0 }),
    ],
    total: 2,
  }
  logs = {
    items: [
      usage('req-aaaaaaaaaaaaaaaa'),
      usage('req-bbbbbbbbbbbbbbbb', { status: 'client_abort', estimated: true, prompt_tokens: 1000, completion_tokens: 40, cost_micro: 300, latency_ms: 800, ttft_ms: null, stream: false, endpoint: 'embeddings' }),
    ],
    total: 45,
  }
  get.mockImplementation(async (path: string) => {
    if (path === '/llm/models') return { items: [model('qwen3-4b'), model('glm-4')], base_url: 'https://x/v1' }
    if (path === '/llm/account') return acct
    if (path === '/llm/usage/daily') return { items: daily }
    if (path === '/llm/settlements') return settlements
    if (path === '/llm/usage') return logs
    if (path === '/llm/keys') return { items: [key(5, '生产'), key(6, '测试')] }
    throw new Error(`unexpected ${path}`)
  })
  session.status = 'authed'
  session.user = { id: 1, phone: '138****0000', kyc_status: 'verified' } as User
})
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  document.body.innerHTML = ''
  session.status = 'unknown'
  session.user = null
  vi.unstubAllGlobals()
})

describe('欠费 / 停用提示条', () => {
  it('debt > 0：红色提示带金额，「去充值」到 /wallet/recharge', async () => {
    acct = account({ debt: 1234 })
    await mountAt('/tokens')
    const alert = byId('tokens-debt-alert')!
    // 测试里 Transition 被替换成 stub，属性落在 stub 上；真实页面落在 .el-alert 根元素上
    expect(alert.matches('.el-alert--error') || !!alert.querySelector('.el-alert--error')).toBe(true)
    expect(alert.textContent).toContain('账户有欠费 ¥12.34，充值后自动补扣，补扣前 API 调用将被拒绝')
    byId('tokens-debt-recharge')!.click()
    await flushPromises()
    expect(router.currentRoute.value.path).toBe('/wallet/recharge')
  })

  it('没有欠费、未停用：不显示提示条', async () => {
    await mountAt('/tokens')
    expect(byId('tokens-debt-alert')).toBeNull()
    expect(byId('tokens-blocked-alert')).toBeNull()
  })

  it('被平台停用：显示原因', async () => {
    acct = account({ blocked: true, blocked_reason: '滥用' })
    await mountAt('/tokens')
    expect(byId('tokens-blocked-alert')!.textContent).toContain('Token 服务已被平台停用')
    expect(byId('tokens-blocked-reason')!.textContent).toBe('原因：滥用')
  })

  it('未登录不请求 /llm/account', async () => {
    session.status = 'anon'
    session.user = null
    await mountAt('/tokens')
    expect(calls('/llm/account')).toHaveLength(0)
    expect(byId('tokens-debt-alert')).toBeNull()
  })

  it('快速开始显示账户的实际限额', async () => {
    await mountAt('/tokens?tab=quickstart')
    expect(byId('tokens-quickstart-note-limits')!.textContent).toMatch(/120 次.*500K token.*16 个并发/s)
  })
})

describe('用量 Tab', () => {
  it('统计卡：今日 / 本月消费、本月 token、未结算、欠费', async () => {
    acct = account({ debt: 30 })
    await mountAt('/tokens?tab=usage')
    expect(byId('tokens-stat-today')!.textContent).toBe('¥1.20')
    expect(byId('tokens-stat-month')!.textContent).toBe('¥45.67')
    expect(byId('tokens-stat-tokens')!.textContent).toBe('1.2M')
    expect(byId('tokens-stat-unsettled')!.textContent).toBe('¥0.03')
    expect(byId('tokens-stat-debt')!.textContent).toBe('¥0.30')
    expect(byId('tokens-stat-debt')!.closest('.stat')!.classList.contains('danger')).toBe(true)
  })

  it('按天 × 模型：最近 30 天，新日期在前，费用按微分显示', async () => {
    await mountAt('/tokens?tab=usage')
    expect(get).toHaveBeenCalledWith('/llm/usage/daily', { days: 30 })
    expect(allById('tokens-daily-row').map((e) => e.textContent)).toEqual(['2026-10-24', '2026-10-23'])
    const first = byId('tokens-daily-row')!.closest('tr')!
    expect(first.textContent).toContain('1,234,567')
    expect(allById('tokens-daily-cost').map((e) => e.textContent)).toEqual(['¥1.234568', '< ¥0.01'])
  })

  it('结算记录：金额为实际扣款，明细写明补扣与转欠费，有流水的链接到 /wallet?ledger=', async () => {
    await mountAt('/tokens?tab=usage')
    expect(get).toHaveBeenCalledWith('/llm/settlements', { page: 1, page_size: 10 })
    expect(byId('tokens-settlement-charged-2')!.textContent).toBe('¥1.00')
    expect(byId('tokens-settlement-detail-2')!.textContent).toBe('用量 ¥1.20；补扣欠费 ¥0.50；余额不足，¥0.70 计入欠费')
    const row2 = byId('tokens-settlement-row-2')!.closest('tr')!
    expect(row2.textContent).toContain('12K / 3.4K')
    expect(byId('tokens-settlement-ledger-2')!.getAttribute('href')).toBe('/wallet?ledger=902')
    // charged = 0：没有流水
    expect(byId('tokens-settlement-ledger-1')).toBeNull()
    expect(byId('tokens-settlement-detail-1')!.textContent).toBe('补扣欠费 ¥0.30；余额不足，¥0.30 计入欠费')
    byId('tokens-settlement-ledger-2')!.click()
    await flushPromises()
    expect(router.currentRoute.value.fullPath).toBe('/wallet?ledger=902')
  })

  it('结算记录分页', async () => {
    settlements = { items: [settlement(30)], total: 25 }
    await mountAt('/tokens?tab=usage')
    const pager = byId('tokens-settlements-pager')!
    pager.querySelector<HTMLElement>('.btn-next')!.click()
    await flushPromises()
    expect(get).toHaveBeenLastCalledWith('/llm/settlements', { page: 2, page_size: 10 })
  })

  it('没打开用量 Tab 不请求明细', async () => {
    await mountAt('/tokens')
    expect(calls('/llm/usage/daily')).toHaveLength(0)
    expect(calls('/llm/settlements')).toHaveLength(0)
  })
})

describe('请求日志 Tab', () => {
  const today = beijingToday(new Date())

  it('默认最近 7 天，第一页；列：时间、模型、Key、端点、流式、token（估算带 ≈）、费用、状态、延迟 / TTFT、request_id', async () => {
    await mountAt('/tokens?tab=logs')
    expect(get).toHaveBeenCalledWith('/llm/usage', { from: addDays(today, -6), to: today, page: 1, page_size: 20, model: undefined, key_id: undefined })
    expect((byId('tokens-logs-from') as HTMLInputElement).type).toBe('date')
    expect((byId('tokens-logs-from') as HTMLInputElement).value).toBe(addDays(today, -6))
    expect((byId('tokens-logs-to') as HTMLInputElement).max).toBe(today)

    const a = byId('tokens-log-row-req-aaaaaaaaaaaaaaaa')!.closest('tr')!
    expect(a.textContent).toContain('2026-10-24 10:30:05')
    expect(a.textContent).toContain('qwen3-4b')
    expect(a.textContent).toContain('生产')
    expect(a.querySelector('[data-testid="tokens-log-endpoint"]')!.textContent).toBe('对话')
    expect(a.querySelector('[data-testid="tokens-log-stream"]')!.textContent).toBe('是')
    expect(a.querySelector('[data-testid="tokens-log-tokens"]')!.textContent).toBe('1,200 / 345')
    expect(a.querySelector('[data-testid="tokens-log-cost"]')!.textContent).toBe('¥0.0125')
    expect(a.querySelector('[data-testid="tokens-log-status"]')!.textContent).toBe('成功')
    expect(a.querySelector('[data-testid="tokens-log-latency"]')!.textContent).toBe('2.35 s / 180 ms')
    expect(a.querySelector('[data-testid="tokens-log-request-id"]')!.textContent).toBe('req-aaaaaaaaaaaaaaaa')

    const b = byId('tokens-log-row-req-bbbbbbbbbbbbbbbb')!.closest('tr')!
    expect(b.querySelector('[data-testid="tokens-log-tokens"]')!.textContent).toBe('≈1,000 / ≈40')
    expect(b.querySelector('[data-testid="tokens-log-cost"]')!.textContent).toBe('< ¥0.01')
    expect(b.querySelector('[data-testid="tokens-log-status"]')!.textContent).toBe('客户端中断')
    expect(b.querySelector('[data-testid="tokens-log-endpoint"]')!.textContent).toBe('向量')
    expect(b.querySelector('[data-testid="tokens-log-stream"]')!.textContent).toBe('否')
    expect(b.querySelector('[data-testid="tokens-log-latency"]')!.textContent).toBe('800 ms / —')
    expect(byId('tokens-logs-total')!.textContent).toBe('共 45 条')

    b.querySelector<HTMLElement>('[data-testid="tokens-log-copy"]')!.click()
    await flushPromises()
    expect(writeText).toHaveBeenCalledWith('req-bbbbbbbbbbbbbbbb')
  })

  it('按模型、Key 筛选，Key 列表来自 /llm/keys；换筛选回到第一页', async () => {
    await mountAt('/tokens?tab=logs')
    expect(get).toHaveBeenCalledWith('/llm/keys')
    byId('tokens-logs-pager')!.querySelector<HTMLElement>('.btn-next')!.click()
    await flushPromises()
    expect(get).toHaveBeenLastCalledWith('/llm/usage', expect.objectContaining({ page: 2 }))

    byId('tokens-logs-model-option-glm-4')!.click()
    await flushPromises()
    expect(get).toHaveBeenLastCalledWith('/llm/usage', expect.objectContaining({ page: 1, model: 'glm-4', key_id: undefined }))
    byId('tokens-logs-key-option-6')!.click()
    await flushPromises()
    expect(get).toHaveBeenLastCalledWith('/llm/usage', expect.objectContaining({ page: 1, model: 'glm-4', key_id: 6 }))
  })

  it('日期范围：超过 90 天或开始晚于结束时提示且不请求；合法后按新范围请求', async () => {
    await mountAt('/tokens?tab=logs')
    const n = calls('/llm/usage').length
    await setDate('tokens-logs-from', addDays(today, -90))
    expect(byId('tokens-logs-range-error')!.textContent).toBe('查询范围最长 90 天')
    await setDate('tokens-logs-from', addDays(today, 1))
    expect(byId('tokens-logs-range-error')!.textContent).toBe('开始日期不能晚于结束日期')
    expect(calls('/llm/usage')).toHaveLength(n)
    await setDate('tokens-logs-from', addDays(today, -89))
    expect(byId('tokens-logs-range-error')).toBeNull()
    expect(get).toHaveBeenLastCalledWith('/llm/usage', expect.objectContaining({ from: addDays(today, -89), to: today, page: 1 }))
  })

  it('手机：卡片列表', async () => {
    vi.stubGlobal('matchMedia', (q: string) => ({ matches: true, media: q, addEventListener: () => {}, removeEventListener: () => {} }))
    await mountAt('/tokens?tab=logs')
    expect(byId('tokens-logs-table')).toBeNull()
    const card = byId('tokens-log-row-req-bbbbbbbbbbbbbbbb')!
    expect(card.querySelector('[data-testid="tokens-log-tokens"]')!.textContent).toBe('≈1,000 / ≈40')
    expect(card.querySelector('[data-testid="tokens-log-status"]')!.textContent).toBe('客户端中断')
    expect(card.textContent).toContain('向量')
  })

  it('空结果', async () => {
    logs = { items: [], total: 0 }
    await mountAt('/tokens?tab=logs')
    expect(byId('tokens-logs-empty')).not.toBeNull()
    expect(byId('tokens-logs-total')).toBeNull()
  })
})
