import { ApiError, type Fabric } from '@spark/shared'
import { flushPromises, mount } from '@vue/test-utils'
import ElementPlus, { ElMessage, ElSelect } from 'element-plus'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter } from 'vue-router'

const { get, post, put, del } = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn(), put: vi.fn(), del: vi.fn() }))
vi.mock('../../api', () => ({ api: { get, post, put, del }, onUnauthorized: vi.fn(), onTotpSetupRequired: vi.fn() }))

import type { NetMachine } from '../../logic/clusters'
import MachineNetCard from '../MachineNetCard.vue'

const lan: Fabric = { id: 3, name: '机房 A 以太网', kind: 'lan', isolation: 'routed', rdma: false, mtu: 0, vlan_min: null, vlan_max: null, description: '', created_at: '', updated_at: '' }
const cx: Fabric = { ...lan, id: 4, name: 'CX-7 直连', kind: 'cx7', isolation: 'direct', rdma: true }
const machine = (over: Partial<NetMachine> = {}): NetMachine => ({
  id: 11, serial: 'SN-11', rack: 'A', slot: '1', lan_ip: '192.168.1.221', product_id: 1, state: 'idle', agent_online: true, last_seen_at: null,
  pdu_id: null, pdu_outlet: null, created_at: '', enrolled: true, fingerprint_prefix: 'ab', connected: true, agent_version: '1', agent_connected_at: null,
  state_reported_at: null, last_metrics: null, group_id: null, display_name: 'A01-01', cached_images: [], cached_images_at: null,
  fabric_id: 3, fabric_name: '机房 A 以太网', cluster_iface: '', agent_features: 3, net_info_at: '2026-10-26T01:00:00Z',
  cluster_ready: true, cluster_rdma: false, cluster_net: '192.168.0.0/23', cluster_reason: '',
  net_info: {
    lan_iface: 'enP7s7',
    ifaces: [
      { name: 'enP7s7', mac: 'aa', up: true, speed_mbps: 10000, mtu: 1500, ipv4: ['192.168.1.221/23'], driver: 'r8127', pci: '0007:01:00.0', wireless: false },
      { name: 'enp1s0f0np0', mac: 'bb', up: false, speed_mbps: 0, mtu: 1500, ipv4: [], driver: 'mlx5_core', pci: '', wireless: false, rdma_device: 'rocep1s0f0', rdma_active: false, uverbs: 'uverbs0' },
    ],
  },
  ...over,
})

let wrapper: ReturnType<typeof mount> | undefined
async function mountCard(m: NetMachine, fabric: Fabric | null = lan, canEdit = true) {
  const router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/:p(.*)*', component: { template: '<div />' } }] })
  wrapper = mount(MachineNetCard, { props: { machine: m, fabric, canEdit }, global: { plugins: [router, ElementPlus] }, attachTo: document.body })
  await flushPromises()
}
const byId = (id: string) => wrapper!.find(`[data-testid="${id}"]`)
const inBody = (id: string) => document.body.querySelector<HTMLElement>(`[data-testid="${id}"]`)

beforeEach(() => {
  for (const f of [get, post, put, del]) f.mockReset()
  get.mockImplementation(async (path: string) => {
    if (path === '/fabrics') return { items: [{ ...lan, machine_count: 1, active_clusters: 0, cluster_nets: [] }, { ...cx, machine_count: 1, active_clusters: 0, cluster_nets: [] }] }
    throw new Error(`unexpected GET ${path}`)
  })
  put.mockResolvedValue({})
})
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  document.body.innerHTML = ''
  vi.restoreAllMocks()
})

describe('MachineNetCard', () => {
  it('就绪判定、能力位、所属网络与网卡表', async () => {
    await mountCard(machine())
    expect(byId('machine-cluster-ready').text()).toBe('就绪（192.168.0.0/23）')
    expect(byId('machine-cluster-ready').attributes('data-ready')).toBe('true')
    expect(byId('machine-agent-features').text()).toBe('共享实例、多机集群')
    expect(byId('machine-fabric').text()).toContain('机房 A 以太网')
    expect(byId('machine-cluster-iface').text()).toBe('自动（管理网卡 enP7s7）')
    const rows = wrapper!.findAll('[data-testid="machine-net-table"] [data-testid="machine-net-iface"]')
    expect(rows).toHaveLength(2)
    expect(rows[0].text()).toContain('enP7s7')
    expect(rows[0].text()).toContain('管理网卡')
    expect(rows[0].text()).toContain('10 Gb/s')
    expect(rows[0].text()).toContain('192.168.1.221/23')
    expect(rows[1].text()).toContain('rocep1s0f0 · 未激活 · uverbs0')
    expect(rows[1].text()).toContain('未连接')
  })

  it('旧 Agent：显示「Agent 不支持集群」原因与能力位', async () => {
    await mountCard(machine({ agent_features: 1, cluster_ready: false, cluster_net: '', cluster_reason: 'Agent 不支持集群（请升级）', net_info: null, net_info_at: null }))
    expect(byId('machine-cluster-ready').text()).toBe('未就绪：Agent 不支持集群（请升级）')
    expect(byId('machine-cluster-ready').attributes('data-ready')).toBe('false')
    expect(byId('machine-agent-features').text()).toContain('不支持多机集群')
    expect(byId('machine-net-none').exists()).toBe(true)
  })

  it('修改：加载集群网络，选 cx7 后网卡下拉禁用管理网卡；只提交变化的键', async () => {
    await mountCard(machine())
    await byId('machine-net-edit').trigger('click')
    await flushPromises()
    expect(get).toHaveBeenCalledWith('/fabrics')
    const selects = wrapper!.findAllComponents(ElSelect)
    const fabricSel = selects.find((s) => s.attributes('data-testid') === 'machine-fabric-select' || s.find('[data-testid="machine-fabric-select"]').exists()) ?? selects[0]
    fabricSel.vm.$emit('update:modelValue', 4)
    await flushPromises()
    const opts = [...document.body.querySelectorAll<HTMLElement>('[data-testid="machine-iface-option"]')]
    expect(opts.map((o) => o.textContent?.trim())).toEqual(['自动（第一块已连接的 CX-7 网卡）', 'enP7s7（管理网卡）', 'enp1s0f0np0（CX-7，未连接）'])
    expect(opts[1].className).toContain('is-disabled')
    inBody('machine-net-save')!.click()
    await flushPromises()
    expect(put).toHaveBeenCalledWith('/machines/11', { fabric_id: 4 })
    expect(wrapper!.emitted('saved')).toHaveLength(1)
  })

  it('移出集群网络：fabric_id null；fabric_full / invalid_state 原文提示', async () => {
    const err = vi.spyOn(ElMessage, 'error')
    put.mockRejectedValueOnce(new ApiError(409, 'invalid_state', '机器上有集群成员，不能修改集群网络'))
    await mountCard(machine())
    await byId('machine-net-edit').trigger('click')
    await flushPromises()
    wrapper!.findAllComponents(ElSelect)[0].vm.$emit('update:modelValue', null)
    await flushPromises()
    inBody('machine-net-save')!.click()
    await flushPromises()
    expect(put).toHaveBeenCalledWith('/machines/11', { fabric_id: null })
    expect(err).toHaveBeenCalledWith('机器上有集群成员，不能修改集群网络')
  })

  it('没有写权限时不显示修改按钮', async () => {
    await mountCard(machine(), lan, false)
    expect(byId('machine-net-edit').exists()).toBe(false)
  })
})
