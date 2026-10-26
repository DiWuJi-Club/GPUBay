// 计划 13：多机集群（规格 §5、§6、§10）。用户集群与 Token 商城节点组共用「集群网络（fabric）+ 实例集群 + 集群部署」。
import type { DeploymentHealth, DeploymentState } from './apps'
import type { Duration } from './catalog'
import type { InstanceState, RenewOption } from './instance'

/** 派生状态：组建中 / 就绪 / 降级；表里的终态 released / failed 原样。 */
export type ClusterStatus = 'forming' | 'ready' | 'degraded' | 'released' | 'failed'
/** instance_clusters.state */
export type ClusterState = 'active' | 'released' | 'failed'
/** 实际网络：RDMA / 普通以太网 / fabric 要 RDMA 但成员探测不可用（已退回以太网）。 */
export type ClusterNetwork = 'rdma' | 'ethernet' | 'rdma_degraded'
export type ClusterMode = 'routed' | 'macvlan'
export type FabricKind = 'lan' | 'cx7'
export type FabricIsolation = 'routed' | 'direct' | 'vlan'
/** 集群部署里的角色：rank0 = head，其余 = worker。 */
export type ClusterRole = 'head' | 'worker'
/** 集群应用的聚合状态（规格 §6.4）。 */
export type ClusterAppStatus = 'installing' | 'running' | 'failed' | 'stopped' | 'stale' | 'removing' | 'removed'
/** 成员集群网络状态（Agent 上报）：'' 非集群 / ok / pending（容器未运行）/ `error: <原因>`。 */
export type ClusterNetStatus = string

/** 集群成员（活动成员，按 rank）。 */
export interface ClusterMember {
  rank: number
  instance_id: string
  state: InstanceState
  ip: string
  machine_label: string
  net_status: ClusterNetStatus
  suspended?: boolean
  machine_id?: number
  expires_at?: string | null
}

/** 集群视图（控制台 GET /clusters/{id}；后台在此基础上扩展为 AdminCluster）。 */
export interface ClusterView {
  id: string
  size: number
  status: ClusterStatus
  network: ClusterNetwork
  fabric_name: string
  fabric_kind: FabricKind
  subnet: string
  head_ip: string
  members: ClusterMember[]
  created_at: string
  // 集群行的其余字段（rental.Cluster；控制台视图可能不带）
  state?: ClusterState
  user_id?: number
  system?: boolean
  fabric_id?: number
  mode?: ClusterMode
  vlan?: number | null
  rdma?: boolean
  mtu?: number
  subnet_index?: number
  version?: number
  batch_id?: string | null
  ready_at?: string | null
  updated_at?: string
  ended_at?: string | null
  // 控制台：到期对齐后的到期时间与集群级续费设置
  expires_at?: string | null
  auto_renew?: boolean
  auto_renew_duration?: Duration
  renew_options?: RenewOption[]
  /** 当前集群应用；没有为 null */
  app?: ClusterApp | null
}

/** 某个台数的可用性：可组成的集群数、是否能组 RDMA 集群。 */
export interface ClusterAvail { available: number; rdma: boolean }
/** GET /products 每个商品的 clusters 字段：键为台数（"2" / "4"）。 */
export type ClusterAvailMap = Partial<Record<'2' | '4', ClusterAvail>>

/** 集群应用的一个成员部署。 */
export interface ClusterAppMember {
  rank: number
  role: ClusterRole
  instance_id: string
  state: DeploymentState
  health: DeploymentHealth
  progress: number
  /** 实例仍是集群当前成员（换机后旧成员为 false） */
  current: boolean
  deployment_id?: string
}

/** 集群应用（GET /clusters/{id}/apps）。 */
export interface ClusterApp {
  id: string
  app_id: number
  app_slug: string
  app_name: string
  version: string
  status: ClusterAppStatus
  health: DeploymentHealth
  phase: string
  progress: number
  members: ClusterAppMember[]
  /** head 的访问地址（rank0 实例的私有端口 URL） */
  head_url?: string | null
  created_at?: string
  updated_at?: string
}

/** 集群网络（fabric）。 */
export interface Fabric {
  id: number
  name: string
  kind: FabricKind
  isolation: FabricIsolation
  rdma: boolean
  /** 0 = 继承网卡 */
  mtu: number
  vlan_min: number | null
  vlan_max: number | null
  description: string
  created_at: string
  updated_at: string
}
/** GET /admin/api/v1/fabrics 的条目。cluster_nets：已就绪成员机器的集群网段（去重升序；lan 出现多个网段时后台标黄）。 */
export interface FabricRow extends Fabric { machine_count: number; active_clusters: number; cluster_nets: string[] }
/** POST / PUT /fabrics */
export type FabricInput = Pick<Fabric, 'name' | 'kind' | 'isolation' | 'rdma' | 'mtu' | 'vlan_min' | 'vlan_max' | 'description'>

/** Agent 网卡探测的一块网卡（machines.net_info）。 */
export interface NetIface {
  name: string
  mac: string
  up: boolean
  speed_mbps: number
  mtu: number
  /** 「地址/前缀长度」 */
  ipv4: string[]
  driver: string
  pci: string
  wireless: boolean
  rdma_device?: string
  rdma_active?: boolean
  uverbs?: string
  link_layer?: string
}
export interface NetInfo {
  /** 仅供参考：收到 AgentConfig 之前为空；平台按 machines.lan_ip 认管理网卡 */
  lan_iface: string
  ifaces: NetIface[]
}

/** 后台机器视图（GET /admin/api/v1/machines/{id}）里的集群网络字段（plan 13）。 */
export interface MachineClusterNet {
  fabric_id: number | null
  fabric_name?: string
  cluster_iface: string
  agent_features: number
  net_info: NetInfo | null
  net_info_at: string | null
  cluster_ready: boolean
  cluster_rdma: boolean
  /** 集群网段：lan = 集群网卡 IPv4 前缀（如 192.168.1.0/24），cx7 = 'l2'，未就绪 = '' */
  cluster_net: string
  /** 未就绪原因（ResolveIface 的错误文案） */
  cluster_reason?: string
}

/** 后台集群列表 / 详情。 */
export interface AdminCluster extends ClusterView {
  state: ClusterState
  user_id: number
  system: boolean
  fabric_id: number
  mode: ClusterMode
  version: number
  /** 用户手机号；平台节点组为空 */
  user_phone?: string
  events?: { at: string; instance_id: string; detail: string }[]
}

/** Token 商城节点组成员（GET /admin/api/v1/llm/nodes* 的 members；后端 llm.NodeMemberRow）。 */
export interface LLMNodeMember {
  rank: number
  machine_id: number
  machine_name: string
  instance_id: string
  instance_state: InstanceState
  suspended: boolean
  deployment: { state: DeploymentState; health: DeploymentHealth; phase?: string; progress: number } | null
}
