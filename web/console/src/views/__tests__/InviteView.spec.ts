import type { ReferralMe } from '@spark/shared'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import ElementPlus from 'element-plus'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter } from 'vue-router'

const mocks = vi.hoisted(() => ({ get: vi.fn(), toDataURL: vi.fn(async () => 'data:image/png;base64,QR') }))
vi.mock('../../api', () => ({ api: { get: mocks.get }, onUnauthorized: vi.fn() }))
vi.mock('qrcode', () => ({ default: { toDataURL: mocks.toDataURL } }))

import InviteView from '../InviteView.vue'

const meOf = (over: Partial<ReferralMe> = {}): ReferralMe => ({
  code: 'ABCDEFGH',
  link: 'http://spark.test/?ic=ABCDEFGH',
  status: 'active',
  kyc_verified: true,
  rebate: { rate_bp: 1000, period_days: 365, hold_days: 7, max_per_invitee: 50000, max_per_inviter_30d: 500000 },
  stats: { invitees: 2, verified_invitees: 1, pending: 690, settled_net: 1380, month_settled: 690, debt: 0 },
  ...over,
})

let me: ReferralMe
let wrapper: VueWrapper | null = null
async function render(url = '/invite') {
  const router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/invite', component: InviteView }, { path: '/kyc', component: { template: '<div />' } }] })
  await router.push(url)
  wrapper = mount(InviteView, { attachTo: document.body, global: { plugins: [ElementPlus, router] } })
  await flushPromises()
}
const $ = (id: string) => document.querySelector<HTMLElement>(`[data-testid="${id}"]`)

beforeEach(() => {
  me = meOf()
  mocks.toDataURL.mockClear()
  mocks.get.mockReset()
  mocks.get.mockImplementation((path: string) => {
    if (path === '/referral/me') return Promise.resolve(me)
    if (path === '/referral/invitees')
      return Promise.resolve({ total: 1, items: [{ phone_masked: '139****5678', bound_at: '2026-10-20T00:00:00Z', status: 'active', kyc_verified: false, contributed: 690, rebate_until: '2027-10-20T00:00:00Z' }] })
    return Promise.resolve({ items: [], total: 0 })
  })
})
afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  document.body.innerHTML = ''
})

describe('InviteView', () => {
  it('正常：显示码、链接、二维码、规则与统计', async () => {
    await render()
    expect($('invite-code')!.textContent).toBe('ABCDEFGH')
    expect($('invite-link')!.textContent).toBe('http://spark.test/?ic=ABCDEFGH')
    expect($('invite-qr')!.getAttribute('src')).toBe('data:image/png;base64,QR')
    expect(mocks.toDataURL).toHaveBeenCalledWith('http://spark.test/?ic=ABCDEFGH', expect.anything())
    const rules = $('invite-rules')!.textContent!
    for (const s of ['12 个月内', '10%', '7 天后到账', '不可提现', '¥500.00', '¥5,000.00']) expect(rules).toContain(s)
    expect($('invite-frozen-alert')).toBeNull()
    expect($('invite-kyc-alert')).toBeNull()
    expect($('invite-stat-settled')!.textContent).toBe('¥13.80')
    expect($('invite-stat-month')!.textContent).toBe('¥6.90')
    expect($('invite-stat-debt')).toBeNull()
    expect($('invitee-phone')!.textContent).toBe('139****5678')
    expect($('invitee-status')!.textContent).toBe('有效')
    expect($('invitee-kyc')!.textContent).toBe('未实名')
  })

  it('冻结：显示暂停提示，隐藏码 / 链接 / 二维码（接口仍返回码），不生成二维码', async () => {
    me = meOf({ status: 'frozen', kyc_verified: false, stats: { ...meOf().stats, debt: 300 } })
    await render()
    expect($('invite-frozen-alert')!.textContent).toContain('邀请功能已暂停')
    expect($('invite-code')).toBeNull()
    expect($('invite-link')).toBeNull()
    expect($('invite-qr')).toBeNull()
    expect($('invite-rules')).toBeNull()
    expect(document.body.textContent).not.toContain('ABCDEFGH')
    expect(mocks.toDataURL).not.toHaveBeenCalled()
    expect($('invite-kyc-alert')).toBeNull() // 冻结提示优先
    expect($('invite-stat-debt')!.textContent).toContain('¥3.00')
  })

  it('未实名：提示条带去实名链接，码仍可见', async () => {
    me = meOf({ kyc_verified: false })
    await render()
    expect($('invite-kyc-alert')!.textContent).toContain('实名认证后才能收到返利')
    expect($('invite-kyc-alert')!.querySelector('a')!.getAttribute('href')).toBe('/kyc')
    expect($('invite-code')!.textContent).toBe('ABCDEFGH')
  })

  it('?tab=rewards 直接加载返利明细，已发放且有扣回时显示已扣回', async () => {
    mocks.get.mockImplementation((path: string) => {
      if (path === '/referral/me') return Promise.resolve(me)
      if (path === '/referral/rewards')
        return Promise.resolve({ total: 1, items: [{ order_id: 'ord-1', invitee_masked: '139****5678', paid_at: '2026-10-20T00:00:00Z', order_amount: 6900, refunded_amount: 3000, reward: 390, status: 'settled', settled_at: '2026-10-27T00:00:00Z', clawed_amount: 300 }] })
      return Promise.resolve({ items: [], total: 0 })
    })
    await render('/invite?tab=rewards')
    expect(mocks.get).toHaveBeenCalledWith('/referral/rewards', { page: 1, page_size: 20 })
    expect($('reward-amount')!.textContent).toBe('¥3.90')
    expect($('reward-status')!.textContent).toBe('已发放')
    expect($('reward-clawed')!.textContent).toContain('已扣回 ¥3.00')
    expect($('reward-settle-after')).toBeNull() // 已发放不再显示预计到账
  })

  it('待发放行显示预计到账时间（settle_after，北京时间）', async () => {
    mocks.get.mockImplementation((path: string) => {
      if (path === '/referral/me') return Promise.resolve(me)
      if (path === '/referral/rewards')
        return Promise.resolve({ total: 1, items: [{ order_id: 'ord-2', invitee_masked: '139****5678', paid_at: '2026-10-20T00:00:00Z', order_amount: 6900, refunded_amount: 0, reward: 690, status: 'pending', settle_after: '2026-10-27T00:00:00Z', settled_at: null, clawed_amount: 0 }] })
      return Promise.resolve({ items: [], total: 0 })
    })
    await render('/invite?tab=rewards')
    expect($('reward-status')!.textContent).toBe('待发放')
    expect($('reward-settle-after')!.textContent).toBe('预计到账 2026-10-27 08:00')
    expect($('reward-refunded-void')).toBeNull()
  })

  it('全额退款的待发放行（reward=0）显示「已退款，将作废」，不显示待发放 / 预计到账', async () => {
    mocks.get.mockImplementation((path: string) => {
      if (path === '/referral/me') return Promise.resolve(me)
      if (path === '/referral/rewards')
        return Promise.resolve({ total: 1, items: [{ order_id: 'ord-3', invitee_masked: '139****5678', paid_at: '2026-10-20T00:00:00Z', order_amount: 6900, refunded_amount: 6900, reward: 0, status: 'pending', settle_after: '2026-10-27T00:00:00Z', settled_at: null, clawed_amount: 0 }] })
      return Promise.resolve({ items: [], total: 0 })
    })
    await render('/invite?tab=rewards')
    expect($('reward-refunded-void')!.textContent).toBe('已退款，将作废')
    expect($('reward-status')).toBeNull()
    expect($('reward-settle-after')).toBeNull()
  })
})
