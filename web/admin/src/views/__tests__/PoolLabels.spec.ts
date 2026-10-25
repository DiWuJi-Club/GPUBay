import type { Admin, AdminInstance, Role } from '@spark/shared'
import { flushPromises, mount } from '@vue/test-utils'
import ElementPlus from 'element-plus'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter, RouterView } from 'vue-router'

const { get } = vi.hoisted(() => ({ get: vi.fn() }))
vi.mock('../../api', () => ({ api: { get, post: vi.fn(), put: vi.fn(), del: vi.fn() }, onUnauthorized: vi.fn(), onTotpSetupRequired: vi.fn() }))

import { session } from '../../store/session'
import GroupsView from '../GroupsView.vue'
import InstanceDetailView from '../InstanceDetailView.vue'
import MachineDetailView from '../MachineDetailView.vue'

// 计划 9 Task 21：池机组 / 平台节点在机器详情、机组列表、实例详情上的标签与限制
const groups = [
  { id: 9, name: 'Token 服务池', region: '', description: '', sort: 0, visible: false, purpose: 'llm', machine_count: 1, idle_now: 0, reserved_upcoming: 0 },
  { id: 2, name: '华东一区', region: '', description: '', sort: 1, visible: true, purpose: 'rental', machine_count: 3, idle_now: 1, reserved_upcoming: 0 },
]
const machine = (group_id: number | null) => ({
  id: 11, serial: 'SN-11', rack: 'A', slot: '1', lan_ip: '10.0.0.11', product_id: 1, state: 'allocated', agent_online: true, last_seen_at: null,
  pdu_id: null, pdu_outlet: null, created_at: '', enrolled: true, fingerprint_prefix: 'ab', connected: true, agent_version: '1', agent_connected_at: null,
  state_reported_at: null, last_metrics: null, group_id, display_name: 'A01-01', cached_images: [], cached_images_at: null,
})
const sysInstance = (): AdminInstance => ({
  id: 'i-sys1', user_id: 1, machine_id: 11, product_id: 1, image_id: 5, state: 'running', suspended: false, pending_seconds: 0, remaining_seconds: 0,
  expires_at: null, stopped_at: null, password_auth: false, created_at: '', scheduled_start_at: null, scheduled_end_at: null, group_id: 9, batch_id: null,
  reassign_count: 0, auto_renew: false, auto_renew_duration: '1d' as AdminInstance['auto_renew_duration'], auto_renew_failed_reason: '', provisioned_at: null, cancelled_at: null, system: true,
})

let wrapper: ReturnType<typeof mount> | undefined
let machineGroup: number | null
function login(role: Role) {
  session.status = 'authed'
  session.admin = { id: 1, username: 'a', role, status: 'active', totp_confirmed: true, last_login_at: null, last_login_ip: '', created_at: '' } as Admin
}
async function mountAt(url: string) {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/machines/:id', component: MachineDetailView },
      { path: '/instances/:id', component: InstanceDetailView },
      { path: '/groups', component: GroupsView },
      { path: '/:p(.*)*', component: { template: '<div />' } },
    ],
  })
  await router.push(url)
  await router.isReady()
  wrapper = mount(RouterView, { global: { plugins: [router, ElementPlus] }, attachTo: document.body })
  await flushPromises()
}
const byId = (id: string) => wrapper!.find(`[data-testid="${id}"]`)
const inBody = (id: string) => document.body.querySelector<HTMLElement>(`[data-testid="${id}"]`)

beforeEach(() => {
  login('ops')
  machineGroup = 9
  get.mockReset()
  get.mockImplementation(async (path: string) => {
    if (path === '/groups') return { items: groups }
    if (path === '/machines/11') return { machine: machine(machineGroup), current_instance: null, history: [], group: machineGroup ? { id: machineGroup, name: 'x', region: '', visible: false } : null, leases: [], upcoming_bookings: 0 }
    if (path === '/llm/pool') return { group_id: 9, free: 0, total: 1, machines: [{ machine: { id: 11, serial: 'SN-11', display_name: 'A01-01', state: 'allocated', agent_online: true }, node: { id: 'n7', model_slug: 'qwen3-32b', state: 'ready' } }] }
    if (path === '/instances/i-sys1') {
      return {
        instance: sysInstance(), events: [], orders: [], commands: [], ports: [], ssh_host_pubkey_fingerprint: '',
        apps: [{ id: 'd1', app_name: 'vllm-node', version: '1', state: 'running', error_code: '', message: '', phase: '', progress: 100, port: 8000, updated_at: '', update_available: false }],
      }
    }
    if (path === '/images' || path === '/products') return { items: [] }
    if (path.endsWith('/ssh-sessions')) return { items: [], total: 0 }
    throw new Error(`unexpected GET ${path}`)
  })
})
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  document.body.innerHTML = ''
})

describe('机器详情：Token 服务池', () => {
  it('池内机器：标签 + 链接到本机节点；编辑时机组不可改并提示', async () => {
    await mountAt('/machines/11')
    expect(byId('mdetail-pool-tag').text()).toBe('Token 服务池')
    expect(get).toHaveBeenCalledWith('/llm/pool')
    expect(byId('mdetail-pool-link').attributes('href')).toBe('/llm?tab=nodes&node=n7')
    await byId('mdetail-edit-btn').trigger('click')
    await flushPromises()
    expect(inBody('medit-pool-hint')!.textContent).toBe('Token 服务池机器请在 Token 商城 → 节点与机器池 中移出')
    expect(document.body.querySelector('[data-testid="medit-dialog"] .el-select.is-disabled, [data-testid="medit-dialog"] .el-select__wrapper.is-disabled')).not.toBeNull()
  })
  it('出租机组的机器：没有池标签，不查池', async () => {
    machineGroup = 2
    await mountAt('/machines/11')
    expect(byId('mdetail-pool-tag').exists()).toBe(false)
    expect(get).not.toHaveBeenCalledWith('/llm/pool')
  })
})

describe('机组列表：Token 服务池', () => {
  it('池机组显示标签与节点页链接、不能删除、编辑时可见开关禁用', async () => {
    await mountAt('/groups')
    const rows = wrapper!.findAll('.el-table__body .el-table__row')
    expect(rows).toHaveLength(2)
    expect(rows[0].find('[data-testid="group-pool-tag"]').exists()).toBe(true)
    expect(rows[0].find('[data-testid="group-pool-link"]').attributes('href')).toBe('/llm?tab=nodes')
    expect(rows[0].find('[data-testid="group-delete"]').exists()).toBe(false)
    expect(rows[1].find('[data-testid="group-pool-tag"]').exists()).toBe(false)
    expect(rows[1].find('[data-testid="group-delete"]').exists()).toBe(true)
    await rows[0].find('[data-testid="group-edit"]').trigger('click')
    await flushPromises()
    expect(inBody('group-form-visible')!.classList.contains('is-disabled')).toBe(true)
    expect(inBody('group-form-pool-hint')).not.toBeNull()
  })
})

describe('实例详情：平台节点', () => {
  it('标签、「不计时」、不显示退款 / 延长 / 换机 / 强制卸载', async () => {
    await mountAt('/instances/i-sys1')
    expect(byId('adetail-system').text()).toBe('平台节点')
    expect(byId('adetail-remaining').text()).toBe('不计时')
    expect(byId('adetail-expires').text()).toBe('不计时')
    for (const id of ['adetail-extend-btn', 'adetail-migrate-btn', 'adetail-refund-btn', 'admin-instance-app-force-remove']) expect(byId(id).exists(), id).toBe(false)
    expect(byId('admin-instance-apps-system-hint').exists()).toBe(true)
  })
})
