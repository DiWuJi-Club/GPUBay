import {
  clusterStatusLabel, clusterStatusType, labelOf,
  type AppCatalogItem, type AppSpec, type ClusterAppStatus, type ClusterAvailMap, type ClusterNetwork, type ClusterRole, type ClusterStatus,
  type Deployment, type DeploymentHealth, type DeploymentState, type Duration, type FabricKind, type Instance, type InstanceState, type Product, type RenewOption,
} from '@spark/shared'

// 计划 13 控制台：多机集群（规格 §9、§10.1）。这里的类型按 internal/api/clusters.go 的真实 JSON 定义（控制台私有，不改 shared）。

/** 服务层只接受的集群台数（rental.ClusterSizes）。 */
export const CLUSTER_SIZES = [2, 4] as const
export type ClusterSize = (typeof CLUSTER_SIZES)[number]
/** clusterspec.MasterPort：torchrun 的 rendezvous 端口。 */
export const MASTER_PORT = 29500
/** Ray head 的 GCS 端口（vllm-cluster 的 app.sh 同值）。 */
export const RAY_PORT = 6379

/** 实例视图的 cluster 段（/instances*、下单响应）；非成员为 null。 */
export interface InstanceClusterRef { id: string; size: number; rank: number; status: ClusterStatus }
export type ClusterInstance = Instance & { cluster?: InstanceClusterRef | null }

export type ConsoleNetStatus = 'ok' | 'pending' | 'error'

/** GET /clusters/{id} 的成员（活动集群只列在位成员，按 rank）。 */
export interface ConsoleClusterMember {
  rank: number
  instance_id: string
  state: InstanceState
  suspended: boolean
  ip: string
  machine_label: string
  /** 后端归一化（终审 B-2）：ok / pending（未上报或容器未运行）/ error；Agent 原始错误只在后台显示 */
  net_status: ConsoleNetStatus
  /** error 时的固定说明（「集群网络异常，平台已告警处理」）；其余为 '' */
  net_reason: string
  expires_at: string | null
}

export interface ConsoleClusterAppMember {
  rank: number
  role: ClusterRole
  instance_id: string
  deployment_id: string
  state: DeploymentState
  health: DeploymentHealth
  phase: string
  progress: number
  message: string
  /** false = 换机前的旧成员（需要重新部署） */
  current: boolean
}

/** 集群应用（GET /clusters/{id}/apps、ClusterView.app）。 */
export interface ConsoleClusterApp {
  id: string
  state: 'active' | 'removed'
  app_id: number
  app_slug: string
  app_name: string
  app_icon: string
  version_id: number
  version: string
  latest_version_id: number | null
  latest_version: string
  update_available: boolean
  status: ClusterAppStatus
  health: DeploymentHealth
  phase: string
  progress: number
  /** head 的「打开」地址；端口未登记 / head 已换机为 null */
  url: string | null
  env: Record<string, string>
  secret_env: Record<string, boolean>
  spec: AppSpec
  members: ConsoleClusterAppMember[]
  created_at: string
  updated_at: string
}

/** 控制台集群视图（GET /clusters、/clusters/{id}；不含 machine_id / fabric_id 等后台字段）。 */
export interface ConsoleClusterView {
  id: string
  size: number
  status: ClusterStatus
  network: ClusterNetwork
  fabric_kind: FabricKind
  subnet: string
  head_ip: string
  batch_id: string | null
  /** 在位成员里最早的到期（对齐后全员相同） */
  expires_at: string | null
  expires_mismatch: boolean
  auto_renew: boolean
  auto_renew_duration: Duration | ''
  auto_renew_failed_reason: string
  renewable_until: string | null
  renew_options: RenewOption[]
  members: ConsoleClusterMember[]
  app: ConsoleClusterApp | null
  ready_at: string | null
  ended_at: string | null
  created_at: string
}

/** 部署视图的集群字段（T7：成员上的集群部署带 cluster_role / cluster_deployment_id）。 */
export type ClusterAwareDeployment = Deployment & { cluster_role?: ClusterRole; cluster_deployment_id?: string }
/** 目录版本的 spec.cluster（不支持集群时省略）。 */
export interface AppClusterSupport { mode: 'optional' | 'required'; min_size: number; max_size: number }
type CatalogWithCluster = AppCatalogItem & { version: AppCatalogItem['version'] & { cluster?: AppClusterSupport } }

/** GET /products 的条目带 clusters（计划 13）。 */
export type ProductWithClusters = Product & { clusters?: ClusterAvailMap }
/** 下单响应（POST /bookings）：只有集群批次带 cluster。 */
export interface ClusterBookingExtra { cluster?: ConsoleClusterView }

// ---- 价格 ----

/** 集群总价（分）= 单价 × 台数 × 数量。 */
export function clusterTotalPrice(unit: number, size: number, quantity: number): number {
  return unit * size * quantity
}

// ---- 状态 ----

export function clusterStatusBadge(v: Pick<ConsoleClusterView, 'status'>): { text: string; type: string } {
  return { text: labelOf(clusterStatusLabel, v.status), type: labelOf(clusterStatusType, v.status) }
}

export const isClusterActive = (v: Pick<ConsoleClusterView, 'status'>) => v.status === 'forming' || v.status === 'ready' || v.status === 'degraded'

export interface ClusterNotice { key: string; type: 'info' | 'warning' | 'error'; text: string }

/** 集群详情顶部的提示：组建中 / 降级 / RDMA 退回以太网 / 成员到期不一致 / 开通失败 / 已释放。 */
export function clusterNotices(v: ConsoleClusterView): ClusterNotice[] {
  if (v.status === 'failed') return [{ key: 'failed', type: 'error', text: '集群开通失败，费用已全额退回余额' }]
  if (v.status === 'released') return [{ key: 'released', type: 'info', text: '集群已释放' }]
  const out: ClusterNotice[] = []
  if (v.status === 'forming') out.push({ key: 'forming', type: 'info', text: '集群组建中：全部成员开通、集群网络连通后变为「就绪」；30 分钟内未就绪会整组中止并全额退款' })
  if (v.status === 'degraded') out.push({ key: 'degraded', type: 'warning', text: '集群降级：有成员停机、计时暂停、更换机器中或集群网络异常，分布式任务可能无法运行；请查看下方成员表' })
  if (v.network === 'rdma_degraded') out.push({ key: 'rdma_degraded', type: 'warning', text: 'RDMA 不可用，已退回以太网通信（NCCL_IB_DISABLE=1），性能会下降' })
  if (v.expires_mismatch) out.push({ key: 'expires_mismatch', type: 'warning', text: '成员到期时间不一致（有成员曾因机器维护暂停计时），续费按各成员同样加时，到期按各自时间' })
  return out
}

/** 成员网络状态（后端已归一为 ok / pending / error + 固定原因）；未知值按异常显示，但绝不展示原始文本。 */
export function netStatusText(s: ConsoleNetStatus, reason = ''): { text: string; type: string } {
  if (s === 'ok') return { text: '正常', type: 'success' }
  if (s === 'pending') return { text: '等待中', type: 'primary' }
  return { text: reason ? `异常：${reason}` : '异常', type: 'danger' }
}

export interface ClusterActions { active: boolean; renew: boolean; autoRenew: boolean; release: boolean; releaseReason: string | null }

/** 集群级操作按钮：只对未结束的集群；有成员开通中时不能释放（后端 invalid_state）。 */
export function clusterActions(v: ConsoleClusterView): ClusterActions {
  if (!isClusterActive(v)) return { active: false, renew: false, autoRenew: false, release: false, releaseReason: null }
  const creating = v.members.some((m) => m.state === 'creating')
  return {
    active: true,
    renew: v.renew_options.some((o) => o.quantity_max > 0),
    autoRenew: true,
    release: !creating,
    releaseReason: creating ? '有成员正在开通，开通完成后才能释放' : null,
  }
}

/** 某档位整组最多能续的单位数（renew_options 已按全员取最小）；没有该档位为 0。 */
export function clusterRenewMax(v: Pick<ConsoleClusterView, 'renew_options'>, d: Duration): number {
  return v.renew_options.find((o) => o.duration === d)?.quantity_max ?? 0
}

// ---- 连接说明 ----

export interface ConnectSnippets { env: string; torchrun: string; ray: string }

/**
 * 集群详情「连接信息」的示例文本。/.spark/cluster.env 每行都是 `export KEY='value'`，source 一次即导出给子进程；
 * `ssh host cmd` 这类非登录会话不读 /etc/profile.d，所以每个示例都先 `. /.spark/cluster.env`。
 */
export function connectSnippets(v: Pick<ConsoleClusterView, 'size' | 'head_ip'>): ConnectSnippets {
  const env = [
    '# 登录 shell 已自动加载；ssh 远程执行命令、脚本、cron 等非登录会话请先 source（文件每行都是 export，source 一次即可）',
    '. /.spark/cluster.env',
    'echo "rank=$SPARK_CLUSTER_RANK/$SPARK_CLUSTER_SIZE ip=$SPARK_CLUSTER_IP head=$SPARK_CLUSTER_HEAD_IP"',
    '# 主要变量：SPARK_CLUSTER_ID / SIZE / RANK / IP / HEAD_IP / IPS / IFNAME(spc0) / NETWORK',
    '#          MASTER_ADDR / MASTER_PORT(29500) / NNODES / NODE_RANK / VLLM_HOST_IP',
    '#          NCCL_SOCKET_IFNAME / GLOO_SOCKET_IFNAME / TP_SOCKET_IFNAME（= spc0）、NCCL_IB_*（RDMA 时）',
    '# 完整拓扑（全部成员的 rank 与 IP）：cat /etc/spark-cluster.json',
  ].join('\n')
  const torchrun = [
    `# 在全部 ${v.size} 台上各执行一次（head = rank 0，地址 ${v.head_ip || '$MASTER_ADDR'}）`,
    '. /.spark/cluster.env',
    `torchrun --nnodes $NNODES --node-rank $NODE_RANK --master-addr $MASTER_ADDR --master-port ${MASTER_PORT} \\`,
    '  --nproc-per-node 1 train.py',
  ].join('\n')
  const ray = [
    '# head（rank 0）：',
    '. /.spark/cluster.env',
    `ray start --head --node-ip-address=$SPARK_CLUSTER_IP --port=${RAY_PORT}`,
    '# 其余成员（worker）：',
    '. /.spark/cluster.env',
    `ray start --address=$SPARK_CLUSTER_HEAD_IP:${RAY_PORT} --node-ip-address=$SPARK_CLUSTER_IP`,
    '# 运行 driver 前显式指定地址（容器有 eth0 与 spc0 两块网卡，避免自动选错）：',
    `export RAY_ADDRESS=$SPARK_CLUSTER_HEAD_IP:${RAY_PORT}`,
    'python3 my_job.py',
  ].join('\n')
  return { env, torchrun, ray }
}

// ---- 实例列表 ----

export type InstanceListEntry<T extends ClusterInstance = ClusterInstance> =
  | { kind: 'instance'; instance: T }
  | { kind: 'cluster'; cluster: { id: string; size: number; status: ClusterStatus }; members: T[] }

/** 同一集群的成员折叠为一项（成员按 rank），位置取第一个出现的成员；其余实例原样。 */
export function groupInstancesByCluster<T extends ClusterInstance>(instances: T[]): InstanceListEntry<T>[] {
  const out: InstanceListEntry<T>[] = []
  const byId = new Map<string, Extract<InstanceListEntry<T>, { kind: 'cluster' }>>()
  for (const i of instances) {
    const c = i.cluster
    if (!c) {
      out.push({ kind: 'instance', instance: i })
      continue
    }
    let g = byId.get(c.id)
    if (!g) {
      g = { kind: 'cluster', cluster: { id: c.id, size: c.size, status: c.status }, members: [] }
      byId.set(c.id, g)
      out.push(g)
    }
    g.members.push(i)
  }
  for (const g of byId.values()) g.members.sort((a, b) => a.cluster!.rank - b.cluster!.rank)
  return out
}

// ---- 应用目录 ----

const clusterOf = (a: AppCatalogItem) => (a as CatalogWithCluster).version.cluster
/** 只能在集群上部署的应用（如 vllm-cluster）。 */
export const isClusterRequired = (a: AppCatalogItem) => clusterOf(a)?.mode === 'required'
/** 单实例「添加应用」与下单预装可选的应用：排除 required（后端 app_cluster_required）。 */
export const singleInstanceApps = (apps: AppCatalogItem[]) => apps.filter((a) => !isClusterRequired(a))
/** 集群页「部署应用」可选：支持集群且台数在 [min, max] 内。 */
export function clusterAppsFor(apps: AppCatalogItem[], size: number): AppCatalogItem[] {
  return apps.filter((a) => {
    const c = clusterOf(a)
    return !!c && size >= c.min_size && size <= c.max_size
  })
}
export function clusterSupportOf(a: AppCatalogItem): AppClusterSupport | null {
  return clusterOf(a) ?? null
}

/** 张量并行默认 = 集群台数（vllm-cluster 的 TENSOR_PARALLEL_SIZE 留空也按台数）。 */
export const defaultTP = (size: number) => String(size)
export const TP_ENV = 'TENSOR_PARALLEL_SIZE'

// ---- 下单 ----

export interface ClusterRentOption { size: ClusterSize; available: number; rdma: boolean; disabled: boolean }

/** 下单对话框「租用方式」里的集群选项：按 /products 的 clusters（没有该字段 = 旧后端，不禁用，以实时查询为准）。 */
export function clusterRentOptions(m: ClusterAvailMap | undefined): ClusterRentOption[] {
  return CLUSTER_SIZES.map((size) => {
    const a = m?.[String(size) as '2' | '4']
    return { size, available: a?.available ?? 0, rdma: a?.rdma ?? false, disabled: m != null && (a?.available ?? 0) === 0 }
  })
}

// ---- 成员守卫 ----

export const memberGuardText = (c: InstanceClusterRef) => `属于集群 ${c.id}（rank ${c.rank} / ${c.size}）`
export const MEMBER_GUARD_REASON = '集群成员请在集群页统一续费、设置自动续费或释放'
/** 成员上的集群部署：单实例操作一律 409 app_cluster_managed，只读显示。 */
export const isClusterManaged = (d: { cluster_deployment_id?: string | null }) => !!d.cluster_deployment_id

// ---- 集群应用 ----

export function clusterAppActions(a: Pick<ConsoleClusterApp, 'status'>): { redeploy: boolean; stop: boolean; start: boolean; remove: boolean } {
  const s = a.status
  return {
    redeploy: s === 'running' || s === 'stopped' || s === 'failed' || s === 'stale',
    stop: s === 'installing' || s === 'running',
    start: s === 'stopped',
    remove: s !== 'removing' && s !== 'removed',
  }
}

/** 集群应用需要较快刷新：部署中 / 卸载中。 */
export const clusterAppBusy = (a: Pick<ConsoleClusterApp, 'status'> | null) => !!a && (a.status === 'installing' || a.status === 'removing')

export interface ClusterAppCallInfo { headInstance: string; localBase: string; publicBase: string | null; model: string; keyed: boolean; curl: string }

/**
 * vllm-cluster 的调用方式：vLLM 只在 head 实例（rank 0）上监听 127.0.0.1:端口；外部经 head 的端口网址（私有需登录控制台）。
 * 模型名 = SERVED_MODEL_NAME（默认 model）。不是 vLLM 集群应用返回 null。
 */
export function clusterAppCallInfo(a: ConsoleClusterApp): ClusterAppCallInfo | null {
  if (a.app_slug !== 'vllm-cluster' && a.app_slug !== 'vllm-node') return null
  const spec = a.spec.env ?? []
  const model = a.env.SERVED_MODEL_NAME || spec.find((e) => e.name === 'SERVED_MODEL_NAME')?.default || a.env.MODEL_ID || ''
  if (!model) return null
  const port = a.spec.ports[0]?.port ?? 8000
  const head = a.members.find((m) => m.role === 'head' && m.current) ?? a.members.find((m) => m.role === 'head')
  const localBase = `http://127.0.0.1:${port}/v1`
  let publicBase: string | null = null
  if (a.url) {
    const path = a.spec.ports[0]?.path ?? ''
    const root = path && a.url.endsWith(path) ? a.url.slice(0, -path.length) : a.url
    publicBase = `${root.replace(/\/+$/, '')}/v1`
  }
  const keyed = !!a.secret_env.API_KEY
  const curl = [
    `curl ${localBase}/chat/completions \\`,
    `  -H 'Content-Type: application/json' \\`,
    ...(keyed ? [`  -H 'Authorization: Bearer <接口密钥>' \\`] : []),
    `  -d '{"model": "${model}", "messages": [{"role": "user", "content": "你好"}]}'`,
  ].join('\n')
  return { headInstance: head?.instance_id ?? '', localBase, publicBase, model, keyed, curl }
}
