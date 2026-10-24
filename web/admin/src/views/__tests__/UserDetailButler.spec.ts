import type { Admin, UserButlerStats, UserDetail } from '@spark/shared'
import { flushPromises, mount, RouterLinkStub, type VueWrapper } from '@vue/test-utils'
import ElementPlus from 'element-plus'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter } from 'vue-router'

const get = vi.hoisted(() => vi.fn())
vi.mock('../../api', () => ({ api: { get, post: vi.fn() }, onUnauthorized: vi.fn(), onTotpSetupRequired: vi.fn() }))

import { session } from '../../store/session'
import UserDetailView from '../UserDetailView.vue'

const detail = (butler?: UserButlerStats): UserDetail & { butler?: UserButlerStats } => ({
  user: { id: 7, phone: '13800001234', kind: 'personal', kyc_status: 'verified', status: 'active', created_at: '2026-10-01T00:00:00Z' },
  balance: 0, instances: [], orders: [], ledger: [], kyc: [], ssh_keys: [], referral: {}, ...(butler ? { butler } : {}),
})

let w: VueWrapper | null = null
async function render(role: Admin['role'], d: ReturnType<typeof detail>) {
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

describe('用户详情 AI 管家卡', () => {
  it('三个数字 + ops 有「查看对话」链接（切到对话 tab 并预填用户）', async () => {
    await render('ops', detail({ conversations: 3, tokens_30d: 123456, write_calls_30d: 2 }))
    expect(t('admin-user-butler-card').exists()).toBe(true)
    expect(t('admin-user-butler-conversations').text()).toBe('3')
    expect(t('admin-user-butler-tokens').text()).toBe('123,456')
    expect(t('admin-user-butler-writes').text()).toBe('2')
    const link = w!.findAllComponents(RouterLinkStub).find((l) => l.attributes('data-testid') === 'admin-user-butler-link')!
    expect(link.props('to')).toEqual({ path: '/butler', query: { tab: 'conversations', user_id: '7' } })
  })

  it('finance 看得到统计但没有对话链接', async () => {
    await render('finance', detail({ conversations: 1, tokens_30d: 0, write_calls_30d: 0 }))
    expect(t('admin-user-butler-card').exists()).toBe(true)
    expect(t('admin-user-butler-link').exists()).toBe(false)
  })

  it('平台未启用管家（无 butler 字段）不显示卡片', async () => {
    await render('super', detail())
    expect(t('admin-user-butler-card').exists()).toBe(false)
  })
})
