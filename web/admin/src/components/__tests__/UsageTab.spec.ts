import type { Admin, LLMDailyAdminRow, LLMSettlementAdmin, LLMUsageAdminRow, Role } from '@spark/shared'
import { flushPromises, mount } from '@vue/test-utils'
import ElementPlus from 'element-plus'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter } from 'vue-router'

const { get, download } = vi.hoisted(() => ({ get: vi.fn(), download: vi.fn() }))
vi.mock('../../api', () => ({ api: { get, post: vi.fn(), put: vi.fn(), del: vi.fn(), download }, onUnauthorized: vi.fn(), onTotpSetupRequired: vi.fn() }))
const { downloadBlob } = vi.hoisted(() => ({ downloadBlob: vi.fn() }))
vi.mock('../../logic/csv', async (orig) => ({ ...(await orig<object>()), downloadBlob }))

import { defaultUsageRange } from '../../logic/llm'
import { session } from '../../store/session'
import UsageTab from '../llm/UsageTab.vue'

const usage = (over: Partial<LLMUsageAdminRow> = {}): LLMUsageAdminRow => ({
  request_id: 'req-abc', created_at: '2026-10-24T02:00:00Z', user_id: 42, user_phone: '13800001234', model: 'qwen3-32b', key_name: '生产', key_hint: 'sk-…abcd',
  endpoint: 'chat', stream: true, prompt_tokens: 1200, completion_tokens: 300, estimated: true, input_price: 200, output_price: 800, cost_micro: 4_800_000,
  status: 'ok', http_status: 200, latency_ms: 1500, ttft_ms: 120, node_id: 'n1', client_ip: '1.2.3.4', ...over,
})
const daily: LLMDailyAdminRow[] = [
  { day: '2026-10-23', model: 'a', requests: 1, prompt_tokens: 10, completion_tokens: 5, cost_micro: 1_000_000 },
  { day: '2026-10-24', model: 'a', requests: 2, prompt_tokens: 20, completion_tokens: 6, cost_micro: 2_000_000 },
  { day: '2026-10-24', model: 'b', requests: 3, prompt_tokens: 30, completion_tokens: 7, cost_micro: 3_000_000 },
]
const dailyUsers: LLMDailyAdminRow[] = [{ day: '2026-10-24', user_id: 42, phone: '13800001234', requests: 6, prompt_tokens: 60, completion_tokens: 18, cost_micro: 6_000_000 }]
const settlement = (): LLMSettlementAdmin => ({
  id: 9, user_id: 42, created_at: '2026-10-24T02:00:00Z', requests: 6, prompt_tokens: 60, completion_tokens: 18, cost_micro: 6_000_000, usage_amount: 6, debt_amount: 0, charged: 6,
  wallet_ledger_id: 77, period_from: '2026-10-24T01:50:00Z', period_to: '2026-10-24T02:00:00Z',
})

let wrapper: ReturnType<typeof mount> | undefined
function login(role: Role) {
  session.status = 'authed'
  session.admin = { id: 1, username: 'a', role, status: 'active', totp_confirmed: true, last_login_at: null, last_login_ip: '', created_at: '' } as Admin
}
async function mountTab(url = '/llm?tab=usage') {
  const router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/:p(.*)*', component: { template: '<div />' } }] })
  await router.push(url)
  wrapper = mount(UsageTab, { global: { plugins: [router, ElementPlus] }, attachTo: document.body })
  await flushPromises()
}
const byId = (id: string) => wrapper!.find(`[data-testid="${id}"]`)
const allById = (id: string) => wrapper!.findAll(`.el-table__body [data-testid="${id}"]`)
const calls = (path: string) => get.mock.calls.filter((c) => c[0] === path)
async function setInput(id: string, v: string) {
  const host = byId(id)
  const input = host.element.tagName === 'INPUT' ? host : host.find('input')
  await input.setValue(v)
}
async function pick(view: string) {
  await byId(view).find('input').setValue(true)
  await flushPromises()
}

beforeEach(() => {
  login('finance')
  get.mockReset()
  download.mockReset()
  downloadBlob.mockReset()
  get.mockImplementation(async (path: string, q?: Record<string, unknown>) => {
    if (path === '/llm/models') return { items: [{ id: 3, slug: 'qwen3-32b' }] }
    if (path === '/llm/usage') return { items: [usage(), usage({ request_id: 'req-2', status: 'upstream_error', estimated: false, cost_micro: 0, ttft_ms: null })], total: 2 }
    if (path === '/llm/usage/daily') return { items: q?.group_by === 'user' ? dailyUsers : daily }
    if (path === '/llm/settlements') return { items: [settlement()], total: 1 }
    throw new Error(`unexpected GET ${path}`)
  })
  download.mockResolvedValue({ blob: new Blob(['x']), filename: 'llm-usage.csv' })
})
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  document.body.innerHTML = ''
  vi.restoreAllMocks()
})

describe('UsageTab 明细', () => {
  it('默认最近 7 天；表格显示费用（微分 → 元）、token、估算、状态', async () => {
    await mountTab()
    const r = defaultUsageRange()
    expect(calls('/llm/usage')[0][1]).toEqual({ user_id: undefined, model_id: undefined, key_id: undefined, status: undefined, from: r.from, to: r.to, page: 1, page_size: 20 })
    expect(allById('llm-usage-rid').map((x) => x.text())).toEqual(['req-abc', 'req-2'])
    expect(allById('llm-usage-cost')[0].text()).toBe('¥0.048')
    expect(allById('llm-usage-cost')[1].text()).toBe('¥0.00')
    expect(allById('llm-usage-tokens')[0].text()).toContain('1,200 / 300')
    expect(allById('llm-usage-row-status')[1].text()).toBe('上游错误')
    expect(allById('llm-usage-row-user')[0].attributes('href')).toBe('/users/42')
  })

  it('筛选：点查询才生效；导出用同一筛选', async () => {
    await mountTab('/llm?tab=usage&user_id=42')
    expect(calls('/llm/usage')[0][1]).toMatchObject({ user_id: '42' })
    await setInput('llm-usage-key', '9x')
    await setInput('llm-usage-from', '2026-10-01')
    await setInput('llm-usage-to', '2026-10-10')
    expect(calls('/llm/usage')).toHaveLength(1)
    await wrapper!.find('form').trigger('submit')
    await flushPromises()
    expect(calls('/llm/usage').at(-1)![1]).toMatchObject({ user_id: '42', key_id: '9', from: '2026-10-01', to: '2026-10-10', page: 1 })
    await byId('llm-usage-export').trigger('click')
    await flushPromises()
    expect(download).toHaveBeenCalledWith('/llm/usage/export', { user_id: '42', model_id: undefined, key_id: '9', status: undefined, from: '2026-10-01', to: '2026-10-10' })
    expect(downloadBlob).toHaveBeenCalledWith('llm-usage.csv', expect.any(Blob))
  })

  it('日期范围超过 90 天：提示且不发请求', async () => {
    await mountTab()
    await setInput('llm-usage-from', '2026-01-01')
    await setInput('llm-usage-to', '2026-10-10')
    await wrapper!.find('form').trigger('submit')
    await flushPromises()
    expect(byId('llm-usage-range-error').text()).toBe('查询范围最多 90 天')
    expect(calls('/llm/usage')).toHaveLength(1)
  })
})

describe('UsageTab 汇总与结算', () => {
  it('汇总：按天合计、按模型（同一份数据不重复请求）、按用户（group_by=user）', async () => {
    await mountTab()
    await pick('llm-usage-view-daily')
    expect(calls('/llm/usage/daily')).toHaveLength(1)
    expect(calls('/llm/usage/daily')[0][1]).toMatchObject({ group_by: 'model' })
    expect(allById('llm-daily-key').map((x) => x.text())).toEqual(['2026-10-24', '2026-10-23'])
    expect(allById('llm-daily-requests').map((x) => x.text())).toEqual(['5', '1'])
    expect(allById('llm-daily-cost')[0].text()).toBe('¥0.05')
    expect(byId('llm-daily-total').text()).toContain('6 次请求')

    await pick('llm-usage-group-model')
    expect(calls('/llm/usage/daily')).toHaveLength(1)
    expect(allById('llm-daily-key').map((x) => x.text())).toEqual(['a', 'b']) // 费用相同按 slug

    await pick('llm-usage-group-user')
    expect(calls('/llm/usage/daily')).toHaveLength(2)
    expect(calls('/llm/usage/daily')[1][1]).toMatchObject({ group_by: 'user' })
    expect(allById('llm-daily-key')[0].text()).toBe('13800001234')
  })

  it('结算记录：按用户与日期筛选', async () => {
    await mountTab('/llm?tab=usage&user_id=42')
    await pick('llm-usage-view-settlements')
    const r = defaultUsageRange()
    expect(calls('/llm/settlements')[0][1]).toEqual({ user_id: '42', from: r.from, to: r.to, page: 1, page_size: 20 })
    expect(allById('llm-settle-id')[0].text()).toBe('9')
    expect(allById('llm-settle-charged')[0].text()).toBe('¥0.06')
    expect(byId('llm-usage-export').exists()).toBe(false) // 导出只针对明细
  })
})
