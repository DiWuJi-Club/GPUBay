import type { Admin, Role } from '@spark/shared'
import { flushPromises, mount } from '@vue/test-utils'
import ElementPlus, { ElMessage } from 'element-plus'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter, RouterView, type Router } from 'vue-router'

const { get, post, put, del } = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn(), put: vi.fn(), del: vi.fn() }))
vi.mock('../../api', () => ({ api: { get, post, put, del }, onUnauthorized: vi.fn(), onTotpSetupRequired: vi.fn() }))

import type { AdminClusterApp, AdminClusterDetail, AdminClusterView } from '../../logic/clusters'
import { session } from '../../store/session'
import ClusterDetailView from '../ClusterDetailView.vue'
import ClustersView from '../ClustersView.vue'

const cluster = (over: Partial<AdminClusterView> = {}): AdminClusterView => ({
  id: 'clu-ab12cd34', user_id: 7, system: false, size: 2, fabric_id: 3, mode: 'routed', vlan: null, rdma: false, mtu: 0, subnet_index: 4, version: 2,
  state: 'active', batch_id: 'bat-1', ready_at: '2026-10-26T01:00:00Z', created_at: '2026-10-26T00:50:00Z', updated_at: '', ended_at: null,
  status: 'degraded', network: 'ethernet', fabric_name: '机房 A 以太网', fabric_kind: 'lan', subnet: '10.210.0.64/28', head_ip: '10.210.0.75',
  members: [
    { rank: 0, instance_id: 'ins-aaaa0000', state: 'running', suspended: false, machine_id: 11, machine_label: 'A01-01', ip: '10.210.0.75', net_status: 'ok', expires_at: '2026-10-27T00:50:00Z' },
    { rank: 1, instance_id: 'ins-bbbb1111', state: 'running', suspended: false, machine_id: 12, machine_label: 'A01-02', ip: '10.210.0.76', net_status: 'error: 路由下发失败', expires_at: '2026-10-27T00:50:00Z' },
  ],
  ...over,
})
const app = (): AdminClusterApp => ({
  id: 'acd-1', cluster_id: 'clu-ab12cd34', user_id: 7, app_id: 4, app_slug: 'vllm-cluster', app_name: 'vLLM 集群', version_id: 9, version: '1.0.0',
  state: 'active', status: 'stale', health: 'ok', phase: '', progress: 100, url: 'https://p8000-ins-aaaa0000.example.com', created_at: '', updated_at: '',
  members: [
    { rank: 0, role: 'head', instance_id: 'ins-aaaa0000', current: true, deployment_id: 'dep-h', state: 'running', health: 'ok', phase: '', progress: 100, deployment: null },
    { rank: 1, role: 'worker', instance_id: 'ins-old11111', current: false, deployment_id: 'dep-w', state: 'running', health: 'ok', phase: '', progress: 100, deployment: null },
  ],
})
let detail: AdminClusterDetail
let wrapper: ReturnType<typeof mount> | undefined
let router: Router

function login(role: Role) {
  session.status = 'authed'
  session.admin = { id: 1, username: 'a', role, status: 'active', totp_confirmed: true, last_login_at: null, last_login_ip: '', created_at: '' } as Admin
}
async function mountAt(url: string) {
  router = createRouter({
    history: createMemoryHistory(),
    routes: [{ path: '/clusters', component: ClustersView }, { path: '/clusters/:id', component: ClusterDetailView }, { path: '/:p(.*)*', component: { template: '<div />' } }],
  })
  await router.push(url)
  await router.isReady()
  wrapper = mount(RouterView, { global: { plugins: [router, ElementPlus] }, attachTo: document.body })
  await flushPromises()
}
const byId = (id: string) => wrapper!.find(`[data-testid="${id}"]`)
const allById = (id: string) => wrapper!.findAll(`[data-testid="${id}"]`)
const inBody = (id: string) => document.body.querySelector<HTMLElement>(`[data-testid="${id}"]`)
const inputOf = (id: string) => {
  const el = inBody(id)
  return (el?.tagName === 'INPUT' || el?.tagName === 'TEXTAREA' ? el : el?.querySelector('input')) as HTMLInputElement | null
}
async function typeIn(id: string, v: string) {
  const input = inputOf(id)
  expect(input, id).toBeTruthy()
  input!.value = v
  input!.dispatchEvent(new Event('input'))
  await flushPromises()
}
const click = async (el: HTMLElement | null) => {
  expect(el).not.toBeNull()
  el!.click()
  await flushPromises()
}

beforeEach(() => {
  login('ops')
  detail = {
    cluster: cluster(), app: app(),
    events: [{ id: 5, instance_id: 'ins-bbbb1111', from_state: 'creating', to_state: 'running', detail: '开通完成', at: '2026-10-26T01:00:00Z' }],
  }
  for (const f of [get, post, put, del]) f.mockReset()
  get.mockImplementation(async (path: string) => {
    if (path === '/clusters/clu-ab12cd34') return detail
    if (path === '/clusters') return { items: [cluster(), cluster({ id: 'clu-sys00001', system: true, user_id: 0, status: 'ready', size: 4 })], total: 2 }
    if (path === '/fabrics') return { items: [{ id: 3, name: '机房 A 以太网' }] }
    throw new Error(`unexpected GET ${path}`)
  })
  post.mockImplementation(async (path: string) => {
    if (path.endsWith('/resync')) return cluster({ version: 3 })
    if (path.endsWith('/extend')) return cluster()
    if (path.endsWith('/migrate')) return { instance: { id: 'ins-cccc2222', machine_id: 13 } }
    return {}
  })
})
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  document.body.innerHTML = ''
  vi.restoreAllMocks()
})

describe('ClusterDetailView', () => {
  it('概要、成员表（rank / 实例 / 机器 / IP / 状态 / 网络状态）、版本与事件', async () => {
    await mountAt('/clusters/clu-ab12cd34')
    expect(byId('cluster-detail-id').text()).toBe('clu-ab12cd34')
    expect(byId('cluster-detail-status').text()).toBe('降级')
    expect(byId('cluster-detail-network').text()).toBe('普通网络')
    expect(byId('cluster-detail-version').text()).toBe('2')
    expect(byId('cluster-detail-subnet').text()).toBe('10.210.0.64/28')
    expect(byId('cluster-detail-head').text()).toBe('10.210.0.75')
    const rows = wrapper!.findAll('[data-testid="cluster-detail-members"] [data-testid="cluster-member-row"]')
    expect(rows).toHaveLength(2)
    expect(rows[0].attributes('data-rank')).toBe('0')
    expect(rows[0].find('[data-testid="cluster-member-instance"]').text()).toBe('ins-aaaa0000')
    expect(rows[0].find('[data-testid="cluster-member-machine"]').text()).toContain('A01-01')
    expect(rows[0].find('[data-testid="cluster-member-ip"]').text()).toBe('10.210.0.75')
    expect(rows[0].find('[data-testid="cluster-member-net"]').text()).toBe('正常')
    expect(rows[1].find('[data-testid="cluster-member-net"]').text()).toBe('异常：路由下发失败')
    expect(rows[1].find('[data-testid="cluster-member-state"]').text()).toBe('运行中')
    expect(allById('cluster-event')).toHaveLength(1)
    expect(byId('cluster-event').text()).toContain('开通完成')
    expect(byId('cluster-degraded-hint').exists()).toBe(true)
  })

  it('集群应用：聚合状态、head 地址、成员角色；stale 提示成员已更换', async () => {
    await mountAt('/clusters/clu-ab12cd34')
    expect(byId('cluster-app-status').text()).toBe('需要重新部署')
    expect(byId('cluster-app-stale').exists()).toBe(true)
    expect(byId('cluster-app-url').text()).toBe('https://p8000-ins-aaaa0000.example.com')
    const ms = allById('cluster-app-member')
    expect(ms).toHaveLength(2)
    expect(ms[0].find('[data-testid="cluster-app-member-role"]').text()).toBe('主节点')
    expect(ms[1].find('[data-testid="cluster-app-member-current"]').text()).toBe('已换出')
  })

  it('没有集群应用时显示占位', async () => {
    detail.app = null
    await mountAt('/clusters/clu-ab12cd34')
    expect(byId('cluster-app-none').exists()).toBe(true)
  })

  it('延长：POST /clusters/{id}/extend {seconds, reason}', async () => {
    await mountAt('/clusters/clu-ab12cd34')
    await byId('cluster-extend').trigger('click')
    await flushPromises()
    await click(inBody('cluster-extend-submit'))
    expect(post).not.toHaveBeenCalled()
    expect(inBody('cluster-extend-error')!.textContent).toBe('请填写补偿原因')
    await typeIn('cluster-extend-reason', '网络故障补偿')
    await click(inBody('cluster-extend-submit'))
    expect(post).toHaveBeenCalledWith('/clusters/clu-ab12cd34/extend', { seconds: 86400, reason: '网络故障补偿' })
  })

  it('重新下发：确认后 POST /clusters/{id}/resync，版本更新', async () => {
    const ok = vi.spyOn(ElMessage, 'success')
    await mountAt('/clusters/clu-ab12cd34')
    await byId('cluster-resync').trigger('click')
    await flushPromises()
    await click(inBody('cluster-resync-ok'))
    expect(post).toHaveBeenCalledWith('/clusters/clu-ab12cd34/resync', {})
    expect(ok).toHaveBeenCalledWith('已重新下发集群网络（版本 3）')
  })

  it('成员换机：提示只能换到同一集群网络，POST /instances/{id}/migrate', async () => {
    await mountAt('/clusters/clu-ab12cd34')
    await allById('cluster-member-migrate')[1].trigger('click')
    await flushPromises()
    expect(inBody('cluster-migrate-dialog')!.textContent).toContain('只能换到同一集群网络')
    await typeIn('cluster-migrate-reason', '机器故障')
    await click(inBody('cluster-migrate-submit'))
    expect(post).toHaveBeenCalledWith('/instances/ins-bbbb1111/migrate', { reason: '机器故障' })
    expect(inBody('cluster-migrate-result')!.textContent).toContain('ins-cccc2222')
  })

  it('finance 只读：没有延长 / 重新下发 / 换机，机器不链接', async () => {
    login('finance')
    await mountAt('/clusters/clu-ab12cd34')
    expect(byId('cluster-extend').exists()).toBe(false)
    expect(byId('cluster-resync').exists()).toBe(false)
    expect(byId('cluster-member-migrate').exists()).toBe(false)
    expect(byId('cluster-member-machine').find('a').exists()).toBe(false)
  })

  it('平台节点组：显示「平台」，不能延长（不计时）但可重新下发', async () => {
    detail.cluster = cluster({ system: true, user_id: 0 })
    await mountAt('/clusters/clu-ab12cd34')
    expect(byId('cluster-detail-owner').text()).toContain('平台')
    expect(byId('cluster-extend').exists()).toBe(false)
    expect(byId('cluster-resync').exists()).toBe(true)
    expect(byId('cluster-member-migrate').exists()).toBe(false)
  })

  it('已结束的集群：没有操作按钮', async () => {
    detail.cluster = cluster({ state: 'released', status: 'released', ended_at: '2026-10-26T05:00:00Z' })
    await mountAt('/clusters/clu-ab12cd34')
    expect(byId('cluster-detail-status').text()).toBe('已释放')
    expect(byId('cluster-extend').exists()).toBe(false)
    expect(byId('cluster-resync').exists()).toBe(false)
  })
})

describe('ClustersView', () => {
  it('默认看活动集群；筛选用户 / 平台；行显示规模、状态、网络、fabric、子网', async () => {
    await mountAt('/clusters')
    expect(get).toHaveBeenCalledWith('/clusters', { state: 'active', user_id: undefined, fabric_id: undefined, system: undefined, page: 1, page_size: 20 })
    const rows = allById('cluster-row')
    expect(rows).toHaveLength(2)
    expect(rows[0].find('[data-testid="cluster-row-id"]').text()).toBe('clu-ab12cd34')
    expect(rows[0].find('[data-testid="cluster-row-owner"]').text()).toBe('7')
    expect(rows[1].find('[data-testid="cluster-row-owner"]').text()).toBe('平台')
    expect(rows[1].find('[data-testid="cluster-row-size"]').text()).toBe('4 台')
    expect(rows[0].find('[data-testid="cluster-row-status"]').text()).toBe('降级')
    expect(rows[0].find('[data-testid="cluster-row-fabric"]').text()).toBe('机房 A 以太网')
    expect(rows[0].find('[data-testid="cluster-row-subnet"]').text()).toBe('10.210.0.64/28')
  })

  it('?fabric_id= 预填筛选（集群网络页的链接）', async () => {
    await mountAt('/clusters?fabric_id=3')
    expect(get).toHaveBeenCalledWith('/clusters', expect.objectContaining({ fabric_id: 3 }))
  })
})
