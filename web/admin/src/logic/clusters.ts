import type {
  AdminCluster, AdminInstance, DeploymentHealth, Fabric, FabricInput, FabricIsolation, FabricKind, FabricRow, Machine, MachineClusterNet, MachineDetail, NetIface, NetInfo, Role,
} from '@spark/shared'
import type { AdminDeployment } from '@spark/shared'
import { OPS_ROLES } from '../menu'

// 计划 13：集群网络（fabric）、机器集群网卡、实例集群的后台类型与纯逻辑（规格 §3、§10.2、§11）。
// 字段以后端实际 JSON 为准（adminapi / fleet / rental / llm），与 @spark/shared 的通用类型不同处在此补齐。

// ---- 类型 ----

/** GET /machines、GET /machines/{id} 的机器（fleet.Machine 带集群网络字段）。 */
export type NetMachine = Machine & MachineClusterNet
/** GET /machines/{id}：多 fabric（所属集群网络；不在 fabric 时为 null）。 */
export type NetMachineDetail = Omit<MachineDetail, 'machine'> & { machine: NetMachine; fabric?: Fabric | null }

/** 后台集群视图（rental.ClusterView：集群行全部字段 + 派生状态与成员；没有 user_phone）。 */
export type AdminClusterView = Omit<AdminCluster, 'user_phone' | 'events'> & {
  vlan: number | null
  rdma: boolean
  mtu: number
  subnet_index: number
  batch_id: string | null
  ready_at: string | null
  updated_at: string
  ended_at: string | null
}
/** 集群详情的事件：全部成员实例（含换机前的旧成员）的实例状态变化，新的在前。 */
export interface ClusterEvent { id: number; instance_id: string; from_state: string; to_state: string; detail: string; at: string }
/** 后台集群应用（adminapi clusterAppView）。 */
export interface AdminClusterAppMember {
  rank: number
  role: 'head' | 'worker'
  instance_id: string
  current: boolean
  deployment_id: string
  state: string
  health: DeploymentHealth
  phase: string
  progress: number
  deployment: AdminDeployment | null
}
export interface AdminClusterApp {
  id: string
  cluster_id: string
  user_id: number
  app_id: number
  app_slug: string
  app_name: string
  version_id: number
  version: string
  state: string
  status: string
  health: DeploymentHealth
  phase: string
  progress: number
  /** head（rank0 当前成员）的访问地址；端口不在时为 null */
  url: string | null
  members: AdminClusterAppMember[]
  created_at: string
  updated_at: string
}
/** GET /clusters/{id} */
export interface AdminClusterDetail { cluster: AdminClusterView; app: AdminClusterApp | null; events: ClusterEvent[] }
/** GET /instances/{id} 的 cluster 区块：所属集群视图 + 本实例的 rank（非成员为 null）。 */
export type InstanceClusterBlock = AdminClusterView & { rank: number }
/** 实例视图（计划 13 起带 cluster_id / cluster_rank，非成员 null）。 */
export type ClusterAwareInstance = AdminInstance & { cluster_id?: string | null; cluster_rank?: number | null }
/** 仪表盘集群卡片（state = active 的集群及其派生状态分布）。 */
export interface DashboardClusters { active: number; forming: number; ready: number; degraded: number }

// ---- fabric 表单（与 fleet.FabricInput.normalize 一致）----

export interface FabricForm {
  name: string
  kind: FabricKind
  isolation: FabricIsolation
  rdma: boolean
  /** 0 = 继承网卡 */
  mtu: number | undefined
  vlan_min: number | undefined
  vlan_max: number | undefined
  description: string
}

/** kind 决定可选的 isolation：lan 只能三层路由；cx7 直连或 VLAN。 */
export function isolationOptions(kind: FabricKind): FabricIsolation[] {
  return kind === 'lan' ? ['routed'] : ['direct', 'vlan']
}

export const emptyFabricForm = (): FabricForm => ({ name: '', kind: 'lan', isolation: 'routed', rdma: false, mtu: 0, vlan_min: undefined, vlan_max: undefined, description: '' })

export function fabricFormOf(f: Fabric): FabricForm {
  return {
    name: f.name, kind: f.kind, isolation: f.isolation, rdma: f.rdma, mtu: f.mtu,
    vlan_min: f.vlan_min ?? undefined, vlan_max: f.vlan_max ?? undefined, description: f.description,
  }
}

/** 联动：换了 kind 后 isolation 不在可选项内就取第一个；lan 关掉 RDMA；非 vlan 清空 VLAN 范围。返回新对象。 */
export function normalizeFabricForm(f: FabricForm): FabricForm {
  const opts = isolationOptions(f.kind)
  const isolation = opts.includes(f.isolation) ? f.isolation : opts[0]
  const out = { ...f, isolation }
  if (f.kind === 'lan') out.rdma = false
  if (isolation !== 'vlan') Object.assign(out, { vlan_min: undefined, vlan_max: undefined })
  return out
}

const isInt = (v: unknown): v is number => typeof v === 'number' && Number.isInteger(v)
const runes = (s: string) => [...s].length
// eslint-disable-next-line no-control-regex
const CONTROL_RE = /[\u0000-\u001f\u007f]/
// eslint-disable-next-line no-control-regex
const CONTROL_EXCEPT_NL_RE = /[\u0000-\u0009\u000b-\u001f\u007f]/

export type FabricFormResult = { ok: true; input: FabricInput } | { ok: false; errors: Partial<Record<keyof FabricForm, string>> }

/** 前端预校验（服务器仍是准绳，错误 fabric_invalid 按字段返回）。 */
export function validateFabricForm(raw: FabricForm): FabricFormResult {
  const f = normalizeFabricForm(raw)
  const errors: Partial<Record<keyof FabricForm, string>> = {}
  const name = f.name.trim()
  const description = f.description.trim()
  if (runes(name) < 1 || runes(name) > 32) errors.name = '名称需为 1～32 个字符'
  else if (CONTROL_RE.test(name)) errors.name = '名称不能包含控制字符'
  if (runes(description) > 200) errors.description = '说明最多 200 个字符'
  else if (CONTROL_EXCEPT_NL_RE.test(description)) errors.description = '说明不能包含控制字符'
  const mtu = f.mtu ?? 0
  if (!isInt(mtu) || (mtu !== 0 && (mtu < 1280 || mtu > 9216))) errors.mtu = 'MTU 需为 0（继承网卡）或 1280～9216'
  let vlanMin: number | null = null
  let vlanMax: number | null = null
  if (f.isolation === 'vlan') {
    if (!isInt(f.vlan_min) || !isInt(f.vlan_max)) errors.vlan_min = 'VLAN 隔离需填写 VLAN 范围'
    else if (f.vlan_min < 2 || f.vlan_min > 4094 || f.vlan_max < f.vlan_min || f.vlan_max > 4094) errors.vlan_min = 'VLAN 范围需在 2～4094 之间且起始不大于结束'
    else [vlanMin, vlanMax] = [f.vlan_min, f.vlan_max]
  }
  if (Object.keys(errors).length) return { ok: false, errors }
  return { ok: true, input: { name, kind: f.kind, isolation: f.isolation, rdma: f.rdma, mtu, vlan_min: vlanMin, vlan_max: vlanMax, description } }
}

/** 表格「VLAN 范围」列。 */
export const vlanRangeText = (f: Pick<Fabric, 'isolation' | 'vlan_min' | 'vlan_max'>) =>
  f.isolation === 'vlan' && f.vlan_min != null && f.vlan_max != null ? `${f.vlan_min}～${f.vlan_max}` : '—'
export const mtuText = (mtu: number) => (mtu ? String(mtu) : '继承网卡')

/** lan fabric 下就绪机器出现多个网段：routed 只在同一网段内组队，提示运营（规格 §3.2）。 */
export function fabricNetsWarning(f: Pick<FabricRow, 'kind' | 'cluster_nets'>): string {
  const nets = f.cluster_nets ?? []
  return f.kind === 'lan' && nets.length > 1 ? `机器分属 ${nets.length} 个网段（${nets.join('、')}），只有同一网段的机器能组成集群` : ''
}

/** 删除前的提示：有机器或活跃集群时后端会拒绝（fabric_in_use）。 */
export function fabricDeleteBlocker(f: Pick<FabricRow, 'machine_count' | 'active_clusters'>): string {
  if (f.active_clusters > 0) return `还有 ${f.active_clusters} 个运行中的集群`
  if (f.machine_count > 0) return `还有 ${f.machine_count} 台机器，请先在机器详情里移出`
  return ''
}

export const canEditFabrics = (role: Role | undefined) => !!role && OPS_ROLES.includes(role)

// ---- 机器集群网卡 ----

/** Agent 能力位（internal/agent/features）。 */
export const FEATURE_SHARED_SLOTS = 1 << 0
export const FEATURE_CLUSTER = 1 << 1
export function agentFeatureLabels(bits: number): string[] {
  const out: string[] = []
  if (bits & FEATURE_SHARED_SLOTS) out.push('共享实例')
  if (bits & FEATURE_CLUSTER) out.push('多机集群')
  return out
}
export const supportsCluster = (bits: number | undefined) => ((bits ?? 0) & FEATURE_CLUSTER) !== 0

/** 管理网卡：IPv4（CIDR）里含 lanIP 的网卡；找不到退回 net_info.lan_iface（与 clusterspec.MgmtIface 一致）。 */
export function mgmtIface(n: NetInfo | null | undefined, lanIP: string): string {
  if (!n) return ''
  if (lanIP) for (const f of n.ifaces ?? []) if ((f.ipv4 ?? []).some((c) => c.split('/')[0] === lanIP)) return f.name
  return n.lan_iface ?? ''
}

export const MLX5_DRIVER = 'mlx5_core'

export interface IfaceChoice { value: string; label: string; disabled: boolean }
/**
 * 集群网卡下拉：「自动」+ 探测到的网卡。lan：自动 = 管理网卡；cx7：自动 = 第一块已连接的 CX-7（mlx5_core）网卡，
 * 管理网卡与无线网卡不能选（ResolveIface 会拒绝）。当前配置的网卡不在探测结果里时仍保留一项（标「未探测到」）。
 */
export function ifaceChoices(n: NetInfo | null | undefined, kind: FabricKind | undefined, lanIP: string, current = ''): IfaceChoice[] {
  const mgmt = mgmtIface(n, lanIP)
  const out: IfaceChoice[] = [{ value: '', label: kind === 'cx7' ? '自动（第一块已连接的 CX-7 网卡）' : '自动（管理网卡）', disabled: false }]
  for (const f of n?.ifaces ?? []) {
    const notes: string[] = []
    if (f.name === mgmt) notes.push('管理网卡')
    if (f.driver === MLX5_DRIVER) notes.push('CX-7')
    if (f.wireless) notes.push('无线')
    if (!f.up) notes.push('未连接')
    out.push({ value: f.name, label: notes.length ? `${f.name}（${notes.join('，')}）` : f.name, disabled: kind === 'cx7' && (f.name === mgmt || f.wireless) })
  }
  if (current && !out.some((c) => c.value === current)) out.push({ value: current, label: `${current}（未探测到）`, disabled: false })
  return out
}

/** 速率：10000 → 10 Gb/s；0 / 负数（未连接或未知）→ —。 */
export function fmtSpeed(mbps: number): string {
  if (!mbps || mbps < 0) return '—'
  return mbps >= 1000 ? `${+(mbps / 1000).toFixed(1)} Gb/s` : `${mbps} Mb/s`
}

/** 网卡表的 RDMA 列：设备名 + 状态 + uverbs。 */
export function rdmaText(f: Pick<NetIface, 'rdma_device' | 'rdma_active' | 'uverbs' | 'link_layer'>): string {
  if (!f.rdma_device) return '—'
  const parts = [f.rdma_device, f.rdma_active ? 'ACTIVE' : '未激活']
  if (f.link_layer) parts.push(f.link_layer)
  if (f.uverbs) parts.push(f.uverbs)
  return parts.join(' · ')
}

/** 就绪判定文案：就绪（网段 / RDMA）或未就绪原因。 */
export function readinessText(m: Pick<MachineClusterNet, 'cluster_ready' | 'cluster_rdma' | 'cluster_net' | 'cluster_reason'>): string {
  if (!m.cluster_ready) return `未就绪：${m.cluster_reason || '原因未知'}`
  const net = m.cluster_net === 'l2' ? '二层直连 / VLAN' : m.cluster_net
  return `就绪${net ? `（${net}）` : ''}${m.cluster_rdma ? ' · RDMA 可用' : ''}`
}

/**
 * PUT /machines/{id} 的集群网络部分：只放有变化的键（fabric_id 键存在即改，null = 移出；cluster_iface "" = 自动）。
 * 空对象表示没有改动。
 */
export function machineNetPatch(m: Pick<MachineClusterNet, 'fabric_id' | 'cluster_iface'>, fabricId: number | null, iface: string): { fabric_id?: number | null; cluster_iface?: string } {
  const out: { fabric_id?: number | null; cluster_iface?: string } = {}
  if ((m.fabric_id ?? null) !== fabricId) out.fabric_id = fabricId
  if ((m.cluster_iface ?? '') !== iface) out.cluster_iface = iface
  return out
}

// ---- 集群 ----

export type ClusterStateFilter = '' | 'active' | 'released' | 'failed'
export interface ClusterFilter { state: ClusterStateFilter; user_id: string; fabric_id: number | ''; system: '' | 'true' | 'false' }
export const emptyClusterFilter = (): ClusterFilter => ({ state: 'active', user_id: '', fabric_id: '', system: '' })
/** GET /clusters 的查询串（空值省略）。 */
export function clusterListQuery(f: ClusterFilter, page: number, pageSize: number): Record<string, string | number | undefined> {
  const uid = f.user_id.replace(/\D/g, '')
  return { state: f.state || undefined, user_id: uid || undefined, fabric_id: f.fabric_id === '' ? undefined : f.fabric_id, system: f.system || undefined, page, page_size: pageSize }
}

/** 延长 / 重新下发：ops 与 super；延长只对活动的用户集群（系统集群不计时），重新下发对活动集群。 */
export const canExtendCluster = (role: Role | undefined, c: Pick<AdminClusterView, 'state' | 'system'>) => canEditFabrics(role) && c.state === 'active' && !c.system
export const canResyncCluster = (role: Role | undefined, c: Pick<AdminClusterView, 'state'>) => canEditFabrics(role) && c.state === 'active'
/** 强制卸载集群应用（ops）：成员部署不能单独强制卸载（409 app_cluster_managed），只能整组卸载；已卸载 / 卸载中不再显示。 */
export const canForceRemoveClusterApp = (role: Role | undefined, app: Pick<AdminClusterApp, 'state'> | null) =>
  canEditFabrics(role) && !!app && app.state !== 'removed' && app.state !== 'removing'
/** 成员换机：ops、活动的用户集群、成员实例在运行（与实例页 showMigrate 一致）。 */
export const canMigrateMember = (role: Role | undefined, c: Pick<AdminClusterView, 'state' | 'system'>, memberState: string) =>
  canEditFabrics(role) && c.state === 'active' && !c.system && memberState === 'running'

/** 成员网络状态（Agent 上报）：'' 未上报 / ok 正常 / pending 等待容器 / error: 原因。 */
export function netStatusText(s: string): string {
  if (!s) return '未上报'
  if (s === 'ok') return '正常'
  if (s === 'pending') return '等待容器运行'
  return s.startsWith('error:') ? `异常：${s.slice(6).trim()}` : s
}
export const netStatusBad = (s: string) => s.startsWith('error')

export const MIGRATE_MEMBER_HINT = '只能换到同一集群网络（且同一网段）的空闲机器；换机后集群需重新下发网络，集群应用需重新部署'
export const MEMBER_EXTEND_HINT = '该实例属于集群，请在集群页统一延长'
