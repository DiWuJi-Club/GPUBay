import type { Dashboard, DashboardButler } from '@spark/shared'
import { flushPromises, mount, RouterLinkStub, type VueWrapper } from '@vue/test-utils'
import ElementPlus from 'element-plus'
import { afterEach, describe, expect, it, vi } from 'vitest'

const get = vi.hoisted(() => vi.fn())
vi.mock('../../api', () => ({ api: { get, post: vi.fn() }, onUnauthorized: vi.fn(), onTotpSetupRequired: vi.fn() }))

import { session } from '../../store/session'
import DashboardView from '../DashboardView.vue'

const base: Dashboard = {
  machines: { by_state: {}, online: 0, total: 0 }, instances: {},
  recharge_today: 0, recharge_month: 0, order_today: 0, order_month: 0, utilization: [], ssh_sessions_open: 0, bookings_upcoming_7d: 0,
  referral_month_settled: 0, apps_installing: 0, apps_running: 0, apps_failed_24h: 0, workflow_installs: 0, workflow_runs_24h: 0,
}

let w: VueWrapper | null = null
async function render(d: Dashboard & DashboardButler) {
  session.admin = { id: 1, username: 'a', role: 'finance', status: 'active', totp_confirmed: true, last_login_at: null, last_login_ip: '', created_at: '' }
  get.mockResolvedValue(d)
  w = mount(DashboardView, { global: { plugins: [ElementPlus], stubs: { RouterLink: RouterLinkStub } } })
  await flushPromises()
  return w
}
const t = (id: string) => w!.find(`[data-testid="${id}"]`)
afterEach(() => {
  w?.unmount()
  w = null
  session.admin = null
  get.mockReset()
})

describe('仪表盘 AI 管家数字', () => {
  it('显示今日请求 / token / 失败', async () => {
    await render({ ...base, butler_requests_today: 12, butler_tokens_today: 34567, butler_failures_today: 2 })
    expect(t('dash-butler-requests').text()).toBe('12')
    expect(t('dash-butler-tokens').text()).toBe('34,567')
    expect(t('dash-butler-failures').text()).toBe('2')
    expect(t('dash-butler-failures').classes()).toContain('bad')
  })

  it('平台未启用管家（无字段）不显示', async () => {
    await render(base)
    expect(t('dash-butler-requests').exists()).toBe(false)
  })
})
