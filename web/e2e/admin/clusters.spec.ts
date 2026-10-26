import { expect, test, type Page } from '@playwright/test'
import { adminApi, createAdmin, expectAdminUrl, getJSON, loginAdmin, logout, pick } from '../helpers/admin'
import { clusterGroup, cleanupClusters, completeAgents, deployClusterApp, getCluster, orderClusterApi, ownCluster, readyCluster, tickClusterApp } from '../helpers/clusters'
import { seedUser, toast, userApi } from '../helpers/console'
import { hooks } from '../helpers/hooks'

// 后台集群（计划 13 规格 §10.2、§11）。集群由控制台 API 在用例自己的机组里开（helpers/clusters.ts），
// afterEach 卸载集群应用、释放集群并洗完机。
test.afterEach(cleanupClusters)

type AuditList = { total: number; items: { action: string; target_id: string; diff: Record<string, unknown> }[] }
type AdminCluster = { id: string; version: number; status: string; members: { rank: number; instance_id: string; machine_id: number; machine_label: string; ip: string; expires_at: string }[] }

const clusterRow = (page: Page, id: string) => page.locator(`[data-testid="cluster-row"][data-id="${id}"]`)
const memberRow = (page: Page, rank: number) => page.locator(`[data-testid="cluster-member-row"][data-rank="${rank}"]`)

test('A-CLU-01 集群列表：缺省只看活动集群，按状态 / 归属 / 用户 / 集群网络筛选，?user_id= / ?fabric_id= 预填，点击进详情；财务可看', async ({ page }) => {
  const u = await seedUser()
  const api = await userApi(u.phone)
  const g1 = await clusterGroup(2)
  const g2 = await clusterGroup(2)
  const c1 = await orderClusterApi(api, 2, g1.groupId)
  ownCluster(api, c1.clusterId)
  const c2 = await orderClusterApi(api, 2, g2.groupId)
  await completeAgents()
  const r = await api.post(`/api/v1/clusters/${c2.clusterId}/release`, { data: {} })
  expect(r.ok()).toBe(true)
  await completeAgents()
  expect((await getCluster(api, c2.clusterId)).status).toBe('released')
  const v1 = await getCluster(api, c1.clusterId)

  const ops = await createAdmin('ops')
  await loginAdmin(page, ops)
  await page.goto(`/admin/clusters?user_id=${u.id}`)
  await expect(page.getByTestId('clusters-filter-user')).toHaveValue(String(u.id))
  await expect(page.getByTestId('cluster-row')).toHaveCount(1) // 缺省「活动」
  const row = clusterRow(page, c1.clusterId)
  await expect(row).toHaveAttribute('data-status', 'ready')
  await expect(row.getByTestId('cluster-row-id')).toHaveText(c1.clusterId)
  await expect(row.getByTestId('cluster-row-owner')).toHaveText(String(u.id))
  await expect(row.getByTestId('cluster-row-size')).toHaveText('2 台')
  await expect(row.getByTestId('cluster-row-status')).toHaveText('就绪')
  await expect(row.getByTestId('cluster-row-network')).toHaveText('普通网络')
  await expect(row.getByTestId('cluster-row-fabric')).toHaveText(g1.fabricName)
  await expect(row.getByTestId('cluster-row-subnet')).toHaveText(v1.subnet)
  await expect(page.getByTestId('clusters-total')).toHaveText('共 1 个')

  await pick(page, 'clusters-filter-state', '全部')
  await page.getByTestId('clusters-search').click()
  await expect(page.getByTestId('cluster-row')).toHaveCount(2)
  await expect(clusterRow(page, c2.clusterId).getByTestId('cluster-row-status')).toHaveText('已释放')

  await pick(page, 'clusters-filter-state', '已释放')
  await page.getByTestId('clusters-search').click()
  await expect(page.getByTestId('cluster-row')).toHaveCount(1)
  await expect(clusterRow(page, c2.clusterId)).toHaveAttribute('data-status', 'released')

  await pick(page, 'clusters-filter-state', '全部')
  await pick(page, 'clusters-filter-fabric', g1.fabricName)
  await page.getByTestId('clusters-search').click()
  await expect(page.getByTestId('cluster-row')).toHaveCount(1)
  await expect(clusterRow(page, c1.clusterId)).toHaveCount(1)

  await pick(page, 'clusters-filter-system', '平台节点组')
  await page.getByTestId('clusters-search').click()
  await expect(page.getByTestId('clusters-empty')).toHaveText('没有集群')

  await page.getByTestId('clusters-reset').click()
  await expect(page.getByTestId('clusters-filter-user')).toHaveValue('')

  await page.goto(`/admin/clusters?fabric_id=${g1.fabricId}`)
  await expect(page.getByTestId('cluster-row')).toHaveCount(1)
  await clusterRow(page, c1.clusterId).getByTestId('cluster-row-size').click()
  await expectAdminUrl(page, `/clusters/${c1.clusterId}`)
  await expect(page.getByTestId('cluster-detail-id')).toHaveText(c1.clusterId)

  await logout(page)
  await loginAdmin(page, await createAdmin('finance'))
  await page.goto(`/admin/clusters?user_id=${u.id}`)
  await expect(clusterRow(page, c1.clusterId)).toBeVisible()
})

test('A-CLU-02 集群详情：成员 rank / 实例 / 机器 / 集群 IP / 状态 / 网络状态、版本与事件、集群应用成员；重新下发（版本 + 1、审计）；财务无操作', async ({ page }) => {
  const u = await seedUser()
  const vc = (await hooks.appsSeed()).apps.find((a) => a.slug === 'vllm-cluster')!
  const { api, clusterId, group, view } = await readyCluster(u.phone)
  await deployClusterApp(api, clusterId, vc.id, { MODEL_ID: 'Qwen/Qwen3-8B' })
  await tickClusterApp(api, clusterId, 'running')
  const ops = await createAdmin('ops')
  const ctx = await adminApi(ops)
  const detail = await getJSON<{ cluster: AdminCluster }>(ctx, `/admin/api/v1/clusters/${clusterId}`)
  expect(detail.cluster.version).toBe(1)
  await loginAdmin(page, ops)
  await page.goto(`/admin/clusters/${clusterId}`)

  await expect(page.getByTestId('cluster-detail-id')).toHaveText(clusterId)
  await expect(page.getByTestId('cluster-detail-status')).toHaveAttribute('data-status', 'ready')
  await expect(page.getByTestId('cluster-detail-status')).toHaveText('就绪')
  await expect(page.getByTestId('cluster-detail-network')).toHaveText('普通网络')
  await expect(page.getByTestId('cluster-detail-owner')).toContainText(`用户 ${u.id}`)
  await expect(page.getByTestId('cluster-detail-fabric')).toHaveText(group.fabricName)
  await expect(page.getByTestId('cluster-detail-subnet')).toHaveText(view.subnet)
  await expect(page.getByTestId('cluster-detail-head')).toHaveText(view.head_ip)
  await expect(page.getByTestId('cluster-detail-version')).toHaveText('1')
  for (const m of detail.cluster.members) {
    const row = memberRow(page, m.rank)
    await expect(row).toHaveAttribute('data-instance', m.instance_id)
    await expect(row.getByTestId('cluster-member-instance')).toHaveText(m.instance_id)
    await expect(row.getByTestId('cluster-member-machine')).toContainText(m.machine_label)
    await expect(row.getByTestId('cluster-member-machine')).toContainText(String(m.machine_id))
    await expect(row.getByTestId('cluster-member-ip')).toHaveText(m.ip)
    await expect(row.getByTestId('cluster-member-state')).toHaveText('运行中')
    await expect(row.getByTestId('cluster-member-net')).toHaveAttribute('data-net', 'ok')
    await expect(row.getByTestId('cluster-member-net')).toHaveText('正常')
    await expect(row.getByTestId('cluster-member-migrate')).toBeVisible()
  }
  expect(group.machineIds.sort()).toEqual(detail.cluster.members.map((m) => m.machine_id).sort())
  await expect(memberRow(page, 0)).toContainText('（head）')
  await expect(page.getByTestId('cluster-event').first()).toBeVisible()
  await expect(page.getByTestId('cluster-detail-events')).toContainText('开通中 → 运行中')

  // 集群应用：聚合状态、head 地址、成员角色与在位
  await expect(page.getByTestId('cluster-app-name')).toHaveText('vLLM 多机推理 · 1.0.0')
  await expect(page.getByTestId('cluster-app-status')).toHaveAttribute('data-status', 'running')
  await expect(page.getByTestId('cluster-app-url')).toContainText(view.members[0].instance_id)
  const appMember = (rank: number) => page.locator(`[data-testid="cluster-app-member"][data-rank="${rank}"]`)
  await expect(appMember(0)).toHaveAttribute('data-role', 'head')
  await expect(appMember(0).getByTestId('cluster-app-member-role')).toHaveText('主节点')
  await expect(appMember(1).getByTestId('cluster-app-member-role')).toHaveText('工作节点')
  await expect(page.getByTestId('cluster-app-member-current')).toHaveText(['在位', '在位'])
  await expect(page.getByTestId('cluster-app-member-state')).toHaveText(['运行中', '运行中'])

  await test.step('重新下发集群网络：确认后版本 + 1、提示版本号、审计 cluster.resync；fake Agent 执行后仍就绪', async () => {
    await page.getByTestId('cluster-resync').click()
    await expect(page.getByTestId('cluster-resync-dialog')).toBeVisible()
    await page.getByTestId('cluster-resync-ok').click()
    await toast(page, '已重新下发集群网络（版本 2）')
    await expect(page.getByTestId('cluster-detail-version')).toHaveText('2')
    expect((await getJSON<AuditList>(ctx, `/admin/api/v1/audit?action=cluster.resync&target_type=cluster&target_id=${clusterId}`)).total).toBe(1)
    await hooks.agent()
    await page.getByTestId('cluster-detail-refresh').click()
    await expect(page.getByTestId('cluster-detail-status')).toHaveAttribute('data-status', 'ready')
    await expect(page.getByTestId('cluster-member-net')).toHaveText(['正常', '正常'])
  })

  await test.step('财务：只读（无重新下发 / 延长 / 换机），接口 403', async () => {
    const fin = await createAdmin('finance')
    await logout(page)
    await loginAdmin(page, fin)
    await page.goto(`/admin/clusters/${clusterId}`)
    await expect(page.getByTestId('cluster-detail-version')).toHaveText('2')
    for (const id of ['cluster-resync', 'cluster-extend', 'cluster-member-migrate']) await expect(page.getByTestId(id)).toHaveCount(0)
    const r = await (await adminApi(fin)).post(`/admin/api/v1/clusters/${clusterId}/resync`, { data: {} })
    expect(r.status()).toBe(403)
  })
})

test('A-CLU-03 集群免费延长：缺省 1 天、原因必填，全部成员到期一起后延、审计 cluster.extend；成员单独延长 409；已释放的集群没有延长', async ({ page }) => {
  const u = await seedUser()
  const { api, clusterId, view } = await readyCluster(u.phone)
  const ops = await createAdmin('ops')
  const ctx = await adminApi(ops)
  await loginAdmin(page, ops)
  await page.goto(`/admin/clusters/${clusterId}`)

  await page.getByTestId('cluster-extend').click()
  await expect(page.getByTestId('cluster-extend-dialog')).toBeVisible()
  await expect(page.getByTestId('cluster-extend-amount').locator('input')).toHaveValue('1')
  await page.getByTestId('cluster-extend-submit').click()
  await expect(page.getByTestId('cluster-extend-error')).toHaveText('请填写补偿原因')
  await page.getByTestId('cluster-extend-reason').fill('网络维护补偿')
  await page.getByTestId('cluster-extend-submit').click()
  await toast(page, '已为全部成员延长 1 天')
  await expect(page.getByTestId('cluster-extend-dialog')).toBeHidden()
  let after = await getCluster(api, clusterId)
  for (const [i, m] of after.members.entries()) expect(new Date(m.expires_at!).getTime() - new Date(view.members[i].expires_at!).getTime()).toBe(86400_000)

  await page.getByTestId('cluster-extend').click()
  await page.getByTestId('cluster-extend-amount').locator('input').fill('3')
  await pick(page, 'cluster-extend-unit', '小时')
  await page.getByTestId('cluster-extend-reason').fill('再补 3 小时')
  await page.getByTestId('cluster-extend-submit').click()
  await toast(page, '已为全部成员延长 3 小时')
  after = await getCluster(api, clusterId)
  expect(new Date(after.expires_at!).getTime() - new Date(view.expires_at!).getTime()).toBe(86400_000 + 3 * 3600_000)
  const audit = await getJSON<AuditList>(ctx, `/admin/api/v1/audit?action=cluster.extend&target_type=cluster&target_id=${clusterId}`)
  expect(audit.total).toBe(2)
  expect(JSON.stringify(audit.items)).toContain('网络维护补偿')

  const single = await ctx.post(`/admin/api/v1/instances/${view.members[0].instance_id}/extend`, { data: { seconds: 3600, reason: 'e2e' } })
  expect(single.status()).toBe(409)
  expect(((await single.json()) as { code: string }).code).toBe('cluster_member')

  const rel = await api.post(`/api/v1/clusters/${clusterId}/release`, { data: {} })
  expect(rel.ok()).toBe(true)
  await completeAgents()
  await page.reload()
  await expect(page.getByTestId('cluster-detail-status')).toHaveAttribute('data-status', 'released')
  await expect(page.getByTestId('cluster-extend')).toHaveCount(0)
  await expect(page.getByTestId('cluster-resync')).toHaveCount(0)
  const late = await ctx.post(`/admin/api/v1/clusters/${clusterId}/extend`, { data: { seconds: 3600, reason: 'e2e' } })
  expect(late.status()).toBe(409)
})
