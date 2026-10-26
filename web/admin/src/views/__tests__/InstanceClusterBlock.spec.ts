import type { Admin, AdminInstance, Role } from '@spark/shared'
import { flushPromises, mount } from '@vue/test-utils'
import ElementPlus from 'element-plus'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter, RouterView, type Router } from 'vue-router'

const { get } = vi.hoisted(() => ({ get: vi.fn() }))
vi.mock('../../api', () => ({ api: { get, post: vi.fn(), put: vi.fn(), del: vi.fn() }, onUnauthorized: vi.fn(), onTotpSetupRequired: vi.fn() }))

import { session } from '../../store/session'
import InstanceDetailView from '../InstanceDetailView.vue'
import InstancesView from '../InstancesView.vue'

// 计划 13（A-INS-17）：实例详情的集群区块；成员的「延长」引导到集群页、「换机」提示只能换到同一集群网络
const inst = (): AdminInstance & { cluster_id: string | null; cluster_rank: number | null } => ({
  id: 'ins-bbbb1111', user_id: 7, machine_id: 12, product_id: 1, image_id: 5, state: 'running', suspended: false, pending_seconds: 0, remaining_seconds: 3600,
  expires_at: null, stopped_at: null, password_auth: false, created_at: '', scheduled_start_at: null, scheduled_end_at: null, group_id: null, batch_id: null,
  reassign_count: 0, auto_renew: false, auto_renew_duration: '1d' as AdminInstance['auto_renew_duration'], auto_renew_failed_reason: '', provisioned_at: null, cancelled_at: null,
  cluster_id: 'clu-ab12cd34', cluster_rank: 1,
})
const clusterBlock = {
  id: 'clu-ab12cd34', size: 2, status: 'ready', network: 'ethernet', fabric_name: '机房 A 以太网', fabric_kind: 'lan', subnet: '10.210.0.64/28', head_ip: '10.210.0.75',
  state: 'active', system: false, user_id: 7, fabric_id: 3, mode: 'routed', version: 1, created_at: '', rank: 1,
  members: [
    { rank: 0, instance_id: 'ins-aaaa0000', state: 'running', ip: '10.210.0.75', machine_id: 11, machine_label: 'A01-01', net_status: 'ok' },
    { rank: 1, instance_id: 'ins-bbbb1111', state: 'running', ip: '10.210.0.76', machine_id: 12, machine_label: 'A01-02', net_status: 'pending' },
  ],
}
let member = true
let wrapper: ReturnType<typeof mount> | undefined
let router: Router
function login(role: Role) {
  session.status = 'authed'
  session.admin = { id: 1, username: 'a', role, status: 'active', totp_confirmed: true, last_login_at: null, last_login_ip: '', created_at: '' } as Admin
}
async function mountAt(url: string) {
  router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/instances', component: InstancesView }, { path: '/instances/:id', component: InstanceDetailView }, { path: '/:p(.*)*', component: { template: '<div />' } }] })
  await router.push(url)
  await router.isReady()
  wrapper = mount(RouterView, { global: { plugins: [router, ElementPlus] }, attachTo: document.body })
  await flushPromises()
}
const byId = (id: string) => wrapper!.find(`[data-testid="${id}"]`)

beforeEach(() => {
  login('ops')
  member = true
  get.mockReset()
  get.mockImplementation(async (path: string) => {
    if (path === '/instances/ins-bbbb1111') {
      const i = inst()
      if (!member) Object.assign(i, { cluster_id: null, cluster_rank: null })
      return { instance: i, cluster: member ? clusterBlock : null, events: [], orders: [], commands: [], ports: [], ssh_host_pubkey_fingerprint: '', apps: [] }
    }
    if (path === '/instances') return { items: [inst(), { ...inst(), id: 'ins-plain000', cluster_id: null, cluster_rank: null }], total: 2 }
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

describe('实例详情：集群区块', () => {
  it('成员：显示集群链接、rank、成员表；延长引导到集群页；换机弹窗提示同一集群网络', async () => {
    await mountAt('/instances/ins-bbbb1111')
    expect(byId('instance-cluster-block').exists()).toBe(true)
    expect(byId('instance-cluster-link').text()).toBe('clu-ab12cd34')
    expect(byId('instance-cluster-rank').text()).toContain('rank 1 / 2 台')
    expect(byId('instance-cluster-status').text()).toBe('就绪')
    expect(wrapper!.findAll('[data-testid="instance-cluster-member"]')).toHaveLength(2)
    expect(byId('adetail-extend-btn').exists()).toBe(false)
    await byId('adetail-extend-cluster-btn').trigger('click')
    await flushPromises()
    expect(router.currentRoute.value.path).toBe('/clusters/clu-ab12cd34')
  })

  it('换机弹窗里的集群提示', async () => {
    await mountAt('/instances/ins-bbbb1111')
    await byId('adetail-migrate-btn').trigger('click')
    await flushPromises()
    expect(document.body.querySelector('[data-testid="migrate-cluster-hint"]')!.textContent).toContain('只能换到同一集群网络')
  })

  it('非成员：没有集群区块，延长照旧', async () => {
    member = false
    await mountAt('/instances/ins-bbbb1111')
    expect(byId('instance-cluster-block').exists()).toBe(false)
    expect(byId('adetail-extend-btn').exists()).toBe(true)
    expect(byId('adetail-extend-cluster-btn').exists()).toBe(false)
  })
})

describe('实例列表：集群标记', () => {
  it('成员行带「集群 · rank r」并链接到集群页；非成员没有', async () => {
    await mountAt('/instances')
    const tags = wrapper!.findAll('.el-table__body [data-testid="ainst-cluster"]')
    expect(tags).toHaveLength(1)
    expect(tags[0].text()).toBe('集群 · rank 1')
    expect(tags[0].attributes('href')).toContain('/clusters/clu-ab12cd34')
  })
})
