import { expect, type APIRequestContext, type Page } from '@playwright/test'
import { uniqueGroupName } from './booking'
import { userApi } from './console'
import { hooks, type MachineClusterOpts } from './hooks'

// 多机集群（计划 13）用例的公共部分。
// - 机器经 /hooks/machines {fabric_id, net, old_agent} 造（fake Agent 上报网卡预设，返回时就绪判定已算好），
//   fabric 经 /hooks/fabrics 建。整轮共享一个机器池：其它用例留下的 fabric 机器也会被「不限机组」的集群下单选中，
//   所以集群都下在用例自己的可见机组里（clusterGroup），「可开 X 个」按机组查询才是确定的。
// - 开通：控制台 POST /bookings {cluster: true, count: N} → hooks.agent()（fake Agent 完成带集群 Spec 的 Create）→
//   hooks.sweep()（SweepClusters 首次就绪 + 到期对齐）→ 集群 ready。不推进假时钟。
// - 集群应用（vllm-cluster）由 hooks.appsTick 逐轮推进（每轮推进 5 秒）；fake 服务是假容器里 setsid 出去的 sleep，
//   所以造了集群的用例都 ownCluster 登记，afterEach 的 cleanupClusters 卸载集群应用、释放集群并让 fake Agent 洗完机。

export type ClusterMemberJSON = {
  rank: number
  instance_id: string
  state: string
  suspended: boolean
  ip: string
  machine_label: string
  net_status: string
  expires_at: string | null
}

export type ClusterAppMemberJSON = { rank: number; role: 'head' | 'worker'; instance_id: string; deployment_id: string; state: string; current: boolean }

export type ClusterAppJSON = {
  id: string
  state: 'active' | 'removed'
  app_id: number
  app_slug: string
  version_id: number
  status: string
  url: string | null
  env: Record<string, string>
  members: ClusterAppMemberJSON[]
}

export type ClusterJSON = {
  id: string
  size: number
  status: 'forming' | 'ready' | 'degraded' | 'released' | 'failed'
  network: 'rdma' | 'ethernet' | 'rdma_degraded'
  subnet: string
  head_ip: string
  batch_id: string | null
  expires_at: string | null
  auto_renew: boolean
  members: ClusterMemberJSON[]
  app: ClusterAppJSON | null
}

/** 建一个集群网络，返回 {id, name}。 */
export function seedFabric(kind: 'lan' | 'cx7', opts: Omit<Parameters<typeof hooks.fabrics>[0], 'kind'> = {}) {
  return hooks.fabrics({ kind, ...opts })
}

/** 在 fabric 里造 count 台机器（net 缺省 lan）；不是旧 Agent 时断言全部集群就绪。 */
export async function seedClusterMachines(count: number, fabricId: number, net: MachineClusterOpts['net'] = 'lan', opts: { groupId?: number; oldAgent?: boolean } = {}): Promise<number[]> {
  const out = await hooks.clusterMachines({ count, fabric_id: fabricId, net, group_id: opts.groupId, old_agent: opts.oldAgent })
  if (!opts.oldAgent && out.cluster_ready.some((r) => !r)) throw new Error(`seedClusterMachines: 机器未集群就绪 ${JSON.stringify(out)}`)
  return out.ids
}

export type ClusterGroup = { groupId: number; groupName: string; fabricId: number; fabricName: string; machineIds: number[] }

/**
 * 用例私有的可见机组 + 集群网络 + count 台机器（同时在机组与 fabric 里）。缺省 lan fabric（三层路由）+ lan 网卡；
 * cx7-rdma 4 台用 {kind: 'cx7', isolation: 'vlan', rdma: true, net: 'cx7-rdma'}。
 */
export async function clusterGroup(
  count: number,
  opts: { kind?: 'lan' | 'cx7'; isolation?: 'routed' | 'direct' | 'vlan'; rdma?: boolean; net?: MachineClusterOpts['net']; oldAgent?: boolean } = {},
): Promise<ClusterGroup> {
  const groupName = uniqueGroupName()
  const { id: groupId } = await hooks.groups(groupName)
  const fab = await seedFabric(opts.kind ?? 'lan', { isolation: opts.isolation, rdma: opts.rdma })
  const machineIds = await seedClusterMachines(count, fab.id, opts.net ?? 'lan', { groupId, oldAgent: opts.oldAgent })
  return { groupId, groupName, fabricId: fab.id, fabricName: fab.name, machineIds }
}

async function json<T>(r: Awaited<ReturnType<APIRequestContext['get']>>, what: string): Promise<T> {
  if (!r.ok()) throw new Error(`${what} ${r.status()} ${await r.text()}`)
  return (await r.json()) as T
}

/** POST /api/v1/bookings {cluster: true}（商品、镜像取第一个，缺省 1 天）：返回集群 ID 与按 rank 排列的成员实例 ID。 */
export async function orderClusterApi(api: APIRequestContext, size: 2 | 4, groupId: number, opts: { duration?: '1d' | '1w' | '1m' } = {}) {
  const products = (await json<{ items: { id: number }[] }>(await api.get('/api/v1/products'), 'GET products')).items
  const images = (await json<{ items: { id: number }[] }>(await api.get('/api/v1/images'), 'GET images')).items
  const r = await api.post('/api/v1/bookings', {
    data: { product_id: products[0].id, image_id: images[0].id, group_id: groupId, count: size, duration: opts.duration ?? '1d', quantity: 1, cluster: true },
    headers: { 'Idempotency-Key': crypto.randomUUID() },
  })
  if (r.status() !== 201) throw new Error(`cluster booking ${r.status()} ${await r.text()}`)
  const out = (await r.json()) as { batch: { id: string } | null; cluster: ClusterJSON }
  return { clusterId: out.cluster.id, batchId: out.batch?.id ?? null, instanceIds: out.cluster.members.map((m) => m.instance_id) }
}

/** fake Agent 完成全部排队指令（带集群 Spec 的 Create / configure_cluster / 洗机）后跑一轮清扫（集群首次就绪、到期对齐）。 */
export async function completeAgents() {
  await hooks.agent()
  await hooks.sweep()
}

export async function getCluster(api: APIRequestContext, id: string): Promise<ClusterJSON> {
  return json<ClusterJSON>(await api.get(`/api/v1/clusters/${id}`), `GET cluster ${id}`)
}

/** 新用户在自己的机组里开一个就绪的集群（缺省 2 台 lan）；集群登记到 cleanupClusters。 */
export async function readyCluster(phone: string, size: 2 | 4 = 2, groupOpts: Parameters<typeof clusterGroup>[1] = {}) {
  const api = await userApi(phone)
  const group = await clusterGroup(size, groupOpts)
  const placed = await orderClusterApi(api, size, group.groupId)
  ownCluster(api, placed.clusterId)
  await completeAgents()
  const view = await getCluster(api, placed.clusterId)
  expect(view.status).toBe('ready')
  return { api, group, clusterId: placed.clusterId, batchId: placed.batchId, view, members: view.members }
}

/** 市场页打开第一个商品的下单对话框并进入表单步（立即租用 → 确认步 → 更多选项）；groupName 给出时选该机组。 */
export async function openRentForm(page: Page, groupName?: string) {
  await page.goto('/market')
  await page.getByTestId('market-card').first().getByTestId('market-rent-btn').click()
  await page.getByTestId('market-confirm-more').click()
  await expect(page.getByTestId('rent-mode')).toBeVisible()
  if (groupName) await pickGroup(page, groupName)
}

/** 下单表单选机组（选项文字是「名称（共 N · 现空闲 M）」）。 */
export async function pickGroup(page: Page, groupName: string) {
  const opt = page.getByRole('option', { name: new RegExp(`^${groupName}（`) })
  await page.getByTestId('rent-group').click()
  await opt.click()
  await expect(opt).toBeHidden()
}

/** 经下单对话框开一个 size 台集群（在 groupName 机组里），确认后应跳到集群页，返回集群 ID。 */
export async function orderCluster(page: Page, size: 2 | 4, groupName: string): Promise<string> {
  await openRentForm(page, groupName)
  await page.getByTestId(`rent-mode-cluster-${size}`).click()
  await expect(page.getByTestId('rent-cluster-available')).toHaveText(/^可开 \d+ 个$/)
  await page.getByTestId('rent-next').click()
  await page.getByTestId('market-confirm-ok').click()
  await expect(page).toHaveURL(/\/clusters\/clu-[a-z0-9]+$/)
  return page.url().split('/').pop()!
}

// ---- 集群应用 ----

export async function getClusterApp(api: APIRequestContext, clusterId: string): Promise<ClusterAppJSON | null> {
  return json<ClusterAppJSON | null>(await api.get(`/api/v1/clusters/${clusterId}/apps`), `GET cluster apps ${clusterId}`)
}

/** 经控制台 API 部署集群应用（POST /clusters/{id}/apps），返回集群应用视图（installing）。 */
export async function deployClusterApp(api: APIRequestContext, clusterId: string, appId: number, env: Record<string, string>): Promise<ClusterAppJSON> {
  const r = await api.post(`/api/v1/clusters/${clusterId}/apps`, { data: { app_id: appId, env } })
  if (r.status() !== 201) throw new Error(`deploy cluster app on ${clusterId}: ${r.status()} ${await r.text()}`)
  return (await r.json()) as ClusterAppJSON
}

/** 逐轮 hooks.appsTick(1) 直到集群应用聚合状态为 status（意外 failed 立即报错）；status = removed 时 null 也算到达。 */
export async function tickClusterApp(api: APIRequestContext, clusterId: string, status: string, maxRounds = 30): Promise<ClusterAppJSON | null> {
  let a = await getClusterApp(api, clusterId)
  for (let round = 0; !(a === null ? status === 'removed' : a.status === status); round++) {
    if (a?.status === 'failed' && status !== 'failed') throw new Error(`tickClusterApp: 集群应用失败 ${JSON.stringify(a.members)}`)
    if (round >= maxRounds) throw new Error(`tickClusterApp: 推进 ${maxRounds} 轮后仍是 ${a?.status ?? 'null'}，期望 ${status}`)
    await hooks.appsTick(1)
    a = await getClusterApp(api, clusterId)
  }
  return a
}

// ---- 清理 ----

const owned: { api: APIRequestContext; id: string }[] = []

/** 登记本用例造的集群：afterEach 里 cleanupClusters 卸载集群应用并释放集群。 */
export function ownCluster(api: APIRequestContext, id: string) {
  owned.push({ api, id })
}

const ACTIVE = new Set(['forming', 'ready', 'degraded'])

/** afterEach：卸载登记集群上的集群应用（推进到 removed），释放仍活动的集群并让 fake Agent 洗完机（失败的用例也清理）。 */
export async function cleanupClusters() {
  const list = owned.splice(0)
  for (const { api, id } of list) {
    let v = await getCluster(api, id)
    if (v.app && v.app.status !== 'removed') {
      if (v.app.status !== 'removing') {
        const r = await api.delete(`/api/v1/clusters/${id}/apps/${v.app.id}`, { headers: { 'Content-Type': 'application/json' } })
        if (!r.ok()) throw new Error(`DELETE cluster app ${v.app.id} ${r.status()} ${await r.text()}`)
      }
      await tickClusterApp(api, id, 'removed', 40)
    }
    v = await getCluster(api, id)
    if (ACTIVE.has(v.status)) {
      if (v.members.some((m) => m.state === 'creating')) await hooks.agent()
      const r = await api.post(`/api/v1/clusters/${id}/release`, { data: {} })
      if (!r.ok()) throw new Error(`release cluster ${id} ${r.status()} ${await r.text()}`)
    }
  }
  if (list.length) await completeAgents()
}
