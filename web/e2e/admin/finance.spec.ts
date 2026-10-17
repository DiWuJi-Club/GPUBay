import { readFileSync } from 'node:fs'
import { expect, test, type Download, type Page } from '@playwright/test'
import {
  adminApi, adminPost, clickTwiceSync, createAdmin, drainTasks, expectAdminUrl, fail500, getJSON, isPath, loginAdmin, logout, pick, recordRequests, trackToasts,
} from '../helpers/admin'
import { placeOrder, seedUser, toast, userApi } from '../helpers/console'
import { hooks } from '../helpers/hooks'
import { cstDate, cstTime, cstTimeSec } from '../helpers/time'

interface Order { id: string; user_id: number; kind: string; instance_id: string; duration: string; amount: number; refunded_amount: number; status: string; created_at: string }
interface Ledger { id: number; user_id: number; kind: string; amount: number; balance_after: number; ref_type: string; ref_id: string; operator_id: number | null; remark: string; created_at: string }
interface Recharge { id: string; user_id: number; channel: string; amount: number; status: string; channel_trade_no?: string; operator_id?: number | null; remark?: string; paid_at: string | null; closed_at: string | null; created_at: string }
type PagedOf<T> = { items: T[]; total: number }

/** 表格数据行（表头行没有这些 testid）。 */
const rows = (page: Page, testid: string) => page.getByRole('row').filter({ has: page.getByTestId(testid) })
/** 某个单号所在的行（单号精确匹配）。 */
const rowOf = (page: Page, testid: string, id: string | number) => page.getByRole('row').filter({ has: page.getByTestId(testid).filter({ hasText: new RegExp(`^${id}$`) }) })
const searchesOf = (seen: { search: string }[]) => () => seen.map((r) => r.search)
const prevDay = (iso: string) => cstDate(new Date(Date.parse(iso) - 86_400_000))

/** 分 → 元两位小数（与后端 csvx.Yuan 同格式，独立实现）。 */
function yuan2(cents: number): string {
  const abs = Math.abs(cents)
  return `${cents < 0 ? '-' : ''}${Math.floor(abs / 100)}.${String(abs % 100).padStart(2, '0')}`
}

/** RFC 4180 CSV 解析（引号字段、"" 转义、字段内逗号 / 换行）。 */
function parseCSV(text: string): string[][] {
  const out: string[][] = []
  let row: string[] = []
  let cell = ''
  let quoted = false
  for (let i = 0; i < text.length; i++) {
    const c = text[i]
    if (quoted) {
      if (c === '"' && text[i + 1] === '"') {
        cell += '"'
        i++
      } else if (c === '"') quoted = false
      else cell += c
    } else if (c === '"') quoted = true
    else if (c === ',') {
      row.push(cell)
      cell = ''
    } else if (c === '\n' || c === '\r') {
      if (c === '\r' && text[i + 1] === '\n') i++
      row.push(cell)
      out.push(row)
      row = []
      cell = ''
    } else cell += c
  }
  if (cell !== '' || row.length) {
    row.push(cell)
    out.push(row)
  }
  return out
}

/** 读取下载文件：返回原始字节与去掉 BOM 后解析出的行。 */
function readDownload(path: string) {
  const bytes = readFileSync(path)
  const text = bytes.toString('utf8')
  return { bytes, text, table: parseCSV(text.replace(/^﻿/, '')) }
}

/** 记录页面创建 / 释放过的 object URL（导出后必须释放）。在 page.goto 之前调用。 */
async function trackBlobUrls(page: Page): Promise<() => Promise<{ created: string[]; revoked: string[] }>> {
  await page.addInitScript(() => {
    const w = window as unknown as { __blobs: { created: string[]; revoked: string[] } }
    w.__blobs = { created: [], revoked: [] }
    const create = URL.createObjectURL.bind(URL)
    const revoke = URL.revokeObjectURL.bind(URL)
    URL.createObjectURL = (o: Blob | MediaSource) => {
      const u = create(o)
      w.__blobs.created.push(u)
      return u
    }
    URL.revokeObjectURL = (u: string) => {
      w.__blobs.revoked.push(u)
      revoke(u)
    }
  })
  return () => page.evaluate(() => {
    const b = (window as unknown as { __blobs: { created: string[]; revoked: string[] } }).__blobs
    return { created: b.created.slice(), revoked: b.revoked.slice() }
  })
}

/** 应用内导航（与点击站内链接相同，组件复用）：同一路由只换查询串时用。 */
function routerPush(page: Page, path: string) {
  return page.evaluate(async (to) => {
    const app = (document.querySelector('#app') as unknown as { __vue_app__: { config: { globalProperties: { $router: { push(p: string): Promise<unknown> } } } } }).__vue_app__
    await app.config.globalProperties.$router.push(to)
  }, path)
}

async function renew(api: Awaited<ReturnType<typeof userApi>>, id: string, duration: '1d' | '1w') {
  const r = await api.post(`/api/v1/instances/${id}/renew`, { data: { duration }, headers: { 'Idempotency-Key': crypto.randomUUID() } })
  if (!r.ok()) throw new Error(`renew ${r.status()} ${await r.text()}`)
}

test('A-FIN-01 订单表：仅超管 / 财务可见；首次加载失败可重试；?user_id= 预填（同一路由换查询串重新预填）；筛选用户 / 类型 / 状态 / 日期原样发出；行内容；重置；分页与导出按已查询的条件', async ({ page }) => {
  // 用户 U：实例 1 新购 1 天（已支付）+ 续费 1 周（退 ¥10 → 部分退款）；实例 2 新购后开通失败（全额退款）
  const u = await seedUser({ balance: 300000 })
  const api = await userApi(u.phone)
  await hooks.machines(1)
  const inst1 = await placeOrder(api)
  await hooks.agent()
  await renew(api, inst1, '1w')
  const admin = await createAdmin('finance')
  const ctx = await adminApi(admin)
  await adminPost(ctx, admin, `/admin/api/v1/instances/${inst1}/refund-release`, { amount: 1000, reason: 'e2e 部分退款' })
  await hooks.machines(1)
  const inst2 = await placeOrder(api)
  const m2 = (await getJSON<{ instance: { machine_id: number } }>(ctx, `/admin/api/v1/instances/${inst2}`)).instance.machine_id
  await hooks.agent({ machine_id: m2, fail: true })
  const all = (await getJSON<PagedOf<Order>>(ctx, `/admin/api/v1/orders?user_id=${u.id}`)).items
  expect(all).toHaveLength(3)
  const oNew = all.find((o) => o.instance_id === inst1 && o.kind === 'new')!
  const oRenew = all.find((o) => o.instance_id === inst1 && o.kind === 'renew')!
  const oFail = all.find((o) => o.instance_id === inst2)!
  expect([oNew.status, oRenew.status, oFail.status]).toEqual(['paid', 'partially_refunded', 'refunded'])

  const toasts = await trackToasts(page)
  const listReqs = recordRequests(page, 'GET', '/orders')
  const searches = searchesOf(listReqs)

  await test.step('运维看不到财务菜单，直接打开订单 / 流水 / 充值单都到 403，且不请求列表', async () => {
    const ops = await createAdmin('ops')
    await loginAdmin(page, ops)
    for (const key of ['orders', 'ledger', 'recharges']) await expect(page.getByTestId(`menu-${key}`)).toHaveCount(0)
    await expect(page.getByTestId('menu-instances')).toBeVisible() // 菜单本身已渲染（不是还没加载）
    for (const key of ['orders', 'ledger', 'recharges']) {
      await page.goto(`/admin/${key}`)
      await expectAdminUrl(page, '/403', { from: `/${key}` })
    }
    await drainTasks(page)
    expect(listReqs).toHaveLength(0)
    await logout(page)
  })

  await test.step('财务可见三个菜单；?user_id= 预填并作为首个请求的筛选；首次加载失败：表格为空、内联错误 + 重试、只提示一次、不自动重试', async () => {
    await loginAdmin(page, admin)
    for (const key of ['orders', 'ledger', 'recharges']) await expect(page.getByTestId(`menu-${key}`)).toBeVisible()
    const isOrders = isPath('/orders')
    await page.route(isOrders, fail500('req-fin-1'))
    await page.goto(`/admin/orders?user_id=${u.id}`)
    await expect(page.getByTestId('fin-error')).toContainText('服务器内部错误（请求号 req-fin-1）')
    await expect(page.getByTestId('fin-filter-user')).toHaveValue(String(u.id))
    await expect(rows(page, 'ord-id')).toHaveCount(0)
    await drainTasks(page)
    expect(searches()).toEqual([`?user_id=${u.id}&page=1&page_size=20`])
    expect(await toasts()).toEqual(['服务器内部错误（请求号 req-fin-1）'])
    await page.unroute(isOrders)
    await page.getByTestId('fin-retry').click()
    await expect(rows(page, 'ord-id')).toHaveCount(3)
    await expect(page.getByTestId('fin-error')).toHaveCount(0)
    expect(searches()).toEqual([`?user_id=${u.id}&page=1&page_size=20`, `?user_id=${u.id}&page=1&page_size=20`])
    await expect(page.getByTestId('fin-total')).toHaveText('共 3 条')
  })

  await test.step('行内容：订单号、用户 / 实例链接、类型、租期、金额、已退、状态、下单时间', async () => {
    const expected: [Order, string, string, string, string, string][] = [
      [oNew, '新购', '1 天', '¥69.00', '¥0.00', '已支付'],
      [oRenew, '续费', '1 周', '¥414.00', '¥10.00', '部分退款'],
      [oFail, '新购', '1 天', '¥69.00', '¥69.00', '已退款'],
    ]
    for (const [o, kind, dur, amount, refunded, status] of expected) {
      const row = rowOf(page, 'ord-id', o.id)
      await expect(row).toHaveCount(1)
      await expect(row.getByTestId('ord-user')).toHaveText(String(u.id))
      await expect(row.getByTestId('ord-user')).toHaveAttribute('href', `/admin/users/${u.id}`)
      await expect(row.getByTestId('ord-instance')).toHaveText(o.instance_id)
      await expect(row.getByTestId('ord-instance')).toHaveAttribute('href', `/admin/instances/${o.instance_id}`)
      await expect(row.getByTestId('ord-kind')).toHaveText(kind)
      await expect(row.getByTestId('ord-duration')).toHaveText(dur)
      await expect(row.getByTestId('ord-amount')).toHaveText(amount)
      await expect(row.getByTestId('ord-refunded')).toHaveText(refunded)
      await expect(row.getByTestId('ord-status')).toHaveText(status)
      await expect(row.getByTestId('ord-time')).toHaveText(cstTime(o.created_at))
    }
    // 列表按下单时间倒序（与接口一致）
    await expect(page.getByTestId('ord-id')).toHaveText(all.map((o) => o.id))
  })

  await test.step('筛选：类型、状态、日期（北京时间日期原样发出，to 含当天）', async () => {
    await pick(page, 'fin-filter-kind', '续费')
    await page.getByTestId('fin-search').click()
    await expect.poll(() => searches().at(-1)).toBe(`?user_id=${u.id}&kind=renew&page=1&page_size=20`)
    await expect(page.getByTestId('ord-id')).toHaveText([oRenew.id])
    await expect(page.getByTestId('fin-total')).toHaveText('共 1 条')

    await pick(page, 'fin-filter-kind', '新购')
    await pick(page, 'fin-filter-status', '已支付')
    await page.getByTestId('fin-search').click()
    await expect.poll(() => searches().at(-1)).toBe(`?user_id=${u.id}&kind=new&status=paid&page=1&page_size=20`)
    await expect(page.getByTestId('ord-id')).toHaveText([oNew.id])

    await pick(page, 'fin-filter-status', '已退款')
    const day = cstDate(oFail.created_at)
    await page.getByTestId('fin-from').fill(day)
    await page.getByTestId('fin-to').fill(day)
    await page.getByTestId('fin-search').click()
    await expect.poll(() => searches().at(-1)).toBe(`?user_id=${u.id}&kind=new&status=refunded&from=${day}&to=${day}&page=1&page_size=20`)
    await expect(page.getByTestId('ord-id')).toHaveText([oFail.id]) // from = to = 下单当天也能查到：to 含当天

    const before = prevDay(oFail.created_at)
    await page.getByTestId('fin-from').fill(before)
    await page.getByTestId('fin-to').fill(before)
    await page.getByTestId('fin-search').click()
    await expect.poll(() => searches().at(-1)).toBe(`?user_id=${u.id}&kind=new&status=refunded&from=${before}&to=${before}&page=1&page_size=20`)
    await expect(rows(page, 'ord-id')).toHaveCount(0)
    await expect(page.getByTestId('fin-total')).toHaveText('共 0 条')
  })

  await test.step('用户 ID 只接受数字（全角转半角、去掉非数字）', async () => {
    await page.getByTestId('fin-filter-user').fill(`a${String(u.id).replace(/\d/g, (d) => String.fromCharCode(0xff10 + Number(d)))} `)
    await expect(page.getByTestId('fin-filter-user')).toHaveValue(String(u.id))
  })

  await test.step('重置：清空全部条件，只发一次不带筛选的请求', async () => {
    const n = listReqs.length
    await page.getByTestId('fin-reset').click()
    await expect.poll(() => listReqs.length).toBe(n + 1)
    expect(searches().at(-1)).toBe('?page=1&page_size=20')
    for (const id of ['fin-filter-user', 'fin-from', 'fin-to']) await expect(page.getByTestId(id)).toHaveValue('')
    await expect(page.getByTestId('fin-filter-kind')).not.toContainText('新购')
    await expect(page.getByTestId('fin-filter-status')).not.toContainText('已退款')
    await drainTasks(page)
    expect(listReqs).toHaveLength(n + 1)
  })

  await test.step('实例链接跳到实例详情', async () => {
    await page.getByTestId('fin-filter-user').fill(String(u.id))
    await page.getByTestId('fin-search').click()
    await expect(rows(page, 'ord-id')).toHaveCount(3)
    await rowOf(page, 'ord-id', oRenew.id).getByTestId('ord-instance').click()
    await expectAdminUrl(page, `/instances/${inst1}`)
  })

  // 22 笔订单的用户 P：新购 1 天 + 续费 21 次
  const p = await seedUser({ balance: 160000 })
  const papi = await userApi(p.phone)
  await hooks.machines(1)
  const pinst = await placeOrder(papi)
  for (let i = 0; i < 21; i++) await renew(papi, pinst, '1d')
  const page2 = await getJSON<PagedOf<Order>>(ctx, `/admin/api/v1/orders?user_id=${p.id}&page=2&page_size=20`)
  expect(page2.total).toBe(22)

  await test.step('同一路由只换查询串：?user_id=U → ?user_id=P、→ 无参数（菜单）、浏览器后退回 ?user_id=P，都重新预填并加载', async () => {
    await page.goto(`/admin/orders?user_id=${u.id}`)
    await expect(rows(page, 'ord-id')).toHaveCount(3)
    await routerPush(page, `/orders?user_id=${p.id}`)
    await expectAdminUrl(page, '/orders', { user_id: String(p.id) })
    await expect(page.getByTestId('fin-filter-user')).toHaveValue(String(p.id))
    await expect.poll(() => searches().at(-1)).toBe(`?user_id=${p.id}&page=1&page_size=20`)
    await expect(page.getByTestId('fin-total')).toHaveText('共 22 条')
    await page.getByTestId('menu-orders').click()
    await expectAdminUrl(page, '/orders')
    await expect(page.getByTestId('fin-filter-user')).toHaveValue('')
    await expect.poll(() => searches().at(-1)).toBe('?page=1&page_size=20')
    await page.goBack()
    await expectAdminUrl(page, '/orders', { user_id: String(p.id) })
    await expect(page.getByTestId('fin-filter-user')).toHaveValue(String(p.id))
    await expect.poll(() => searches().at(-1)).toBe(`?user_id=${p.id}&page=1&page_size=20`)
    await expect(page.getByTestId('fin-total')).toHaveText('共 22 条')
  })

  await test.step('分页：第 1 页 20 条；改了筛选但没点「查询」时翻到第 2 页、导出仍按已查询的条件；换条件查询回到第 1 页且只发一次请求', async () => {
    await expect(rows(page, 'ord-id')).toHaveCount(20)
    // 未提交的修改：用户换成 U、开始日期改成 2000 年
    await page.getByTestId('fin-filter-user').fill(String(u.id))
    await page.getByTestId('fin-from').fill('2000-01-01')
    await page.getByTestId('fin-pager').getByText('2', { exact: true }).click()
    await expect.poll(() => searches().at(-1)).toBe(`?user_id=${p.id}&page=2&page_size=20`)
    await expect(page.getByTestId('ord-id')).toHaveText(page2.items.map((o) => o.id))
    const exports = recordRequests(page, 'GET', '/orders/export')
    const pending = page.waitForEvent('download')
    await page.getByTestId('fin-export').click()
    const file = await pending
    expect(exports.map((x) => x.search)).toEqual([`?user_id=${p.id}`])
    const table = readDownload(await file.path()).table
    expect(table.slice(1)).toHaveLength(22)
    expect(new Set(table.slice(1).map((c) => c[1]))).toEqual(new Set([String(p.id)]))

    await page.getByTestId('fin-filter-user').fill(String(p.id))
    await page.getByTestId('fin-from').fill('')
    await pick(page, 'fin-filter-kind', '新购')
    const n = listReqs.length
    await page.getByTestId('fin-search').click()
    await expect.poll(() => searches().at(-1)).toBe(`?user_id=${p.id}&kind=new&page=1&page_size=20`)
    await expect(page.getByTestId('ord-kind')).toHaveText(['新购'])
    await drainTasks(page)
    expect(listReqs).toHaveLength(n + 1)
  })
})

test('A-FIN-02 流水表：?user_id= 预填；筛选用户 / 类型 / 日期原样发出；正负金额、余额、关联、操作人、备注；无状态筛选；分页', async ({ page }) => {
  // 用户 L：种子调账 +¥1,000.00（无操作人）→ 调账 -¥2.50 → 调账 +¥1.00 → 下单扣款 -¥69.00
  const l = await seedUser({ balance: 100000 })
  const admin = await createAdmin('finance')
  const ctx = await adminApi(admin)
  await adminPost(ctx, admin, `/admin/api/v1/users/${l.id}/adjust`, { amount: -250, remark: '扣回多充' })
  await adminPost(ctx, admin, `/admin/api/v1/users/${l.id}/adjust`, { amount: 100, remark: '补偿' })
  const api = await userApi(l.phone)
  await hooks.machines(1)
  await placeOrder(api)
  const order = (await getJSON<PagedOf<Order>>(ctx, `/admin/api/v1/orders?user_id=${l.id}`)).items[0]
  const ledger = (await getJSON<PagedOf<Ledger>>(ctx, `/admin/api/v1/ledger?user_id=${l.id}`)).items
  expect(ledger.map((r) => [r.kind, r.amount])).toEqual([['order_pay', -6900], ['admin_adjust', 100], ['admin_adjust', -250], ['admin_adjust', 100000]])
  const [pay, plus, minus, seed] = ledger

  const listReqs = recordRequests(page, 'GET', '/ledger')
  const searches = searchesOf(listReqs)
  await loginAdmin(page, admin)

  await test.step('?user_id= 预填并作为首个请求的筛选；没有状态筛选', async () => {
    await page.goto(`/admin/ledger?user_id=${l.id}`)
    await expect(page.getByTestId('fin-filter-user')).toHaveValue(String(l.id))
    await expect(rows(page, 'led-id')).toHaveCount(4)
    expect(searches()).toEqual([`?user_id=${l.id}&page=1&page_size=20`])
    await expect(page.getByTestId('fin-filter-status')).toHaveCount(0)
    await expect(page.getByTestId('fin-total')).toHaveText('共 4 条')
  })

  await test.step('行内容：流水号、用户链接、类型、带符号金额与颜色、变动后余额、关联、操作人、备注、时间', async () => {
    await expect(page.getByTestId('led-id')).toHaveText(ledger.map((r) => String(r.id)))
    const expected: [Ledger, string, string, RegExp, string, string, string, string][] = [
      [pay, '扣款', '-¥69.00', /money-minus/, '¥929.50', `order ${order.id}`, '—', pay.remark],
      [plus, '调账', '+¥1.00', /money-plus/, '¥998.50', '—', String(admin.id), '补偿'],
      [minus, '调账', '-¥2.50', /money-minus/, '¥997.50', '—', String(admin.id), '扣回多充'],
      [seed, '调账', '+¥1,000.00', /money-plus/, '¥1,000.00', '—', '—', 'e2e-seed-balance'],
    ]
    for (const [r, kind, amount, cls, after, ref, operator, remark] of expected) {
      const row = rowOf(page, 'led-id', r.id)
      await expect(row.getByTestId('led-user')).toHaveText(String(l.id))
      await expect(row.getByTestId('led-user')).toHaveAttribute('href', `/admin/users/${l.id}`)
      await expect(row.getByTestId('led-kind')).toHaveText(kind)
      await expect(row.getByTestId('led-amount')).toHaveText(amount)
      await expect(row.getByTestId('led-amount')).toHaveClass(cls)
      await expect(row.getByTestId('led-after')).toHaveText(after)
      await expect(row.getByTestId('led-ref')).toHaveText(ref)
      await expect(row.getByTestId('led-operator')).toHaveText(operator)
      await expect(row.getByTestId('led-remark')).toHaveText(remark)
      await expect(row.getByTestId('led-time')).toHaveText(cstTime(r.created_at))
    }
  })

  await test.step('筛选：类型（扣款 = order_pay）、日期（to 含当天）、早一天为空', async () => {
    await pick(page, 'fin-filter-kind', '扣款')
    await page.getByTestId('fin-search').click()
    await expect.poll(() => searches().at(-1)).toBe(`?user_id=${l.id}&kind=order_pay&page=1&page_size=20`)
    await expect(page.getByTestId('led-id')).toHaveText([String(pay.id)])
    const day = cstDate(pay.created_at)
    await page.getByTestId('fin-from').fill(day)
    await page.getByTestId('fin-to').fill(day)
    await page.getByTestId('fin-search').click()
    await expect.poll(() => searches().at(-1)).toBe(`?user_id=${l.id}&kind=order_pay&from=${day}&to=${day}&page=1&page_size=20`)
    await expect(page.getByTestId('led-id')).toHaveText([String(pay.id)])
    const before = prevDay(pay.created_at)
    await page.getByTestId('fin-from').fill(before)
    await page.getByTestId('fin-to').fill(before)
    await page.getByTestId('fin-search').click()
    await expect.poll(() => searches().at(-1)).toBe(`?user_id=${l.id}&kind=order_pay&from=${before}&to=${before}&page=1&page_size=20`)
    await expect(rows(page, 'led-id')).toHaveCount(0)
    await expect(page.getByTestId('fin-total')).toHaveText('共 0 条')
  })

  await test.step('重置：清空全部条件，只发一次不带筛选的请求', async () => {
    const n = listReqs.length
    await page.getByTestId('fin-reset').click()
    await expect.poll(() => listReqs.length).toBe(n + 1)
    expect(searches().at(-1)).toBe('?page=1&page_size=20')
    for (const id of ['fin-filter-user', 'fin-from', 'fin-to']) await expect(page.getByTestId(id)).toHaveValue('')
    await expect(page.getByTestId('fin-filter-kind')).not.toContainText('扣款')
    await drainTasks(page)
    expect(listReqs).toHaveLength(n + 1)
  })

  await test.step('分页：21 条流水的用户，第 2 页只剩最早的一条', async () => {
    const p = await seedUser({ balance: 21, ledger_rows: 21 })
    await page.getByTestId('fin-filter-user').fill(String(p.id))
    await page.getByTestId('fin-search').click()
    await expect.poll(() => searches().at(-1)).toBe(`?user_id=${p.id}&page=1&page_size=20`)
    await expect(rows(page, 'led-id')).toHaveCount(20)
    await expect(page.getByTestId('led-remark').first()).toHaveText('e2e-seed-21')
    await expect(page.getByTestId('fin-total')).toHaveText('共 21 条')
    await page.getByTestId('fin-pager').getByText('2', { exact: true }).click()
    await expect.poll(() => searches().at(-1)).toBe(`?user_id=${p.id}&page=2&page_size=20`)
    await expect(page.getByTestId('led-remark')).toHaveText(['e2e-seed-1'])
    await expect(page.getByTestId('led-amount')).toHaveText(['+¥0.01'])
  })
})

test('A-FIN-03 充值表：筛选用户 / 渠道 / 状态 / 日期原样发出；渠道、状态、渠道单号、操作人、备注、支付时间', async ({ page }) => {
  // 用户 R：微信已支付、支付宝超时关闭、支付宝待支付、后台手动入账
  const r = await seedUser()
  const api = await userApi(r.phone)
  const create = async (channel: string) => ((await (await api.post('/api/v1/recharges', { data: { amount: 5000, channel } })).json()) as { id: string }).id
  const wechat = await create('wechat')
  expect((await api.post(`/api/v1/dev/recharges/${wechat}/pay`, { data: {} })).ok()).toBe(true)
  const closed = await create('alipay')
  await hooks.advance(7200)
  await hooks.sweep()
  const pending = await create('alipay')
  const admin = await createAdmin('finance') // 推进 2 小时之后再登录：后台会话 2 小时空闲失效
  const ctx = await adminApi(admin)
  const manual = (await adminPost<{ id: string }>(ctx, admin, `/admin/api/v1/users/${r.id}/manual-recharge`, { amount: 12345, remark: '线下转账' })).id
  const all = (await getJSON<PagedOf<Recharge>>(ctx, `/admin/api/v1/recharges?user_id=${r.id}`)).items
  const byId = Object.fromEntries(all.map((x) => [x.id, x]))
  expect([byId[wechat].status, byId[closed].status, byId[pending].status, byId[manual].status]).toEqual(['paid', 'closed', 'pending', 'paid'])
  expect(byId[wechat].channel_trade_no).toBeTruthy()

  const listReqs = recordRequests(page, 'GET', '/recharges')
  const searches = searchesOf(listReqs)
  await loginAdmin(page, admin)
  await page.getByTestId('menu-recharges').click()
  await expectAdminUrl(page, '/recharges')

  await test.step('按用户筛选：四张单，行内容', async () => {
    await page.getByTestId('fin-filter-user').fill(String(r.id))
    await page.getByTestId('fin-search').click()
    await expect.poll(() => searches().at(-1)).toBe(`?user_id=${r.id}&page=1&page_size=20`)
    await expect(page.getByTestId('rch-id')).toHaveText(all.map((x) => x.id))
    await expect(page.getByTestId('fin-total')).toHaveText('共 4 条')
    const expected: [string, string, string, string, string, string, string, string][] = [
      // id, 渠道, 金额, 状态, 渠道单号, 操作人, 备注, 支付时间
      [wechat, '微信支付', '¥50.00', '已支付', byId[wechat].channel_trade_no!, '—', '', cstTime(byId[wechat].paid_at!)],
      [closed, '支付宝', '¥50.00', '已关闭', '—', '—', '', '—'],
      [pending, '支付宝', '¥50.00', '待支付', '—', '—', '', '—'],
      [manual, '手动', '¥123.45', '已支付', '—', String(admin.id), '线下转账', cstTime(byId[manual].paid_at!)],
    ]
    for (const [id, channel, amount, status, tradeNo, operator, remark, paid] of expected) {
      const row = rowOf(page, 'rch-id', id)
      await expect(row.getByTestId('rch-user')).toHaveText(String(r.id))
      await expect(row.getByTestId('rch-user')).toHaveAttribute('href', `/admin/users/${r.id}`)
      await expect(row.getByTestId('rch-channel')).toHaveText(channel)
      await expect(row.getByTestId('rch-amount')).toHaveText(amount)
      await expect(row.getByTestId('rch-status')).toHaveText(status)
      await expect(row.getByTestId('rch-trade-no')).toHaveText(tradeNo)
      await expect(row.getByTestId('rch-operator')).toHaveText(operator)
      await expect(row.getByTestId('rch-remark')).toHaveText(remark)
      await expect(row.getByTestId('rch-created')).toHaveText(cstTime(byId[id].created_at))
      await expect(row.getByTestId('rch-paid')).toHaveText(paid)
    }
  })

  await test.step('筛选：渠道（channel 参数）、状态、日期（to 含当天）', async () => {
    await pick(page, 'fin-filter-kind', '支付宝')
    await page.getByTestId('fin-search').click()
    await expect.poll(() => searches().at(-1)).toBe(`?user_id=${r.id}&channel=alipay&page=1&page_size=20`)
    await expect(page.getByTestId('rch-id')).toHaveText([pending, closed])
    await pick(page, 'fin-filter-status', '已关闭')
    await page.getByTestId('fin-search').click()
    await expect.poll(() => searches().at(-1)).toBe(`?user_id=${r.id}&channel=alipay&status=closed&page=1&page_size=20`)
    await expect(page.getByTestId('rch-id')).toHaveText([closed])
    const day = cstDate(byId[closed].created_at)
    await page.getByTestId('fin-from').fill(day)
    await page.getByTestId('fin-to').fill(day)
    await page.getByTestId('fin-search').click()
    await expect.poll(() => searches().at(-1)).toBe(`?user_id=${r.id}&channel=alipay&status=closed&from=${day}&to=${day}&page=1&page_size=20`)
    await expect(page.getByTestId('rch-id')).toHaveText([closed])
    const before = prevDay(byId[closed].created_at)
    await page.getByTestId('fin-from').fill(before)
    await page.getByTestId('fin-to').fill(before)
    await page.getByTestId('fin-search').click()
    await expect.poll(() => searches().at(-1)).toBe(`?user_id=${r.id}&channel=alipay&status=closed&from=${before}&to=${before}&page=1&page_size=20`)
    await expect(rows(page, 'rch-id')).toHaveCount(0)
  })

  await test.step('重置后按「已支付」「手动」筛选', async () => {
    const n = listReqs.length
    await page.getByTestId('fin-reset').click()
    await expect.poll(() => listReqs.length).toBe(n + 1)
    expect(searches().at(-1)).toBe('?page=1&page_size=20')
    for (const id of ['fin-filter-user', 'fin-from', 'fin-to']) await expect(page.getByTestId(id)).toHaveValue('')
    await expect(page.getByTestId('fin-filter-kind')).not.toContainText('支付宝')
    await expect(page.getByTestId('fin-filter-status')).not.toContainText('已关闭')
    await page.getByTestId('fin-filter-user').fill(String(r.id))
    await pick(page, 'fin-filter-status', '已支付')
    await page.getByTestId('fin-search').click()
    await expect.poll(() => searches().at(-1)).toBe(`?user_id=${r.id}&status=paid&page=1&page_size=20`)
    await expect(page.getByTestId('rch-id')).toHaveText([manual, wechat])
    await pick(page, 'fin-filter-kind', '手动')
    await page.getByTestId('fin-search').click()
    await expect.poll(() => searches().at(-1)).toBe(`?user_id=${r.id}&channel=manual&status=paid&page=1&page_size=20`)
    await expect(page.getByTestId('rch-id')).toHaveText([manual])
  })
})

test('A-FIN-04 三张表导出 CSV：与列表同筛选参数（含状态、跨北京时间午夜的日期、未提交的修改不生效）、文件名、BOM、中文表头、行内容（元两位小数、防公式注入）、释放 URL；失败只提示一次；连点只导出一次；超管可导出', async ({ page }) => {
  // 用户 B：北京时间午夜前 1 分钟、后 1 分钟各一张支付宝待支付单（推进共享假时钟到下一个北京时间午夜附近；之后才建管理员会话）
  const b = await seedUser()
  const bapi = await userApi(b.phone)
  const bCreate = async () => {
    const res = await bapi.post('/api/v1/recharges', { data: { amount: 5000, channel: 'alipay' } })
    if (!res.ok()) throw new Error(`recharge ${res.status()} ${await res.text()}`)
    return ((await res.json()) as { id: string }).id
  }
  const DAY = 86_400_000
  const CST = 8 * 3_600_000
  const now = (await hooks.clock()).getTime()
  const midnight = (Math.floor((now + CST) / DAY) + 1) * DAY - CST
  let wait = Math.ceil((midnight - now) / 1000) - 60
  if (wait < 1) wait += 86_400
  await hooks.advance(wait)
  const bBefore = await bCreate()
  await hooks.advance(120)
  const bAfter = await bCreate()
  const bPaid = await bCreate() // 午夜后、同渠道但已支付：只被状态筛选排除
  expect((await bapi.post(`/api/v1/dev/recharges/${bPaid}/pay`, { data: {} })).ok()).toBe(true)

  // 用户 E：一笔订单；调账备注以 = + @ - 开头（需转义）与一笔负数调账（金额不转义）；手动充值备注以 = 开头且带引号；一张支付宝待支付单（被渠道筛选排除）
  const e = await seedUser({ balance: 100000 })
  const api = await userApi(e.phone)
  await hooks.machines(1)
  await placeOrder(api)
  const pendingRes = await api.post('/api/v1/recharges', { data: { amount: 5000, channel: 'alipay' } })
  expect(pendingRes.ok()).toBe(true)
  const admin = await createAdmin('finance')
  const ctx = await adminApi(admin)
  const adjusts: [number, string, string][] = [
    // 金额, 备注, CSV 里的备注
    [100, '=1+2', "'=1+2"],
    [200, '+plus', "'+plus"],
    [300, '@at', "'@at"],
    [400, '-minus', "'-minus"],
    [-250, 'neg', 'neg'],
  ]
  for (const [amount, remark] of adjusts) await adminPost(ctx, admin, `/admin/api/v1/users/${e.id}/adjust`, { amount, remark })
  const manualRemark = '=HYPERLINK("http://x")'
  await adminPost(ctx, admin, `/admin/api/v1/users/${e.id}/manual-recharge`, { amount: 12345, remark: manualRemark })

  const orders = (await getJSON<PagedOf<Order>>(ctx, `/admin/api/v1/orders?user_id=${e.id}`)).items
  const adjustRows = (await getJSON<PagedOf<Ledger>>(ctx, `/admin/api/v1/ledger?user_id=${e.id}&kind=admin_adjust`)).items
  const manualRows = (await getJSON<PagedOf<Recharge>>(ctx, `/admin/api/v1/recharges?user_id=${e.id}&channel=manual`)).items
  expect(orders).toHaveLength(1)
  expect(adjustRows).toHaveLength(6) // 种子 + 5 笔
  expect(manualRows).toHaveLength(1)
  const bRows = Object.fromEntries((await getJSON<PagedOf<Recharge>>(ctx, `/admin/api/v1/recharges?user_id=${b.id}`)).items.map((x) => [x.id, x]))
  const d1 = cstDate(bRows[bBefore].created_at)
  const d2 = cstDate(bRows[bAfter].created_at)
  expect(d2).not.toBe(d1) // 两张单跨过北京时间午夜
  expect([bRows[bPaid].status, cstDate(bRows[bPaid].created_at)]).toEqual(['paid', d2])
  expect(prevDay(bRows[bAfter].created_at)).toBe(d1)
  expect(Date.parse(bRows[bAfter].created_at) - Date.parse(bRows[bBefore].created_at)).toBeLessThanOrEqual(200_000)

  const toasts = await trackToasts(page)
  const blobs = await trackBlobUrls(page)
  const downloads: Download[] = []
  page.on('download', (d) => downloads.push(d))
  await loginAdmin(page, admin)

  /** 点「导出 CSV」：导出请求只有一个且查询串为 expectSearch；核对文件名、URL 释放、BOM；返回解析后的表格。 */
  async function exportNow(endpoint: string, expectSearch: string) {
    const exports = recordRequests(page, 'GET', `/${endpoint}/export`)
    const before = (await blobs()).created.length
    const pending = page.waitForEvent('download')
    await page.getByTestId('fin-export').click()
    const file = await pending
    expect(exports.map((x) => x.search)).toEqual([`?${expectSearch}`])
    expect(file.suggestedFilename()).toMatch(new RegExp(`^${endpoint}-\\d{8}-\\d{6}\\.csv$`))
    // object URL 用完即释放（下载开始后的下一个任务）
    await expect.poll(async () => {
      const b = await blobs()
      return b.created.length === before + 1 && b.revoked.includes(b.created.at(-1)!)
    }).toBe(true)
    const out = readDownload(await file.path())
    expect([...out.bytes.subarray(0, 3)]).toEqual([0xef, 0xbb, 0xbf])
    expect(out.text.charCodeAt(0)).toBe(0xfeff)
    return out.table
  }

  /** 设好筛选后查询再导出：导出请求的查询串 = 列表请求去掉分页参数。 */
  async function exportWith(endpoint: string, setFilter: () => Promise<void>, expectSearch: string) {
    const lists = recordRequests(page, 'GET', `/${endpoint}`)
    await page.getByTestId(`menu-${endpoint}`).click()
    await expectAdminUrl(page, `/${endpoint}`)
    await setFilter()
    await page.getByTestId('fin-search').click()
    await expect.poll(() => lists.at(-1)?.search).toBe(`?${expectSearch}&page=1&page_size=20`)
    return exportNow(endpoint, expectSearch)
  }

  await test.step('订单：按用户 + 状态 + 日期导出，一行，金额元两位小数', async () => {
    const o = orders[0]
    const day = cstDate(o.created_at)
    const table = await exportWith('orders', async () => {
      await page.getByTestId('fin-filter-user').fill(String(e.id))
      await pick(page, 'fin-filter-status', '已支付')
      await page.getByTestId('fin-from').fill(day)
      await page.getByTestId('fin-to').fill(day)
    }, `user_id=${e.id}&status=paid&from=${day}&to=${day}`)
    expect(table).toEqual([
      ['订单号', '用户ID', '类型', '实例', '租期', '金额(元)', '已退(元)', '状态', '下单时间'],
      [o.id, String(e.id), 'new', o.instance_id, '1d', '69.00', '0.00', 'paid', cstTimeSec(o.created_at)],
    ])
  })

  await test.step('流水：按用户 + 类型（调账）导出；= + @ - 开头的备注加单引号，负数金额不加', async () => {
    const table = await exportWith('ledger', async () => {
      await page.getByTestId('fin-filter-user').fill(String(e.id))
      await pick(page, 'fin-filter-kind', '调账')
    }, `user_id=${e.id}&kind=admin_adjust`)
    expect(table[0]).toEqual(['流水ID', '用户ID', '类型', '金额(元)', '变动后余额(元)', '关联类型', '关联单号', '操作人', '备注', '时间'])
    const escaped: Record<string, string> = { 'e2e-seed-balance': 'e2e-seed-balance', ...Object.fromEntries(adjusts.map(([, r, csv]) => [r, csv])) }
    const expected = [...adjustRows].reverse().map((l) => [
      String(l.id), String(e.id), 'admin_adjust', yuan2(l.amount), yuan2(l.balance_after), '', '', l.operator_id == null ? '' : String(l.operator_id), escaped[l.remark], cstTimeSec(l.created_at),
    ])
    expect(table.slice(1)).toEqual(expected)
    // 逐项核对关键单元格（不依赖上面的映射）
    const remarks = table.slice(1).map((c) => c[8])
    expect(remarks).toEqual(['e2e-seed-balance', "'=1+2", "'+plus", "'@at", "'-minus", 'neg'])
    expect(table.slice(1).map((c) => c[3])).toEqual(['1000.00', '1.00', '2.00', '3.00', '4.00', '-2.50'])
    expect(table.slice(1).map((c) => c[7])).toEqual(['', ...Array(5).fill(String(admin.id))])
  })

  await test.step('充值单：按用户 + 渠道（手动）导出；备注以 = 开头加单引号、引号按 CSV 转义', async () => {
    const table = await exportWith('recharges', async () => {
      await page.getByTestId('fin-filter-user').fill(String(e.id))
      await pick(page, 'fin-filter-kind', '手动')
    }, `user_id=${e.id}&channel=manual`)
    const m = manualRows[0]
    expect(table).toEqual([
      ['充值单号', '用户ID', '渠道', '金额(元)', '状态', '渠道单号', '操作人', '备注', '创建时间', '支付时间', '关闭时间'],
      [m.id, String(e.id), 'manual', '123.45', 'paid', '', String(admin.id), `'${manualRemark}`, cstTimeSec(m.created_at), cstTimeSec(m.paid_at!), ''],
    ])
  })

  await test.step('充值单跨北京时间午夜：按渠道 + 状态 + 日期导出；to = 前一天只含午夜前的单（含当天），from = to = 后一天只含午夜后的单', async () => {
    const ids = (t: string[][]) => t.slice(1).map((c) => c[0])
    const t1 = await exportWith('recharges', async () => {
      await page.getByTestId('fin-filter-user').fill(String(b.id))
      await pick(page, 'fin-filter-kind', '支付宝')
      await pick(page, 'fin-filter-status', '待支付')
      await page.getByTestId('fin-from').fill(d1)
      await page.getByTestId('fin-to').fill(d1)
    }, `user_id=${b.id}&channel=alipay&status=pending&from=${d1}&to=${d1}`)
    await expect(page.getByTestId('rch-id')).toHaveText([bBefore])
    expect(ids(t1)).toEqual([bBefore])
    expect(t1[1].slice(0, 5)).toEqual([bBefore, String(b.id), 'alipay', '50.00', 'pending'])

    const lists = recordRequests(page, 'GET', '/recharges')
    await page.getByTestId('fin-from').fill(d2)
    await page.getByTestId('fin-to').fill(d2)
    await page.getByTestId('fin-search').click()
    await expect.poll(() => lists.at(-1)?.search).toBe(`?user_id=${b.id}&channel=alipay&status=pending&from=${d2}&to=${d2}&page=1&page_size=20`)
    await expect(page.getByTestId('rch-id')).toHaveText([bAfter])
    expect(ids(await exportNow('recharges', `user_id=${b.id}&channel=alipay&status=pending&from=${d2}&to=${d2}`))).toEqual([bAfter])
  })

  await test.step('改了筛选但没点「查询」：导出仍按已查询的条件（与屏幕上的表格一致）', async () => {
    await page.getByTestId('fin-filter-user').fill(String(e.id))
    await pick(page, 'fin-filter-status', '已关闭')
    await page.getByTestId('fin-from').fill(d1)
    await page.getByTestId('fin-to').fill(d1)
    const t = await exportNow('recharges', `user_id=${b.id}&channel=alipay&status=pending&from=${d2}&to=${d2}`)
    expect(t.slice(1).map((c) => c[0])).toEqual([bAfter])
    await expect(page.getByTestId('rch-id')).toHaveText([bAfter])
  })

  await test.step('导出失败（超过 10 万行）：原文提示一次、不下载、按钮恢复可用，再点可以导出', async () => {
    const exportPath = isPath('/recharges/export')
    const exports = recordRequests(page, 'GET', '/recharges/export')
    await page.route(exportPath, (r) => r.fulfill({ status: 400, contentType: 'application/json', body: JSON.stringify({ code: 'export_too_large', message: '导出行数超过 10 万，请缩小时间范围', request_id: 'req-exp-1' }) }), { times: 1 })
    const toastsBefore = (await toasts()).length
    const downloadsBefore = downloads.length
    await page.getByTestId('fin-export').click()
    await toast(page, '导出行数超过 10 万，请缩小时间范围')
    await expect(page.getByTestId('fin-export')).toBeEnabled()
    await drainTasks(page)
    expect((await toasts()).slice(toastsBefore)).toEqual(['导出行数超过 10 万，请缩小时间范围'])
    expect(downloads).toHaveLength(downloadsBefore)
    expect(exports).toHaveLength(1)
    const pending = page.waitForEvent('download')
    await page.getByTestId('fin-export').click()
    expect((await pending).suggestedFilename()).toMatch(/^recharges-\d{8}-\d{6}\.csv$/)
    expect(exports).toHaveLength(2)
    expect((await toasts()).slice(toastsBefore)).toHaveLength(1)
  })

  await test.step('连点两次导出（同一任务内）：只发一个导出请求、只下载一次', async () => {
    const exports = recordRequests(page, 'GET', '/recharges/export')
    const downloadsBefore = downloads.length
    const pending = page.waitForEvent('download')
    await clickTwiceSync(page.getByTestId('fin-export'))
    await pending
    await expect(page.getByTestId('fin-export')).toBeEnabled()
    await drainTasks(page)
    expect(exports).toHaveLength(1)
    expect(downloads).toHaveLength(downloadsBefore + 1)
  })

  await test.step('超级管理员：三个财务菜单可见，列表可加载，可导出', async () => {
    await logout(page)
    const sup = await createAdmin('super')
    await loginAdmin(page, sup)
    for (const endpoint of ['orders', 'ledger', 'recharges']) {
      const table = await exportWith(endpoint, async () => {
        await page.getByTestId('fin-filter-user').fill(String(e.id))
      }, `user_id=${e.id}`)
      const api = (await getJSON<PagedOf<{ id: string | number }>>(ctx, `/admin/api/v1/${endpoint}?user_id=${e.id}&page_size=100`)).items
      await expect(page.getByTestId('fin-total')).toHaveText(`共 ${api.length} 条`)
      expect(table.slice(1).map((c) => c[0]).sort()).toEqual(api.map((x) => String(x.id)).sort())
    }
  })
})
