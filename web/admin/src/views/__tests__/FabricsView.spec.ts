import { ApiError, type Admin, type FabricRow, type Role } from '@spark/shared'
import { flushPromises, mount } from '@vue/test-utils'
import ElementPlus, { ElMessage } from 'element-plus'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter, RouterView } from 'vue-router'

const { get, post, put, del } = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn(), put: vi.fn(), del: vi.fn() }))
vi.mock('../../api', () => ({ api: { get, post, put, del }, onUnauthorized: vi.fn(), onTotpSetupRequired: vi.fn() }))

import { session } from '../../store/session'
import FabricsView from '../FabricsView.vue'

const row = (over: Partial<FabricRow> = {}): FabricRow => ({
  id: 1, name: '机房 A 以太网', kind: 'lan', isolation: 'routed', rdma: false, mtu: 0, vlan_min: null, vlan_max: null, description: '一楼交换机',
  created_at: '', updated_at: '', machine_count: 3, active_clusters: 1, cluster_nets: ['192.168.0.0/23'], ...over,
})
let rows: FabricRow[]
let wrapper: ReturnType<typeof mount> | undefined

function login(role: Role) {
  session.status = 'authed'
  session.admin = { id: 1, username: 'a', role, status: 'active', totp_confirmed: true, last_login_at: null, last_login_ip: '', created_at: '' } as Admin
}
async function mountView() {
  const router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/fabrics', component: FabricsView }, { path: '/:p(.*)*', component: { template: '<div />' } }] })
  await router.push('/fabrics')
  await router.isReady()
  wrapper = mount(RouterView, { global: { plugins: [router, ElementPlus] }, attachTo: document.body })
  await flushPromises()
}
const byId = (id: string) => wrapper!.find(`[data-testid="${id}"]`)
const allById = (id: string) => wrapper!.findAll(`[data-testid="${id}"]`)
const inBody = (id: string) => document.body.querySelector<HTMLElement>(`[data-testid="${id}"]`)
const click = async (el: HTMLElement | null | undefined) => {
  expect(el).toBeTruthy()
  el!.click()
  await flushPromises()
}
/** el-radio-group 里按文字点选项 */
async function pick(groupId: string, text: string) {
  const labels = [...(inBody(groupId)?.querySelectorAll<HTMLElement>('label') ?? [])]
  await click(labels.find((l) => l.textContent?.includes(text)))
}
/** el-input 把 data-testid 透传到 <input> 上，el-input-number 在外层 */
const inputOf = (id: string) => {
  const el = inBody(id)
  return (el?.tagName === 'INPUT' || el?.tagName === 'TEXTAREA' ? el : el?.querySelector('input')) as HTMLInputElement | null
}
async function typeIn(id: string, v: string) {
  const input = inputOf(id)
  expect(input).toBeTruthy()
  ;(input as HTMLInputElement).value = v
  input!.dispatchEvent(new Event('input'))
  await flushPromises()
}

beforeEach(() => {
  login('ops')
  rows = [row(), row({ id: 2, name: 'CX-7 直连', kind: 'cx7', isolation: 'direct', rdma: true, mtu: 9000, machine_count: 2, active_clusters: 0, cluster_nets: ['l2'] })]
  for (const f of [get, post, put, del]) f.mockReset()
  get.mockImplementation(async (path: string) => {
    if (path === '/fabrics') return { items: rows }
    throw new Error(`unexpected GET ${path}`)
  })
  post.mockResolvedValue({})
  put.mockResolvedValue({})
  del.mockResolvedValue({ ok: true })
})
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  document.body.innerHTML = ''
  vi.restoreAllMocks()
})

describe('FabricsView 列表', () => {
  it('列出名称 / 类型 / 隔离 / RDMA / MTU / VLAN / 机器数 / 活跃集群 / 网段', async () => {
    await mountView()
    expect(byId('fabrics-table').exists()).toBe(true)
    const trs = allById('fabric-row')
    expect(trs).toHaveLength(2)
    expect(trs[0].find('[data-testid="fabric-name"]').text()).toBe('机房 A 以太网')
    expect(trs[0].find('[data-testid="fabric-kind"]').text()).toBe('普通以太网')
    expect(trs[0].find('[data-testid="fabric-isolation"]').text()).toBe('三层路由')
    expect(trs[0].find('[data-testid="fabric-mtu"]').text()).toBe('继承网卡')
    expect(trs[0].find('[data-testid="fabric-machines"]').text()).toBe('3')
    expect(trs[0].find('[data-testid="fabric-clusters"]').text()).toBe('1')
    expect(trs[1].find('[data-testid="fabric-kind"]').text()).toBe('ConnectX-7')
    expect(trs[1].find('[data-testid="fabric-rdma"]').text()).toBe('是')
    expect(trs[1].find('[data-testid="fabric-mtu"]').text()).toBe('9000')
  })

  it('lan 下机器分属多个网段时标黄', async () => {
    rows = [row({ cluster_nets: ['192.168.0.0/23', '10.0.0.0/24'] })]
    await mountView()
    expect(byId('fabric-nets-warning').text()).toContain('2 个网段')
  })

  it('finance 只读：没有新建 / 编辑 / 删除', async () => {
    login('finance')
    await mountView()
    expect(allById('fabric-row')).toHaveLength(2)
    expect(byId('fabric-create').exists()).toBe(false)
    expect(byId('fabric-edit').exists()).toBe(false)
    expect(byId('fabric-delete').exists()).toBe(false)
  })
})

describe('FabricDrawer 联动', () => {
  it('lan：隔离只有三层路由、RDMA 禁用；cx7：直连 / VLAN，选 VLAN 后出现范围；提交 POST /fabrics', async () => {
    await mountView()
    await byId('fabric-create').trigger('click')
    await flushPromises()
    expect(inBody('fabric-drawer')).not.toBeNull()
    const iso = () => [...(inBody('fabric-drawer-isolation')?.querySelectorAll('label') ?? [])].map((l) => l.textContent?.trim())
    expect(iso()).toEqual(['三层路由'])
    expect(inBody('fabric-drawer-rdma')!.className).toContain('is-disabled')
    expect(inBody('fabric-drawer-vlan-min')).toBeNull()

    await pick('fabric-drawer-kind', 'ConnectX-7')
    expect(iso()).toEqual(['直连', 'VLAN 隔离'])
    expect(inBody('fabric-drawer-rdma')!.className).not.toContain('is-disabled')
    await pick('fabric-drawer-isolation', 'VLAN 隔离')
    expect(inBody('fabric-drawer-vlan-min')).not.toBeNull()

    await typeIn('fabric-drawer-name', 'CX-7 交换机')
    await typeIn('fabric-drawer-vlan-min', '100')
    await typeIn('fabric-drawer-vlan-max', '199')
    await click(inBody('fabric-drawer-save'))
    expect(post).toHaveBeenCalledWith('/fabrics', { name: 'CX-7 交换机', kind: 'cx7', isolation: 'vlan', rdma: false, mtu: 0, vlan_min: 100, vlan_max: 199, description: '' })
  })

  it('VLAN 未填范围：就地报错、不提交', async () => {
    await mountView()
    await byId('fabric-create').trigger('click')
    await flushPromises()
    await pick('fabric-drawer-kind', 'ConnectX-7')
    await pick('fabric-drawer-isolation', 'VLAN 隔离')
    await typeIn('fabric-drawer-name', 'x')
    await click(inBody('fabric-drawer-save'))
    expect(post).not.toHaveBeenCalled()
    expect(inBody('fabric-drawer-error')!.textContent).toBe('VLAN 隔离需填写 VLAN 范围')
  })

  it('编辑：回显并 PUT /fabrics/{id}', async () => {
    await mountView()
    await allById('fabric-edit')[1].trigger('click')
    await flushPromises()
    expect(inputOf('fabric-drawer-name')!.value).toBe('CX-7 直连')
    await click(inBody('fabric-drawer-save'))
    expect(put).toHaveBeenCalledWith('/fabrics/2', { name: 'CX-7 直连', kind: 'cx7', isolation: 'direct', rdma: true, mtu: 9000, vlan_min: null, vlan_max: null, description: '一楼交换机' })
  })
})

describe('删除', () => {
  it('确认后 DELETE；占用时提示原因，409 文案原样提示', async () => {
    const err = vi.spyOn(ElMessage, 'error')
    del.mockRejectedValueOnce(new ApiError(409, 'fabric_in_use', '该集群网络下还有机器或运行中的集群'))
    await mountView()
    await allById('fabric-delete')[0].trigger('click')
    await flushPromises()
    expect(inBody('fabric-delete-dialog')!.textContent).toContain('1 个运行中的集群')
    await click(inBody('fabric-delete-ok'))
    expect(del).toHaveBeenCalledWith('/fabrics/1')
    expect(err).toHaveBeenCalledWith('该集群网络下还有机器或运行中的集群')
  })
})
