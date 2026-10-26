import type { AppCatalogItem, Instance } from '@spark/shared'
import { describe, expect, it } from 'vitest'
import {
  clusterActions, clusterAppActions, clusterAppCallInfo, clusterAppsFor, clusterNotices, clusterRentOptions, clusterStatusBadge, clusterTotalPrice,
  connectSnippets, defaultTP, groupInstancesByCluster, isClusterManaged, isClusterRequired, memberGuardText, netStatusText, singleInstanceApps,
  type ClusterInstance, type ConsoleClusterApp, type ConsoleClusterView,
} from '../cluster'

const view = (over: Partial<ConsoleClusterView> = {}): ConsoleClusterView => ({
  id: 'clu-abc123defg', size: 2, status: 'ready', network: 'ethernet', fabric_kind: 'lan', subnet: '10.210.7.0/24', head_ip: '10.210.7.11',
  batch_id: 'bat-1', expires_at: '2026-10-27T10:00:00Z', expires_mismatch: false, auto_renew: false, auto_renew_duration: '1d',
  auto_renew_failed_reason: '', renewable_until: null, renew_options: [{ duration: '1d', quantity_max: 90 }],
  members: [
    { rank: 0, instance_id: 'ins-aaaaaaaa', state: 'running', suspended: false, ip: '10.210.7.11', machine_label: 'A-01', net_status: 'ok', expires_at: '2026-10-27T10:00:00Z' },
    { rank: 1, instance_id: 'ins-bbbbbbbb', state: 'running', suspended: false, ip: '10.210.7.12', machine_label: 'A-02', net_status: 'ok', expires_at: '2026-10-27T10:00:00Z' },
  ],
  app: null, ready_at: '2026-10-26T10:00:00Z', ended_at: null, created_at: '2026-10-26T09:55:00Z', ...over,
})

const inst = (id: string, over: Partial<ClusterInstance> = {}): ClusterInstance =>
  ({ id, state: 'running', batch_id: null, cluster: null, ...over }) as unknown as ClusterInstance

const app = (slug: string, cluster?: { mode: 'optional' | 'required'; min_size: number; max_size: number }): AppCatalogItem =>
  ({ id: slug.length, slug, name: slug, version: { id: 1, version: '1.0.0', env: [], ports: [], links: [], timeout_minutes: 30, ...(cluster ? { cluster } : {}) }, images: [] }) as unknown as AppCatalogItem

const capp = (over: Partial<ConsoleClusterApp> = {}): ConsoleClusterApp => ({
  id: 'acd-1', state: 'active', app_id: 9, app_slug: 'vllm-cluster', app_name: 'vLLM 集群', app_icon: '', version_id: 3, version: '1.0.0',
  latest_version_id: 3, latest_version: '1.0.0', update_available: false, status: 'running', health: 'ok', phase: '', progress: 100,
  url: 'https://ins-aaaaaaaa-8000.example.com/docs', env: { MODEL_ID: 'Qwen/Qwen3-32B' }, secret_env: {},
  spec: { ports: [{ port: 8000, path: '/docs' }], env: [{ name: 'SERVED_MODEL_NAME', type: 'string', default: 'model' }, { name: 'API_KEY', type: 'secret' }] },
  members: [
    { rank: 0, role: 'head', instance_id: 'ins-aaaaaaaa', deployment_id: 'dep-1', state: 'running', health: 'ok', phase: '', progress: 100, message: '', current: true },
    { rank: 1, role: 'worker', instance_id: 'ins-bbbbbbbb', deployment_id: 'dep-2', state: 'running', health: 'ok', phase: '', progress: 100, message: '', current: true },
  ],
  created_at: '2026-10-26T10:00:00Z', updated_at: '2026-10-26T10:00:00Z', ...over,
})

describe('价格与 TP', () => {
  it('集群总价 = 单价 × 台数 × 数量', () => {
    expect(clusterTotalPrice(10000, 2, 1)).toBe(20000)
    expect(clusterTotalPrice(10000, 4, 3)).toBe(120000)
  })
  it('TP 默认 = 台数', () => {
    expect(defaultTP(2)).toBe('2')
    expect(defaultTP(4)).toBe('4')
  })
})

describe('状态徽标与提示', () => {
  it('徽标按派生状态', () => {
    expect(clusterStatusBadge(view())).toEqual({ text: '就绪', type: 'success' })
    expect(clusterStatusBadge(view({ status: 'forming' }))).toEqual({ text: '组建中', type: 'primary' })
    expect(clusterStatusBadge(view({ status: 'degraded' }))).toEqual({ text: '降级', type: 'warning' })
    expect(clusterStatusBadge({ status: 'failed' })).toEqual({ text: '开通失败', type: 'danger' })
  })
  it('降级、RDMA 退回、到期不一致、组建中、开通失败各有提示', () => {
    expect(clusterNotices(view())).toEqual([])
    const kinds = (v: ConsoleClusterView) => clusterNotices(v).map((n) => n.key)
    expect(kinds(view({ status: 'degraded' }))).toContain('degraded')
    expect(kinds(view({ network: 'rdma_degraded' }))).toContain('rdma_degraded')
    expect(kinds(view({ expires_mismatch: true }))).toContain('expires_mismatch')
    expect(kinds(view({ status: 'forming', ready_at: null }))).toContain('forming')
    expect(kinds(view({ status: 'failed' }))).toEqual(['failed'])
    expect(clusterNotices(view({ expires_mismatch: true })).find((n) => n.key === 'expires_mismatch')!.text).toContain('成员到期时间不一致')
  })
  it('成员网络状态文案', () => {
    expect(netStatusText('ok')).toEqual({ text: '正常', type: 'success' })
    expect(netStatusText('pending').text).toBe('等待中')
    expect(netStatusText('')).toEqual({ text: '未上报', type: 'info' })
    expect(netStatusText('error: no route')).toEqual({ text: '异常：no route', type: 'danger' })
  })
})

describe('集群操作按钮状态', () => {
  it('就绪：续费 / 自动续费 / 释放都可用', () => {
    expect(clusterActions(view())).toEqual({ active: true, renew: true, autoRenew: true, release: true, releaseReason: null })
  })
  it('有成员开通中：释放置灰并说明', () => {
    const a = clusterActions(view({ status: 'forming', members: [{ ...view().members[0], state: 'creating' }, view().members[1]] }))
    expect(a.release).toBe(false)
    expect(a.releaseReason).toContain('开通')
  })
  it('续费档位全为 0 时续费不可用', () => {
    expect(clusterActions(view({ renew_options: [{ duration: '1d', quantity_max: 0 }] })).renew).toBe(false)
  })
  it('已结束：全部不可用', () => {
    expect(clusterActions(view({ status: 'released', renew_options: [] }))).toEqual({ active: false, renew: false, autoRenew: false, release: false, releaseReason: null })
  })
})

describe('连接说明 connectSnippets', () => {
  const s = connectSnippets(view({ size: 4 }))
  it('环境变量：先 source cluster.env（每行 export），列出关键变量', () => {
    expect(s.env).toContain('. /.spark/cluster.env')
    for (const k of ['SPARK_CLUSTER_IP', 'SPARK_CLUSTER_HEAD_IP', 'SPARK_CLUSTER_RANK', 'MASTER_ADDR', 'NODE_RANK', 'NNODES', 'NCCL_SOCKET_IFNAME', '/etc/spark-cluster.json']) expect(s.env).toContain(k)
  })
  it('torchrun 用 cluster.env 的变量与 29500 端口', () => {
    expect(s.torchrun).toContain('. /.spark/cluster.env')
    expect(s.torchrun).toContain('torchrun --nnodes $NNODES --node-rank $NODE_RANK --master-addr $MASTER_ADDR --master-port 29500')
  })
  it('Ray：head / worker 显式指定集群 IP，driver 前 export RAY_ADDRESS', () => {
    expect(s.ray).toContain('ray start --head --node-ip-address=$SPARK_CLUSTER_IP --port=6379')
    expect(s.ray).toContain('ray start --address=$SPARK_CLUSTER_HEAD_IP:6379 --node-ip-address=$SPARK_CLUSTER_IP')
    expect(s.ray).toContain('export RAY_ADDRESS=$SPARK_CLUSTER_HEAD_IP:6379')
  })
})

describe('实例列表按集群分组', () => {
  it('同一集群的成员折叠为一项（按 rank），保持首个成员出现的位置；单机实例原样', () => {
    const ref = (rank: number) => ({ id: 'clu-1', size: 2, rank, status: 'ready' as const })
    const list = [inst('ins-1'), inst('ins-c1', { cluster: ref(1) }), inst('ins-2'), inst('ins-c0', { cluster: ref(0) }), inst('ins-d0', { cluster: { id: 'clu-2', size: 2, rank: 0, status: 'forming' } })]
    const g = groupInstancesByCluster(list)
    expect(g.map((x) => (x.kind === 'instance' ? x.instance.id : `${x.cluster.id}:${x.members.map((m) => m.id).join(',')}`)))
      .toEqual(['ins-1', 'clu-1:ins-c0,ins-c1', 'ins-2', 'clu-2:ins-d0'])
    const c = g[1]
    expect(c.kind === 'cluster' && c.cluster).toEqual({ id: 'clu-1', size: 2, status: 'ready' })
  })
  it('没有 cluster 字段（旧数据）的实例按单机处理', () => {
    expect(groupInstancesByCluster([{ id: 'ins-x' } as unknown as Instance]).map((x) => x.kind)).toEqual(['instance'])
  })
})

describe('应用目录：单实例与集群', () => {
  const apps = [app('vllm'), app('vllm-node', { mode: 'optional', min_size: 2, max_size: 4 }), app('vllm-cluster', { mode: 'required', min_size: 2, max_size: 4 }), app('big', { mode: 'required', min_size: 4, max_size: 8 })]
  it('单实例「添加应用」与下单预装排除 required', () => {
    expect(isClusterRequired(apps[2])).toBe(true)
    expect(isClusterRequired(apps[1])).toBe(false)
    expect(singleInstanceApps(apps).map((a) => a.slug)).toEqual(['vllm', 'vllm-node'])
  })
  it('集群页只列支持集群且台数在范围内的应用', () => {
    expect(clusterAppsFor(apps, 2).map((a) => a.slug)).toEqual(['vllm-node', 'vllm-cluster'])
    expect(clusterAppsFor(apps, 4).map((a) => a.slug)).toEqual(['vllm-node', 'vllm-cluster', 'big'])
  })
})

describe('下单对话框的租用方式', () => {
  it('按 /products 的 clusters 给出可选项；没有该字段时不禁用', () => {
    expect(clusterRentOptions({ '2': { available: 1, rdma: true }, '4': { available: 0, rdma: false } }))
      .toEqual([{ size: 2, available: 1, rdma: true, disabled: false }, { size: 4, available: 0, rdma: false, disabled: true }])
    expect(clusterRentOptions(undefined).every((o) => !o.disabled)).toBe(true)
  })
})

describe('成员守卫与集群部署', () => {
  it('成员横幅文案带集群 ID 与 rank / N', () => {
    expect(memberGuardText({ id: 'clu-1', size: 4, rank: 2, status: 'ready' })).toBe('属于集群 clu-1（rank 2 / 4）')
  })
  it('成员上的集群部署为只读', () => {
    expect(isClusterManaged({ cluster_deployment_id: 'acd-1' })).toBe(true)
    expect(isClusterManaged({})).toBe(false)
  })
})

describe('集群应用', () => {
  it('操作按聚合状态', () => {
    expect(clusterAppActions(capp())).toEqual({ redeploy: true, stop: true, start: false, remove: true })
    expect(clusterAppActions(capp({ status: 'stopped' }))).toEqual({ redeploy: true, stop: false, start: true, remove: true })
    expect(clusterAppActions(capp({ status: 'stale' }))).toMatchObject({ redeploy: true, start: false })
    expect(clusterAppActions(capp({ status: 'installing' }))).toMatchObject({ redeploy: false, stop: true, remove: true })
    expect(clusterAppActions(capp({ status: 'removing' }))).toEqual({ redeploy: false, stop: false, start: false, remove: false })
  })
  it('vLLM 集群调用方式：在 head 实例内 127.0.0.1:8000，模型名 = SERVED_MODEL_NAME（默认 model）', () => {
    const c = clusterAppCallInfo(capp())!
    expect(c.model).toBe('model')
    expect(c.localBase).toBe('http://127.0.0.1:8000/v1')
    expect(c.headInstance).toBe('ins-aaaaaaaa')
    expect(c.publicBase).toBe('https://ins-aaaaaaaa-8000.example.com/v1')
    expect(c.keyed).toBe(false)
    expect(c.curl).toContain('"model": "model"')
    const k = clusterAppCallInfo(capp({ env: { SERVED_MODEL_NAME: 'qwen' }, secret_env: { API_KEY: true }, url: null }))!
    expect(k.model).toBe('qwen')
    expect(k.publicBase).toBeNull()
    expect(k.curl).toContain('Authorization: Bearer')
    expect(clusterAppCallInfo(capp({ app_slug: 'other' }))).toBeNull()
  })
})
