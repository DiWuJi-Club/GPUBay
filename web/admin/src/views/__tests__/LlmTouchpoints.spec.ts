import type { Admin, Role } from '@spark/shared'
import { flushPromises, mount } from '@vue/test-utils'
import ElementPlus from 'element-plus'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter, RouterView } from 'vue-router'

const { get } = vi.hoisted(() => ({ get: vi.fn() }))
vi.mock('../../api', () => ({ api: { get, post: vi.fn(), put: vi.fn(), del: vi.fn(), download: vi.fn() }, onUnauthorized: vi.fn(), onTotpSetupRequired: vi.fn() }))

import { session } from '../../store/session'
import AppsView from '../AppsView.vue'
import DashboardView from '../DashboardView.vue'
import LedgerView from '../LedgerView.vue'
import LlmView from '../LlmView.vue'
import UserDetailView from '../UserDetailView.vue'
import UsersView from '../UsersView.vue'

// 计划 9 Task 22：用户详情的 Token 商城区块、用户列表系统账号开关、仪表盘卡片、流水类型、应用「内部」标签、/llm 账户 / 用量 Tab 接入
const userDetail = (llm?: object) => ({
  user: { id: 42, phone: '13800001234', kind: 'personal', kyc_status: 'verified', status: 'active', created_at: '' },
  balance: 1000, instances: [], orders: [], ledger: [], kyc: [], ssh_keys: [], ...(llm ? { llm } : {}),
})
const dashboard = (llm?: object) => ({
  machines: { by_state: {}, online: 0, total: 0 }, instances: {}, recharge_today: 0, recharge_month: 0, order_today: 0, order_month: 0, utilization: [],
  ssh_sessions_open: 0, bookings_upcoming_7d: 0, apps_installing: 0, apps_running: 0, apps_failed_24h: 0, ...(llm ? { llm } : {}),
})

let wrapper: ReturnType<typeof mount> | undefined
let detailLLM: object | undefined
let dashLLM: object | undefined
function login(role: Role) {
  session.status = 'authed'
  session.admin = { id: 1, username: 'a', role, status: 'active', totp_confirmed: true, last_login_at: null, last_login_ip: '', created_at: '' } as Admin
}
async function mountAt(url: string) {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', component: DashboardView },
      { path: '/users', component: UsersView },
      { path: '/users/:id', component: UserDetailView },
      { path: '/ledger', component: LedgerView },
      { path: '/apps', component: AppsView },
      { path: '/llm', component: LlmView },
      { path: '/:p(.*)*', component: { template: '<div />' } },
    ],
  })
  await router.push(url)
  await router.isReady()
  wrapper = mount(RouterView, { global: { plugins: [router, ElementPlus] }, attachTo: document.body })
  await flushPromises()
}
const byId = (id: string) => wrapper!.find(`[data-testid="${id}"]`)
const calls = (path: string) => get.mock.calls.filter((c) => c[0] === path)

beforeEach(() => {
  login('super')
  detailLLM = { keys: 3, month_cost: 1234, unsettled: 5, debt: 200, blocked: true }
  dashLLM = { nodes_ready: 1, nodes_total: 2, today_requests: 12345, today_settled_amount: 990 }
  get.mockReset()
  get.mockImplementation(async (path: string) => {
    if (path === '/users/42') return userDetail(detailLLM)
    if (path === '/users') return { items: [], total: 0 }
    if (path === '/dashboard') return dashboard(dashLLM)
    if (path === '/ledger') return { items: [], total: 0 }
    if (path === '/apps') return { items: [{ id: 1, slug: 'vllm-node', name: 'vLLM 节点', category: 'llm', status: 'published', versions: 1, deployments_live: 2, sort: 0, internal: true }, { id: 2, slug: 'comfy', name: 'ComfyUI', category: 'image', status: 'published', versions: 1, deployments_live: 0, sort: 1 }] }
    if (path === '/llm/accounts') return { items: [], total: 0 }
    if (path === '/llm/models') return { items: [] }
    if (path === '/llm/usage') return { items: [], total: 0 }
    throw new Error(`unexpected GET ${path}`)
  })
})
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  document.body.innerHTML = ''
  vi.restoreAllMocks()
})

describe('用户详情「Token 商城」区块', () => {
  it('显示 Key 数、本月消费、未结算、欠费、停用状态，链接到账户 Tab', async () => {
    await mountAt('/users/42')
    expect(byId('udetail-llm-keys').text()).toBe('3')
    expect(byId('udetail-llm-month').text()).toBe('¥12.34')
    expect(byId('udetail-llm-unsettled').text()).toBe('¥0.05')
    expect(byId('udetail-llm-debt').text()).toBe('¥2.00')
    expect(byId('udetail-llm-blocked').text()).toBe('已停用')
    expect(byId('udetail-llm-link').attributes('href')).toBe('/llm?tab=accounts&user_id=42')
  })
  it('后端没有 llm 字段时不显示', async () => {
    detailLLM = undefined
    await mountAt('/users/42')
    expect(byId('udetail-phone').exists()).toBe(true)
    expect(byId('udetail-llm').exists()).toBe(false)
  })
})

describe('用户列表 include_system', () => {
  it('默认不带；打开开关后带 include_system=true', async () => {
    await mountAt('/users')
    expect(calls('/users')[0][1]).toMatchObject({ include_system: undefined })
    await byId('users-include-system').trigger('click')
    await flushPromises()
    expect(calls('/users').at(-1)![1]).toMatchObject({ include_system: 'true', page: 1 })
  })
})

describe('仪表盘 Token 商城卡片', () => {
  it('有 llm 时显示节点、今日请求、今日收入', async () => {
    await mountAt('/')
    expect(byId('dash-llm-nodes-ready').text()).toBe('1')
    expect(byId('dash-llm-nodes-total').text()).toBe('2')
    expect(byId('dash-llm-requests').text()).toBe('12,345')
    expect(byId('dash-llm-income').text()).toBe('¥9.90')
  })
  it('没有 llm 时不显示', async () => {
    dashLLM = undefined
    await mountAt('/')
    expect(byId('dash-machines-total').exists()).toBe(true)
    expect(byId('dash-llm').exists()).toBe(false)
  })
})

describe('流水类型与应用标签', () => {
  it('流水类型下拉含「Token 消费」（llm_pay）', async () => {
    await mountAt('/ledger')
    await byId('fin-filter-kind').trigger('click')
    await flushPromises()
    const opts = [...document.body.querySelectorAll('.el-select-dropdown__item')].map((o) => o.textContent?.trim())
    expect(opts).toContain('Token 消费')
  })
  it('内部应用显示「内部」标签', async () => {
    await mountAt('/apps')
    const tags = wrapper!.findAll('.el-table__body [data-testid="apps-row-internal"]')
    expect(tags).toHaveLength(1)
    expect(tags[0].text()).toBe('内部')
  })
})

describe('/llm 账户 / 用量 Tab', () => {
  it('?tab=accounts&user_id= 打开账户 Tab 并预填；用量 Tab 接入', async () => {
    await mountAt('/llm?tab=accounts&user_id=42')
    expect(byId('llm-accounts').exists()).toBe(true)
    expect(calls('/llm/accounts')[0][1]).toMatchObject({ q: '42' })
    wrapper!.unmount()
    await mountAt('/llm?tab=usage')
    expect(byId('llm-usage').exists()).toBe(true)
    expect(calls('/llm/usage')).toHaveLength(1)
  })
})
