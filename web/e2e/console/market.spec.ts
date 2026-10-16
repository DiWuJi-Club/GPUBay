import { expect, request, test, type Page } from '@playwright/test'
import { adminApi, createAdmin, drainIdleMachines } from '../helpers/admin'
import { dialog, loginConsole, nav, placeOrder, seedUser, toast, userApi } from '../helpers/console'
import { CONSOLE_URL, hooks } from '../helpers/hooks'

const card = (page: Page) => page.getByTestId('market-card').first()
const guideBox = (page: Page) => dialog(page).filter({ has: page.getByTestId('guide-dialog') })

/** 记录浏览器发出的下单请求（POST /api/v1/orders）及其幂等键。 */
function trackOrders(page: Page) {
  const keys: (string | undefined)[] = []
  page.on('request', (r) => {
    if (r.method() === 'POST' && new URL(r.url()).pathname === '/api/v1/orders') keys.push(r.headers()['idempotency-key'])
  })
  return keys
}

test('C-MKT-01 市场未登录可见：规格、空闲台数、三档价格', async ({ page }) => {
  await hooks.machines(1)
  const api = await request.newContext({ baseURL: CONSOLE_URL })
  const product = ((await (await api.get('/api/v1/products')).json()) as { items: { idle_count: number }[] }).items[0]
  // 首次加载失败：显示错误占位与重试按钮，不渲染卡片；重试成功后卡片出现
  let failFirst = true
  await page.route('**/api/v1/products', async (route) => {
    if (failFirst) {
      failFirst = false
      return route.fulfill({ status: 503, contentType: 'application/json', body: JSON.stringify({ code: 'unavailable', message: '服务暂不可用' }) })
    }
    return route.continue()
  })
  await page.goto('/market')
  await expect(page.getByTestId('market-error')).toBeVisible()
  await expect(page.getByTestId('market-card')).toHaveCount(0)
  await page.getByTestId('market-retry').click()
  await expect(page.getByTestId('market-error')).toBeHidden()
  await expect(card(page).getByTestId('market-name')).toHaveText('DGX Spark 整机')
  await expect(card(page).getByTestId('market-spec-gpu')).toHaveText('NVIDIA GB10 Grace Blackwell')
  await expect(card(page).getByTestId('market-spec-memory')).toHaveText('128 GB')
  await expect(card(page).getByTestId('market-spec-disk')).toHaveText('4 TB')
  await expect(card(page).getByTestId('market-spec-cpu')).toHaveText('20 核 Arm')
  await expect(card(page).getByTestId('market-idle')).toHaveText(String(product.idle_count))
  await expect(card(page).getByTestId('market-price-1d')).toHaveText('1 天 ¥69.00')
  await expect(card(page).getByTestId('market-price-1w')).toHaveText('1 周 ¥414.00')
  await expect(card(page).getByTestId('market-price-1m')).toHaveText('1 月 ¥1,599.00')
  await expect(card(page).getByTestId('market-rent-btn')).toHaveText('立即租用')
  await expect(card(page).getByTestId('market-rent-btn')).toBeEnabled()
})

test('C-MKT-02 未登录点立即租用跳登录，登录后回到市场', async ({ page }) => {
  await hooks.machines(1)
  const u = await seedUser()
  const orders = trackOrders(page)
  await page.goto('/market')
  await card(page).getByTestId('market-rent-btn').click()
  await expect(page).toHaveURL(/\/login\?redirect=(%2F|\/)market$/)
  await hooks.advance(61)
  await page.getByTestId('login-phone').fill(u.phone)
  await page.getByTestId('login-send').click()
  await page.getByTestId('login-code').fill('000000')
  await page.getByTestId('login-submit').click()
  await expect(page).toHaveURL(/\/market$/)
  await expect(card(page).getByTestId('market-rent-btn')).toHaveText('立即租用')
  expect(orders).toHaveLength(0)
})

test('C-MKT-03 未实名用户点租用引导去实名', async ({ page }, info) => {
  await hooks.machines(1)
  const u = await seedUser({ verified: false })
  const orders = trackOrders(page)
  await loginConsole(page, u.phone)
  await nav(page, info, 'market')
  await card(page).getByTestId('market-rent-btn').click()
  await expect(guideBox(page)).toBeVisible()
  await expect(guideBox(page)).toContainText('请先完成实名认证')
  await expect(page.getByTestId('guide-dialog')).toHaveText('按监管要求，下单与充值前需要完成实名认证。')
  await expect(page.getByTestId('guide-action')).toHaveText('去实名')
  await expect(page.getByTestId('market-confirm-dialog')).toHaveCount(0) // 不打开下单确认框
  await page.getByTestId('guide-action').click()
  await expect(page).toHaveURL(/\/kyc\?redirect=(%2F|\/)market$/)
  await expect(page.getByTestId('guide-dialog')).toBeHidden()
  expect(orders).toHaveLength(0)
})

test('C-MKT-04 余额不足：确认框提示并改为去充值；下单时后端判余额不足弹引导', async ({ page }, info) => {
  await hooks.machines(2)
  const u = await seedUser({ balance: 7900 })
  const orders = trackOrders(page)
  await loginConsole(page, u.phone)
  await nav(page, info, 'market')

  // 1) 打开确认框时余额够（¥79），确认前在别处花掉 ¥69 → 后端返回 insufficient_balance → 关闭确认框、弹「余额不足」引导
  await card(page).getByTestId('market-rent-btn').click()
  await expect(page.getByTestId('market-confirm-balance')).toHaveText('¥79.00')
  await expect(page.getByTestId('market-confirm-after')).toHaveText('¥10.00')
  await expect(page.getByTestId('market-confirm-ok')).toHaveText('确认下单')
  await placeOrder(page.request) // 与浏览器共享会话 Cookie
  await page.getByTestId('market-confirm-ok').click()
  await expect(guideBox(page)).toBeVisible()
  await expect(guideBox(page)).toContainText('余额不足')
  await expect(page.getByTestId('guide-dialog')).toHaveText('当前余额不足以支付本次费用，请先充值。')
  await expect(page.getByTestId('guide-action')).toHaveText('去充值')
  await expect(page.getByTestId('market-confirm-dialog')).toBeHidden()
  expect(orders).toHaveLength(1)
  await page.getByTestId('guide-action').click()
  await expect(page).toHaveURL(/\/wallet\/recharge\?redirect=(%2F|\/)market$/)

  // 2) 余额 ¥10：确认框直接提示余额不足，按钮为「去充值」，点击跳充值页，不发下单请求
  await page.goto('/market')
  await card(page).getByTestId('market-rent-btn').click()
  await expect(page.getByTestId('market-confirm-balance')).toHaveText('¥10.00')
  await expect(page.getByTestId('market-confirm-after')).toHaveText('-¥59.00（余额不足）')
  await expect(page.getByTestId('market-confirm-ok')).toHaveText('去充值')
  await page.getByTestId('market-confirm-ok').click()
  await expect(page).toHaveURL(/\/wallet\/recharge\?redirect=(%2F|\/)market$/)
  expect(orders).toHaveLength(1)
})

test('C-MKT-05 下单成功跳转实例详情，余额减少', async ({ page }, info) => {
  await hooks.machines(1)
  const u = await seedUser({ balance: 100000 })
  const orders = trackOrders(page)
  await loginConsole(page, u.phone)
  await nav(page, info, 'market')
  await card(page).getByTestId('market-rent-btn').click()
  await expect(page.getByTestId('market-confirm-product')).toHaveText('DGX Spark 整机')
  await expect(page.getByTestId('market-confirm-duration')).toHaveText('1 天')
  await expect(page.getByTestId('market-confirm-price')).toHaveText('¥69.00')
  await expect(page.getByTestId('market-confirm-balance')).toHaveText('¥1,000.00')
  await expect(page.getByTestId('market-confirm-after')).toHaveText('¥931.00')
  await page.getByTestId('market-confirm-ok').click()
  await toast(page, '下单成功，正在开通')
  await expect(page).toHaveURL(/\/instances\/ins-[a-z0-9]+$/)
  await expect(page.getByTestId('detail-state')).toHaveText('开通中')
  await expect(page.getByTestId('detail-id')).toHaveText(page.url().split('/').pop()!)
  expect(orders).toHaveLength(1)
  expect(orders[0]).toMatch(/^[0-9a-f-]{36}$/) // 下单请求带 Idempotency-Key
  const api = await userApi(u.phone)
  expect((await (await api.get('/api/v1/wallet')).json()).balance).toBe(93100)
})

test('C-MKT-06 无空闲机器时按钮禁用，30 秒刷新后恢复', async ({ page }) => {
  const admin = await createAdmin('ops')
  await drainIdleMachines(await adminApi(admin), admin)
  let productRequests = 0
  page.on('request', (r) => {
    if (r.method() === 'GET' && new URL(r.url()).pathname === '/api/v1/products') productRequests += 1
  })
  await page.clock.install()
  await page.goto('/market')
  await expect(card(page).getByTestId('market-rent-btn')).toBeDisabled()
  await expect(card(page).getByTestId('market-rent-btn')).toHaveText('暂无空闲')
  await expect(card(page).getByTestId('market-idle')).toHaveText('0')
  await hooks.machines(1)
  expect(productRequests).toBe(1)
  await page.clock.runFor(29_000) // 不到 30 秒不刷新
  expect(productRequests).toBe(1)
  await expect(card(page).getByTestId('market-idle')).toHaveText('0')
  await page.clock.runFor(1_000)
  await expect.poll(() => productRequests).toBe(2)
  await expect(card(page).getByTestId('market-idle')).toHaveText('1')
  await expect(card(page).getByTestId('market-rent-btn')).toBeEnabled()
  await expect(card(page).getByTestId('market-rent-btn')).toHaveText('立即租用')
})

test('C-MKT-07 幂等：响应丢失后重试复用同一个键；连点两次只发一个请求，只扣一次款', async ({ page }, info) => {
  await hooks.machines(2)
  const u = await seedUser({ balance: 100000 })
  const keys: string[] = []
  let dropped = false
  await page.route('**/api/v1/orders', async (route) => {
    if (route.request().method() !== 'POST') return route.continue()
    keys.push(route.request().headers()['idempotency-key'])
    if (!dropped) {
      dropped = true
      await route.fetch() // 请求真实到达后端（订单已创建），再让浏览器看到网络错误
      return route.abort('failed')
    }
    return route.continue()
  })
  await loginConsole(page, u.phone)
  await nav(page, info, 'market')
  await card(page).getByTestId('market-rent-btn').click()
  await page.getByTestId('market-confirm-ok').click()
  await toast(page, '网络异常')
  await expect(page.getByTestId('market-confirm-dialog')).toBeVisible() // 网络错误不关确认框
  expect(keys).toHaveLength(1)
  await page.getByTestId('market-confirm-ok').dblclick() // 连点两次
  await toast(page, '下单成功')
  await expect(page).toHaveURL(/\/instances\/ins-/)
  expect(keys).toHaveLength(2) // 丢失响应的一次 + 连点只发出的一次
  expect(keys[0]).toBe(keys[1])
  expect(keys[0]).toMatch(/^[0-9a-f-]{36}$/)
  const api = await userApi(u.phone)
  expect(((await (await api.get('/api/v1/instances')).json()) as { items: unknown[] }).items).toHaveLength(1)
  expect((await (await api.get('/api/v1/wallet')).json()).balance).toBe(93100)
})

test('C-MKT-08 选择 1 周与第二个镜像，确认框与详情一致', async ({ page }, info) => {
  await hooks.machines(1)
  const u = await seedUser({ balance: 100000 })
  await loginConsole(page, u.phone)
  await nav(page, info, 'market')
  await card(page).getByTestId('market-duration-1w').click()
  await card(page).getByTestId('market-image').click()
  await page.getByRole('option', { name: 'PyTorch' }).click()
  await card(page).getByTestId('market-rent-btn').click()
  await expect(page.getByTestId('market-confirm-duration')).toHaveText('1 周')
  await expect(page.getByTestId('market-confirm-price')).toHaveText('¥414.00')
  await expect(page.getByTestId('market-confirm-image')).toHaveText('PyTorch')
  await expect(page.getByTestId('market-confirm-after')).toHaveText('¥586.00')
  await page.getByTestId('market-confirm-ok').click()
  await expect(page).toHaveURL(/\/instances\/ins-/)
  await expect(page.getByTestId('detail-image')).toHaveText('PyTorch')
  await expect(page.getByTestId('detail-pending')).toHaveText('7 天')
})
