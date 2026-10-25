import { expect, test, type APIRequestContext, type Page } from '@playwright/test'
import {
  adminApi, clickTwiceSync, createAdmin, drainIdleMachines, drainTasks, enterTotpDialog, expectAdminUrl, expectStillOpen, fail500, failOnce, getJSON, isPath, loginAdmin, logout, pick, recordRequests, type SeededAdmin, totpNow, trackToasts,
} from '../helpers/admin'
import { cleanupApps, deployViaApi, ownInstance, seedApps, tickUntil } from '../helpers/apps'
import { bookViaApi, privateGroup, startAfter } from '../helpers/booking'
import { placeOrder, seedUser, toast, userApi } from '../helpers/console'
import { hooks } from '../helpers/hooks'
import { runningInstance } from '../helpers/instances'
import { cleanupLLM, createPoolMachineAndNode, seedLLMModel } from '../helpers/llm'
import { yuan } from '../helpers/money'
import { comfySetup, installedRunning, installWorkflowViaApi, seedExtraWorkflows } from '../helpers/workflows'
import { freshPublicKey, XSS, xssFired } from '../helpers/sshkeys'
import { cstTime, duration } from '../helpers/time'

type AdminInst = {
  id: string; user_id: number; machine_id: number; product_id: number; image_id: number; state: string; suspended: boolean
  pending_seconds: number; remaining_seconds: number; expires_at: string | null; created_at: string
}
type Detail = {
  instance: AdminInst
  events: { from_state: string; to_state: string; reason: string; at: string }[]
  orders: { id: string; kind: string; amount: number; refunded_amount: number; status: string }[]
  commands: { type: string; status: string; attempts: number; result: string }[]
}
type Quote = { remaining_seconds: number; suggested: number; max: number }

const stateText: Record<string, string> = { creating: '开通中', running: '运行中', stopped: '已到期停机', wiping: '洗机中', released: '已释放', failed: '开通失败' }
const cmdType: Record<string, string> = { create_instance: '开通实例', start_instance: '启动实例', stop_instance: '停止实例', wipe_instance: '洗机' }
const cmdStatus: Record<string, string> = { pending: '待执行', sent: '已下发', done: '已完成', failed: '失败' }
const orderStatus: Record<string, string> = { paid: '已支付', refunded: '已退款', partially_refunded: '部分退款' }

const detailOf = (ctx: APIRequestContext, id: string) => getJSON<Detail>(ctx, `/admin/api/v1/instances/${id}`)
const eventText = (e: Detail['events'][number]) => `${e.from_state || '—'} → ${e.to_state} · ${e.reason} · ${cstTime(e.at)}`
const balanceOf = async (api: APIRequestContext) => ((await (await api.get('/api/v1/wallet')).json()) as { balance: number }).balance

const rows = (page: Page) => page.getByRole('row').filter({ has: page.getByTestId('ainst-id') })

// A-INS-14 / 15 部署了应用：afterEach 卸载（其余用例没有部署，清理是空操作）
test.afterEach(cleanupApps)
// A-INS-16 造了 Token 商城模型与平台节点：afterEach 下线模型、释放节点、机器移出池
test.afterEach(cleanupLLM)
const rowOf = (page: Page, id: string) => rows(page).filter({ has: page.getByTestId('ainst-id').getByText(id, { exact: true }) })

test('A-INS-01 实例列表：状态 / 用户 / 机器筛选作为查询参数、地址栏预填、重置、超过一页翻页、首次加载失败可重试', async ({ page }) => {
  // 数据库整轮共享：断言只针对本用例造的用户与实例
  const a = await seedUser({ balance: 300000 })
  const b = await seedUser()
  const aApi = (await runningInstance(a.phone)).api
  await hooks.machines(20)
  for (let i = 0; i < 20; i++) await placeOrder(aApi)
  await hooks.agent() // a 的 21 个实例全部开通成功
  const ib = await runningInstance(b.phone)
  // c：一个真实洗机中（释放后不让 Agent 完成）、一个真实开通中（下单后不让 Agent 完成）；此后本用例不再调用 hooks.agent
  const c = await seedUser()
  const wipingC = await runningInstance(c.phone)
  await wipingC.api.post(`/api/v1/instances/${wipingC.id}/release`, { data: {} })
  await hooks.machines(1)
  const creatingId = await placeOrder(wipingC.api)
  const admin = await createAdmin('finance')
  const ctx = await adminApi(admin)
  const aList = await getJSON<{ items: AdminInst[]; total: number }>(ctx, `/admin/api/v1/instances?user_id=${a.id}&page_size=100`)
  expect(aList.total).toBe(21)
  const bDetail = (await detailOf(ctx, ib.id)).instance
  const images = (await getJSON<{ items: { id: number; name: string }[] }>(ctx, '/admin/api/v1/images')).items

  const listRequests = recordRequests(page, 'GET', '/instances')
  const searches = () => listRequests.map((r) => r.search)
  const isList = isPath('/instances')

  await test.step('首次加载失败：页内错误与重试，不自动重试；重试后正常显示', async () => {
    await page.route(isList, fail500('req-ins-1'))
    await loginAdmin(page, admin)
    await page.getByTestId('menu-instances').click()
    await expectAdminUrl(page, '/instances')
    await expect(page.getByTestId('ainst-error')).toContainText('服务器内部错误（请求号 req-ins-1）')
    await expect(rows(page)).toHaveCount(0)
    await drainTasks(page)
    expect(searches()).toEqual(['?page=1&page_size=20'])
    await page.unroute(isList)
    await page.getByTestId('ainst-retry').click()
    await expect(page.getByTestId('ainst-error')).toHaveCount(0)
    await expect(rows(page).first()).toBeVisible()
    expect(searches()).toEqual(['?page=1&page_size=20', '?page=1&page_size=20'])
  })

  await test.step('用户筛选：user_id 作为查询参数；21 个实例第 1 页 20 行，各列与接口一致；财务看机器只显示 ID', async () => {
    await page.getByTestId('ainst-filter-user').fill(`${a.id}x`) // 非数字被过滤
    await expect(page.getByTestId('ainst-filter-user')).toHaveValue(String(a.id))
    await page.getByTestId('ainst-search').click()
    await expect.poll(() => searches().at(-1)).toBe(`?user_id=${a.id}&page=1&page_size=20`)
    await expect(page.getByTestId('ainst-total')).toHaveText('共 21 个')
    await expect(rows(page)).toHaveCount(20)
    await expect(page.getByTestId('ainst-user')).toHaveText(Array(20).fill(String(a.id)))
    // 登录推进过假时钟：剩余时长按登录之后（此刻）的接口数据比对
    const first = (await getJSON<{ items: AdminInst[] }>(ctx, `/admin/api/v1/instances?user_id=${a.id}&page_size=1`)).items[0]
    expect(first.id).toBe(aList.items[0].id)
    const row = rows(page).first()
    await expect(row.getByTestId('ainst-id')).toHaveText(first.id)
    await expect(row.getByTestId('ainst-machine')).toHaveText(String(first.machine_id))
    await expect(row.getByTestId('ainst-machine').locator('a')).toHaveCount(0)
    await expect(row.getByTestId('ainst-user').locator('a')).toHaveAttribute('href', `/admin/users/${a.id}`)
    await expect(row.getByTestId('ainst-image')).toHaveText(images.find((i) => i.id === first.image_id)!.name)
    await expect(row.getByTestId('ainst-state')).toHaveText('运行中')
    await expect(row.getByTestId('ainst-suspended')).toHaveCount(0)
    await expect(row.getByTestId('ainst-time')).toHaveText(`剩余 ${duration(first.remaining_seconds)}`)
    await expect(row.getByTestId('ainst-expires')).toHaveText(cstTime(first.expires_at!))
    await expect(row.getByTestId('ainst-created')).toHaveText(cstTime(first.created_at))
  })

  await test.step('翻到第 2 页：page=2，显示剩下 1 行', async () => {
    await page.getByTestId('ainst-pager').getByText('2', { exact: true }).click()
    await expect.poll(() => searches().at(-1)).toBe(`?user_id=${a.id}&page=2&page_size=20`)
    await expect(rows(page)).toHaveCount(1)
    await expect(rows(page).first().getByTestId('ainst-id')).toHaveText(aList.items[20].id)
  })

  await test.step('机器筛选：machine_id 作为查询参数，每行都是这台机器，含 b 的实例', async () => {
    await page.getByTestId('ainst-filter-user').fill('')
    await page.getByTestId('ainst-filter-machine').fill(String(bDetail.machine_id))
    await page.getByTestId('ainst-search').click()
    await expect.poll(() => searches().at(-1)).toBe(`?machine_id=${bDetail.machine_id}&page=1&page_size=20`)
    await expect(rowOf(page, ib.id)).toHaveCount(1)
    const n = await rows(page).count()
    await expect(page.getByTestId('ainst-machine')).toHaveText(Array(n).fill(String(bDetail.machine_id)))
  })

  await test.step('重置：清空条件，只发一次不带筛选的请求', async () => {
    const before = listRequests.length
    await page.getByTestId('ainst-reset').click()
    await expect.poll(() => searches().at(-1)).toBe('?page=1&page_size=20')
    await expect(page.getByTestId('ainst-filter-machine')).toHaveValue('')
    await drainTasks(page)
    expect(listRequests).toHaveLength(before + 1)
  })

  await test.step('状态筛选：b 的实例按「开通失败」为空、按「运行中」命中', async () => {
    await page.getByTestId('ainst-filter-user').fill(String(b.id))
    await pick(page, 'ainst-filter-state', '开通失败')
    await page.getByTestId('ainst-search').click()
    await expect.poll(() => searches().at(-1)).toBe(`?state=failed&user_id=${b.id}&page=1&page_size=20`)
    await expect(page.getByTestId('ainst-total')).toHaveText('共 0 个')
    await expect(rows(page)).toHaveCount(0)
    await pick(page, 'ainst-filter-state', '运行中')
    await page.getByTestId('ainst-search').click()
    await expect.poll(() => searches().at(-1)).toBe(`?state=running&user_id=${b.id}&page=1&page_size=20`)
    await expect(page.getByTestId('ainst-total')).toHaveText('共 1 个')
    await expect(rows(page).first().getByTestId('ainst-id')).toHaveText(ib.id)
    await expect(rows(page).first().getByTestId('ainst-state')).toHaveText('运行中')
  })

  await test.step('地址栏预填 ?user_id=&machine_id=&state=：首个请求就带上，输入框显示预填值', async () => {
    const before = listRequests.length
    await page.goto(`/admin/instances?user_id=${a.id}&state=running`)
    await expect.poll(() => listRequests.length).toBe(before + 1)
    expect(searches().at(-1)).toBe(`?state=running&user_id=${a.id}&page=1&page_size=20`)
    await expect(page.getByTestId('ainst-filter-user')).toHaveValue(String(a.id))
    await expect(page.getByTestId('ainst-filter-state')).toContainText('运行中')
    await expect(page.getByTestId('ainst-total')).toHaveText('共 21 个')
    await page.goto(`/admin/instances?machine_id=${bDetail.machine_id}&user_id=${b.id}`)
    await expect.poll(() => searches().at(-1)).toBe(`?user_id=${b.id}&machine_id=${bDetail.machine_id}&page=1&page_size=20`)
    await expect(page.getByTestId('ainst-filter-machine')).toHaveValue(String(bDetail.machine_id))
    await expect(rows(page)).toHaveCount(1)
    await expect(rows(page).first().getByTestId('ainst-id')).toHaveText(ib.id)
  })

  await test.step('同页换查询串（点菜单回到 /instances）：按新查询串重新预填并请求', async () => {
    const before = listRequests.length
    await page.getByTestId('menu-instances').click()
    await expectAdminUrl(page, '/instances')
    await expect.poll(() => searches().at(-1)).toBe('?page=1&page_size=20')
    await expect(page.getByTestId('ainst-filter-user')).toHaveValue('')
    await expect(page.getByTestId('ainst-filter-machine')).toHaveValue('')
    await drainTasks(page)
    expect(listRequests).toHaveLength(before + 1)
  })

  await test.step('真实开通中实例显示「待开始」时长、无到期时间；真实洗机中实例剩余显示 —', async () => {
    const cList = (await getJSON<{ items: AdminInst[] }>(ctx, `/admin/api/v1/instances?user_id=${c.id}`)).items
    const creating = cList.find((i) => i.id === creatingId)!
    expect(creating.state).toBe('creating')
    expect(creating.pending_seconds).toBeGreaterThan(0)
    expect(cList.find((i) => i.id === wipingC.id)!.state).toBe('wiping')
    await page.goto(`/admin/instances?user_id=${c.id}`)
    await expect(rows(page)).toHaveCount(2)
    await expect(rowOf(page, creatingId).getByTestId('ainst-state')).toHaveText('开通中')
    await expect(rowOf(page, creatingId).getByTestId('ainst-time')).toHaveText(`待开始 ${duration(creating.pending_seconds)}`)
    await expect(rowOf(page, creatingId).getByTestId('ainst-expires')).toHaveText('—')
    await expect(rowOf(page, wipingC.id).getByTestId('ainst-state')).toHaveText('洗机中')
    await expect(rowOf(page, wipingC.id).getByTestId('ainst-time')).toHaveText('—')
  })

  await test.step('暂停计时标签（真实暂停需要全局离线巡检，改写本用例实例的列表响应）', async () => {
    await page.route(isList, async (r) => {
      const resp = await r.fetch()
      const body = (await resp.json()) as { items: AdminInst[] }
      body.items = body.items.map((i) => (i.id === ib.id ? { ...i, suspended: true } : i))
      await r.fulfill({ response: resp, json: body })
    })
    await page.goto(`/admin/instances?machine_id=${bDetail.machine_id}`)
    await expect(rowOf(page, ib.id).getByTestId('ainst-suspended')).toHaveText('计时已暂停')
    await expect(page.getByTestId('ainst-suspended')).toHaveCount(1)
    await page.unroute(isList)
  })

  await test.step('镜像名称加载失败：只提示一次，列表照常显示，镜像列退回镜像 ID', async () => {
    const isImages = isPath('/images')
    await page.route(isImages, fail500('req-ins-limg'))
    const toasts = await trackToasts(page)
    await page.goto(`/admin/instances?user_id=${b.id}`)
    await expect(rowOf(page, ib.id).getByTestId('ainst-image')).toHaveText(String(bDetail.image_id))
    await expect(rowOf(page, ib.id).getByTestId('ainst-state')).toHaveText('运行中')
    await expect(page.getByTestId('ainst-error')).toHaveCount(0)
    await drainTasks(page)
    expect(await toasts()).toEqual(['服务器内部错误（请求号 req-ins-limg）'])
    await page.unroute(isImages)
  })

  await test.step('运维看机器是链接；点实例 ID 进详情', async () => {
    const ops = await createAdmin('ops')
    await logout(page)
    await loginAdmin(page, ops)
    await page.goto(`/admin/instances?user_id=${b.id}`)
    await expect(rowOf(page, ib.id).getByTestId('ainst-machine').locator('a')).toHaveAttribute('href', `/admin/machines/${bDetail.machine_id}`)
    await rowOf(page, ib.id).getByTestId('ainst-id').getByRole('link').click()
    await expectAdminUrl(page, `/instances/${ib.id}`)
    await expect(page.getByTestId('adetail-id')).toHaveText(ib.id)
  })
})

test('A-INS-02 实例详情：基本信息、时间线、订单、指令按接口逐项显示；首次加载失败可重试；名称加载失败只提示一次', async ({ page }) => {
  const u = await seedUser()
  const { id } = await runningInstance(u.phone)
  const admin = await createAdmin('ops')
  const ctx = await adminApi(admin)
  const toasts = await trackToasts(page)
  const gets = recordRequests(page, 'GET', `/instances/${id}`)
  const isDetail = isPath(`/instances/${id}`)
  const isImages = isPath('/images')

  await test.step('首次加载失败：页内错误与重试，不自动重试', async () => {
    await page.route(isDetail, fail500('req-ins-2'))
    await loginAdmin(page, admin)
    await page.goto(`/admin/instances/${id}`)
    await expect(page.getByTestId('adetail-error')).toContainText('服务器内部错误（请求号 req-ins-2）')
    await expect(page.getByTestId('adetail-id')).toHaveCount(0)
    await drainTasks(page)
    expect(gets).toHaveLength(1)
    expect(await toasts()).toEqual(['服务器内部错误（请求号 req-ins-2）'])
    await page.unroute(isDetail)
    await page.getByTestId('adetail-retry').click()
    await expect(page.getByTestId('adetail-id')).toHaveText(id)
    await expect(page.getByTestId('adetail-error')).toHaveCount(0)
    expect(gets).toHaveLength(2)
  })

  const d = await detailOf(ctx, id)
  const products = (await getJSON<{ items: { id: number; name: string }[] }>(ctx, '/admin/api/v1/products')).items
  const images = (await getJSON<{ items: { id: number; name: string }[] }>(ctx, '/admin/api/v1/images')).items

  await test.step('基本信息', async () => {
    await expect(page.getByTestId('adetail-state')).toHaveText('运行中')
    await expect(page.getByTestId('adetail-suspended')).toHaveCount(0)
    await expect(page.getByTestId('adetail-user')).toHaveText(String(u.id))
    await expect(page.getByTestId('adetail-user').locator('a')).toHaveAttribute('href', `/admin/users/${u.id}`)
    await expect(page.getByTestId('adetail-machine')).toHaveText(String(d.instance.machine_id))
    await expect(page.getByTestId('adetail-machine').locator('a')).toHaveAttribute('href', `/admin/machines/${d.instance.machine_id}`)
    await expect(page.getByTestId('adetail-product')).toHaveText(products.find((p) => p.id === d.instance.product_id)!.name)
    await expect(page.getByTestId('adetail-image')).toHaveText(images.find((i) => i.id === d.instance.image_id)!.name)
    await expect(page.getByTestId('adetail-image')).toHaveText('Ubuntu 24.04 + CUDA')
    await expect(page.getByTestId('adetail-remaining')).toHaveText(duration(d.instance.remaining_seconds))
    await expect(page.getByTestId('adetail-remaining')).toHaveAttribute('data-remaining', String(d.instance.remaining_seconds))
    await expect(page.getByTestId('adetail-pending')).toHaveText('—')
    await expect(page.getByTestId('adetail-expires')).toHaveText(cstTime(d.instance.expires_at!))
  })

  await test.step('时间线：每条「from → to · 原因 · 时间」，含 creating → running', async () => {
    await expect(page.getByTestId('adetail-event')).toHaveText(d.events.map(eventText))
    await expect(page.getByTestId('adetail-event').filter({ hasText: /^creating → running · / })).toHaveCount(1)
    await expect(page.getByTestId('adetail-event').filter({ hasText: /^— → creating · / })).toHaveCount(1)
  })

  await test.step('订单：新购 ¥69.00、已退 ¥0.00、已支付', async () => {
    const order = page.getByTestId('adetail-order-row')
    await expect(order).toHaveCount(1)
    await expect(order.getByTestId('adetail-order-id')).toHaveText(d.orders[0].id)
    await expect(order.getByTestId('adetail-order-kind')).toHaveText('新购')
    await expect(order.getByTestId('adetail-order-amount')).toHaveText('¥69.00')
    await expect(order.getByTestId('adetail-order-refunded')).toHaveText('¥0.00')
    await expect(order.getByTestId('adetail-order-status')).toHaveText('已支付')
  })

  await test.step('指令：开通实例 · 已完成，尝试次数与结果与接口一致', async () => {
    const cmd = page.getByTestId('adetail-cmd-row')
    await expect(cmd).toHaveCount(1)
    await expect(cmd.getByTestId('adetail-cmd-type')).toHaveText('开通实例')
    await expect(cmd.getByTestId('adetail-cmd-status')).toHaveText('已完成')
    await expect(cmd.getByTestId('adetail-cmd-attempts')).toHaveText(String(d.commands[0].attempts))
    await expect(cmd.getByTestId('adetail-cmd-result')).toHaveText(d.commands[0].result)
    expect(d.commands[0].status).toBe('done')
  })

  await test.step('暂停计时标签（真实暂停需要全局离线巡检，改写本实例的详情响应）', async () => {
    await expect(page.getByTestId('adetail-suspended')).toHaveCount(0)
    await page.route(isDetail, async (r) => {
      const resp = await r.fetch()
      const body = (await resp.json()) as Detail
      await r.fulfill({ response: resp, json: { ...body, instance: { ...body.instance, suspended: true } } })
    })
    await page.reload()
    await expect(page.getByTestId('adetail-suspended')).toHaveText('计时已暂停')
    await page.unroute(isDetail)
  })

  await test.step('镜像 / 商品名称加载失败：只提示一次，详情照常显示（名称退回 ID）', async () => {
    await page.route(isImages, fail500('req-ins-img'))
    await page.reload()
    await expect(page.getByTestId('adetail-id')).toHaveText(id)
    await expect(page.getByTestId('adetail-image')).toHaveText(String(d.instance.image_id))
    await drainTasks(page)
    expect(await toasts()).toEqual(['服务器内部错误（请求号 req-ins-img）'])
    await page.unroute(isImages)
  })
})

test('A-INS-03 免费延长：范围与原因前端拦截、小时 / 天换算、剩余与到期更新、审计、失败保留输入、连点只发一次', async ({ page }) => {
  const u = await seedUser()
  const { id } = await runningInstance(u.phone)
  const admin = await createAdmin('ops')
  const ctx = await adminApi(admin)
  const posts = recordRequests(page, 'POST', `/instances/${id}/extend`)
  const toasts = await trackToasts(page)
  await loginAdmin(page, admin)
  await page.goto(`/admin/instances/${id}`)
  const before = (await detailOf(ctx, id)).instance
  await expect(page.getByTestId('adetail-remaining')).toHaveAttribute('data-remaining', String(before.remaining_seconds))
  const amount = page.getByTestId('extend-amount').locator('input')

  await test.step('31 天超出范围；1.5 小时缺原因；0.01 小时（36 秒）不足 1 分钟；都不发请求', async () => {
    await page.getByTestId('adetail-extend-btn').click()
    await expect(page.getByTestId('extend-dialog')).toBeVisible()
    await expect(amount).toHaveValue('1')
    await expect(page.getByTestId('extend-unit')).toContainText('天')
    await amount.fill('31')
    await page.getByTestId('extend-reason').fill('机器故障补偿')
    await page.getByTestId('extend-submit').click()
    await expect(page.getByTestId('extend-error')).toHaveText('补偿时长需在 1 分钟到 30 天之间')
    await pick(page, 'extend-unit', '小时')
    await amount.fill('1.5')
    await page.getByTestId('extend-reason').fill('  ')
    await page.getByTestId('extend-submit').click()
    await expect(page.getByTestId('extend-error')).toHaveText('请填写补偿原因')
    await amount.fill('0.01')
    await page.getByTestId('extend-reason').fill('网络抖动补偿')
    await page.getByTestId('extend-submit').click()
    await expect(page.getByTestId('extend-error')).toHaveText('补偿时长需在 1 分钟到 30 天之间')
    await drainTasks(page)
    expect(posts).toHaveLength(0)
  })

  await test.step('接口失败：只提示一次，弹窗不关、输入保留', async () => {
    await amount.fill('1.5')
    await failOnce(page, `/instances/${id}/extend`, 'req-ext-1')
    await page.getByTestId('extend-submit').click()
    await toast(page, '服务器内部错误（请求号 req-ext-1）')
    await drainTasks(page)
    expect(await toasts()).toEqual(['服务器内部错误（请求号 req-ext-1）'])
    await expectStillOpen(page, 'extend-dialog', '服务器内部错误（请求号 req-ext-1）')
    await expect(amount).toHaveValue('1.5')
    await expect(page.getByTestId('extend-reason')).toHaveValue('网络抖动补偿')
    await expect(page.getByTestId('extend-error')).toHaveCount(0)
    expect(posts).toHaveLength(1)
  })

  await test.step('1.5 小时 → seconds=5400；连点只发一次；剩余时长 +5400', async () => {
    await clickTwiceSync(page.getByTestId('extend-submit'))
    await toast(page, '已延长 1 小时 30 分')
    await expect(page.getByTestId('extend-dialog')).toBeHidden()
    await drainTasks(page)
    expect(posts.map((p) => p.body)).toEqual([{ seconds: 5400, reason: '网络抖动补偿' }, { seconds: 5400, reason: '网络抖动补偿' }])
    await expect(page.getByTestId('adetail-remaining')).toHaveAttribute('data-remaining', String(before.remaining_seconds + 5400))
  })

  await test.step('2 天 → seconds=172800；剩余 +48 小时、到期时间同步更新；时间线新增事件；审计 2 条', async () => {
    await page.getByTestId('adetail-extend-btn').click()
    await expect(amount).toHaveValue('1') // 重新打开是全新表单：1 天、无原因
    await expect(page.getByTestId('extend-unit')).toContainText('天')
    await expect(page.getByTestId('extend-reason')).toHaveValue('')
    await amount.fill('2')
    await page.getByTestId('extend-reason').fill('机器故障补偿')
    await page.getByTestId('extend-submit').click()
    await toast(page, '已延长 2 天')
    expect(posts.at(-1)!.body).toEqual({ seconds: 172800, reason: '机器故障补偿' })
    const after = (await detailOf(ctx, id)).instance
    expect(after.remaining_seconds).toBe(before.remaining_seconds + 5400 + 172800)
    expect(Date.parse(after.expires_at!) - Date.parse(before.expires_at!)).toBe((5400 + 172800) * 1000)
    await expect(page.getByTestId('adetail-remaining')).toHaveAttribute('data-remaining', String(after.remaining_seconds))
    await expect(page.getByTestId('adetail-remaining')).toHaveText(duration(after.remaining_seconds))
    await expect(page.getByTestId('adetail-expires')).toHaveText(cstTime(after.expires_at!))
    await expect(page.getByTestId('adetail-event').filter({ hasText: /^running → running · admin extend \+172800s: 机器故障补偿 · / })).toHaveCount(1)
    const audit = await getJSON<{ total: number }>(ctx, `/admin/api/v1/audit?action=instance.extend&target_id=${id}`)
    expect(audit.total).toBe(2)
    expect(posts).toHaveLength(3)
  })

  await test.step('延长成功但随后的详情刷新失败：仍提示成功，失败只提示一次，详情保留不变成错误页', async () => {
    const shown = (await detailOf(ctx, id)).instance.remaining_seconds
    await page.getByTestId('adetail-extend-btn').click()
    await pick(page, 'extend-unit', '小时')
    await page.getByTestId('extend-reason').fill('刷新失败测试')
    await failOnce(page, `/instances/${id}`, 'req-ext-refresh')
    const n = (await toasts()).length
    await page.getByTestId('extend-submit').click()
    await toast(page, '服务器内部错误（请求号 req-ext-refresh）')
    await drainTasks(page)
    expect((await toasts()).slice(n)).toEqual(['已延长 1 小时', '服务器内部错误（请求号 req-ext-refresh）'])
    expect(posts.at(-1)!.body).toEqual({ seconds: 3600, reason: '刷新失败测试' })
    await expect(page.getByTestId('extend-dialog')).toBeHidden()
    await expect(page.getByTestId('adetail-error')).toHaveCount(0)
    await expect(page.getByTestId('adetail-id')).toHaveText(id)
    await expect(page.getByTestId('adetail-remaining')).toHaveAttribute('data-remaining', String(shown)) // 保留刷新前的数据
    expect((await detailOf(ctx, id)).instance.remaining_seconds).toBe(shown + 3600) // 服务器确已延长
    await page.reload()
    await expect(page.getByTestId('adetail-remaining')).toHaveAttribute('data-remaining', String(shown + 3600))
  })
})

test('A-INS-04 换机：缺原因与无空闲机器提示且保留输入；成功提示新实例与新机器、密码变化，点新实例进入其详情', async ({ page }) => {
  const admin = await createAdmin('ops')
  const ctx = await adminApi(admin)
  await drainIdleMachines(ctx, admin)
  const u = await seedUser()
  const { id } = await runningInstance(u.phone) // 唯一空闲机器被占用
  const oldMachine = (await detailOf(ctx, id)).instance.machine_id
  const posts = recordRequests(page, 'POST', `/instances/${id}/migrate`)
  const toasts = await trackToasts(page)
  await loginAdmin(page, admin)
  await page.goto(`/admin/instances/${id}`)

  await test.step('缺原因：提示且不发请求', async () => {
    await page.getByTestId('adetail-migrate-btn').click()
    await page.getByTestId('migrate-submit').click()
    await toast(page, '请填写换机原因')
    await drainTasks(page)
    expect(posts).toHaveLength(0)
  })

  await test.step('无空闲机器：原文提示一次，弹窗不关、原因保留', async () => {
    await page.getByTestId('migrate-reason').fill('硬件告警')
    await page.getByTestId('migrate-submit').click()
    await toast(page, '暂无空闲机器，请稍后再试')
    await drainTasks(page)
    expect(await toasts()).toEqual(['请填写换机原因', '暂无空闲机器，请稍后再试'])
    await expectStillOpen(page, 'migrate-dialog', '暂无空闲机器，请稍后再试')
    await expect(page.getByTestId('migrate-reason')).toHaveValue('硬件告警')
    await expect(page.getByTestId('adetail-state')).toHaveText('运行中')
    expect(posts).toHaveLength(1)
  })

  let newId = ''
  let newMachine = 0
  await test.step('有空闲机器：连点只发一次；结果显示新实例与新机器、提示密码变化；原实例洗机中', async () => {
    await hooks.machines(1)
    const resp = page.waitForResponse((r) => r.url().endsWith(`/instances/${id}/migrate`) && r.request().method() === 'POST' && r.ok())
    await clickTwiceSync(page.getByTestId('migrate-submit'))
    const out = (await (await resp).json()) as { instance: AdminInst }
    newId = out.instance.id
    newMachine = out.instance.machine_id
    expect(newId).toMatch(/^ins-/)
    expect(newId).not.toBe(id)
    expect(newMachine).not.toBe(oldMachine)
    await expect(page.getByTestId('migrate-result')).toContainText('SSH 密码已变化，请通知用户')
    await expect(page.getByTestId('adetail-migrate-new')).toHaveText(newId)
    await expect(page.getByTestId('adetail-migrate-machine')).toHaveText(String(newMachine))
    await expect(page.getByTestId('adetail-migrate-machine').locator('a')).toHaveAttribute('href', `/admin/machines/${newMachine}`)
    await expect(page.getByTestId('migrate-dialog')).toBeHidden()
    await expect(page.getByTestId('adetail-state')).toHaveText('洗机中')
    await drainTasks(page)
    expect(posts).toHaveLength(2)
    expect(posts[1].body).toEqual({ reason: '硬件告警' })
  })

  await test.step('点新实例：地址变为新实例，详情按新 ID 重新加载，结果弹窗关闭', async () => {
    const newGets = recordRequests(page, 'GET', `/instances/${newId}`)
    await page.getByTestId('adetail-migrate-new').getByRole('link').click()
    await expectAdminUrl(page, `/instances/${newId}`)
    await expect(page.getByTestId('adetail-id')).toHaveText(newId)
    await expect(page.getByTestId('adetail-state')).toHaveText('开通中')
    await expect(page.getByTestId('adetail-machine')).toHaveText(String(newMachine))
    const fresh = (await detailOf(ctx, newId)).instance
    expect(fresh.pending_seconds).toBeGreaterThan(0) // 剩余时长转移到新实例，开通完成前记为待开始
    await expect(page.getByTestId('adetail-pending')).toHaveText(duration(fresh.pending_seconds))
    await expect(page.getByTestId('adetail-remaining')).toHaveText('—')
    await expect(page.getByTestId('migrate-result')).toHaveCount(0)
    await expect(page.getByTestId('adetail-event').filter({ hasText: new RegExp(`^— → creating · migrated from ${id}: 硬件告警 · `) })).toHaveCount(1)
    await drainTasks(page)
    expect(newGets).toHaveLength(1)
  })

  await test.step('先发后到的旧实例响应被丢弃：后退时旧实例请求被拖住，前进到新实例后它才返回，页面仍显示新实例', async () => {
    let release!: () => void
    const held = new Promise<void>((r) => (release = r))
    let caught = false
    const isOld = isPath(`/instances/${id}`)
    await page.route(isOld, async (r) => {
      caught = true
      await held
      await r.continue()
    })
    await page.goBack()
    await expectAdminUrl(page, `/instances/${id}`)
    await expect.poll(() => caught).toBe(true)
    await page.goForward()
    await expectAdminUrl(page, `/instances/${newId}`)
    await expect(page.getByTestId('adetail-id')).toHaveText(newId)
    const staleDone = page.waitForResponse((r) => r.url().endsWith(`/instances/${id}`))
    release()
    await staleDone
    await drainTasks(page)
    await expect(page.getByTestId('adetail-id')).toHaveText(newId)
    await expect(page.getByTestId('adetail-state')).toHaveText('开通中')
    await page.unroute(isOld)
  })
})

test('A-INS-05 洗机失败后重新洗机：需确认、取消不发请求、失败保留弹窗、连点只下发一次，指令表新增洗机', async ({ page }) => {
  const u = await seedUser()
  const { api, id } = await runningInstance(u.phone)
  await api.post(`/api/v1/instances/${id}/release`, { data: {} })
  await hooks.agent({ fail: true })
  const admin = await createAdmin('ops')
  const ctx = await adminApi(admin)
  const posts = recordRequests(page, 'POST', `/instances/${id}/rewipe`)
  const toasts = await trackToasts(page)
  await loginAdmin(page, admin)
  await page.goto(`/admin/instances/${id}`)
  await expect(page.getByTestId('adetail-state')).toHaveText('洗机中')
  const failed = (await detailOf(ctx, id)).commands.find((c) => c.type === 'wipe_instance')!
  expect(failed.status).toBe('failed')
  const wipeRows = page.getByTestId('adetail-cmd-row').filter({ has: page.getByTestId('adetail-cmd-type').getByText('洗机', { exact: true }) })
  await expect(wipeRows).toHaveCount(1)
  await expect(wipeRows.getByTestId('adetail-cmd-status')).toHaveText('失败')
  await expect(wipeRows.getByTestId('adetail-cmd-result')).toHaveText(failed.result)

  await test.step('取消确认：不发请求', async () => {
    await page.getByTestId('adetail-rewipe-btn').click()
    await expect(page.getByTestId('rewipe-dialog')).toBeVisible()
    await page.getByTestId('rewipe-cancel').click()
    await expect(page.getByTestId('rewipe-dialog')).toBeHidden()
    await drainTasks(page)
    expect(posts).toHaveLength(0)
  })

  await test.step('接口失败：只提示一次，确认框不关', async () => {
    await page.getByTestId('adetail-rewipe-btn').click()
    await failOnce(page, `/instances/${id}/rewipe`, 'req-rw-1')
    await page.getByTestId('rewipe-ok').click()
    await toast(page, '服务器内部错误（请求号 req-rw-1）')
    await drainTasks(page)
    expect(await toasts()).toEqual(['服务器内部错误（请求号 req-rw-1）'])
    await expectStillOpen(page, 'rewipe-dialog', '服务器内部错误（请求号 req-rw-1）')
    expect(posts).toHaveLength(1)
  })

  await test.step('连点确定只发一次；指令表新增一条洗机，经真实网关投递后为已下发', async () => {
    await clickTwiceSync(page.getByTestId('rewipe-ok'))
    await toast(page, '已重新下发洗机指令')
    await expect(page.getByTestId('rewipe-dialog')).toBeHidden()
    await expect(wipeRows).toHaveCount(2)
    await drainTasks(page)
    expect(posts).toHaveLength(2)
    // 新指令先是 pending，网关几十毫秒内投递给步进式 fake Agent 变为 sent；步进式 Agent 不调 hooks.agent() 就不执行，
    // 假时钟不推进也不会超时，所以 sent 是稳定终态。等到 sent 再刷新页面，界面与接口逐行一致。
    const wipeStatuses = async () => (await detailOf(ctx, id)).commands.filter((c) => c.type === 'wipe_instance').map((c) => c.status)
    await expect.poll(wipeStatuses).toEqual(['sent', 'failed']) // 指令表新的在前
    await page.reload()
    const cmds = (await detailOf(ctx, id)).commands
    expect(cmds.filter((c) => c.type === 'wipe_instance')).toHaveLength(2)
    await expect(page.getByTestId('adetail-cmd-type')).toHaveText(cmds.map((c) => cmdType[c.type]))
    await expect(page.getByTestId('adetail-cmd-status')).toHaveText(cmds.map((c) => cmdStatus[c.status]))
    await expect(wipeRows.getByTestId('adetail-cmd-status')).toContainText(['已下发'])
    await expect(page.getByTestId('adetail-event').filter({ hasText: /^wiping → wiping · admin rewipe · / })).toHaveCount(1)
  })
})

test('A-INS-06 退款并释放：建议金额与上限来自后端、金额校验、失败保留输入、动态码后原请求带 totp 重发一次、余额与订单已退金额、最终释放', async ({ page }) => {
  const u = await seedUser({ balance: 100000 })
  const { api, id } = await runningInstance(u.phone)
  const admin = await createAdmin('finance')
  const ctx = await adminApi(admin)
  const posts = recordRequests(page, 'POST', `/instances/${id}/refund-release`)
  const toasts = await trackToasts(page)
  await loginAdmin(page, admin)
  await page.goto(`/admin/instances/${id}`)
  const q = await getJSON<Quote>(ctx, `/admin/api/v1/instances/${id}/refund-quote`) // 假时钟在此之后不再推进，界面取到的报价与之相同
  expect(q.max).toBe(6900)
  expect(q.suggested).toBeGreaterThan(0)
  expect(q.suggested).toBeLessThanOrEqual(6900)

  await test.step('预填建议金额，提示建议、剩余时长与可退上限', async () => {
    await page.getByTestId('adetail-refund-btn').click()
    await expect(page.getByTestId('refund-dialog')).toBeVisible()
    await expect(page.getByTestId('refund-amount')).toHaveValue((q.suggested / 100).toFixed(2))
    await expect(page.getByTestId('refund-hint')).toHaveText(`建议 ${yuan(q.suggested)}（按剩余时长 ${duration(q.remaining_seconds)} 折算），最多可退 ${yuan(q.max)}`)
  })

  await test.step('金额校验：格式、负数、多于两位小数、超上限；缺原因；都不发请求', async () => {
    const submitExpect = async (value: string, message: string) => {
      await page.getByTestId('refund-amount').fill(value)
      await page.getByTestId('refund-submit').click()
      await expect(page.getByTestId('refund-error')).toHaveText(message)
    }
    await page.getByTestId('refund-reason').fill('机器故障')
    await submitExpect('abc', '金额格式不正确')
    await submitExpect('-1', '金额格式不正确')
    await submitExpect('1.234', '金额格式不正确')
    await submitExpect('69.01', '退款金额不能超过可退上限')
    await page.getByTestId('refund-reason').fill('')
    await submitExpect('1.00', '请填写退款原因')
    await drainTasks(page)
    expect(posts).toHaveLength(0)
  })

  const cents = q.suggested
  const amount = (cents / 100).toFixed(2)
  await test.step('接口失败：只提示一次，弹窗不关、金额与原因保留', async () => {
    await page.getByTestId('refund-amount').fill(amount)
    await page.getByTestId('refund-reason').fill('机器故障')
    await failOnce(page, `/instances/${id}/refund-release`, 'req-rf-1')
    await page.getByTestId('refund-submit').click()
    await toast(page, '服务器内部错误（请求号 req-rf-1）')
    await drainTasks(page)
    expect(await toasts()).toEqual(['服务器内部错误（请求号 req-rf-1）'])
    await expectStillOpen(page, 'refund-dialog', '服务器内部错误（请求号 req-rf-1）')
    await expect(page.getByTestId('refund-amount')).toHaveValue(amount)
    await expect(page.getByTestId('refund-reason')).toHaveValue('机器故障')
    await expect(page.getByTestId('refund-error')).toHaveCount(0)
    expect(posts).toHaveLength(1)
  })

  await test.step('服务器以超上限拒绝（400）：原文提示一次，弹窗不关、金额与原因保留', async () => {
    const isRefund = isPath(`/instances/${id}/refund-release`)
    await page.route(isRefund, (r) => r.fulfill({ status: 400, contentType: 'application/json', body: JSON.stringify({ code: 'bad_request', message: '退款金额不能超过可退金额 0.50 元' }) }), { times: 1 })
    const n = (await toasts()).length
    await page.getByTestId('refund-submit').click()
    await toast(page, '退款金额不能超过可退金额 0.50 元')
    await drainTasks(page)
    expect((await toasts()).slice(n)).toEqual(['退款金额不能超过可退金额 0.50 元'])
    await expectStillOpen(page, 'refund-dialog', '退款金额不能超过可退金额 0.50 元')
    await expect(page.getByTestId('refund-amount')).toHaveValue(amount)
    await expect(page.getByTestId('refund-reason')).toHaveValue('机器故障')
    await expect(page.getByTestId('totp-dialog')).toBeHidden()
    expect(posts).toHaveLength(2)
  })

  await test.step('连点提交：弹动态码，原请求带 totp 只重发一次；提示已退款', async () => {
    await clickTwiceSync(page.getByTestId('refund-submit'))
    await enterTotpDialog(page, admin.totp_secret!)
    await toast(page, `已退款 ${yuan(cents)}，实例进入洗机`)
    await expect(page.getByTestId('refund-dialog')).toBeHidden()
    await drainTasks(page)
    const bodies = posts.slice(2).map((p) => p.body)
    expect(bodies).toHaveLength(2)
    expect(bodies[0]).toEqual({ amount: cents, reason: '机器故障' })
    expect(bodies[1]).toEqual({ amount: cents, reason: '机器故障', totp: expect.stringMatching(/^\d{6}$/) })
  })

  await test.step('实例洗机中、退款按钮消失；订单已退金额与状态；用户余额增加', async () => {
    await expect(page.getByTestId('adetail-state')).toHaveText('洗机中')
    await expect(page.getByTestId('adetail-refund-btn')).toHaveCount(0)
    const d = await detailOf(ctx, id)
    expect(d.orders[0].refunded_amount).toBe(cents)
    await expect(page.getByTestId('adetail-order-refunded')).toHaveText([yuan(cents)])
    await expect(page.getByTestId('adetail-order-status')).toHaveText([orderStatus[d.orders[0].status]])
    expect(d.orders[0].status).toBe(cents === 6900 ? 'refunded' : 'partially_refunded')
    expect(await balanceOf(api)).toBe(100000 - 6900 + cents)
    await expect(page.getByTestId('adetail-event').filter({ hasText: /^running → wiping · admin refund-release: 机器故障 · / })).toHaveCount(1)
  })

  await test.step('洗机完成后实例已释放', async () => {
    await hooks.agent()
    await page.reload()
    await expect(page.getByTestId('adetail-state')).toHaveText('已释放')
    await expect(page.getByTestId('adetail-event').filter({ hasText: /^wiping → released · / })).toHaveCount(1)
  })

  await test.step('金额 0 = 只释放、不退款：显示提示，经动态码提交，实例释放，订单已退仍为 0、余额不变', async () => {
    await hooks.machines(1)
    const id2 = await placeOrder(api)
    await hooks.agent()
    await hooks.advance(301) // 让上一次动态码的 5 分钟窗口过期，这一次重新经过动态码
    const posts2 = recordRequests(page, 'POST', `/instances/${id2}/refund-release`)
    const before = await balanceOf(api)
    await page.goto(`/admin/instances/${id2}`)
    await expect(page.getByTestId('adetail-state')).toHaveText('运行中')
    await page.getByTestId('adetail-refund-btn').click()
    await expect(page.getByTestId('refund-dialog')).toBeVisible()
    await expect(page.getByTestId('refund-zero-hint')).toHaveCount(0) // 预填建议金额（> 0）时不提示
    await page.getByTestId('refund-amount').fill('0')
    await expect(page.getByTestId('refund-zero-hint')).toHaveText('金额为 0 表示只释放、不退款')
    await page.getByTestId('refund-reason').fill('只释放')
    await page.getByTestId('refund-submit').click()
    await enterTotpDialog(page, admin.totp_secret!)
    await toast(page, '已释放（未退款），实例进入洗机')
    await expect(page.getByTestId('refund-dialog')).toBeHidden()
    await expect(page.getByTestId('refund-error')).toHaveCount(0)
    await drainTasks(page)
    expect(posts2.map((p) => p.body)).toEqual([{ amount: 0, reason: '只释放' }, { amount: 0, reason: '只释放', totp: expect.stringMatching(/^\d{6}$/) }])
    await expect(page.getByTestId('adetail-state')).toHaveText('洗机中')
    const d = await detailOf(ctx, id2)
    expect(d.orders[0].refunded_amount).toBe(0)
    expect(d.orders[0].status).toBe('paid')
    await expect(page.getByTestId('adetail-order-refunded')).toHaveText(['¥0.00'])
    expect(await balanceOf(api)).toBe(before)
    await hooks.agent()
    await page.reload()
    await expect(page.getByTestId('adetail-state')).toHaveText('已释放')
  })
})

test('A-INS-07 五分钟内连续两次高风险操作只弹一次动态码；超过五分钟再次要求', async ({ page }) => {
  const u = await seedUser({ balance: 100000 })
  const first = await runningInstance(u.phone)
  await hooks.machines(2)
  const secondId = await placeOrder(first.api)
  const thirdId = await placeOrder(first.api)
  await hooks.agent()
  const admin = await createAdmin('finance')
  await loginAdmin(page, admin)
  const refund = async (id: string, prompt: boolean) => {
    const seen = recordRequests(page, 'POST', `/instances/${id}/refund-release`)
    await page.goto(`/admin/instances/${id}`)
    await page.getByTestId('adetail-refund-btn').click()
    await page.getByTestId('refund-reason').fill('批量退款')
    await page.getByTestId('refund-submit').click()
    if (prompt) await enterTotpDialog(page, admin.totp_secret!)
    await toast(page, /^已退款 ¥/)
    await expect(page.getByTestId('totp-dialog')).toBeHidden()
    await expect(page.getByTestId('adetail-state')).toHaveText('洗机中')
    await drainTasks(page)
    return seen.map((s) => 'totp' in (s.body ?? {}))
  }
  await test.step('第一次：弹动态码，无 totp → 带 totp', async () => {
    expect(await refund(first.id, true)).toEqual([false, true])
  })
  await test.step('五分钟内第二次：不弹动态码，只发一次不带 totp 的请求', async () => {
    expect(await refund(secondId, false)).toEqual([false])
  })
  await test.step('超过五分钟：后端窗口关闭，再次要求动态码', async () => {
    await hooks.advance(301)
    expect(await refund(thirdId, true)).toEqual([false, true])
  })
})

test('A-INS-08 动态码取消不执行且不提示；错误码提示后可重试', async ({ page }) => {
  const u = await seedUser({ balance: 100000 })
  const { api, id } = await runningInstance(u.phone)
  const admin = await createAdmin('finance')
  const posts = recordRequests(page, 'POST', `/instances/${id}/refund-release`)
  const toasts = await trackToasts(page)
  await loginAdmin(page, admin)
  await page.goto(`/admin/instances/${id}`)

  await test.step('取消动态码：只发出不带 totp 的那一次，无提示，退款弹窗保留输入；实例与余额不变', async () => {
    await page.getByTestId('adetail-refund-btn').click()
    const amount = await page.getByTestId('refund-amount').inputValue()
    await page.getByTestId('refund-reason').fill('测试取消')
    await page.getByTestId('refund-submit').click()
    await expect(page.getByTestId('totp-dialog')).toBeVisible()
    await page.getByTestId('totp-cancel').click()
    await expect(page.getByTestId('totp-dialog')).toBeHidden()
    await drainTasks(page)
    expect(posts.map((p) => p.body)).toEqual([{ amount: Math.round(Number(amount) * 100), reason: '测试取消' }])
    expect(await toasts()).toEqual([])
    await expect(page.getByTestId('refund-dialog')).toBeVisible()
    await expect(page.getByTestId('refund-amount')).toHaveValue(amount)
    await expect(page.getByTestId('refund-reason')).toHaveValue('测试取消')
    await page.reload()
    await expect(page.getByTestId('adetail-state')).toHaveText('运行中')
    expect(await balanceOf(api)).toBe(93100)
  })

  await test.step('错误码：动态码框内提示，可重新输入后成功', async () => {
    await page.getByTestId('adetail-refund-btn').click()
    await page.getByTestId('refund-reason').fill('测试重试')
    await page.getByTestId('refund-submit').click()
    await page.getByTestId('totp-input').fill('000000')
    await page.getByTestId('totp-ok').click()
    await expect(page.getByTestId('totp-message')).toContainText('动态码错误或已使用过')
    await page.getByTestId('totp-input').fill(await totpNow(admin.totp_secret!))
    await page.getByTestId('totp-ok').click()
    await toast(page, /^已退款 ¥/)
    await expect(page.getByTestId('adetail-state')).toHaveText('洗机中')
    await drainTasks(page)
    const second = posts.slice(1).map((p) => p.body?.totp ?? null)
    expect(second).toHaveLength(3)
    expect(second[0]).toBeNull()
    expect(second[1]).toBe('000000')
    expect(second[2]).toMatch(/^\d{6}$/)
  })
})

test('A-INS-09 操作按钮按角色与状态：财务只有退款、运维只有延长 / 换机 / 重新洗机、超级管理员都有', async ({ page }) => {
  const u = await seedUser()
  const { id: running } = await runningInstance(u.phone)
  const second = await runningInstance(u.phone)
  await second.api.post(`/api/v1/instances/${second.id}/release`, { data: {} }) // 洗机中（不让 Agent 完成）
  const all = ['adetail-extend-btn', 'adetail-migrate-btn', 'adetail-rewipe-btn', 'adetail-refund-btn']
  const expectButtons = async (id: string, state: string, visible: string[]) => {
    await page.goto(`/admin/instances/${id}`)
    await expect(page.getByTestId('adetail-state')).toHaveText(stateText[state])
    for (const t of all) await expect(page.getByTestId(t)).toHaveCount(visible.includes(t) ? 1 : 0)
  }
  const as = async (admin: SeededAdmin) => {
    await logout(page)
    await loginAdmin(page, admin)
  }

  await test.step('财务：运行中只有退款，机器只显示 ID；洗机中没有任何操作', async () => {
    await as(await createAdmin('finance'))
    await expectButtons(running, 'running', ['adetail-refund-btn'])
    await expect(page.getByTestId('adetail-machine').locator('a')).toHaveCount(0)
    await expectButtons(second.id, 'wiping', [])
  })
  await test.step('运维：运行中延长 + 换机，机器可点；洗机中只有重新洗机', async () => {
    await as(await createAdmin('ops'))
    await expectButtons(running, 'running', ['adetail-extend-btn', 'adetail-migrate-btn'])
    await expect(page.getByTestId('adetail-machine').locator('a')).toHaveCount(1)
    await expectButtons(second.id, 'wiping', ['adetail-rewipe-btn'])
  })
  await test.step('超级管理员：运行中延长 + 换机 + 退款；洗机中只有重新洗机', async () => {
    await as(await createAdmin('super'))
    await expectButtons(running, 'running', ['adetail-extend-btn', 'adetail-migrate-btn', 'adetail-refund-btn'])
    await expectButtons(second.id, 'wiping', ['adetail-rewipe-btn'])
  })
})

test('A-INS-10 实例详情「访问」卡片：密码登录、主机密钥、端口（备注按文本显示）与 SSH 会话；财务无端口处置按钮', async ({ page }) => {
  const u = await seedUser()
  const { api, id } = await runningInstance(u.phone)
  await api.post(`/api/v1/instances/${id}/ports`, { data: { port: 8888, visibility: 'public', note: XSS } })
  const pw = ((await (await api.get(`/api/v1/instances/${id}/password`)).json()) as { password: string }).password
  expect((await hooks.ssh({ instance_id: id, password: pw, command: 'echo 1' })).output).toBe('1\n')
  const admin = await createAdmin('finance')
  await loginAdmin(page, admin)
  await page.goto(`/admin/instances/${id}`)
  await expect(page.getByTestId('adetail-password-auth')).toHaveText('已开启')
  await expect(page.getByTestId('adetail-hostkey')).toHaveText(/^SHA256:/)
  await expect(page.getByTestId('adetail-port-row')).toHaveCount(1)
  await expect(page.getByTestId('adetail-port-url')).toHaveText(`http://${id}-8888.spark.test:18080/`)
  await expect(page.getByTestId('adetail-port-visibility')).toHaveText('公开')
  await expect(page.getByTestId('adetail-port-row')).toContainText(XSS)
  await expect(page.getByTestId('adetail-port-row').locator('img')).toHaveCount(0)
  expect(await xssFired(page)).toBe(false)
  await expect(page.getByTestId('adetail-port-force')).toHaveCount(0) // 财务无处置按钮
  await expect(page.getByTestId('adetail-port-delete')).toHaveCount(0)
  await expect(page.getByTestId('adetail-session-row')).toHaveCount(1)
  await expect(page.getByTestId('adetail-session-method')).toHaveText('密码')
  await expect(page.getByTestId('adetail-session-ip')).toHaveText('127.0.0.1')
  // 关闭密码登录后卡片显示「仅公钥」
  await api.post('/api/v1/ssh-keys', { data: { public_key: freshPublicKey() } })
  expect((await api.put(`/api/v1/instances/${id}/access`, { data: { password_auth: false } })).status()).toBe(200)
  await page.reload()
  await expect(page.getByTestId('adetail-password-auth')).toHaveText('已关闭（仅公钥）')
  await expect(page.getByTestId('adetail-session-row')).toHaveCount(1)

  // 同一组件换实例（站内跳转）时，旧实例迟到的会话列表被丢弃，不会显示在新实例下
  await hooks.machines(1)
  const other = await placeOrder(api)
  await hooks.agent()
  let release!: () => void
  const held = new Promise<void>((r) => (release = r))
  await page.route(`**/admin/api/v1/instances/${id}/ssh-sessions?*`, async (route) => {
    await held
    await route.continue()
  })
  await page.reload()
  await expect(page.getByTestId('adetail-id')).toHaveText(id)
  await page.evaluate((path) => {
    history.pushState(history.state, '', path)
    dispatchEvent(new PopStateEvent('popstate', { state: history.state }))
  }, `/admin/instances/${other}`)
  await expect(page.getByTestId('adetail-id')).toHaveText(other)
  await expect(page.getByTestId('adetail-sessions-empty')).toBeVisible()
  const lateA = page.waitForResponse((r) => r.url().includes(`/instances/${id}/ssh-sessions`))
  release()
  await lateA
  await drainTasks(page)
  await expect(page.getByTestId('adetail-session-row')).toHaveCount(0)
  await expect(page.getByTestId('adetail-sessions-empty')).toBeVisible()
  await page.unroute(`**/admin/api/v1/instances/${id}/ssh-sessions?*`)
})

test('A-INS-11 强制私有：需原因、写审计、用户端显示平台限制', async ({ page }) => {
  const u = await seedUser()
  const { api, id } = await runningInstance(u.phone)
  await api.post(`/api/v1/instances/${id}/ports`, { data: { port: 8888, visibility: 'public' } })
  const admin = await createAdmin('ops')
  await loginAdmin(page, admin)
  await page.goto(`/admin/instances/${id}`)
  await expect(page.getByTestId('adetail-port-visibility')).toHaveText('公开')
  const posts = recordRequests(page, 'POST', `/instances/${id}/ports/8888/force-private`)
  await page.getByTestId('adetail-port-force').click()
  await page.getByTestId('port-action-submit').click()
  await expect(page.getByTestId('port-action-error')).toHaveText('请填写原因')
  await page.getByTestId('port-action-reason').fill('   ')
  await page.getByTestId('port-action-submit').click()
  await expect(page.getByTestId('port-action-error')).toHaveText('请填写原因')
  expect(posts).toHaveLength(0)
  await page.getByTestId('port-action-reason').fill('钓鱼页面')
  await page.getByTestId('port-action-submit').click()
  await toast(page, '已限制为私有')
  expect(posts.map((p) => p.body)).toEqual([{ reason: '钓鱼页面' }])
  await expect(page.getByTestId('adetail-port-visibility')).toHaveText('私有')
  await expect(page.getByTestId('adetail-port-forced')).toBeVisible()
  await expect(page.getByTestId('adetail-port-force')).toHaveCount(0)
  const ctx = await adminApi(admin)
  const audit = (await getJSON<{ items: { action: string; target_id: string; diff: { reason: string; port: number } }[] }>(ctx, `/admin/api/v1/audit?action=instance.port_force_private&target_id=${id}`)).items
  expect(audit.map((a) => [a.target_id, a.diff.port, a.diff.reason])).toEqual([[id, 8888, '钓鱼页面']])
  const ports = ((await (await api.get(`/api/v1/instances/${id}/ports`)).json()) as { items: { visibility: string; forced_private: boolean }[] }).items
  expect(ports).toEqual([expect.objectContaining({ visibility: 'private', forced_private: true })])
})

test('A-INS-12 删除端口：需原因、写审计、用户端消失', async ({ page }) => {
  const u = await seedUser()
  const { api, id } = await runningInstance(u.phone)
  await api.post(`/api/v1/instances/${id}/ports`, { data: { port: 8888 } })
  const admin = await createAdmin('ops')
  await loginAdmin(page, admin)
  await page.goto(`/admin/instances/${id}`)
  await page.getByTestId('adetail-port-delete').click()
  await page.getByTestId('port-action-submit').click()
  await expect(page.getByTestId('port-action-error')).toHaveText('请填写原因')
  await expect(page.getByTestId('adetail-port-row')).toHaveCount(1)
  await page.getByTestId('port-action-reason').fill('abuse')
  await page.getByTestId('port-action-submit').click()
  await toast(page, '已删除端口')
  await expect(page.getByTestId('adetail-port-row')).toHaveCount(0)
  const list = (await (await api.get(`/api/v1/instances/${id}/ports`)).json()) as { items: unknown[] }
  expect(list.items).toHaveLength(0)
  const ctx = await adminApi(admin)
  const audit = (await getJSON<{ items: { target_id: string; actor_id: number; diff: { reason: string; port: number } }[] }>(ctx, `/admin/api/v1/audit?action=instance.port_delete&target_id=${id}`)).items
  expect(audit.map((a) => [a.actor_id, a.diff.port, a.diff.reason])).toEqual([[admin.id, 8888, 'abuse']])
})

test('A-INS-13 实例详情显示预订字段与只读自动续费；财务无改派 / 取消预订，运维有', async ({ page }) => {
  const g = await privateGroup(1)
  const u = await seedUser({ balance: 1_000_000 })
  const api = await userApi(u.phone)
  const s = await startAfter(30)
  const { ids, batchId } = await bookViaApi(api, { startAt: s.iso, groupId: g.id, autoRenew: true, autoRenewDuration: '1w' })
  const finance = await createAdmin('finance')
  await loginAdmin(page, finance)
  await page.goto(`/admin/instances/${ids[0]}`)
  await expect(page.getByTestId('adetail-state')).toHaveText('已预订')
  await expect(page.getByTestId('adetail-start')).toHaveText(`${cstTime(s.iso)} ～ ${cstTime(new Date(s.date.getTime() + 86400e3).toISOString())}`)
  await expect(page.getByTestId('adetail-batch')).toHaveText(batchId!)
  await expect(page.getByTestId('adetail-machine')).toHaveText(String(g.machineIds[0]))
  await expect(page.getByTestId('adetail-auto-renew')).toHaveText('开启（1 周）')
  await expect(page.getByTestId('adetail-reassign-btn')).toHaveCount(0) // finance 无操作
  await expect(page.getByTestId('adetail-cancel-booking-btn')).toHaveCount(0)
  await logout(page)
  await loginAdmin(page, await createAdmin('ops'))
  await page.goto(`/admin/instances/${ids[0]}`)
  await expect(page.getByTestId('adetail-state')).toHaveText('已预订')
  await expect(page.getByTestId('adetail-reassign-btn')).toBeVisible()
  await expect(page.getByTestId('adetail-cancel-booking-btn')).toBeVisible()
})

test('A-INS-14 实例详情「应用」卡片：部署状态、进度 / 错误、详情链接到部署抽屉；运维可强制卸载，财务不可', async ({ page }) => {
  const apps = await seedApps()
  const u = await seedUser()
  const { api, id } = await runningInstance(u.phone)
  ownInstance(api, id)
  const vllm = await deployViaApi(api, id, apps.vllm.id)
  await tickUntil(api, id, vllm.id, 'running')
  const webui = await deployViaApi(api, id, apps['open-webui'].id)
  await tickUntil(api, id, webui.id, 'installing')
  const app = (dep: string) => page.locator(`[data-testid="admin-instance-app"][data-dep="${dep}"]`)

  await loginAdmin(page, await createAdmin('finance'))
  await page.goto(`/admin/instances/${id}`)
  await expect(page.getByTestId('admin-instance-app')).toHaveCount(2)
  await expect(app(vllm.id)).toHaveAttribute('data-state', 'running')
  await expect(app(vllm.id).getByTestId('admin-instance-app-name')).toHaveText('vLLM 推理服务 · 1.0.0')
  await expect(app(vllm.id).getByTestId('admin-instance-app-state')).toHaveText('运行中')
  await expect(app(webui.id)).toHaveAttribute('data-state', 'installing')
  await expect(app(webui.id).getByTestId('admin-instance-app-state')).toHaveText('部署中')
  await expect(app(webui.id).getByTestId('admin-instance-app-progress')).toHaveText(/%$/)
  await expect(page.getByTestId('admin-instance-app-force-remove')).toHaveCount(0) // 财务只读
  await expect(page.getByTestId('admin-instance-apps-all')).toHaveAttribute('href', `/admin/deployments?instance_id=${id}`)
  await app(vllm.id).getByTestId('admin-instance-app-detail').click()
  await expectAdminUrl(page, '/deployments', { instance_id: id, dep: vllm.id })
  await expect(page.getByTestId('deployment-app')).toContainText('vLLM 推理服务')
  await expect(page.getByTestId('deployment-state')).toHaveAttribute('data-state', 'running')

  await logout(page)
  await loginAdmin(page, await createAdmin('ops'))
  await page.goto(`/admin/instances/${id}`)
  await expect(app(vllm.id).getByTestId('admin-instance-app-force-remove')).toBeVisible()
  await expect(app(webui.id).getByTestId('admin-instance-app-force-remove')).toBeVisible()
})

test('A-INS-15 部署抽屉的工作流表：版本、有新版本、安装状态，名称链接到工作流详情；没装时显示「未安装工作流」', async ({ page }) => {
  const [c] = await seedExtraWorkflows(1)
  const s = await comfySetup()
  await loginAdmin(page, await createAdmin('ops'))
  await page.goto(`/admin/deployments?instance_id=${s.id}&dep=${s.dep.id}`)
  await expect(page.getByTestId('deployment-state')).toHaveAttribute('data-state', 'running')
  await expect(page.getByTestId('adm-dep-workflows')).toContainText('未安装工作流')
  await expect(page.getByTestId('adm-dep-workflow-row')).toHaveCount(0)

  const [, installedC] = await installedRunning(s, s.wf.a.id, c.id)
  const nv = await hooks.workflowsVersion({ slug: c.slug }) // C 有新版本
  await installWorkflowViaApi(s.api, s.id, s.dep.id, s.wf.b.id) // B 待安装（部署重新排队）
  await page.getByTestId('deployment-refresh').click()
  await expect(page.getByTestId('deployment-state')).toHaveAttribute('data-state', 'queued')
  const row = (slug: string) => page.locator(`[data-testid="adm-dep-workflow-row"][data-slug="${slug}"]`)
  await expect(page.getByTestId('adm-dep-workflow-row')).toHaveCount(3)
  await expect(row('e2e-wf-a').getByTestId('adm-dep-workflow-name')).toHaveText('E2E 文生图 A')
  await expect(row('e2e-wf-a').getByTestId('adm-dep-workflow-version')).toHaveText('1.0.0')
  await expect(row('e2e-wf-a').getByTestId('adm-dep-workflow-state')).toHaveAttribute('data-state', 'installed')
  await expect(row('e2e-wf-a').getByTestId('adm-dep-workflow-state')).toHaveText('已安装')
  await expect(row('e2e-wf-a').getByTestId('adm-dep-workflow-update')).toHaveCount(0)
  await expect(row(c.slug).getByTestId('adm-dep-workflow-version')).toHaveText(installedC.version)
  await expect(row(c.slug).getByTestId('adm-dep-workflow-update')).toHaveText(`有新版本 ${nv.version}`)
  await expect(row('e2e-wf-b').getByTestId('adm-dep-workflow-state')).toHaveAttribute('data-state', 'pending')
  await expect(row('e2e-wf-b').getByTestId('adm-dep-workflow-state')).toHaveText('待安装')
  await row('e2e-wf-a').getByTestId('adm-dep-workflow-name').click()
  await expectAdminUrl(page, `/workflows/${s.wf.a.id}`)
  await expect(page.getByTestId('adm-wf-detail-name')).toHaveText('E2E 文生图 A')
})

test('A-INS-16 平台节点（系统实例）：列表「平台节点」标签与「不计时」，详情没有退款 / 延长 / 换机 / 改派', async ({ page }) => {
  const m = await seedLLMModel()
  const { machineId, instanceId } = await createPoolMachineAndNode(m)
  await loginAdmin(page, await createAdmin('super')) // 超级管理员：普通实例上这些按钮都有（A-INS-09）
  await page.goto(`/admin/instances?machine_id=${machineId}`)
  const row = page.getByRole('row').filter({ has: page.getByTestId('ainst-id').getByText(instanceId, { exact: true }) })
  await expect(row.getByTestId('ainst-system')).toHaveText('平台节点')
  await expect(row).toContainText('不计时')
  await row.getByTestId('ainst-id').getByText(instanceId, { exact: true }).click()
  await expectAdminUrl(page, `/instances/${instanceId}`)
  await expect(page.getByTestId('adetail-system')).toHaveText('平台节点')
  await expect(page.getByTestId('adetail-remaining')).toHaveText('不计时')
  await expect(page.getByTestId('adetail-expires')).toHaveText('不计时')
  await expect(page.getByTestId('admin-instance-apps-system-hint')).toBeVisible()
  for (const b of ['extend', 'migrate', 'refund', 'reassign', 'cancel-booking']) await expect(page.getByTestId(`adetail-${b}-btn`)).toHaveCount(0)
  // 接口层同样拒绝
  const admin = await createAdmin('super')
  const ctx = await adminApi(admin)
  const r = await ctx.post(`/admin/api/v1/instances/${instanceId}/extend`, { data: { seconds: 3600, reason: 'e2e' } })
  expect(r.status()).toBe(409)
  expect(((await r.json()) as { code: string }).code).toBe('invalid_state')
})
