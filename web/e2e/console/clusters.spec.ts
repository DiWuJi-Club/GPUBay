import { expect, test, type APIRequestContext, type Page, type TestInfo } from '@playwright/test'
import { appRow } from '../helpers/apps'
import { privateGroup } from '../helpers/booking'
import {
  clusterGroup, cleanupClusters, completeAgents, deployClusterApp, getCluster, getClusterApp, openRentForm, orderClusterApi, ownCluster, pickGroup,
  readyCluster, tickClusterApp,
} from '../helpers/clusters'
import { isMobile, loginConsole, nav, placeOrder, seedUser, toast, userApi } from '../helpers/console'
import { hooks } from '../helpers/hooks'
import { instanceOf, instanceRow } from '../helpers/instances'
import { yuan } from '../helpers/money'
import { cstTime } from '../helpers/time'

// 多机集群控制台（计划 13 规格 §9、§10.1、§16.3）。集群都开在用例自己的可见机组里（helpers/clusters.ts clusterGroup），
// 「可开 X 个」按机组查询才确定；不推进假时钟。afterEach 卸载集群应用、释放集群并洗完机。
test.afterEach(cleanupClusters)

const PRICE_1D = 6900

const balanceOf = async (api: APIRequestContext) => ((await (await api.get('/api/v1/wallet')).json()) as { balance: number }).balance

/** 集群详情成员表里 rank 的那一项：电脑是表格行（testid 在 rank 单元格里），手机是卡片。 */
function memberOf(page: Page, info: TestInfo, rank: number) {
  const tag = page.locator(`[data-testid="cluster-member"][data-rank="${rank}"]`)
  return isMobile(info) ? tag : page.getByRole('row').filter({ has: tag })
}

/** 记录下单请求（POST /api/v1/bookings）的请求体。 */
function trackBookings(page: Page) {
  const bodies: Record<string, unknown>[] = []
  page.on('request', (r) => {
    if (r.method() === 'POST' && new URL(r.url()).pathname === '/api/v1/bookings') bodies.push(JSON.parse(r.postData() ?? '{}') as Record<string, unknown>)
  })
  return bodies
}

/** 记录集群可用性查询（GET /api/v1/availability?cluster_size=）的查询参数。 */
function trackClusterAvailability(page: Page) {
  const seen: Record<string, string>[] = []
  page.on('request', (r) => {
    const u = new URL(r.url())
    if (r.method() === 'GET' && u.pathname === '/api/v1/availability' && u.searchParams.has('cluster_size')) seen.push(Object.fromEntries(u.searchParams))
  })
  return seen
}

test('C-CLU-01 市场页集群选项：租用方式单机 / 集群 2 台 / 4 台，按机组显示网络类型（RDMA / 普通）与可开数量，选集群后隐藏数量 / 预订 / 预装 / 指定机器', async ({ page }) => {
  const u = await seedUser()
  await hooks.appsSeed() // 预装应用下拉要有内容，才能断言选集群后它被隐藏、单机下拉里没有集群应用
  const lan = await clusterGroup(2) // 普通网络：2 台 × 1，凑不出 4 台
  const rdma = await clusterGroup(4, { kind: 'cx7', isolation: 'vlan', rdma: true, net: 'cx7-rdma' }) // RDMA：2 台 × 2、4 台 × 1
  const queries = trackClusterAvailability(page)
  await loginConsole(page, u.phone)
  await openRentForm(page, lan.groupName)

  await test.step('缺省单机：数量、开始、预装、指定机器都在；预装下拉不含只能集群部署的应用', async () => {
    await expect(page.getByTestId('rent-mode-single').locator('input')).toBeChecked()
    await expect(page.getByTestId('rent-mode-cluster-2')).toHaveText('集群 2 台')
    await expect(page.getByTestId('rent-mode-cluster-4')).toHaveText('集群 4 台')
    await expect(page.getByTestId('rent-cluster-unavailable')).toHaveCount(0)
    for (const id of ['rent-count', 'rent-start-later', 'rent-app', 'rent-machine']) await expect(page.getByTestId(id)).toBeVisible()
    await page.getByTestId('rent-app').click()
    await expect(page.getByRole('option', { name: /^vLLM 推理服务/ })).toBeVisible()
    await expect(page.getByRole('option', { name: /vLLM 多机推理/ })).toHaveCount(0)
    await page.keyboard.press('Escape')
    await expect(page.getByRole('option', { name: /^vLLM 推理服务/ })).toBeHidden()
  })

  await test.step('集群 2 台（普通网络机组）：普通网络、可开 1 个；数量 / 开始 / 预装 / 指定机器隐藏，说明暂不支持预订', async () => {
    await page.getByTestId('rent-mode-cluster-2').click()
    await expect(page.getByTestId('rent-cluster-network')).toHaveText('普通网络（性能有限）')
    await expect(page.getByTestId('rent-cluster-network')).toHaveAttribute('data-rdma', 'false')
    await expect(page.getByTestId('rent-cluster-available')).toHaveText('可开 1 个')
    await expect(page.getByTestId('rent-availability')).toHaveText('可开 1 个 2 台集群')
    for (const id of ['rent-count', 'rent-start-later', 'rent-app', 'rent-machine']) await expect(page.getByTestId(id)).toHaveCount(0)
    await expect(page.getByTestId('rent-cluster-note')).toContainText('集群暂不支持预订，只能立即开通')
    await expect(page.getByTestId('rent-next')).toBeEnabled()
  })

  await test.step('集群 4 台：普通网络机组只有 2 台 → 暂无，下一步禁用', async () => {
    await page.getByTestId('rent-mode-cluster-4').click()
    await expect(page.getByTestId('rent-cluster-available')).toHaveText('暂无可组成该规模集群的空闲机器')
    await expect(page.getByTestId('rent-availability')).toHaveText('暂无可组成 4 台集群的空闲机器，请稍后再试或改为单机')
    await expect(page.getByTestId('rent-next')).toBeDisabled()
  })

  await test.step('换到 RDMA 机组：4 台可开 1 个、2 台可开 2 个，网络为同一高速网络（RDMA）', async () => {
    await pickGroup(page, rdma.groupName)
    await expect(page.getByTestId('rent-cluster-network')).toHaveText('同一高速网络（RDMA）')
    await expect(page.getByTestId('rent-cluster-network')).toHaveAttribute('data-rdma', 'true')
    await expect(page.getByTestId('rent-cluster-available')).toHaveText('可开 1 个')
    await expect(page.getByTestId('rent-next')).toBeEnabled()
    await page.getByTestId('rent-mode-cluster-2').click()
    await expect(page.getByTestId('rent-cluster-available')).toHaveText('可开 2 个')
    await expect(page.getByTestId('rent-cluster-network')).toHaveText('同一高速网络（RDMA）')
    expect(queries).toContainEqual(expect.objectContaining({ cluster_size: '4', group_id: String(rdma.groupId) }))
    expect(queries).toContainEqual(expect.objectContaining({ cluster_size: '2', group_id: String(lan.groupId) }))
  })

  await test.step('回到单机：数量恢复为 1，单机可用性', async () => {
    await page.getByTestId('rent-mode-single').click()
    await expect(page.getByTestId('rent-count').locator('input')).toHaveValue('1')
    await expect(page.getByTestId('rent-cluster-network')).toHaveCount(0)
    await expect(page.getByTestId('rent-availability')).toHaveText('该时段可租 4 台')
  })
})

test('C-CLU-02 下单 2 台集群：确认页集群 2 台 / 网络 / 总价 = 2 × 单价，请求带 cluster，扣两单后进入集群页；组建中不能释放，开通完成后就绪', async ({ page }) => {
  const u = await seedUser()
  const api = await userApi(u.phone)
  const g = await clusterGroup(2)
  const before = await balanceOf(api)
  const bodies = trackBookings(page)
  await loginConsole(page, u.phone)
  await openRentForm(page, g.groupName)
  await page.getByTestId('rent-mode-cluster-2').click()
  await expect(page.getByTestId('rent-cluster-available')).toHaveText('可开 1 个')
  await page.getByTestId('rent-next').click()
  await expect(page.getByTestId('market-confirm-dialog')).toBeVisible()
  await expect(page.getByTestId('market-confirm-count')).toHaveText('集群 2 台')
  await expect(page.getByTestId('market-confirm-cluster-network')).toHaveText('普通网络（性能有限）')
  await expect(page.getByTestId('market-confirm-start')).toHaveText('立即')
  await expect(page.getByTestId('market-confirm-group')).toHaveText(g.groupName)
  await expect(page.getByTestId('market-confirm-unit')).toHaveText(yuan(PRICE_1D))
  await expect(page.getByTestId('market-confirm-price')).toHaveText(`${yuan(2 * PRICE_1D)}（2 台 × ${yuan(PRICE_1D)}）`)
  await expect(page.getByTestId('market-confirm-after')).toHaveText(yuan(before - 2 * PRICE_1D))
  await page.getByTestId('market-confirm-ok').click()
  await expect(page).toHaveURL(/\/clusters\/clu-[a-z0-9]+$/)
  const id = page.url().split('/').pop()!
  ownCluster(api, id)

  expect(bodies).toHaveLength(1)
  expect(bodies[0]).toMatchObject({ cluster: true, count: 2, group_id: g.groupId, duration: '1d', quantity: 1 })
  expect(bodies[0]).not.toHaveProperty('start_at')
  expect(bodies[0]).not.toHaveProperty('app_id')
  expect(await balanceOf(api)).toBe(before - 2 * PRICE_1D)

  await expect(page.getByTestId('cluster-id')).toHaveText(id)
  await expect(page.getByTestId('cluster-status')).toHaveAttribute('data-status', 'forming')
  await expect(page.getByTestId('cluster-status')).toHaveText('组建中')
  await expect(page.getByTestId('cluster-notice-forming')).toContainText('30 分钟内未就绪会整组中止并全额退款')
  await expect(page.getByTestId('cluster-member-state')).toHaveText(['开通中', '开通中'])
  await expect(page.getByTestId('cluster-release')).toBeDisabled()
  await expect(page.getByTestId('cluster-release-reason')).toHaveText('有成员正在开通，开通完成后才能释放')
  const v = await getCluster(api, id)
  expect(v.members.map((m) => m.rank)).toEqual([0, 1])
  await expect(page.getByTestId('cluster-batch')).toHaveText(v.batch_id!)

  await completeAgents()
  await expect(page.getByTestId('cluster-status')).toHaveAttribute('data-status', 'ready', { timeout: 15_000 })
  await expect(page.getByTestId('cluster-status')).toHaveText('就绪')
  await expect(page.getByTestId('cluster-notice-forming')).toHaveCount(0)
  await expect(page.getByTestId('cluster-member-state')).toHaveText(['运行中', '运行中'])
  await expect(page.getByTestId('cluster-release')).toBeEnabled()
  await expect(page.getByTestId('cluster-renew')).toHaveText('续费（2 台）')
})

test('C-CLU-03 实例列表：同一集群的成员折叠成一张集群卡（组建中 → 就绪 → 降级 → 恢复），成员不出现在单机列表里，点击进集群页', async ({ page }, info) => {
  const u = await seedUser()
  const api = await userApi(u.phone)
  await hooks.machines(1)
  const single = await placeOrder(api)
  const g = await clusterGroup(2)
  const placed = await orderClusterApi(api, 2, g.groupId)
  ownCluster(api, placed.clusterId)
  await loginConsole(page, u.phone)
  await nav(page, info, 'instances')
  const card = page.locator(`[data-testid="instances-cluster-card"][data-id="${placed.clusterId}"]`)
  await expect(card).toHaveAttribute('data-status', 'forming')
  await expect(card.getByTestId('instances-cluster-link')).toHaveText(`集群 ${placed.clusterId} · 2 台`)
  await expect(card.getByTestId('instances-cluster-status')).toHaveText('组建中')
  await expect(card.getByTestId('instances-cluster-member')).toHaveCount(2)
  for (const [rank, iid] of placed.instanceIds.entries()) {
    const m = card.locator(`[data-testid="instances-cluster-member"][data-rank="${rank}"]`)
    await expect(m).toHaveAttribute('data-id', iid)
    await expect(m.getByTestId('instances-cluster-member-id')).toHaveText(iid)
    await expect(m.getByTestId('instances-cluster-member-state')).toHaveText('开通中')
  }
  await expect(instanceRow(page, info, single)).toHaveCount(1)
  for (const iid of placed.instanceIds) await expect(page.getByTestId('instance-id').filter({ hasText: iid })).toHaveCount(0)

  await completeAgents()
  await page.reload()
  await expect(card).toHaveAttribute('data-status', 'ready')
  await expect(card.getByTestId('instances-cluster-status')).toHaveText('就绪')
  await expect(card.getByTestId('instances-cluster-member-state')).toHaveText(['运行中', '运行中'])

  await hooks.agentClusterNet({ machine_id: g.machineIds[0], check_ok: false }) // 一台成员的集群网络异常 → 降级
  await page.reload()
  await expect(card).toHaveAttribute('data-status', 'degraded')
  await expect(card.getByTestId('instances-cluster-status')).toHaveText('降级')
  await hooks.agentClusterNet({ machine_id: g.machineIds[0], check_ok: true })
  await page.reload()
  await expect(card).toHaveAttribute('data-status', 'ready')

  await card.getByTestId('instances-cluster-status').click()
  await expect(page).toHaveURL(new RegExp(`/clusters/${placed.clusterId}$`))
  await expect(page.getByTestId('cluster-id')).toHaveText(placed.clusterId)
})

test('C-CLU-04 集群详情：状态、网络类型（RDMA）、网段与 head 地址、成员 rank / 集群 IP / 网络状态、连接说明与示例；不存在的集群报错', async ({ page }, info) => {
  const u = await seedUser()
  const { clusterId, view } = await readyCluster(u.phone, 2, { kind: 'cx7', isolation: 'direct', rdma: true, net: 'cx7-rdma' })
  expect(view.network).toBe('rdma')
  const head = view.members[0]
  const worker = view.members[1]
  expect(view.head_ip).toBe(head.ip)
  expect(head.ip).toMatch(/^10\.210\.\d+\.11$/)
  expect(worker.ip).toBe(head.ip.replace(/\.11$/, '.12'))
  await loginConsole(page, u.phone)
  await page.goto(`/clusters/${clusterId}`)
  await expect(page.getByTestId('cluster-id')).toHaveText(clusterId)
  await expect(page.getByTestId('cluster-status')).toHaveText('就绪')
  await expect(page.getByTestId('cluster-network')).toHaveText('RDMA 高速互联')
  await expect(page.getByTestId('cluster-network')).toHaveAttribute('data-network', 'rdma')
  await expect(page.getByTestId('cluster-subnet')).toHaveText(view.subnet)
  await expect(page.getByTestId('cluster-head-ip')).toHaveText(view.head_ip)
  await expect(page.getByTestId('cluster-expires')).toHaveText(cstTime(view.expires_at!))
  await expect(page.getByTestId('cluster-expires-mismatch')).toHaveCount(0)

  for (const m of [head, worker]) {
    const row = memberOf(page, info, m.rank)
    await expect(page.locator(`[data-testid="cluster-member"][data-rank="${m.rank}"]`)).toHaveAttribute('data-instance', m.instance_id)
    await expect(row.getByTestId('cluster-member-instance')).toHaveText(m.instance_id)
    await expect(row.getByTestId('cluster-member-ip')).toHaveText(m.ip)
    await expect(row.getByTestId('cluster-member-state')).toHaveText('运行中')
    await expect(row.getByTestId('cluster-member-net')).toContainText('正常')
    await expect(row.getByTestId('cluster-member-machine')).toHaveText(m.machine_label)
  }

  await expect(page.getByTestId('cluster-connect-head')).toHaveText(view.head_ip)
  await expect(page.getByTestId('cluster-connect-subnet')).toHaveText(view.subnet)
  await expect(page.getByTestId('cluster-connect')).toContainText('/etc/spark-cluster.json')
  await expect(page.getByTestId('cluster-connect')).toContainText('没有预配的 SSH 互信')
  await expect(page.getByTestId('cluster-connect-env')).toContainText('. /.spark/cluster.env')
  await expect(page.getByTestId('cluster-connect-torchrun')).toContainText(`head = rank 0，地址 ${view.head_ip}`)
  await expect(page.getByTestId('cluster-connect-torchrun')).toContainText('--master-addr $MASTER_ADDR --master-port 29500')
  await expect(page.getByTestId('cluster-connect-ray')).toContainText('ray start --head --node-ip-address=$SPARK_CLUSTER_IP --port=6379')
  await expect(page.getByTestId('cluster-connect-ray')).toContainText('export RAY_ADDRESS=$SPARK_CLUSTER_HEAD_IP:6379')
  await page.getByTestId('cluster-connect-head-copy').click()
  await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe(view.head_ip)

  // 成员链接进实例详情
  await memberOf(page, info, 1).getByTestId('cluster-member-instance').click()
  await expect(page).toHaveURL(new RegExp(`/instances/${worker.instance_id}$`))

  await page.goto('/clusters/clu-zzzzzzzz')
  await expect(page.getByTestId('cluster-error')).toContainText('集群不存在')
})

test('C-CLU-05 成员实例详情：「属于集群（rank r / N）」横幅与链接，续费 / 释放 / 自动续费置灰并引导到集群页；接口 409 cluster_member', async ({ page }) => {
  const u = await seedUser()
  const { api, clusterId, members } = await readyCluster(u.phone)
  const worker = members[1].instance_id
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${worker}`)
  await expect(page.getByTestId('instance-cluster-banner')).toHaveAttribute('data-cluster', clusterId)
  await expect(page.getByTestId('instance-cluster-banner')).toContainText(`属于集群 ${clusterId}（rank 1 / 2）`)
  await expect(page.getByTestId('detail-renew-btn')).toBeDisabled()
  await expect(page.getByTestId('detail-release-btn')).toBeDisabled()
  await expect(page.getByTestId('detail-cluster-guard')).toContainText('集群成员请在集群页统一续费、设置自动续费或释放')
  await expect(page.getByTestId('detail-auto-renew')).toHaveClass(/is-disabled/)
  await expect(page.getByTestId('detail-auto-renew-cluster')).toContainText('集群成员的自动续费在集群页为全部成员统一设置')

  const key = { 'Idempotency-Key': crypto.randomUUID() }
  for (const r of [
    await api.post(`/api/v1/instances/${worker}/renew`, { data: { duration: '1d' }, headers: key }),
    await api.post(`/api/v1/instances/${worker}/release`, { data: {} }),
    await api.put(`/api/v1/instances/${worker}/auto-renew`, { data: { enabled: true, duration: '1d' } }),
  ]) {
    expect(r.status()).toBe(409)
    expect(((await r.json()) as { code: string }).code).toBe('cluster_member')
  }
  expect((await instanceOf(api, worker)).auto_renew).toBe(false)

  await page.getByTestId('instance-cluster-link').click()
  await expect(page).toHaveURL(new RegExp(`/clusters/${clusterId}$`))
  await expect(page.getByTestId('cluster-id')).toHaveText(clusterId)
})

test('C-CLU-06 集群续费（两台各一单、总价 = 单价 × 台数 × 数量、到期一起后延）与全员自动续费开关 / 档位', async ({ page }) => {
  const u = await seedUser()
  const { api, clusterId, view } = await readyCluster(u.phone)
  const before = await balanceOf(api)
  const keys: string[] = []
  page.on('request', (r) => {
    if (r.method() === 'POST' && new URL(r.url()).pathname === `/api/v1/clusters/${clusterId}/renew`) keys.push(r.headers()['idempotency-key'] ?? '')
  })
  await loginConsole(page, u.phone)
  await page.goto(`/clusters/${clusterId}`)

  await test.step('续费 1 天 × 2：每台 ¥138.00、总价 ¥276.00（2 台），成功后到期 +2 天、余额扣 ¥276.00', async () => {
    await page.getByTestId('cluster-renew').click()
    await expect(page.getByTestId('cluster-renew-dialog')).toBeVisible()
    await expect(page.getByTestId('cluster-renew-duration-1d').locator('input')).toBeChecked()
    await expect(page.getByTestId('cluster-renew-unit')).toHaveText(yuan(PRICE_1D))
    await expect(page.getByTestId('cluster-renew-price')).toHaveText(`${yuan(2 * PRICE_1D)}（2 台）`)
    const qty = page.getByTestId('cluster-renew-quantity').locator('input')
    await qty.fill('2')
    await qty.blur()
    await expect(page.getByTestId('cluster-renew-unit')).toHaveText(yuan(2 * PRICE_1D))
    await expect(page.getByTestId('cluster-renew-price')).toHaveText(`${yuan(4 * PRICE_1D)}（2 台）`)
    await expect(page.getByTestId('cluster-renew-after')).toHaveText(yuan(before - 4 * PRICE_1D))
    await page.getByTestId('cluster-renew-ok').click()
    await toast(page, '续费成功（2 台）')
    expect(keys).toHaveLength(1)
    expect(keys[0]).not.toBe('')
    const after = await getCluster(api, clusterId)
    expect(new Date(after.expires_at!).getTime() - new Date(view.expires_at!).getTime()).toBe(2 * 86400_000)
    expect(new Set(after.members.map((m) => m.expires_at)).size).toBe(1)
    await expect(page.getByTestId('cluster-expires')).toHaveText(cstTime(after.expires_at!))
    expect(await balanceOf(api)).toBe(before - 4 * PRICE_1D)
  })

  await test.step('自动续费：开启后全部成员开启（缺省 1 天）、改为 1 周后全员 1 周、关闭后全员关闭', async () => {
    await page.getByTestId('cluster-auto-renew').click()
    await toast(page, '已为全部成员开启自动续费')
    await expect(page.getByTestId('cluster-auto-renew')).toHaveClass(/is-checked/)
    for (const m of view.members) expect(await instanceOf(api, m.instance_id)).toMatchObject({ auto_renew: true, auto_renew_duration: '1d' })
    await page.getByTestId('cluster-auto-renew-duration').click()
    await page.getByRole('option', { name: '1 周', exact: true }).click()
    await toast(page, '已为全部成员开启自动续费')
    await expect.poll(async () => (await instanceOf(api, view.members[1].instance_id)).auto_renew_duration).toBe('1w')
    expect((await instanceOf(api, view.members[0].instance_id)).auto_renew_duration).toBe('1w')
    await page.getByTestId('cluster-auto-renew').click()
    await toast(page, '已关闭自动续费')
    await expect(page.getByTestId('cluster-auto-renew')).not.toHaveClass(/is-checked/)
    for (const m of view.members) expect((await instanceOf(api, m.instance_id)).auto_renew).toBe(false)
    expect((await getCluster(api, clusterId)).auto_renew).toBe(false)
  })
})

test('C-CLU-07 释放集群：二次确认文案、取消不发请求，确认后全部成员洗机、完成后集群已释放且操作按钮消失', async ({ page }) => {
  const u = await seedUser()
  const { api, clusterId, members } = await readyCluster(u.phone)
  const posts: string[] = []
  page.on('request', (r) => {
    if (r.method() === 'POST' && new URL(r.url()).pathname === `/api/v1/clusters/${clusterId}/release`) posts.push(r.url())
  })
  await loginConsole(page, u.phone)
  await page.goto(`/clusters/${clusterId}`)
  await page.getByTestId('cluster-release').click()
  await expect(page.getByTestId('cluster-release-text')).toHaveText('将释放集群的全部 2 台成员，所有成员的数据都会被清除，已付费用不退。确定释放吗？')
  await page.getByTestId('cluster-release-cancel').click()
  await expect(page.getByTestId('cluster-release-dialog')).toBeHidden()
  expect(posts).toHaveLength(0)

  await page.getByTestId('cluster-release').click()
  await expect(page.getByTestId('cluster-release-ok')).toHaveText('确定释放 2 台')
  await page.getByTestId('cluster-release-ok').click()
  await toast(page, '已提交释放，全部成员正在清除数据')
  expect(posts).toHaveLength(1)
  for (const m of members) expect((await instanceOf(api, m.instance_id)).state).toBe('wiping')

  await completeAgents()
  await expect(page.getByTestId('cluster-status')).toHaveAttribute('data-status', 'released', { timeout: 15_000 })
  await expect(page.getByTestId('cluster-status')).toHaveText('已释放')
  await expect(page.getByTestId('cluster-notice-released')).toBeVisible()
  for (const id of ['cluster-renew', 'cluster-release', 'cluster-connect', 'cluster-auto-renew-card', 'cluster-app-deploy']) await expect(page.getByTestId(id)).toHaveCount(0)
  await expect(page.getByTestId('cluster-ended')).toBeVisible()
  for (const m of members) expect((await instanceOf(api, m.instance_id)).state).toBe('released')
  // 已释放的集群再释放：409
  const again = await api.post(`/api/v1/clusters/${clusterId}/release`, { data: {} })
  expect(again.status()).toBe(409)
})

test('C-CLU-08 部署集群应用：只列支持集群的应用并自动选中，TP 默认 = 台数、必填校验，成员 head / worker 进度与聚合状态，运行后显示调用方式与 head 地址', async ({ page }) => {
  const u = await seedUser()
  const seeded = (await hooks.appsSeed()).apps
  const vc = seeded.find((a) => a.slug === 'vllm-cluster')!
  const { api, clusterId, members } = await readyCluster(u.phone)
  const posts: Record<string, unknown>[] = []
  page.on('request', (r) => {
    if (r.method() === 'POST' && new URL(r.url()).pathname === `/api/v1/clusters/${clusterId}/apps`) posts.push(JSON.parse(r.postData() ?? '{}') as Record<string, unknown>)
  })
  await loginConsole(page, u.phone)
  await page.goto(`/clusters/${clusterId}`)
  await expect(page.getByTestId('cluster-app-empty')).toBeVisible()
  await page.getByTestId('cluster-app-deploy').click()
  await expect(page.getByTestId('cluster-app-deploy-dialog')).toBeVisible()
  await expect(page.getByTestId('cluster-app-deploy-pick-vllm-cluster')).toHaveAttribute('aria-pressed', 'true')
  for (const slug of ['vllm', 'open-webui', 'comfyui', 'jupyterlab']) await expect(page.getByTestId(`cluster-app-deploy-pick-${slug}`)).toHaveCount(0)
  await expect(page.getByTestId('app-env-TENSOR_PARALLEL_SIZE').locator('input')).toHaveValue('2')
  await expect(page.getByTestId('app-env-PIPELINE_PARALLEL_SIZE').locator('input')).toHaveValue('1')
  await page.getByTestId('cluster-app-deploy-ok').click()
  await expect(page.getByTestId('app-env-MODEL_ID-error')).toBeVisible() // 模型必填，不发请求
  expect(posts).toHaveLength(0)
  await page.getByTestId('app-env-MODEL_ID').fill('Qwen/Qwen3-8B')
  await page.getByTestId('cluster-app-deploy-ok').click()
  await toast(page, '已开始在 2 台成员上部署 vLLM 多机推理')
  expect(posts).toHaveLength(1)
  expect(posts[0]).toMatchObject({ app_id: vc.id, env: { MODEL_ID: 'Qwen/Qwen3-8B', TENSOR_PARALLEL_SIZE: '2', PIPELINE_PARALLEL_SIZE: '1' } })

  await expect(page.getByTestId('cluster-app-status')).toHaveAttribute('data-status', 'installing')
  await expect(page.getByTestId('cluster-app-status')).toHaveText('部署中')
  await expect(page.getByTestId('cluster-app-name')).toHaveText('vLLM 多机推理')
  await expect(page.getByTestId('cluster-app-version')).toHaveText('v1.0.0')
  await expect(page.getByTestId('cluster-app-progress')).toBeVisible()
  const appMember = (rank: number) => page.locator(`[data-testid="cluster-app-member"][data-rank="${rank}"]`)
  await expect(appMember(0)).toHaveAttribute('data-role', 'head')
  await expect(appMember(0)).toContainText(`rank 0 · 主节点`)
  await expect(appMember(0)).toContainText(members[0].instance_id)
  await expect(appMember(1)).toHaveAttribute('data-role', 'worker')
  await expect(appMember(1)).toContainText(`rank 1 · 工作节点`)
  await expect(page.getByTestId('cluster-app-member')).toHaveCount(2)

  const running = (await tickClusterApp(api, clusterId, 'running'))!
  await expect(page.getByTestId('cluster-app-status')).toHaveAttribute('data-status', 'running', { timeout: 15_000 })
  await expect(page.getByTestId('cluster-app-status')).toHaveText('运行中')
  await expect(appMember(0)).toHaveAttribute('data-state', 'running')
  await expect(appMember(1)).toHaveAttribute('data-state', 'running')
  await expect(page.getByTestId('cluster-app-member-state')).toHaveText(['运行中', '运行中'])
  await expect(page.getByTestId('cluster-app-progress')).toHaveCount(0)
  await expect(page.getByTestId('cluster-app-param-MODEL_ID')).toContainText('Qwen/Qwen3-8B')
  await expect(page.getByTestId('cluster-app-param-TENSOR_PARALLEL_SIZE')).toContainText('2')
  await expect(page.getByTestId('cluster-app-call-model')).toHaveText('model')
  await expect(page.getByTestId('cluster-app-call-local')).toHaveText('http://127.0.0.1:8000/v1')
  await expect(page.getByTestId('cluster-app-call')).toContainText(members[0].instance_id)
  await expect(page.getByTestId('cluster-app-call-curl')).toContainText('"model": "model"')
  expect(running.url).not.toBeNull()
  expect(running.url!).toContain(members[0].instance_id) // 端口只登记在 head 上
  await expect(page.getByTestId('cluster-app-call-public')).toHaveText(`${running.url!.replace(/\/docs$/, '')}/v1`)
  await expect(page.getByTestId('cluster-app-open')).toBeVisible()
  // worker 不登记端口：成员实例的端口列表里没有 8000
  const ports = (id: string) => api.get(`/api/v1/instances/${id}/ports`).then(async (r) => ((await r.json()) as { items: { port: number }[] }).items.map((p) => p.port))
  expect(await ports(members[0].instance_id)).toContain(8000)
  expect(await ports(members[1].instance_id)).not.toContain(8000)
})

test('C-CLU-09 集群应用重新部署 / 停止 / 卸载；成员实例的应用列表只读显示「集群应用（主节点 / 工作节点）」，单实例操作 409', async ({ page }) => {
  const u = await seedUser()
  const vc = (await hooks.appsSeed()).apps.find((a) => a.slug === 'vllm-cluster')!
  const { api, clusterId, members } = await readyCluster(u.phone)
  await deployClusterApp(api, clusterId, vc.id, { MODEL_ID: 'Qwen/Qwen3-8B' })
  await tickClusterApp(api, clusterId, 'running')
  await loginConsole(page, u.phone)
  await page.goto(`/clusters/${clusterId}`)
  await expect(page.getByTestId('cluster-app-status')).toHaveAttribute('data-status', 'running')

  await test.step('重新部署：版本与变量沿用，改对外模型名后整组重装，运行后调用模型名更新', async () => {
    await page.getByTestId('cluster-app-redeploy').click()
    await expect(page.getByTestId('cluster-app-deploy-dialog')).toBeVisible()
    await expect(page.getByTestId('cluster-app-deploy-version')).toBeVisible()
    await expect(page.getByTestId('app-env-MODEL_ID')).toHaveValue('Qwen/Qwen3-8B')
    await page.getByTestId('app-env-SERVED_MODEL_NAME').fill('qwen8b')
    await page.getByTestId('cluster-app-deploy-ok').click()
    await toast(page, '已提交重新部署 vLLM 多机推理')
    await expect(page.getByTestId('cluster-app-status')).toHaveAttribute('data-status', 'installing')
    const again = (await tickClusterApp(api, clusterId, 'running'))!
    expect(again.env.SERVED_MODEL_NAME).toBe('qwen8b')
    await expect(page.getByTestId('cluster-app-status')).toHaveAttribute('data-status', 'running', { timeout: 15_000 })
    await expect(page.getByTestId('cluster-app-call-model')).toHaveText('qwen8b')
  })

  await test.step('成员实例详情：集群部署只读（无操作菜单），链接回集群页；单实例停止 / 卸载接口 409 app_cluster_managed', async () => {
    const app = (await getClusterApp(api, clusterId))!
    const worker = app.members.find((m) => m.role === 'worker' && m.current)!
    expect(worker.instance_id).toBe(members[1].instance_id)
    await page.goto(`/instances/${worker.instance_id}`)
    const row = appRow(page, worker.deployment_id)
    await expect(row.getByTestId('detail-app-cluster')).toHaveAttribute('data-role', 'worker')
    await expect(row.getByTestId('detail-app-cluster')).toHaveText('集群应用（工作节点 worker）')
    await expect(row.getByTestId('detail-app-cluster-hint')).toContainText('该部署属于集群应用，只读')
    await expect(row.getByTestId('detail-app-actions')).toHaveCount(0)
    await expect(row.getByTestId('detail-app-edit-params')).toHaveCount(0)
    const stop = await api.post(`/api/v1/instances/${worker.instance_id}/apps/${worker.deployment_id}/stop`, { data: {} })
    expect(stop.status()).toBe(409)
    expect(((await stop.json()) as { code: string }).code).toBe('app_cluster_managed')
    const del = await api.delete(`/api/v1/instances/${worker.instance_id}/apps/${worker.deployment_id}`, { headers: { 'Content-Type': 'application/json' } })
    expect(del.status()).toBe(409)
    await row.getByTestId('detail-app-cluster-link').click()
    await expect(page).toHaveURL(new RegExp(`/clusters/${clusterId}$`))
  })

  await test.step('停止：整组停止后显示已停止与「启动」', async () => {
    await page.getByTestId('cluster-app-stop').click()
    await toast(page, '正在停止 vLLM 多机推理')
    await tickClusterApp(api, clusterId, 'stopped')
    await page.reload()
    await expect(page.getByTestId('cluster-app-status')).toHaveAttribute('data-status', 'stopped')
    await expect(page.getByTestId('cluster-app-status')).toHaveText('已停止')
    await expect(page.getByTestId('cluster-app-start')).toBeVisible()
    await expect(page.getByTestId('cluster-app-stop')).toHaveCount(0)
  })

  await test.step('卸载：确认文案、取消不发请求；确认后全部成员卸载，回到「部署应用」', async () => {
    const dels: string[] = []
    page.on('request', (r) => {
      if (r.method() === 'DELETE' && new URL(r.url()).pathname.startsWith(`/api/v1/clusters/${clusterId}/apps/`)) dels.push(r.url())
    })
    await page.getByTestId('cluster-app-remove').click()
    await expect(page.getByTestId('cluster-app-remove-text')).toContainText('将在全部 2 台成员上卸载该应用')
    await page.getByTestId('cluster-app-remove-cancel').click()
    await expect(page.getByTestId('cluster-app-remove-dialog')).toBeHidden()
    expect(dels).toHaveLength(0)
    await page.getByTestId('cluster-app-remove').click()
    await page.getByTestId('cluster-app-remove-ok').click()
    await toast(page, '正在卸载 vLLM 多机推理')
    expect(dels).toHaveLength(1)
    await tickClusterApp(api, clusterId, 'removed')
    await page.reload()
    await expect(page.getByTestId('cluster-app-empty')).toBeVisible()
    await expect(page.getByTestId('cluster-app-deploy')).toBeVisible()
  })
})

test('C-CLU-10 无可用集群：两种台数都不可用时选项禁用并说明；所选机组凑不齐（普通机器 / 旧 Agent）时下一步禁用；集群暂不支持预订（接口 400）', async ({ page }) => {
  const u = await seedUser()
  const api = await userApi(u.phone)
  const plain = await privateGroup(2) // 没有集群网络的普通机器
  const old = await clusterGroup(2, { oldAgent: true }) // 旧 Agent：不支持集群
  const ok = await clusterGroup(2) // 保证全站至少能开一个 2 台集群（选项可点）
  await loginConsole(page, u.phone)

  await test.step('商品的 clusters 两种台数都为 0：集群选项禁用并提示', async () => {
    await page.route('**/api/v1/products', async (route) => {
      const resp = await route.fetch()
      const body = (await resp.json()) as { items: Record<string, unknown>[] }
      for (const it of body.items) it.clusters = { '2': { available: 0, rdma: false }, '4': { available: 0, rdma: false } }
      await route.fulfill({ response: resp, json: body })
    })
    await openRentForm(page)
    await expect(page.getByTestId('rent-mode-cluster-2').locator('input')).toBeDisabled()
    await expect(page.getByTestId('rent-mode-cluster-4').locator('input')).toBeDisabled()
    await expect(page.getByTestId('rent-cluster-unavailable')).toHaveText('暂无可组成集群的空闲机器（需同一高速网络）')
    await expect(page.getByTestId('rent-mode-single').locator('input')).toBeChecked()
    await page.unroute('**/api/v1/products')
  })

  await test.step('普通机器 / 旧 Agent 的机组凑不出集群：暂无、下一步禁用；换到就绪机组后可开 1 个', async () => {
    await openRentForm(page, plain.name)
    await expect(page.getByTestId('rent-cluster-unavailable')).toHaveCount(0)
    await page.getByTestId('rent-mode-cluster-2').click()
    await expect(page.getByTestId('rent-cluster-available')).toHaveText('暂无可组成该规模集群的空闲机器')
    await expect(page.getByTestId('rent-availability')).toHaveText('暂无可组成 2 台集群的空闲机器，请稍后再试或改为单机')
    await expect(page.getByTestId('rent-next')).toBeDisabled()
    await expect(page.getByTestId('rent-cluster-note')).toContainText('集群暂不支持预订')
    await expect(page.getByTestId('rent-start-later')).toHaveCount(0)
    await pickGroup(page, old.groupName)
    await expect(page.getByTestId('rent-cluster-available')).toHaveText('暂无可组成该规模集群的空闲机器')
    await expect(page.getByTestId('rent-next')).toBeDisabled()
    await pickGroup(page, ok.groupName)
    await expect(page.getByTestId('rent-cluster-available')).toHaveText('可开 1 个')
    await expect(page.getByTestId('rent-next')).toBeEnabled()
  })

  await test.step('接口：集群带开始时间 → cluster_booking_unsupported；3 台 → cluster_size_invalid；凑不齐 → no_capacity', async () => {
    const products = (await (await api.get('/api/v1/products')).json()).items as { id: number }[]
    const images = (await (await api.get('/api/v1/images')).json()).items as { id: number }[]
    const start = new Date((await hooks.clock()).getTime() + 3 * 3600_000)
    start.setUTCMinutes(0, 0, 0)
    const book = (extra: Record<string, unknown>) =>
      api.post('/api/v1/bookings', {
        data: { product_id: products[0].id, image_id: images[0].id, count: 2, duration: '1d', quantity: 1, cluster: true, group_id: ok.groupId, ...extra },
        headers: { 'Idempotency-Key': crypto.randomUUID() },
      })
    const later = await book({ start_at: start.toISOString() })
    expect(later.status()).toBe(400)
    expect(await later.json()).toMatchObject({ code: 'cluster_booking_unsupported', message: '集群暂不支持预订，请选择立即开通' })
    const three = await book({ count: 3 })
    expect(three.status()).toBe(400)
    expect(((await three.json()) as { code: string }).code).toBe('cluster_size_invalid')
    const none = await book({ group_id: old.groupId })
    expect(none.status()).toBe(409)
    expect(((await none.json()) as { code: string }).code).toBe('no_capacity')
    const avail = await api.get(`/api/v1/availability?product_id=${products[0].id}&cluster_size=2&start_at=${encodeURIComponent(start.toISOString())}`)
    expect(avail.status()).toBe(400)
    expect(((await avail.json()) as { code: string }).code).toBe('cluster_booking_unsupported')
  })
})
