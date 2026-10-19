import { expect, test, type APIRequestContext, type Page } from '@playwright/test'
import {
  adminApi, adminPost, clickTwiceSync, createAdmin, drainTasks, enterTotpDialog, expectAdminUrl, expectStillOpen, fail500, failOnce, getJSON, isPath, loginAdmin, logout, pick, recordRequests, trackToasts,
} from '../helpers/admin'
import { seedUser, toast, userApi } from '../helpers/console'
import { hooks } from '../helpers/hooks'
import { runningInstance } from '../helpers/instances'
import { freshPublicKey, XSS, xssFired } from '../helpers/sshkeys'
import { cstTime } from '../helpers/time'

type User = { id: number; phone: string; kind: string; kyc_status: string; status: string; created_at: string }
type Detail = {
  user: User
  balance: number
  instances: { id: string; state: string }[]
  orders: { id: string; kind: string; amount: number; status: string }[]
  ledger: { kind: string; amount: number; balance_after: number; remark: string }[]
  kyc: { kind: string; real_name: string; id_number_masked?: string; result: string; reason: string }[]
}

/** 完整身份证号：任何地方（页面、接口响应、浏览器存储）都不能出现 */
const FULL_ID = '110101199001011234'
const MASKED_ID = '110***********1234'

const detailOf = (ctx: APIRequestContext, id: number) => getJSON<Detail>(ctx, `/admin/api/v1/users/${id}`)

const rows = (page: Page) => page.getByRole('row').filter({ has: page.getByTestId('user-id') })
const rowOf = (page: Page, id: number) => rows(page).filter({ has: page.getByTestId('user-id').getByText(String(id), { exact: true }) })

test('A-USR-01 用户搜索：手机号、尾号、ID 都命中；字母被拦截；实名 / 账号筛选与重置；超过一页翻页；首次加载失败可重试', async ({ page }) => {
  // 21 个填充用户 + u + v：同一假时钟时刻创建，列表按 created_at DESC, id DESC，本用例造的 23 人就是最新的 23 人
  const fillers: number[] = []
  for (let i = 0; i < 21; i++) fillers.push((await seedUser()).id)
  const u = await seedUser()
  const v = await seedUser({ verified: false })
  const admin = await createAdmin('ops')
  const ctx = await adminApi(admin)
  const uApi = (await getJSON<{ items: User[] }>(ctx, `/admin/api/v1/users?q=${u.phone}`)).items
  expect(uApi.map((x) => x.id)).toEqual([u.id])

  const listRequests = recordRequests(page, 'GET', '/users')
  const searches = () => listRequests.map((r) => r.search)
  const toasts = await trackToasts(page)
  const isList = isPath('/users')

  await test.step('首次加载失败：页内错误与重试，只提示一次，不自动重试', async () => {
    await page.route(isList, fail500('req-usr-1'))
    await loginAdmin(page, admin)
    await page.getByTestId('menu-users').click()
    await expectAdminUrl(page, '/users')
    await expect(page.getByTestId('users-error')).toContainText('服务器内部错误（请求号 req-usr-1）')
    await expect(rows(page)).toHaveCount(0)
    await drainTasks(page)
    expect(searches()).toEqual(['?page=1&page_size=20'])
    expect(await toasts()).toEqual(['服务器内部错误（请求号 req-usr-1）'])
    await page.unroute(isList)
    await page.getByTestId('users-retry').click()
    await expect(page.getByTestId('users-error')).toHaveCount(0)
    await expect(rows(page)).toHaveCount(20)
    expect(searches()).toEqual(['?page=1&page_size=20', '?page=1&page_size=20'])
  })

  await test.step('完整手机号：q=手机号，只命中 u，各列与接口一致', async () => {
    await page.getByTestId('users-q').fill(u.phone)
    await page.getByTestId('users-search').click()
    await expect.poll(() => searches().at(-1)).toBe(`?q=${u.phone}&page=1&page_size=20`)
    await expect(page.getByTestId('users-total')).toHaveText('共 1 人')
    await expect(rows(page)).toHaveCount(1)
    const row = rows(page).first()
    await expect(row.getByTestId('user-id')).toHaveText(String(u.id))
    await expect(row.getByTestId('user-id').locator('a')).toHaveAttribute('href', `/admin/users/${u.id}`)
    await expect(row.getByTestId('user-phone')).toHaveText(u.phone)
    await expect(row.getByTestId('user-kind')).toHaveText('个人')
    await expect(row.getByTestId('user-kyc')).toHaveText('已认证')
    await expect(row.getByTestId('user-status')).toHaveText('正常')
    await expect(row.getByTestId('user-created')).toHaveText(cstTime(uApi[0].created_at))
  })

  await test.step('手机尾号 4 位：q=尾号，命中 u；每一行都是尾号或 ID 匹配', async () => {
    const tail = u.phone.slice(-4)
    await page.getByTestId('users-q').fill(tail)
    await page.getByTestId('users-search').click()
    await expect.poll(() => searches().at(-1)).toBe(`?q=${tail}&page=1&page_size=20`)
    await expect(rowOf(page, u.id)).toHaveCount(1)
    await expect(rowOf(page, u.id).getByTestId('user-phone')).toHaveText(u.phone)
    const phones = await page.getByTestId('user-phone').allTextContents()
    const ids = await page.getByTestId('user-id').allTextContents()
    expect(phones.every((p, i) => p.endsWith(tail) || ids[i] === tail)).toBe(true)
  })

  await test.step('用户 ID：q=ID，命中 u；每一行都是 ID 或尾号匹配', async () => {
    await page.getByTestId('users-q').fill(String(u.id))
    await page.getByTestId('users-search').click()
    await expect.poll(() => searches().at(-1)).toBe(`?q=${u.id}&page=1&page_size=20`)
    await expect(rowOf(page, u.id)).toHaveCount(1)
    await expect(rowOf(page, u.id).getByTestId('user-phone')).toHaveText(u.phone)
    const phones = await page.getByTestId('user-phone').allTextContents()
    const ids = await page.getByTestId('user-id').allTextContents()
    expect(ids.every((id, i) => id === String(u.id) || phones[i].endsWith(String(u.id)))).toBe(true)
  })

  await test.step('字母被拦截：输入框只留数字，发出的 q 也只有数字', async () => {
    await page.getByTestId('users-q').fill('abc')
    await expect(page.getByTestId('users-q')).toHaveValue('')
    await page.getByTestId('users-q').fill('12ab34')
    await expect(page.getByTestId('users-q')).toHaveValue('1234')
    await page.getByTestId('users-search').click()
    await expect.poll(() => searches().at(-1)).toBe('?q=1234&page=1&page_size=20')
  })

  await test.step('全角数字转半角而不是丢掉：输入法打出的「１２ａｂ３４」→ 1234；全角手机号照样命中', async () => {
    await page.getByTestId('users-q').fill('１２ａｂ３４')
    await expect(page.getByTestId('users-q')).toHaveValue('1234')
    const fullWidth = u.phone.replace(/\d/g, (d) => String.fromCharCode(d.charCodeAt(0) + 0xfee0))
    await page.getByTestId('users-q').fill(fullWidth)
    await expect(page.getByTestId('users-q')).toHaveValue(u.phone)
    await page.getByTestId('users-search').click()
    await expect.poll(() => searches().at(-1)).toBe(`?q=${u.phone}&page=1&page_size=20`)
    await expect(rows(page)).toHaveCount(1)
    await expect(rows(page).first().getByTestId('user-id')).toHaveText(String(u.id))
  })

  await test.step('实名 / 账号筛选作为查询参数：v 未认证；封禁后按「已封禁」命中', async () => {
    await page.getByTestId('users-q').fill(v.phone)
    await pick(page, 'users-filter-kyc', '已认证')
    await page.getByTestId('users-search').click()
    await expect.poll(() => searches().at(-1)).toBe(`?q=${v.phone}&kyc_status=verified&page=1&page_size=20`)
    await expect(page.getByTestId('users-total')).toHaveText('共 0 人')
    await expect(rows(page)).toHaveCount(0)
    await pick(page, 'users-filter-kyc', '未认证')
    await pick(page, 'users-filter-status', '已封禁')
    await page.getByTestId('users-search').click()
    await expect.poll(() => searches().at(-1)).toBe(`?q=${v.phone}&kyc_status=none&status=banned&page=1&page_size=20`)
    await expect(page.getByTestId('users-total')).toHaveText('共 0 人')
    await adminPost(ctx, admin, `/admin/api/v1/users/${v.id}/ban`, { reason: '筛选测试' })
    const before = listRequests.length
    await page.getByTestId('users-search').click()
    await expect.poll(() => listRequests.length).toBe(before + 1)
    await expect(page.getByTestId('users-total')).toHaveText('共 1 人')
    await expect(rows(page).first().getByTestId('user-id')).toHaveText(String(v.id))
    await expect(rows(page).first().getByTestId('user-kyc')).toHaveText('未认证')
    await expect(rows(page).first().getByTestId('user-status')).toHaveText('已封禁')
  })

  await test.step('重置：清空条件，只发一次不带筛选的请求；第 1 页就是本用例最新造的 20 人', async () => {
    const before = listRequests.length
    await page.getByTestId('users-reset').click()
    await expect.poll(() => searches().at(-1)).toBe('?page=1&page_size=20')
    await expect(page.getByTestId('users-q')).toHaveValue('')
    await expect(page.getByTestId('users-filter-kyc')).not.toContainText('未认证')
    await expect(page.getByTestId('users-filter-status')).not.toContainText('已封禁')
    await drainTasks(page)
    expect(listRequests).toHaveLength(before + 1)
    const newest = [v.id, u.id, ...fillers.slice(3).reverse()].map(String)
    await expect(page.getByTestId('user-id')).toHaveText(newest)
  })

  await test.step('翻到第 2 页：page=2，前三行是最早造的三个填充用户', async () => {
    await page.getByTestId('users-pager').getByText('2', { exact: true }).click()
    await expect.poll(() => searches().at(-1)).toBe('?page=2&page_size=20')
    await expect(page.getByTestId('user-id').nth(0)).toHaveText(String(fillers[2]))
    await expect(page.getByTestId('user-id').nth(1)).toHaveText(String(fillers[1]))
    await expect(page.getByTestId('user-id').nth(2)).toHaveText(String(fillers[0]))
  })

  await test.step('点用户 ID 进详情', async () => {
    await page.getByTestId('users-q').fill(u.phone)
    await page.getByTestId('users-search').click()
    await expect(rows(page)).toHaveCount(1)
    await rowOf(page, u.id).getByTestId('user-id').getByRole('link').click()
    await expectAdminUrl(page, `/users/${u.id}`)
    await expect(page.getByTestId('udetail-id')).toHaveText(String(u.id))
  })
})

test('A-USR-02 用户详情：余额、实例、订单、流水、实名记录（身份证脱敏，完整号码不出现在页面 / 接口 / 存储）；按角色显示入口；首次加载失败可重试；换用户重新加载', async ({ page }) => {
  const u = await seedUser({ verified: false, balance: 100000 })
  const uApi = await userApi(u.phone)
  const kyc = await uApi.post('/api/v1/kyc/personal', { data: { name: '张三', id_number: FULL_ID } })
  expect(kyc.ok()).toBe(true)
  const { id } = await runningInstance(u.phone)
  const other = await seedUser({ balance: 5000 })
  const admin = await createAdmin('finance')
  const ctx = await adminApi(admin)
  const d = await detailOf(ctx, u.id)

  await test.step('后端返回的就是脱敏值：完整号码不在用户详情响应里', async () => {
    expect(d.kyc).toHaveLength(1)
    expect(d.kyc[0].id_number_masked).toBe(MASKED_ID)
    expect(JSON.stringify(d)).not.toContain(FULL_ID)
  })

  // 记录页面收到的每个后台接口响应，最后统一检查不含完整身份证号
  const bodies: Promise<string>[] = []
  page.on('response', (r) => {
    if (new URL(r.url()).pathname.startsWith('/admin/api/')) bodies.push(r.text().catch(() => ''))
  })
  const toasts = await trackToasts(page)
  const gets = recordRequests(page, 'GET', `/users/${u.id}`)
  const isDetail = isPath(`/users/${u.id}`)

  await test.step('首次加载失败：页内错误与重试，只提示一次，不自动重试', async () => {
    await page.route(isDetail, fail500('req-usr-2'))
    await loginAdmin(page, admin)
    await page.goto(`/admin/users/${u.id}`)
    await expect(page.getByTestId('udetail-error')).toContainText('服务器内部错误（请求号 req-usr-2）')
    await expect(page.getByTestId('udetail-id')).toHaveCount(0)
    await drainTasks(page)
    expect(gets).toHaveLength(1)
    expect(await toasts()).toEqual(['服务器内部错误（请求号 req-usr-2）'])
    await page.unroute(isDetail)
    await page.getByTestId('udetail-retry').click()
    await expect(page.getByTestId('udetail-id')).toHaveText(String(u.id))
    await expect(page.getByTestId('udetail-error')).toHaveCount(0)
    expect(gets).toHaveLength(2)
  })

  await test.step('基本信息：实名后「已认证」、余额 ¥931.00（1000 − 69）', async () => {
    await expect(page.getByTestId('udetail-phone')).toHaveText(u.phone)
    await expect(page.getByTestId('udetail-kind')).toHaveText('个人')
    await expect(page.getByTestId('udetail-kyc')).toHaveText('已认证')
    await expect(page.getByTestId('udetail-status')).toHaveText('正常')
    expect(d.balance).toBe(93100)
    await expect(page.getByTestId('udetail-balance')).toHaveText('¥931.00')
    await expect(page.getByTestId('udetail-created')).toHaveText(cstTime(d.user.created_at))
  })

  await test.step('实例：运行中实例，链接到实例详情；更多实例带 user_id', async () => {
    const row = page.getByTestId('udetail-instance-row')
    await expect(row).toHaveCount(1)
    await expect(row.getByTestId('udetail-instance-id')).toHaveText(id)
    await expect(row.getByTestId('udetail-instance-id').locator('a')).toHaveAttribute('href', `/admin/instances/${id}`)
    await expect(row.getByTestId('udetail-instance-state')).toHaveText('运行中')
    await expect(page.getByTestId('udetail-more-instances')).toHaveAttribute('href', `/admin/instances?user_id=${u.id}`)
  })

  await test.step('订单：新购 ¥69.00 已支付；财务可见更多订单', async () => {
    await page.getByRole('tab', { name: '订单' }).click()
    const row = page.getByTestId('udetail-order-row')
    await expect(row).toHaveCount(1)
    await expect(row.getByTestId('udetail-order-id')).toHaveText(d.orders[0].id)
    await expect(row.getByTestId('udetail-order-kind')).toHaveText('新购')
    await expect(row.getByTestId('udetail-order-amount')).toHaveText('¥69.00')
    await expect(row.getByTestId('udetail-order-status')).toHaveText('已支付')
    await expect(page.getByTestId('udetail-more-orders')).toHaveAttribute('href', `/admin/orders?user_id=${u.id}`)
  })

  await test.step('流水：与接口逐行一致，最新一行扣款 −¥69.00、余额 ¥931.00；财务可见更多流水与充值单', async () => {
    await page.getByRole('tab', { name: '流水' }).click()
    const sign = (n: number) => (n >= 0 ? '+' : '-') + '¥' + (Math.abs(n) / 100).toLocaleString('en-US', { minimumFractionDigits: 2 })
    await expect(page.getByTestId('udetail-ledger-row')).toHaveCount(d.ledger.length)
    await expect(page.getByTestId('udetail-ledger-amount')).toHaveText(d.ledger.map((l) => sign(l.amount)))
    const first = page.getByTestId('udetail-ledger-row').first()
    await expect(first.getByTestId('udetail-ledger-kind')).toHaveText('扣款')
    await expect(first.getByTestId('udetail-ledger-amount')).toHaveText('-¥69.00')
    await expect(first.getByTestId('udetail-ledger-after')).toHaveText('¥931.00')
    await expect(page.getByTestId('udetail-more-ledger')).toHaveAttribute('href', `/admin/ledger?user_id=${u.id}`)
    await expect(page.getByTestId('udetail-more-recharges')).toHaveAttribute('href', `/admin/recharges?user_id=${u.id}`)
  })

  await test.step('实名记录：个人 · 张三 · 110***********1234 · 已通过', async () => {
    await page.getByRole('tab', { name: '实名记录' }).click()
    const row = page.getByTestId('udetail-kyc-row')
    await expect(row).toHaveCount(1)
    await expect(row.getByTestId('udetail-kyc-kind')).toHaveText('个人')
    await expect(row.getByTestId('udetail-kyc-name')).toHaveText('张三')
    await expect(row.getByTestId('udetail-kyc-idno')).toHaveText(MASKED_ID)
    await expect(row.getByTestId('udetail-kyc-result')).toHaveText('已通过')
    await expect(row.getByTestId('udetail-kyc-reason')).toHaveText('')
  })

  await test.step('完整身份证号不出现在页面（含隐藏标签页）、任何后台接口响应、浏览器存储里', async () => {
    expect(await page.content()).not.toContain(FULL_ID)
    expect(await page.evaluate(() => document.body.innerText)).toContain(MASKED_ID)
    const texts = await Promise.all(bodies)
    expect(texts.some((t) => t.includes(MASKED_ID))).toBe(true) // 确实记录到了详情响应
    expect(texts.filter((t) => t.includes(FULL_ID))).toEqual([])
    const storage = await page.evaluate(() => JSON.stringify({ local: { ...localStorage }, session: { ...sessionStorage }, cookie: document.cookie }))
    expect(storage).not.toContain(FULL_ID)
  })

  await test.step('同一页面换用户（路由参数变化，组件复用）：按新 ID 重新加载；先发后到的旧响应被丢弃', async () => {
    const marker = await page.evaluate(([a, b]) => {
      ;(window as unknown as { __sameDoc: boolean }).__sameDoc = true
      history.pushState(null, '', `/admin/users/${b}`)
      history.pushState(null, '', `/admin/users/${a}`)
      history.back()
      return true
    }, [u.id, other.id])
    expect(marker).toBe(true)
    await expectAdminUrl(page, `/users/${other.id}`)
    await expect(page.getByTestId('udetail-id')).toHaveText(String(other.id))
    await expect(page.getByTestId('udetail-phone')).toHaveText(other.phone)
    await expect(page.getByTestId('udetail-balance')).toHaveText('¥50.00')
    await expect(page.getByTestId('udetail-instance-row')).toHaveCount(0)
    expect(await page.evaluate(() => (window as unknown as { __sameDoc?: boolean }).__sameDoc)).toBe(true) // 没有整页刷新

    // 后退到 u 时把它的请求扣住，再前进到 other；other 显示后才放行 u 的响应
    let release!: () => void
    const gate = new Promise<void>((r) => (release = r))
    let held = 0
    await page.route(isDetail, async (route) => {
      held += 1
      await gate
      await route.continue()
    })
    await page.evaluate(() => history.back())
    await expect.poll(() => held).toBe(1)
    await expectAdminUrl(page, `/users/${u.id}`)
    await page.evaluate(() => history.forward())
    await expectAdminUrl(page, `/users/${other.id}`)
    await expect(page.getByTestId('udetail-phone')).toHaveText(other.phone)
    const late = page.waitForResponse((r) => isDetail(new URL(r.url())))
    release()
    await late
    await drainTasks(page)
    await expect(page.getByTestId('udetail-phone')).toHaveText(other.phone)
    await expect(page.getByTestId('udetail-balance')).toHaveText('¥50.00')
    await page.unroute(isDetail)
  })

  await test.step('按角色显示：财务只有调账 / 手动入账；运维只有封禁，没有订单 / 流水 / 充值单入口；超管全部可见', async () => {
    const actions = ['udetail-ban-btn', 'udetail-unban-btn', 'udetail-adjust-btn', 'udetail-recharge-btn'] as const
    const visible = async () => {
      const out: string[] = []
      for (const t of actions) if ((await page.getByTestId(t).count()) > 0) out.push(t)
      return out
    }
    await page.goto(`/admin/users/${u.id}`)
    await expect(page.getByTestId('udetail-id')).toHaveText(String(u.id))
    expect(await visible()).toEqual(['udetail-adjust-btn', 'udetail-recharge-btn'])

    await logout(page)
    await loginAdmin(page, await createAdmin('ops'))
    await page.goto(`/admin/users/${u.id}`)
    await expect(page.getByTestId('udetail-id')).toHaveText(String(u.id))
    expect(await visible()).toEqual(['udetail-ban-btn'])
    await expect(page.getByTestId('udetail-more-instances')).toHaveAttribute('href', `/admin/instances?user_id=${u.id}`)
    await expect(page.getByTestId('udetail-more-orders')).toHaveCount(0)
    await expect(page.getByTestId('udetail-more-ledger')).toHaveCount(0)
    await expect(page.getByTestId('udetail-more-recharges')).toHaveCount(0)

    await logout(page)
    await loginAdmin(page, await createAdmin('super'))
    await page.goto(`/admin/users/${u.id}`)
    await expect(page.getByTestId('udetail-id')).toHaveText(String(u.id))
    expect(await visible()).toEqual(['udetail-ban-btn', 'udetail-adjust-btn', 'udetail-recharge-btn'])
    await expect(page.getByTestId('udetail-more-orders')).toHaveCount(1)
    await expect(page.getByTestId('udetail-more-ledger')).toHaveCount(1)
    await expect(page.getByTestId('udetail-more-recharges')).toHaveCount(1)
  })
})

test('A-USR-03 封禁需原因；封禁后状态已封禁、用户会话失效；解封恢复；失败保留输入；连点只发一次', async ({ page }) => {
  const u = await seedUser()
  const uApi = await userApi(u.phone)
  expect((await uApi.get('/api/v1/wallet')).status()).toBe(200)
  const admin = await createAdmin('ops')
  const ctx = await adminApi(admin)
  const bans = recordRequests(page, 'POST', `/users/${u.id}/ban`)
  const unbans = recordRequests(page, 'POST', `/users/${u.id}/unban`)
  const toasts = await trackToasts(page)
  await loginAdmin(page, admin)
  await page.goto(`/admin/users/${u.id}`)
  await expect(page.getByTestId('udetail-status')).toHaveText('正常')

  await test.step('原因必填（空白不算），不发请求', async () => {
    await page.getByTestId('udetail-ban-btn').click()
    await expect(page.getByTestId('ban-dialog')).toBeVisible()
    await page.getByTestId('ban-submit').click()
    await expect(page.getByTestId('ban-error')).toHaveText('请填写封禁原因')
    await page.getByTestId('ban-reason').fill('   ')
    await page.getByTestId('ban-submit').click()
    await expect(page.getByTestId('ban-error')).toHaveText('请填写封禁原因')
    await drainTasks(page)
    expect(bans).toHaveLength(0)
  })

  await test.step('接口失败：只提示一次，弹窗不关、原因保留', async () => {
    await page.getByTestId('ban-reason').fill(' 违规使用 ')
    await failOnce(page, `/users/${u.id}/ban`, 'req-ban-1')
    await page.getByTestId('ban-submit').click()
    await toast(page, '服务器内部错误（请求号 req-ban-1）')
    await expectStillOpen(page, 'ban-dialog', '服务器内部错误（请求号 req-ban-1）')
    await expect(page.getByTestId('ban-reason')).toHaveValue(' 违规使用 ')
    await expect(page.getByTestId('udetail-status')).toHaveText('正常')
    expect(await toasts()).toEqual(['服务器内部错误（请求号 req-ban-1）'])
    expect(bans).toHaveLength(1)
  })

  await test.step('连点确定封禁只发一次（原因去掉首尾空白）；状态已封禁，用户会话失效', async () => {
    await clickTwiceSync(page.getByTestId('ban-submit'))
    await toast(page, '已封禁')
    await expect(page.getByTestId('ban-dialog')).toBeHidden()
    await expect(page.getByTestId('udetail-status')).toHaveText('已封禁')
    await expect(page.getByTestId('udetail-ban-btn')).toHaveCount(0)
    await drainTasks(page)
    expect(bans.map((b) => b.body)).toEqual([{ reason: '违规使用' }, { reason: '违规使用' }])
    expect((await detailOf(ctx, u.id)).user.status).toBe('banned')
    expect((await uApi.get('/api/v1/wallet')).status()).toBe(401)
  })

  await test.step('解封：取消不发请求；失败只提示一次且弹窗不关；连点确定只发一次，状态恢复正常', async () => {
    await page.getByTestId('udetail-unban-btn').click()
    await expect(page.getByTestId('unban-dialog')).toBeVisible()
    await page.getByTestId('unban-cancel').click()
    await expect(page.getByTestId('unban-dialog')).toBeHidden()
    expect(unbans).toHaveLength(0)

    await page.getByTestId('udetail-unban-btn').click()
    await failOnce(page, `/users/${u.id}/unban`, 'req-unban-1')
    await page.getByTestId('unban-ok').click()
    await toast(page, '服务器内部错误（请求号 req-unban-1）')
    await expectStillOpen(page, 'unban-dialog', '服务器内部错误（请求号 req-unban-1）')
    await expect(page.getByTestId('udetail-status')).toHaveText('已封禁')

    await clickTwiceSync(page.getByTestId('unban-ok'))
    await toast(page, '已解封')
    await expect(page.getByTestId('unban-dialog')).toBeHidden()
    await expect(page.getByTestId('udetail-status')).toHaveText('正常')
    await expect(page.getByTestId('udetail-ban-btn')).toBeVisible()
    await drainTasks(page)
    expect(unbans).toHaveLength(2) // 失败的一次 + 连点中的一次
    expect((await detailOf(ctx, u.id)).user.status).toBe('active')
    expect(await toasts()).toEqual(['服务器内部错误（请求号 req-ban-1）', '已封禁', '服务器内部错误（请求号 req-unban-1）', '已解封'])
  })
})

test('A-USR-04 调账 +¥10 与 −¥5（动态码）→ 余额与流水；金额按整数分提交；取消动态码保留输入；扣成负数本地拦截，并发导致的余额不足用后台措辞提示一次并刷新', async ({ page }) => {
  const u = await seedUser({ balance: 100000 })
  const admin = await createAdmin('finance')
  const ctx = await adminApi(admin)
  const posts = recordRequests(page, 'POST', `/users/${u.id}/adjust`)
  const toasts = await trackToasts(page)
  await loginAdmin(page, admin)
  await page.goto(`/admin/users/${u.id}`)
  await expect(page.getByTestId('udetail-balance')).toHaveText('¥1,000.00')
  const amount = page.getByTestId('adjust-amount')
  const remark = page.getByTestId('adjust-remark')

  await test.step('金额格式、范围与备注在本地拦截，不发请求、不进确认', async () => {
    await page.getByTestId('udetail-adjust-btn').click()
    await expect(page.getByTestId('adjust-dialog')).toBeVisible()
    await remark.fill('活动补偿')
    for (const bad of ['', '0', '-0', 'abc', '1.234', '--1', '- 5', '+5', '1e3']) {
      await amount.fill(bad)
      await page.getByTestId('adjust-submit').click()
      await expect(page.getByTestId('adjust-error')).toHaveText('金额格式不正确（可为负数，不能为 0）')
    }
    await amount.fill('-1000000.01')
    await page.getByTestId('adjust-submit').click()
    await expect(page.getByTestId('adjust-error')).toHaveText('调整金额需在 ±100 万元之间')
    await amount.fill('10')
    await remark.fill('  ')
    await page.getByTestId('adjust-submit').click()
    await expect(page.getByTestId('adjust-error')).toHaveText('请填写备注')
    await expect(page.getByTestId('adjust-confirm-dialog')).toBeHidden()
    await drainTasks(page)
    expect(posts).toHaveLength(0)
  })

  await test.step('取消动态码：只发出不带 totp 的一次，无提示；回到填写弹窗、输入保留，余额不变', async () => {
    await remark.fill('活动补偿')
    await page.getByTestId('adjust-submit').click()
    await expect(page.getByTestId('adjust-error')).toHaveCount(0)
    await expect(page.getByTestId('adjust-confirm-text')).toHaveText('将把余额从 ¥1,000.00 调整为 ¥1,010.00')
    await page.getByTestId('adjust-confirm-ok').click()
    await expect(page.getByTestId('totp-dialog')).toBeVisible()
    await page.getByTestId('totp-cancel').click()
    await expect(page.getByTestId('totp-dialog')).toBeHidden()
    await expect(page.getByTestId('adjust-confirm-dialog')).toBeHidden()
    await expect(page.getByTestId('adjust-dialog')).toBeVisible()
    await expect(amount).toHaveValue('10')
    await expect(remark).toHaveValue('活动补偿')
    await drainTasks(page)
    expect(posts.map((p) => p.body)).toEqual([{ amount: 1000, remark: '活动补偿' }])
    expect(await toasts()).toEqual([])
    expect((await detailOf(ctx, u.id)).balance).toBe(100000)
  })

  await test.step('+10：连点确定，弹动态码，原请求带 totp 只重发一次；余额与流水更新', async () => {
    await page.getByTestId('adjust-submit').click()
    await expect(page.getByTestId('adjust-confirm-text')).toHaveText('将把余额从 ¥1,000.00 调整为 ¥1,010.00')
    await clickTwiceSync(page.getByTestId('adjust-confirm-ok'))
    await enterTotpDialog(page, admin.totp_secret!)
    await toast(page, '余额已调整为 ¥1,010.00')
    await expect(page.getByTestId('adjust-dialog')).toBeHidden()
    await expect(page.getByTestId('udetail-balance')).toHaveText('¥1,010.00')
    await drainTasks(page)
    expect(posts.map((p) => p.body)).toEqual([
      { amount: 1000, remark: '活动补偿' },
      { amount: 1000, remark: '活动补偿' },
      { amount: 1000, remark: '活动补偿', totp: expect.stringMatching(/^\d{6}$/) },
    ])
    expect((await detailOf(ctx, u.id)).balance).toBe(101000)
    await page.getByRole('tab', { name: '流水' }).click()
    const first = page.getByTestId('udetail-ledger-row').first()
    await expect(first.getByTestId('udetail-ledger-kind')).toHaveText('调账')
    await expect(first.getByTestId('udetail-ledger-amount')).toHaveText('+¥10.00')
    await expect(first.getByTestId('udetail-ledger-after')).toHaveText('¥1,010.00')
    await expect(first.getByTestId('udetail-ledger-remark')).toHaveText('活动补偿')
  })

  await test.step('−5（再打开时输入已清空）：5 分钟内不再要动态码，只发一次 amount=-500', async () => {
    await page.getByTestId('udetail-adjust-btn').click()
    await expect(amount).toHaveValue('')
    await expect(remark).toHaveValue('')
    await amount.fill('-5')
    await remark.fill('纠错')
    await page.getByTestId('adjust-submit').click()
    await expect(page.getByTestId('adjust-confirm-text')).toHaveText('将把余额从 ¥1,010.00 调整为 ¥1,005.00')
    await page.getByTestId('adjust-confirm-ok').click()
    await toast(page, '余额已调整为 ¥1,005.00')
    await expect(page.getByTestId('totp-dialog')).toBeHidden()
    await expect(page.getByTestId('udetail-balance')).toHaveText('¥1,005.00')
    await drainTasks(page)
    expect(posts.slice(3).map((p) => p.body)).toEqual([{ amount: -500, remark: '纠错' }])
    const ledger = page.getByTestId('udetail-ledger-row')
    await expect(ledger.nth(0).getByTestId('udetail-ledger-kind')).toHaveText('调账')
    await expect(ledger.nth(0).getByTestId('udetail-ledger-amount')).toHaveText('-¥5.00')
    await expect(ledger.nth(0).getByTestId('udetail-ledger-after')).toHaveText('¥1,005.00')
    await expect(ledger.nth(0).getByTestId('udetail-ledger-remark')).toHaveText('纠错')
    await expect(ledger.nth(1).getByTestId('udetail-ledger-amount')).toHaveText('+¥10.00')
    const d = await detailOf(ctx, u.id)
    expect(d.ledger.slice(0, 2).map((l) => [l.kind, l.amount, l.balance_after])).toEqual([['admin_adjust', -500, 100500], ['admin_adjust', 1000, 101000]])
  })

  await test.step('扣减超过当前余额：本地拦截（页内提示），不进确认、不发请求', async () => {
    await page.getByTestId('udetail-adjust-btn').click()
    await amount.fill('-1005.01')
    await remark.fill('超额扣减')
    await page.getByTestId('adjust-submit').click()
    await expect(page.getByTestId('adjust-error')).toHaveText('扣减金额超过当前余额 ¥1,005.00')
    await expect(page.getByTestId('adjust-confirm-dialog')).toBeHidden()
    await drainTasks(page)
    expect(posts).toHaveLength(4)
  })

  await test.step('余额在别处变了（真实并发）：后端拒绝，用后台措辞只提示一次并刷新余额；回到填写弹窗、输入保留', async () => {
    // 另一个会话先扣掉 ¥5：服务器余额 ¥1,000.00，页面仍显示 ¥1,005.00
    await adminPost(ctx, admin, `/admin/api/v1/users/${u.id}/adjust`, { amount: -500, remark: '外部扣款' })
    expect((await detailOf(ctx, u.id)).balance).toBe(100000)
    await expect(page.getByTestId('udetail-balance')).toHaveText('¥1,005.00')
    await amount.fill('-1005')
    await page.getByTestId('adjust-submit').click()
    await expect(page.getByTestId('adjust-confirm-text')).toHaveText('将把余额从 ¥1,005.00 调整为 ¥0.00')
    await page.getByTestId('adjust-confirm-ok').click()
    const message = '扣减后余额将为负，已拒绝；余额可能已变化，已刷新'
    await toast(page, message)
    await expect(page.getByTestId('adjust-confirm-dialog')).toBeHidden()
    await expect(page.getByTestId('udetail-balance')).toHaveText('¥1,000.00')
    await expectStillOpen(page, 'adjust-dialog', message)
    await expect(amount).toHaveValue('-1005')
    await expect(remark).toHaveValue('超额扣减')
    await drainTasks(page)
    expect(posts.slice(4).map((p) => p.body)).toEqual([{ amount: -100500, remark: '超额扣减' }])
    expect(await toasts()).toEqual(['余额已调整为 ¥1,010.00', '余额已调整为 ¥1,005.00', message])
    expect((await detailOf(ctx, u.id)).balance).toBe(100000)
    // 刷新后的余额参与下一次本地校验
    await page.getByTestId('adjust-submit').click()
    await expect(page.getByTestId('adjust-error')).toHaveText('扣减金额超过当前余额 ¥1,000.00')
    await expect(page.getByTestId('adjust-confirm-dialog')).toBeHidden()
    expect(posts).toHaveLength(5)
  })
})

test('A-USR-05 手动入账 ¥100：充值单渠道「手动」、余额增加；金额与备注本地校验；确认步骤显示金额与用户；失败保留输入；动态码后带 totp 只重发一次', async ({ page }) => {
  const u = await seedUser({ balance: 100000 })
  const admin = await createAdmin('finance')
  const ctx = await adminApi(admin)
  const posts = recordRequests(page, 'POST', `/users/${u.id}/manual-recharge`)
  const toasts = await trackToasts(page)
  await loginAdmin(page, admin)
  await page.goto(`/admin/users/${u.id}`)
  await expect(page.getByTestId('udetail-balance')).toHaveText('¥1,000.00')
  const amount = page.getByTestId('mrecharge-amount')
  const remark = page.getByTestId('mrecharge-remark')

  await test.step('金额 1 元～100 万元、不接受负数与多位小数，备注必填；都不发请求', async () => {
    await page.getByTestId('udetail-recharge-btn').click()
    await expect(page.getByTestId('mrecharge-dialog')).toBeVisible()
    await remark.fill('对公转账 20261017-001')
    for (const [value, message] of [
      ['0.99', '入账金额需在 1 元到 100 万元之间'],
      ['1000000.01', '入账金额需在 1 元到 100 万元之间'],
      ['-100', '金额格式不正确'],
      ['100.001', '金额格式不正确'],
    ]) {
      await amount.fill(value)
      await page.getByTestId('mrecharge-submit').click()
      await expect(page.getByTestId('mrecharge-error')).toHaveText(message)
    }
    await amount.fill('100')
    await remark.fill(' ')
    await page.getByTestId('mrecharge-submit').click()
    await expect(page.getByTestId('mrecharge-error')).toHaveText('请填写备注（例如转账流水号）')
    await drainTasks(page)
    expect(posts).toHaveLength(0)
  })

  const confirmText = () => `将为用户 ${u.phone}（ID ${u.id}）入账 ¥100.00，余额 ¥1,000.00 → ¥1,100.00`

  await test.step('确认步骤：显示金额与用户；取消回到填写弹窗、不发请求', async () => {
    await remark.fill('对公转账 20261017-001')
    await page.getByTestId('mrecharge-submit').click()
    await expect(page.getByTestId('mrecharge-error')).toHaveCount(0)
    await expect(page.getByTestId('mrecharge-confirm-text')).toHaveText(confirmText())
    await page.getByTestId('mrecharge-confirm-cancel').click()
    await expect(page.getByTestId('mrecharge-confirm-dialog')).toBeHidden()
    await expect(page.getByTestId('mrecharge-dialog')).toBeVisible()
    await expect(amount).toHaveValue('100')
    await drainTasks(page)
    expect(posts).toHaveLength(0)
  })

  await test.step('接口失败：只提示一次，回到填写弹窗、输入保留', async () => {
    await page.getByTestId('mrecharge-submit').click()
    await failOnce(page, `/users/${u.id}/manual-recharge`, 'req-mr-1')
    await page.getByTestId('mrecharge-confirm-ok').click()
    await toast(page, '服务器内部错误（请求号 req-mr-1）')
    await expect(page.getByTestId('mrecharge-confirm-dialog')).toBeHidden()
    await expectStillOpen(page, 'mrecharge-dialog', '服务器内部错误（请求号 req-mr-1）')
    await expect(amount).toHaveValue('100')
    await expect(remark).toHaveValue('对公转账 20261017-001')
    await expect(page.getByTestId('mrecharge-error')).toHaveCount(0)
    expect(await toasts()).toEqual(['服务器内部错误（请求号 req-mr-1）'])
    expect(posts).toHaveLength(1)
  })

  await test.step('连点确定入账：只提交一次，弹动态码，原请求带 totp 只重发一次；余额 ¥1,100.00，流水与充值单', async () => {
    await page.getByTestId('mrecharge-submit').click()
    await expect(page.getByTestId('mrecharge-confirm-text')).toHaveText(confirmText())
    await clickTwiceSync(page.getByTestId('mrecharge-confirm-ok'))
    await enterTotpDialog(page, admin.totp_secret!)
    await toast(page, '已入账 ¥100.00')
    await expect(page.getByTestId('mrecharge-dialog')).toBeHidden()
    await expect(page.getByTestId('mrecharge-confirm-dialog')).toBeHidden()
    await expect(page.getByTestId('udetail-balance')).toHaveText('¥1,100.00')
    await drainTasks(page)
    const body = { amount: 10000, remark: '对公转账 20261017-001' }
    expect(posts.map((p) => p.body)).toEqual([body, body, { ...body, totp: expect.stringMatching(/^\d{6}$/) }])
    expect(await toasts()).toEqual(['服务器内部错误（请求号 req-mr-1）', '已入账 ¥100.00'])
    expect((await detailOf(ctx, u.id)).balance).toBe(110000)
    const recharges = (await getJSON<{ items: { channel: string; status: string; amount: number }[] }>(ctx, `/admin/api/v1/recharges?user_id=${u.id}`)).items
    expect(recharges.filter((r) => r.channel === 'manual').map((r) => [r.status, r.amount])).toEqual([['paid', 10000]])
    await page.getByRole('tab', { name: '流水' }).click()
    const first = page.getByTestId('udetail-ledger-row').first()
    await expect(first.getByTestId('udetail-ledger-kind')).toHaveText('充值')
    await expect(first.getByTestId('udetail-ledger-amount')).toHaveText('+¥100.00')
    await expect(first.getByTestId('udetail-ledger-after')).toHaveText('¥1,100.00')
  })
})

test('A-USR-06 60 秒内相同调账被拦截：只提示一次、保留输入、刷新余额；超过 60 秒可再次提交', async ({ page }) => {
  const u = await seedUser({ balance: 100000 })
  const admin = await createAdmin('finance')
  const ctx = await adminApi(admin)
  const posts = recordRequests(page, 'POST', `/users/${u.id}/adjust`)
  const toasts = await trackToasts(page)
  await loginAdmin(page, admin)
  await page.goto(`/admin/users/${u.id}`)
  const adjustOnce = async () => {
    await page.getByTestId('udetail-adjust-btn').click()
    await page.getByTestId('adjust-amount').fill('10')
    await page.getByTestId('adjust-remark').fill('重复提交测试')
    await page.getByTestId('adjust-submit').click()
    await page.getByTestId('adjust-confirm-ok').click()
  }

  await test.step('第一次成功', async () => {
    await adjustOnce()
    await enterTotpDialog(page, admin.totp_secret!)
    await toast(page, '余额已调整为 ¥1,010.00')
    await expect(page.getByTestId('udetail-balance')).toHaveText('¥1,010.00')
  })

  await test.step('60 秒内相同金额与备注：后端拒绝，只提示一次，回到填写弹窗、输入保留；失败后刷新出别处改过的余额', async () => {
    // 另一个会话加 ¥1：服务器余额 ¥1,011.00，页面仍显示 ¥1,010.00；失败后页面应刷新成最新余额
    await adminPost(ctx, admin, `/admin/api/v1/users/${u.id}/adjust`, { amount: 100, remark: '外部调整' })
    await expect(page.getByTestId('udetail-balance')).toHaveText('¥1,010.00')
    await adjustOnce()
    await toast(page, '60 秒内已提交过相同的操作，请勿重复提交')
    await expect(page.getByTestId('adjust-confirm-dialog')).toBeHidden()
    await expectStillOpen(page, 'adjust-dialog', '60 秒内已提交过相同的操作，请勿重复提交')
    await expect(page.getByTestId('adjust-amount')).toHaveValue('10')
    await expect(page.getByTestId('adjust-remark')).toHaveValue('重复提交测试')
    await expect(page.getByTestId('udetail-balance')).toHaveText('¥1,011.00')
    expect((await detailOf(ctx, u.id)).balance).toBe(101100)
    expect(await toasts()).toEqual(['余额已调整为 ¥1,010.00', '60 秒内已提交过相同的操作，请勿重复提交'])
  })

  await test.step('超过 60 秒后同一笔可以再提交', async () => {
    await hooks.advance(61)
    await page.getByTestId('adjust-submit').click()
    await page.getByTestId('adjust-confirm-ok').click()
    await toast(page, '余额已调整为 ¥1,021.00')
    await expect(page.getByTestId('udetail-balance')).toHaveText('¥1,021.00')
    await drainTasks(page)
    expect(posts.filter((p) => !('totp' in (p.body ?? {}))).map((p) => p.body)).toEqual(Array(3).fill({ amount: 1000, remark: '重复提交测试' }))
  })
})

test('A-USR-07 用户详情显示 SSH 公钥（名称按文本显示）', async ({ page }) => {
  const u = await seedUser()
  const api = await userApi(u.phone)
  expect((await api.post('/api/v1/ssh-keys', { data: { public_key: freshPublicKey('laptop') } })).status()).toBe(201)
  expect((await api.post('/api/v1/ssh-keys', { data: { public_key: freshPublicKey(), name: XSS } })).status()).toBe(201)
  const admin = await createAdmin('finance')
  await loginAdmin(page, admin)
  await page.goto(`/admin/users/${u.id}`)
  await page.getByRole('tab', { name: 'SSH 公钥' }).click()
  await expect(page.getByTestId('udetail-key-row')).toHaveCount(2)
  await expect(page.getByTestId('udetail-key-name')).toHaveText(['laptop', XSS])
  await expect(page.getByTestId('udetail-key-fp').first()).toHaveText(/^SHA256:/)
  await expect(page.locator('[data-testid="udetail-key-row"] img')).toHaveCount(0)
  expect(await xssFired(page)).toBe(false)
})
