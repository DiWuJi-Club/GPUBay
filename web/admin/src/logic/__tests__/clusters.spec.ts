import type { NetInfo } from '@spark/shared'
import { describe, expect, it } from 'vitest'
import {
  agentFeatureLabels, canExtendCluster, canMigrateMember, canResyncCluster, clusterListQuery, emptyClusterFilter, emptyFabricForm, fabricDeleteBlocker, fabricNetsWarning,
  fmtSpeed, ifaceChoices, isolationOptions, machineNetPatch, mgmtIface, netStatusText, normalizeFabricForm, rdmaText, readinessText, supportsCluster, validateFabricForm, vlanRangeText,
} from '../clusters'

const net: NetInfo = {
  lan_iface: '',
  ifaces: [
    { name: 'enP7s7', mac: 'aa', up: true, speed_mbps: 10000, mtu: 1500, ipv4: ['192.168.1.221/23'], driver: 'r8127', pci: '', wireless: false },
    { name: 'enp1s0f0np0', mac: 'bb', up: true, speed_mbps: 200000, mtu: 9000, ipv4: [], driver: 'mlx5_core', pci: '', wireless: false, rdma_device: 'rocep1s0f0', rdma_active: true, uverbs: 'uverbs0', link_layer: 'Ethernet' },
    { name: 'wlP9s9', mac: 'cc', up: false, speed_mbps: 0, mtu: 1500, ipv4: [], driver: 'mt7925e', pci: '', wireless: true },
  ],
}

describe('fabric 表单联动（规格 §3.1）', () => {
  it('lan 只能 routed；cx7 直连 / VLAN', () => {
    expect(isolationOptions('lan')).toEqual(['routed'])
    expect(isolationOptions('cx7')).toEqual(['direct', 'vlan'])
  })
  it('切到 cx7：isolation 取 direct；切回 lan：routed 且关 RDMA、清 VLAN', () => {
    const cx = normalizeFabricForm({ ...emptyFabricForm(), kind: 'cx7' })
    expect(cx.isolation).toBe('direct')
    const lan = normalizeFabricForm({ ...cx, isolation: 'vlan', rdma: true, vlan_min: 100, vlan_max: 200, kind: 'lan' })
    expect(lan).toMatchObject({ isolation: 'routed', rdma: false, vlan_min: undefined, vlan_max: undefined })
  })
  it('非 vlan 清空范围；vlan 保留', () => {
    expect(normalizeFabricForm({ ...emptyFabricForm(), kind: 'cx7', isolation: 'direct', vlan_min: 3, vlan_max: 4 })).toMatchObject({ vlan_min: undefined, vlan_max: undefined })
    expect(normalizeFabricForm({ ...emptyFabricForm(), kind: 'cx7', isolation: 'vlan', vlan_min: 3, vlan_max: 4 })).toMatchObject({ vlan_min: 3, vlan_max: 4 })
  })
  it('校验：名称、MTU、VLAN 范围；合法时给出请求体（非 vlan 的范围为 null）', () => {
    const ok = validateFabricForm({ ...emptyFabricForm(), name: ' 机房 A ', description: ' x ' })
    expect(ok).toEqual({ ok: true, input: { name: '机房 A', kind: 'lan', isolation: 'routed', rdma: false, mtu: 0, vlan_min: null, vlan_max: null, description: 'x' } })
    const vlan = validateFabricForm({ ...emptyFabricForm(), name: 'v', kind: 'cx7', isolation: 'vlan', rdma: true, mtu: 9000, vlan_min: 100, vlan_max: 199 })
    expect(vlan.ok && vlan.input).toMatchObject({ isolation: 'vlan', rdma: true, mtu: 9000, vlan_min: 100, vlan_max: 199 })
    const bad = (over: object) => validateFabricForm({ ...emptyFabricForm(), name: 'n', ...over })
    expect(bad({ name: '' })).toMatchObject({ ok: false, errors: { name: '名称需为 1～32 个字符' } })
    expect(bad({ name: 'x'.repeat(33) }).ok).toBe(false)
    expect(bad({ mtu: 1000 })).toMatchObject({ ok: false, errors: { mtu: 'MTU 需为 0（继承网卡）或 1280～9216' } })
    expect(bad({ kind: 'cx7', isolation: 'vlan' })).toMatchObject({ ok: false, errors: { vlan_min: 'VLAN 隔离需填写 VLAN 范围' } })
    expect(bad({ kind: 'cx7', isolation: 'vlan', vlan_min: 300, vlan_max: 200 })).toMatchObject({ ok: false, errors: { vlan_min: 'VLAN 范围需在 2～4094 之间且起始不大于结束' } })
    expect(bad({ kind: 'cx7', isolation: 'vlan', vlan_min: 1, vlan_max: 200 }).ok).toBe(false)
  })
  it('lan 带 RDMA 提交时被联动关掉', () => {
    const out = validateFabricForm({ ...emptyFabricForm(), name: 'n', rdma: true })
    expect(out.ok && out.input.rdma).toBe(false)
  })
  it('表格文案与删除提示', () => {
    expect(vlanRangeText({ isolation: 'vlan', vlan_min: 100, vlan_max: 199 })).toBe('100～199')
    expect(vlanRangeText({ isolation: 'direct', vlan_min: null, vlan_max: null })).toBe('—')
    expect(fabricNetsWarning({ kind: 'lan', cluster_nets: ['192.168.0.0/23', '192.168.4.0/24'] })).toContain('2 个网段')
    expect(fabricNetsWarning({ kind: 'lan', cluster_nets: ['192.168.0.0/23'] })).toBe('')
    expect(fabricNetsWarning({ kind: 'cx7', cluster_nets: ['l2'] })).toBe('')
    expect(fabricDeleteBlocker({ machine_count: 0, active_clusters: 0 })).toBe('')
    expect(fabricDeleteBlocker({ machine_count: 2, active_clusters: 0 })).toContain('2 台机器')
    expect(fabricDeleteBlocker({ machine_count: 2, active_clusters: 1 })).toContain('1 个运行中的集群')
  })
})

describe('机器集群网卡', () => {
  it('能力位', () => {
    expect(agentFeatureLabels(3)).toEqual(['共享实例', '多机集群'])
    expect(agentFeatureLabels(1)).toEqual(['共享实例'])
    expect(supportsCluster(1)).toBe(false)
    expect(supportsCluster(2)).toBe(true)
  })
  it('管理网卡按 lan_ip 找，找不到退回 lan_iface', () => {
    expect(mgmtIface(net, '192.168.1.221')).toBe('enP7s7')
    expect(mgmtIface({ ...net, lan_iface: 'eth9' }, '10.0.0.1')).toBe('eth9')
    expect(mgmtIface(null, '1.1.1.1')).toBe('')
  })
  it('网卡下拉：自动 + 网卡；cx7 时管理网卡与无线不可选；当前配置未探测到时保留', () => {
    const lan = ifaceChoices(net, 'lan', '192.168.1.221')
    expect(lan.map((c) => c.value)).toEqual(['', 'enP7s7', 'enp1s0f0np0', 'wlP9s9'])
    expect(lan[1].label).toBe('enP7s7（管理网卡）')
    expect(lan.every((c) => !c.disabled)).toBe(true)
    const cx = ifaceChoices(net, 'cx7', '192.168.1.221', 'enp9')
    expect(cx[0].label).toContain('CX-7')
    expect(cx.find((c) => c.value === 'enP7s7')!.disabled).toBe(true)
    expect(cx.find((c) => c.value === 'wlP9s9')!.disabled).toBe(true)
    expect(cx.find((c) => c.value === 'enp1s0f0np0')).toMatchObject({ disabled: false, label: 'enp1s0f0np0（CX-7）' })
    expect(cx.at(-1)).toEqual({ value: 'enp9', label: 'enp9（未探测到）', disabled: false })
    expect(ifaceChoices(null, undefined, '')).toHaveLength(1)
  })
  it('速率 / RDMA 文案', () => {
    expect(fmtSpeed(10000)).toBe('10 Gb/s')
    expect(fmtSpeed(2500)).toBe('2.5 Gb/s')
    expect(fmtSpeed(100)).toBe('100 Mb/s')
    expect(fmtSpeed(0)).toBe('—')
    expect(rdmaText(net.ifaces[1])).toBe('rocep1s0f0 · ACTIVE · Ethernet · uverbs0')
    expect(rdmaText(net.ifaces[0])).toBe('—')
  })
  it('就绪判定文案', () => {
    expect(readinessText({ cluster_ready: false, cluster_rdma: false, cluster_net: '', cluster_reason: 'Agent 不支持集群（请升级）' })).toBe('未就绪：Agent 不支持集群（请升级）')
    expect(readinessText({ cluster_ready: true, cluster_rdma: false, cluster_net: '192.168.0.0/23' })).toBe('就绪（192.168.0.0/23）')
    expect(readinessText({ cluster_ready: true, cluster_rdma: true, cluster_net: 'l2' })).toBe('就绪（二层直连 / VLAN） · RDMA 可用')
  })
  it('PUT 只带变化的键', () => {
    expect(machineNetPatch({ fabric_id: null, cluster_iface: '' }, null, '')).toEqual({})
    expect(machineNetPatch({ fabric_id: null, cluster_iface: '' }, 3, '')).toEqual({ fabric_id: 3 })
    expect(machineNetPatch({ fabric_id: 3, cluster_iface: 'enp1' }, null, '')).toEqual({ fabric_id: null, cluster_iface: '' })
  })
})

describe('集群', () => {
  it('列表查询串', () => {
    expect(clusterListQuery(emptyClusterFilter(), 1, 20)).toEqual({ state: 'active', user_id: undefined, fabric_id: undefined, system: undefined, page: 1, page_size: 20 })
    expect(clusterListQuery({ state: '', user_id: ' 12a ', fabric_id: 3, system: 'false' }, 2, 50)).toEqual({ state: undefined, user_id: '12', fabric_id: 3, system: 'false', page: 2, page_size: 50 })
  })
  it('延长 / 重新下发 / 成员换机的可见性', () => {
    const user = { state: 'active' as const, system: false }
    const sys = { state: 'active' as const, system: true }
    const ended = { state: 'released' as const, system: false }
    expect(canExtendCluster('ops', user)).toBe(true)
    expect(canExtendCluster('finance', user)).toBe(false)
    expect(canExtendCluster('super', sys)).toBe(false)
    expect(canExtendCluster('ops', ended)).toBe(false)
    expect(canResyncCluster('ops', sys)).toBe(true)
    expect(canResyncCluster('ops', ended)).toBe(false)
    expect(canResyncCluster('finance', user)).toBe(false)
    expect(canMigrateMember('ops', user, 'running')).toBe(true)
    expect(canMigrateMember('ops', user, 'creating')).toBe(false)
    expect(canMigrateMember('ops', sys, 'running')).toBe(false)
  })
  it('成员网络状态', () => {
    expect(netStatusText('')).toBe('未上报')
    expect(netStatusText('ok')).toBe('正常')
    expect(netStatusText('pending')).toBe('等待容器运行')
    expect(netStatusText('error: 路由下发失败')).toBe('异常：路由下发失败')
  })
})
