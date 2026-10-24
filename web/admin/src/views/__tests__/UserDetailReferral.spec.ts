import type { Admin, UserDetail } from '@spark/shared'
import { flushPromises, mount, RouterLinkStub, type VueWrapper } from '@vue/test-utils'
import ElementPlus from 'element-plus'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter } from 'vue-router'

const get = vi.hoisted(() => vi.fn())
vi.mock('../../api', () => ({ api: { get, post: vi.fn() }, onUnauthorized: vi.fn(), onTotpSetupRequired: vi.fn() }))

import { session } from '../../store/session'
import UserDetailView from '../UserDetailView.vue'

const detail = (referral: UserDetail['referral']): UserDetail => ({
  user: { id: 7, phone: '13800001234', kind: 'personal', kyc_status: 'verified', status: 'active', created_at: '2026-10-01T00:00:00Z' },
  balance: 0, instances: [], orders: [], ledger: [], kyc: [], ssh_keys: [], referral,
})

let w: VueWrapper | null = null
async function render(role: Admin['role'], d: UserDetail) {
  session.admin = { id: 1, username: 'a', role, status: 'active', totp_confirmed: true, last_login_at: null, last_login_ip: '', created_at: '' }
  get.mockResolvedValue(d)
  const router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/users/:id', component: UserDetailView }] })
  await router.push('/users/7')
  w = mount(UserDetailView, { global: { plugins: [ElementPlus, router], stubs: { RouterLink: RouterLinkStub } } })
  await flushPromises()
  return w
}
const t = (id: string) => w!.find(`[data-testid="${id}"]`)
afterEach(() => {
  w?.unmount()
  w = null
})

describe('用户详情邀请区块', () => {
  it('两者都没有：无邀请关系', async () => {
    await render('ops', detail({}))
    expect(t('user-referral-none').text()).toBe('无邀请关系')
  })

  it('被邀请人：邀请人 ID + 脱敏手机号 + 绑定状态', async () => {
    await render('ops', detail({ inviter: { user_id: 3, phone: '13900005678', binding_status: 'held' } }))
    const line = t('user-referral-inviter').text()
    expect(line).toContain('3')
    expect(line).toContain('139****5678')
    expect(line).not.toContain('13900005678')
    expect(line).toContain('审核中')
    expect(t('user-referral-none').exists()).toBe(false)
  })

  it('邀请人：统计；finance 有跳转链接，ops 没有', async () => {
    const d = detail({ account: { code: 'ABC234', status: 'frozen', invitees: 2, settled_net: 690, pending: 100, debt: 5 } })
    await render('finance', d)
    expect(t('user-referral-code').text()).toBe('ABC234')
    expect(t('user-referral-status').text()).toBe('已冻结')
    expect(t('user-referral-invitees').text()).toBe('2')
    expect(t('user-referral-net').text()).toBe('¥6.90')
    expect(t('user-referral-pending').text()).toBe('¥1.00')
    expect(t('user-referral-debt').text()).toBe('¥0.05')
    expect(w!.findAllComponents(RouterLinkStub).find((l) => l.attributes('data-testid') === 'user-referral-link')!.props('to')).toEqual({
      path: '/referral',
      query: { tab: 'inviters', q: '7' },
    })
    w!.unmount()
    await render('ops', d)
    expect(t('user-referral-code').exists()).toBe(true)
    expect(t('user-referral-link').exists()).toBe(false)
  })
})
