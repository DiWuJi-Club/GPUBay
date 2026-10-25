import { flushPromises, mount } from '@vue/test-utils'
import ElementPlus from 'element-plus'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter, RouterView } from 'vue-router'

const { get } = vi.hoisted(() => ({ get: vi.fn() }))
vi.mock('../../api', () => ({ api: { get }, onUnauthorized: vi.fn(), onTotpSetupRequired: vi.fn() }))

import { session } from '../../store/session'
import AuditView from '../AuditView.vue'

let wrapper: ReturnType<typeof mount> | undefined
async function mountAt(url: string) {
  const router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/audit', component: AuditView }] })
  await router.push(url)
  await router.isReady()
  wrapper = mount(RouterView, { global: { plugins: [router, ElementPlus] }, attachTo: document.body })
  await flushPromises()
}
const auditCalls = () => get.mock.calls.filter((c) => c[0] === '/audit').map((c) => c[1] as Record<string, unknown>)
const input = (id: string) => document.querySelector<HTMLInputElement>(`[data-testid="${id}"] input, input[data-testid="${id}"]`)!

beforeEach(() => {
  session.admin = { id: 1, username: 'a', role: 'ops', status: 'active', totp_confirmed: true, last_login_at: null, last_login_ip: '', created_at: '' }
  get.mockReset()
  get.mockResolvedValue({ items: [], total: 0 })
})
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  session.admin = null
  document.body.innerHTML = ''
})

describe('审计日志：地址栏预填筛选', () => {
  it('?target_type=&target_id= → 预填对象类型 / ID，首次查询就带上', async () => {
    await mountAt('/audit?target_type=butler_tool_call&target_id=tc-abc')
    expect(auditCalls()[0]).toMatchObject({ target_type: 'butler_tool_call', target_id: 'tc-abc', actor_id: '', action: '' })
    expect(input('audit-target-type').value).toBe('butler_tool_call')
    expect(input('audit-target-id').value).toBe('tc-abc')
  })

  it('没有参数 / 参数是数组 → 不预填', async () => {
    await mountAt('/audit?target_id=a&target_id=b')
    expect(auditCalls()[0]).toMatchObject({ target_type: '', target_id: '' })
  })
})
