import { expect, test, type Page } from '@playwright/test'
import { adminApi, clickTwiceSync, createAdmin, drainIdleMachines } from '../helpers/admin'
import { bookViaApi, cancelBookingsAndDispose, exact, groupMachineIds, pickOption, privateGroup, startAfter } from '../helpers/booking'
import { pauseClock } from '../helpers/clock'
import { dialog, loginConsole, nav, seedUser, toast, userApi } from '../helpers/console'
import { hooks } from '../helpers/hooks'
import { cstDate, cstTime } from '../helpers/time'

const HOUR = 3600e3
const TAIL = 7 * HOUR // 租约在租期后的保留 + 洗机缓冲（rental.Tail = 6h + 1h）
const card = (page: Page) => page.getByTestId('market-card').first()
const groupOption = (name: string, total: number, idle: number) => exact(`${name}（共 ${total} · 现空闲 ${idle}）`)

async function openBooking(page: Page) {
  await card(page).getByTestId('market-book-btn').click()
  await expect(page.getByTestId('rent-dialog')).toBeVisible()
}

/** 填入「指定时间」：日期与指定整点。 */
async function fillStart(page: Page, day: string, hour: number) {
  await page.getByTestId('rent-start-later').click()
  await page.getByTestId('rent-day').fill(day)
  await pickOption(page, 'rent-hour', `${String(hour).padStart(2, '0')}:00`)
}

/** el-input-number：填值后失焦（失焦才提交并按 min / max 夹值）。 */
async function setNumber(page: Page, testid: string, value: string) {
  const input = page.getByTestId(testid).locator('input')
  await input.fill(value)
  await input.blur()
}

/** 记录浏览器发出的 POST 请求（路径精确匹配）的幂等键。 */
function trackPosts(page: Page, path: string) {
  const keys: string[] = []
  page.on('request', (r) => {
    if (r.method() === 'POST' && new URL(r.url()).pathname === path) keys.push(r.headers()['idempotency-key'] ?? '')
  })
  return keys
}

test('C-BKG-01 预订对话框：机组 / 数量 / 租期联动与可用台数', async ({ page }, info) => {
  const g = await privateGroup(2)
  const u = await seedUser({ balance: 1_000_000 })
  await loginConsole(page, u.phone)
  await nav(page, info, 'market')
  await openBooking(page)
  await pickOption(page, 'rent-group', groupOption(g.name, 2, 2))
  await expect(page.getByTestId('rent-availability')).toHaveText('该时段可预订 2 台')
  await setNumber(page, 'rent-count', '3')
  await expect(page.getByTestId('rent-availability')).toHaveText('该时段可预订 2 台，不足 3 台，请减少数量或更换时段')
  await expect(page.getByTestId('rent-next')).toBeDisabled()
  await setNumber(page, 'rent-count', '1')
  await page.getByTestId('rent-duration-1m').click()
  await setNumber(page, 'rent-quantity', '4') // 1 月最多 3 个单位（90 天上限）：失焦后夹回 3
  await expect(page.getByTestId('rent-quantity').locator('input')).toHaveValue('3')
  await expect(page.getByTestId('rent-duration-text')).toHaveText('1 月 × 3 = 90 天（单次最长 90 天）')
  await expect(page.getByTestId('rent-availability')).toHaveText('该时段可预订 2 台')
  await expect(page.getByTestId('rent-next')).toBeEnabled()
})

test('C-BKG-02 机组日历：点击日期填入开始日期', async ({ page }, info) => {
  const g = await privateGroup(1)
  const u = await seedUser()
  // 默认开始日期是最早整点（服务器时间 + 2 小时）所在的那天，即今天或明天；点后天才能看出点击生效
  const target = cstDate(new Date((await hooks.clock()).getTime() + 48 * HOUR))
  await loginConsole(page, u.phone)
  await nav(page, info, 'market')
  await openBooking(page)
  await pickOption(page, 'rent-group', groupOption(g.name, 1, 1))
  await expect(page.getByTestId('rent-calendar')).toBeVisible()
  await expect(page.getByTestId('rent-day')).not.toHaveValue(target)
  await expect(page.getByTestId(`rent-day-${target}`)).toContainText('1/1')
  await page.getByTestId(`rent-day-${target}`).click()
  await expect(page.getByTestId('rent-day')).toHaveValue(target)
})

test('C-BKG-03 指定机器与占用条带', async ({ page }, info) => {
  const g = await privateGroup(1)
  const mid = g.machineIds[0]
  expect(await groupMachineIds(g.id)).toEqual([mid])
  const u = await seedUser({ balance: 1_000_000 })
  const api = await userApi(u.phone)
  const s = await startAfter(30)
  const { ids } = await bookViaApi(api, { startAt: s.iso, groupId: g.id, machineId: mid })
  const lower = cstTime(new Date(s.date.getTime() - HOUR).toISOString()) // 开始前 1 小时开通
  const upper = cstTime(new Date(s.date.getTime() + 24 * HOUR + TAIL).toISOString())
  await loginConsole(page, u.phone)
  await nav(page, info, 'market')
  await openBooking(page)
  await pickOption(page, 'rent-group', groupOption(g.name, 1, 1))
  await expect(page.getByTestId('rent-machine')).toBeVisible()
  await pickOption(page, 'rent-machine', exact(`机器 #${mid}（占用至 ${upper}）`))
  await expect(page.getByTestId('rent-machine-periods').locator('li')).toHaveText([`${lower} ～ ${upper}（我的）`])
  await setNumber(page, 'rent-count', '2') // 多台不能指定机器
  await expect(page.getByTestId('rent-machine')).toHaveCount(0)
  await cancelBookingsAndDispose(api, ids)
})

test('C-BKG-04 预订成功跳转详情并显示已预订', async ({ page }, info) => {
  const g = await privateGroup(1)
  const u = await seedUser({ balance: 1_000_000 })
  const s = await startAfter(26)
  const posts = trackPosts(page, '/api/v1/bookings')
  await loginConsole(page, u.phone)
  await nav(page, info, 'market')
  await openBooking(page)
  await pickOption(page, 'rent-group', groupOption(g.name, 1, 1))
  await fillStart(page, s.day, s.hour)
  await expect(page.getByTestId('rent-availability')).toHaveText('该时段可预订 1 台')
  await page.getByTestId('rent-next').click()
  await expect(page.getByTestId('market-confirm-group')).toHaveText(g.name)
  await expect(page.getByTestId('market-confirm-start')).toHaveText(`${s.day} ${String(s.hour).padStart(2, '0')}:00`)
  await expect(page.getByTestId('market-confirm-price')).toHaveText('¥69.00')
  await page.getByTestId('market-confirm-ok').click()
  await toast(page, '预订成功')
  await expect(page).toHaveURL(/\/instances\/ins-[a-z0-9]{8}$/)
  expect(posts).toHaveLength(1)
  await expect(page.getByTestId('detail-state')).toHaveText('已预订')
  await expect(page.getByTestId('detail-group')).toHaveText(g.name)
  await expect(page.getByTestId('detail-start')).toHaveText(cstTime(s.iso))
  await expect(page.getByTestId('detail-cancel-btn')).toBeVisible()
  await expect(page.getByTestId('detail-ssh-hint')).toHaveText('开通后可连接')
  await cancelBookingsAndDispose(await userApi(u.phone), [new URL(page.url()).pathname.split('/').pop()!])
})

test('C-BKG-05 一次 3 台即时下单，按批次筛选显示', async ({ page }, info) => {
  const g = await privateGroup(3)
  const u = await seedUser({ balance: 1_000_000 })
  await loginConsole(page, u.phone)
  await nav(page, info, 'market')
  await openBooking(page)
  await page.getByTestId('rent-start-now').click() // 预订档期按钮打开的是「指定时间」模式，这里要即时
  await pickOption(page, 'rent-group', groupOption(g.name, 3, 3))
  await setNumber(page, 'rent-count', '3')
  await expect(page.getByTestId('rent-availability')).toHaveText('该时段可租 3 台')
  await page.getByTestId('rent-next').click()
  await expect(page.getByTestId('market-confirm-count')).toHaveText('3 台')
  await expect(page.getByTestId('market-confirm-unit')).toHaveText('¥69.00')
  await expect(page.getByTestId('market-confirm-price')).toHaveText('¥207.00')
  await page.getByTestId('market-confirm-ok').click()
  await toast(page, '已下单 3 台')
  await expect(page).toHaveURL(/\/instances\?batch=bat-[a-z0-9]+$/)
  const batch = new URL(page.url()).searchParams.get('batch')!
  await expect(page.getByTestId('instances-batch-head')).toContainText(`批次 ${batch} · 3 台`)
  await expect(page.getByTestId('instance-id')).toHaveCount(3)
  await expect(page.getByTestId('instance-state')).toHaveText(['开通中', '开通中', '开通中'])
})

test('C-BKG-06 台数不足：按钮禁用并提示', async ({ page }, info) => {
  const g = await privateGroup(1)
  const u = await seedUser({ balance: 1_000_000 })
  const api = await userApi(u.phone)
  const s = await startAfter(26)
  const { ids } = await bookViaApi(api, { startAt: s.iso, groupId: g.id, machineId: g.machineIds[0] }) // 订满该组唯一的机器
  await loginConsole(page, u.phone)
  await nav(page, info, 'market')
  await openBooking(page)
  await pickOption(page, 'rent-group', groupOption(g.name, 1, 1))
  await fillStart(page, s.day, s.hour)
  await expect(page.getByTestId('rent-availability')).toHaveText('该时段可预订 0 台，不足 1 台，请减少数量或更换时段')
  await expect(page.getByTestId('rent-next')).toBeDisabled()
  await cancelBookingsAndDispose(api, ids)
})

test('C-BKG-07 余额不足改为去充值', async ({ page }, info) => {
  const g = await privateGroup(2)
  const u = await seedUser({ balance: 5000 })
  const posts = trackPosts(page, '/api/v1/bookings')
  await loginConsole(page, u.phone)
  await nav(page, info, 'market')
  await openBooking(page)
  await pickOption(page, 'rent-group', groupOption(g.name, 2, 2))
  await setNumber(page, 'rent-count', '2')
  await expect(page.getByTestId('rent-availability')).toHaveText('该时段可预订 2 台')
  await page.getByTestId('rent-next').click()
  await expect(page.getByTestId('market-confirm-price')).toHaveText('¥138.00')
  await expect(page.getByTestId('market-confirm-after')).toHaveText('-¥88.00（余额不足）')
  await expect(page.getByTestId('market-confirm-ok')).toHaveText('去充值')
  await page.getByTestId('market-confirm-ok').click()
  await expect(page).toHaveURL(/\/wallet\/recharge\?redirect=(%2F|\/)market$/)
  expect(posts).toHaveLength(0)
})

test('C-BKG-08 开始时间过近：后端文案回到表单', async ({ page }, info) => {
  const g = await privateGroup(1)
  const u = await seedUser({ balance: 1_000_000 })
  await loginConsole(page, u.phone)
  await nav(page, info, 'market')
  await openBooking(page)
  await pickOption(page, 'rent-group', groupOption(g.name, 1, 1))
  const s = await startAfter(2)
  await fillStart(page, s.day, s.hour)
  await page.getByTestId('rent-next').click()
  await hooks.advance(3 * 3600) // 提交前服务器时间已过去 3 小时：该整点不再满足「2 小时后」
  await page.getByTestId('market-confirm-ok').click()
  await toast(page, '开始时间至少在 2 小时之后')
  await expect(page.getByTestId('rent-dialog')).toBeVisible()
  const api = await userApi(u.phone)
  expect(((await (await api.get('/api/v1/instances')).json()) as { items: unknown[] }).items).toHaveLength(0)
})

test('C-BKG-09 预订确认双击只发一次请求', async ({ page }, info) => {
  const g = await privateGroup(2)
  const u = await seedUser({ balance: 1_000_000 })
  const posts = trackPosts(page, '/api/v1/bookings')
  await loginConsole(page, u.phone)
  await nav(page, info, 'market')
  await openBooking(page)
  await pickOption(page, 'rent-group', groupOption(g.name, 2, 2))
  await setNumber(page, 'rent-count', '2')
  await page.getByTestId('rent-next').click()
  await clickTwiceSync(page.getByTestId('market-confirm-ok'))
  await expect(page).toHaveURL(/\/instances\?batch=bat-/)
  expect(posts).toHaveLength(1)
  expect(posts[0]).toMatch(/^[0-9a-f-]{36}$/)
  const api = await userApi(u.phone)
  expect(((await (await api.get('/api/v1/instances')).json()) as { items: unknown[] }).items).toHaveLength(2)
  expect((await (await api.get('/api/v1/wallet')).json()).balance).toBe(1_000_000 - 2 * 6900)
})

test('C-BKG-10 未登录与未实名点预订的引导', async ({ page }, info) => {
  const posts = trackPosts(page, '/api/v1/bookings')
  await page.goto('/market')
  await card(page).getByTestId('market-book-btn').click()
  await expect(page).toHaveURL(/\/login\?redirect=(%2F|\/)market$/)
  const u = await seedUser({ verified: false })
  await loginConsole(page, u.phone)
  await nav(page, info, 'market')
  await card(page).getByTestId('market-book-btn').click()
  await expect(dialog(page).filter({ has: page.getByTestId('guide-dialog') })).toContainText('请先完成实名认证')
  await expect(page.getByTestId('guide-action')).toHaveText('去实名')
  await expect(page.getByTestId('rent-dialog')).toHaveCount(0)
  expect(posts).toHaveLength(0)
})

test('C-BKG-11 立即租用（旧接口）无空闲机器：提示后回到表单，市场台数随即刷新', async ({ page }, info) => {
  await hooks.machines(1)
  const u = await seedUser({ balance: 1_000_000 })
  const orders = trackPosts(page, '/api/v1/orders')
  let productLoads = 0
  page.on('request', (r) => {
    if (r.method() === 'GET' && new URL(r.url()).pathname === '/api/v1/products') productLoads += 1
  })
  await page.clock.install()
  await loginConsole(page, u.phone)
  await nav(page, info, 'market')
  await expect(card(page).getByTestId('market-idle')).not.toHaveText('0')
  await card(page).getByTestId('market-rent-btn').click()
  await expect(page.getByTestId('market-confirm-dialog')).toBeVisible()
  // 冻结页面时间：市场页 30 秒的轮询不会触发，之后台数变化只能来自 no_capacity 后的立即刷新
  await pauseClock(page)
  const loadsBefore = productLoads
  // 确认框打开后空闲机器被占完：数据库整轮共享，只能把全部空闲机器置为维护来构造（同 C-MKT-06）
  const admin = await createAdmin('ops')
  await drainIdleMachines(await adminApi(admin), admin)
  await page.getByTestId('market-confirm-ok').click()
  await toast(page, '暂无空闲机器，请稍后再试')
  expect(orders).toHaveLength(1) // 单台、立即、不限机组：走 POST /orders
  await expect(page.getByTestId('rent-dialog')).toBeVisible() // 回到表单，可改选机组 / 时段
  await expect(page.getByTestId('rent-availability')).toHaveText('该时段可租 0 台，不足 1 台，请减少数量或更换时段')
  await expect(page.getByTestId('rent-next')).toBeDisabled()
  await expect.poll(() => productLoads).toBe(loadsBefore + 1)
  await expect(card(page).getByTestId('market-idle')).toHaveText('0')
  await expect(card(page).getByTestId('market-rent-btn')).toBeDisabled()
  await expect(card(page).getByTestId('market-rent-btn')).toHaveText('暂无空闲')
})
