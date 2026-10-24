import { flushPromises, mount } from '@vue/test-utils'
import type { LedgerRow, User } from '@spark/shared'
import ElementPlus from 'element-plus'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter, RouterView, type Router } from 'vue-router'

const { get } = vi.hoisted(() => ({ get: vi.fn() }))
vi.mock('../../api', () => ({ api: { get, post: vi.fn() }, onUnauthorized: vi.fn() }))

import { session } from '../../store/session'
import AccountView from '../AccountView.vue'
import HelpView from '../HelpView.vue'
import WalletView from '../WalletView.vue'

const user = (over: Partial<User> = {}): User => ({ id: 1, phone: '138****0000', kind: 'personal', kyc_status: 'verified', status: 'active', created_at: '2026-01-01T00:00:00Z', ...over })
const row = (id: number, kind: string, amount: number): LedgerRow => ({ id, amount, balance_after: 10_000, kind, ref_type: '', ref_id: '', remark: '', created_at: '2026-10-24T02:00:00Z' })

let me: User
let ledger: LedgerRow[]
let router: Router
let wrapper: ReturnType<typeof mount> | undefined
async function mountAt(url: string) {
  router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/account', component: AccountView },
      { path: '/wallet', component: WalletView },
      { path: '/help', component: HelpView },
      { path: '/tokens', component: { template: '<div data-testid="tokens-stub" />' } },
      { path: '/:p(.*)*', component: { template: '<div />' } },
    ],
  })
  await router.push(url)
  await router.isReady()
  wrapper = mount(RouterView, { global: { plugins: [router, ElementPlus] }, attachTo: document.body })
  await flushPromises()
}
const byId = (id: string) => document.querySelector<HTMLElement>(`[data-testid="${id}"]`)

beforeEach(() => {
  vi.stubGlobal('matchMedia', (q: string) => ({ matches: false, media: q, addEventListener: () => {}, removeEventListener: () => {} }))
  vi.stubGlobal('IntersectionObserver', class { observe() {} unobserve() {} disconnect() {} })
  get.mockReset()
  me = user({ llm_month_cost: 4567 })
  ledger = [row(50, 'llm_pay', -120), row(49, 'order_pay', -1000), row(48, 'recharge', 100_000)]
  get.mockImplementation(async (path: string, params?: { before_id?: number }) => {
    if (path === '/me') return me
    if (path === '/wallet') return { balance: 10_000 }
    if (path === '/wallet/ledger') return { items: params?.before_id ? ledger.filter((r) => r.id < params.before_id!) : ledger }
    throw new Error(`unexpected ${path}`)
  })
  session.status = 'authed'
  session.user = user()
})
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  document.body.innerHTML = ''
  session.status = 'unknown'
  session.user = null
  vi.unstubAllGlobals()
})

describe('我的：Token 商城入口', () => {
  it('入口链接到 /tokens，进入页面刷新 /me 后显示「本月 ¥X」', async () => {
    await mountAt('/account')
    expect(get).toHaveBeenCalledWith('/me') // 不 quiet：会话过期时与其它需登录页面一样跳登录页
    expect(byId('account-tokens')!.getAttribute('href')).toBe('/tokens')
    expect(byId('account-tokens')!.textContent).toContain('Token 商城')
    expect(byId('account-tokens-month')!.textContent).toBe('本月 ¥45.67')
  })

  it('本月没有消费（字段省略）时不显示小字', async () => {
    me = user()
    await mountAt('/account')
    expect(byId('account-tokens')).not.toBeNull()
    expect(byId('account-tokens-month')).toBeNull()
  })
})

describe('钱包：Token 消费流水', () => {
  it('类型标签：Token 消费与扣款同色', async () => {
    await mountAt('/wallet')
    const kinds = [...document.querySelectorAll('[data-testid="ledger-kind"]')]
    expect(kinds.map((k) => k.textContent)).toEqual(['Token 消费', '扣款', '充值'])
    const tagClass = (i: number) => [...kinds[i].closest('.el-tag')!.classList].find((c) => /^el-tag--(success|warning|info|danger|primary)$/.test(c))
    expect(tagClass(0)).toBe(tagClass(1))
    expect(tagClass(2)).not.toBe(tagClass(0))
    expect(byId('ledger-focus')).toBeNull()
  })

  it('?ledger=<id>：从该条开始加载并高亮，「查看全部流水」恢复', async () => {
    await mountAt('/wallet?ledger=49')
    expect(get).toHaveBeenCalledWith('/wallet/ledger', { before_id: 50, limit: 20 })
    const first = document.querySelector('[data-testid="ledger-kind"]')!
    expect(first.textContent).toBe('扣款')
    expect(first.closest('tr')!.classList.contains('focused')).toBe(true)
    expect(byId('ledger-focus')!.textContent).toContain('已定位到流水 #49')
    expect(byId('ledger-focus-missing')).toBeNull()

    byId('ledger-focus-clear')!.click()
    await flushPromises()
    expect(router.currentRoute.value.query.ledger).toBeUndefined()
    expect(get).toHaveBeenLastCalledWith('/wallet/ledger', { before_id: undefined, limit: 20 })
    expect(document.querySelectorAll('[data-testid="ledger-kind"]')).toHaveLength(3)
    expect(document.querySelector('tr.focused')).toBeNull()
  })

  it('?ledger= 指向不存在的流水：提示未找到', async () => {
    ledger = [row(40, 'recharge', 100)]
    await mountAt('/wallet?ledger=45')
    expect(byId('ledger-focus-missing')!.textContent).toContain('未找到流水 #45')
  })

  it.each(['abc', '0', '123456789012345'])('?ledger=%s 非法值忽略', async (v) => {
    await mountAt(`/wallet?ledger=${v}`)
    expect(get).toHaveBeenCalledWith('/wallet/ledger', { before_id: undefined, limit: 20 })
    expect(byId('ledger-focus')).toBeNull()
  })
})

describe('帮助：Token 商城一节', () => {
  it('计费、结算、限额、Key 安全、欠费、错误码', async () => {
    await mountAt('/help')
    expect(byId('help-tokens')!.textContent).toBe('Token 商城')
    expect(byId('help-tokens-billing')!.textContent).toContain('百万 token')
    expect(byId('help-tokens-settle')!.textContent).toContain('¥1 或最早一笔满 10 分钟')
    expect(byId('help-tokens-limits')!.textContent).toMatch(/60 次.*200K token.*8 个并发/s)
    expect(byId('help-tokens-key')!.textContent).toContain('只在创建时显示一次')
    expect(byId('help-tokens-debt')!.textContent).toContain('402')
    const errors = byId('help-tokens-errors')!.textContent!
    expect(errors).toContain('insufficient_quota')
    expect(errors).toContain('rate_limit_exceeded')
  })
})
