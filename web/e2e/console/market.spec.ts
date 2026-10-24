import { expect, request, test, type Page } from '@playwright/test'
import { adminApi, createAdmin, drainIdleMachines } from '../helpers/admin'
import { appRow, captureWindowOpen, cleanupApps, expectRowState, listDeployments, ownInstance, seedApps, tickUntil } from '../helpers/apps'
import { dialog, loginConsole, nav, placeOrder, seedUser, toast, userApi } from '../helpers/console'
import { CONSOLE_URL, hooks } from '../helpers/hooks'
import { placedInstance, seedWorkflows } from '../helpers/workflows'

// C-MKT-12 / 13 下单预装应用：afterEach 卸载它的部署（其余用例没有部署，清理是空操作）
test.afterEach(cleanupApps)

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
  await expect(card(page).getByTestId('market-no-price')).toHaveCount(0)

  await test.step('商品全部档位下架（有空闲机器）：按钮禁用并提示暂未开放，点击不弹确认框', async () => {
    const real = (await (await api.get('/api/v1/products')).json()) as { items: Record<string, unknown>[] }
    const noPrices = { items: [{ ...real.items[0], idle_count: 3, prices: [] }] }
    await page.route('**/api/v1/products', (route) => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(noPrices) }))
    await page.reload()
    await expect(card(page).getByTestId('market-idle')).toHaveText('3')
    await expect(card(page).getByTestId('market-price-1d')).toHaveCount(0)
    await expect(card(page).getByTestId('market-no-price')).toHaveText('该商品暂无可租的租期，暂不能下单')
    await expect(card(page).getByTestId('market-rent-btn')).toHaveText('暂未开放')
    await expect(card(page).getByTestId('market-rent-btn')).toBeDisabled()
    await card(page).getByTestId('market-rent-btn').click({ force: true })
    await expect(page).toHaveURL(`${CONSOLE_URL}/market`) // 未登录也不会被带去登录页
    await expect(page.getByTestId('market-confirm-dialog')).toBeHidden()
  })
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

/** 登录后到市场页，点商品卡片里的「一键部署」应用卡片（打开租用对话框、预选该应用）。 */
async function openAppCard(page: Page, info: Parameters<typeof nav>[1], slug: string) {
  await seedApps()
  await hooks.machines(1)
  const u = await seedUser()
  await loginConsole(page, u.phone)
  await nav(page, info, 'market')
  await card(page).getByTestId(`market-app-${slug}`).click()
  await expect(page.getByTestId('rent-dialog')).toBeVisible()
  return u
}

test('C-MKT-09 「一键部署」应用卡片打开租用对话框并预选该应用', async ({ page }, info) => {
  await seedApps()
  await hooks.machines(1)
  const u = await seedUser()
  await loginConsole(page, u.phone)
  await nav(page, info, 'market')
  await expect(card(page).getByTestId('market-apps')).toBeVisible()
  for (const slug of ['vllm', 'open-webui', 'comfyui', 'jupyterlab']) await expect(card(page).getByTestId(`market-app-${slug}`)).toBeVisible()
  await card(page).getByTestId('market-app-vllm').click()
  await expect(page.getByTestId('rent-dialog')).toBeVisible() // 预选应用时停在表单步：要看镜像锁定、填变量
  await expect(page.getByTestId('rent-app')).toContainText('vLLM 推理服务 · Qwen/Qwen3-8B')
  await expect(page.getByTestId('app-env-form')).toBeVisible()
  await expect(page.getByTestId('app-env-MODEL_ID')).toContainText('Qwen/Qwen3-8B')
  await expect(page.getByTestId('app-env-MAX_MODEL_LEN').locator('input')).toHaveValue('32768')
})

test('C-MKT-10 选应用后镜像锁定为兼容镜像；密钥一键生成 32 位；变量校验在本地拦截', async ({ page }, info) => {
  const orders: string[] = []
  page.on('request', (r) => {
    if (r.method() === 'POST' && new URL(r.url()).pathname === '/api/v1/orders') orders.push(r.url())
  })
  await openAppCard(page, info, 'vllm')
  const catalog = ((await (await page.request.get('/api/v1/apps')).json()) as { items: { slug: string; images: { name: string }[] }[] }).items
  const compatible = catalog.find((a) => a.slug === 'vllm')!.images.map((i) => i.name)
  expect(compatible).toHaveLength(2) // 默认镜像 + LLM Serving
  await expect(page.getByTestId('rent-image-locked')).toHaveText('该应用需要此镜像')
  await expect(page.getByTestId('rent-image')).toContainText(compatible[0])
  await page.getByTestId('rent-image').click()
  await expect(page.getByRole('option')).toHaveText(compatible) // 下拉只剩兼容镜像，按应用声明的顺序
  await page.getByRole('option', { name: compatible[0], exact: true }).click()

  await page.getByTestId('app-env-API_KEY-gen').click()
  await expect(page.getByTestId('app-env-API_KEY')).toHaveValue(/^[A-Za-z0-9]{32}$/)

  // 格式不对（GPU 显存比例的正则）：停在表单，错误显示在字段下
  await page.getByTestId('app-env-GPU_MEMORY_UTILIZATION').fill('1.5')
  await page.getByTestId('rent-next').click()
  await expect(page.getByTestId('app-env-GPU_MEMORY_UTILIZATION-error')).toHaveText('格式不正确')
  await expect(page.getByTestId('market-confirm-dialog')).toHaveCount(0)

  // 必填且没有默认值（JupyterLab 的访问令牌）：留空不能下一步
  await page.getByTestId('rent-app').click()
  await page.getByRole('option', { name: 'JupyterLab', exact: true }).click()
  await expect(page.getByTestId('app-env-API_KEY')).toHaveCount(0)
  await page.getByTestId('rent-next').click()
  await expect(page.getByTestId('app-env-JUPYTER_TOKEN-error')).toHaveText('请填写 访问令牌')
  await expect(page.getByTestId('market-confirm-dialog')).toHaveCount(0)

  // 改回「不预装」：镜像不再锁定，变量表单消失
  await page.getByTestId('rent-app').click()
  await page.getByRole('option', { name: '不预装', exact: true }).click()
  await expect(page.getByTestId('rent-image-locked')).toHaveCount(0)
  await expect(page.getByTestId('app-env-form')).toHaveCount(0)
  expect(orders).toHaveLength(0)
})

test('C-MKT-11 确认页显示预装应用与所选模型', async ({ page }, info) => {
  await openAppCard(page, info, 'vllm')
  await page.getByTestId('app-env-MODEL_ID').click()
  await page.getByRole('option', { name: 'Qwen/Qwen3-4B', exact: true }).click()
  await page.getByTestId('rent-next').click()
  await expect(page.getByTestId('market-confirm-dialog')).toBeVisible()
  await expect(page.getByTestId('market-confirm-app')).toHaveText('vLLM 推理服务（Qwen/Qwen3-4B）')
  await page.getByTestId('market-confirm-back').click()
  await page.getByTestId('app-env-MODEL_ID').click()
  await page.getByRole('option', { name: 'Qwen/Qwen3-8B', exact: true }).click()
  await page.getByTestId('rent-next').click()
  await expect(page.getByTestId('market-confirm-app')).toHaveText('vLLM 推理服务（Qwen/Qwen3-8B）')
})

test('C-MKT-12 下单预装 vLLM：开通前排队，开通后部署中显示进度，完成后可打开', async ({ page }, info) => {
  const apps = await seedApps()
  const opened = await captureWindowOpen(page)
  await openAppCard(page, info, 'vllm')
  await page.getByTestId('app-env-API_KEY-gen').click()
  await page.getByTestId('rent-next').click()
  await expect(page.getByTestId('market-confirm-app')).toHaveText('vLLM 推理服务（Qwen/Qwen3-8B）')
  const req = page.waitForRequest((r) => r.method() === 'POST' && new URL(r.url()).pathname === '/api/v1/orders')
  const id = await placedInstance(page, () => page.getByTestId('market-confirm-ok').click())
  ownInstance(page.request, id) // 先登记再断言：断言失败也会卸载部署（page.request 与浏览器共享会话 Cookie）
  const body = (await req).postDataJSON() as { app_id: number; app_env: Record<string, string> }
  expect(body.app_id).toBe(apps.vllm.id)
  expect(body.app_env.API_KEY).toMatch(/^[A-Za-z0-9]{32}$/)
  await toast(page, '下单成功，正在开通')
  await expect(page).toHaveURL(new RegExp(`/instances/${id}$`))
  const deps = await listDeployments(page.request, id)
  expect(deps).toHaveLength(1)
  const dep = deps[0].id
  await expectRowState(page, dep, 'queued')
  await expect(appRow(page, dep).getByTestId('detail-app-instance-hint')).toHaveText('将在开通后自动部署')

  await hooks.agent() // 开通
  await expect(page.getByTestId('detail-state')).toHaveText('运行中')
  await tickUntil(page.request, id, dep, 'installing')
  await expectRowState(page, dep, 'installing')
  await expect(appRow(page, dep).getByTestId('detail-app-progress')).toBeVisible()
  const done = await tickUntil(page.request, id, dep, 'running')
  await expectRowState(page, dep, 'running')
  await expect(appRow(page, dep).getByTestId('detail-app-progress')).toHaveCount(0)
  await appRow(page, dep).getByTestId('detail-app-open').click()
  await expect.poll(opened).toEqual([done.url])
  expect(done.url).toContain(`${id}-8000.spark.test`)
})

test('C-MKT-13 预装 ComfyUI 时可多选预装工作流：合计下载量、换应用清空、确认页列出，下单请求带 app_workflow_ids', async ({ page }, info) => {
  const apps = await seedApps()
  const { a, b } = await seedWorkflows()
  await openAppCard(page, info, 'comfyui')
  await expect(page.getByTestId('rent-image')).toContainText('ComfyUI')
  await expect(page.getByTestId('rent-workflows')).toBeVisible()
  await expect(page.getByTestId('rent-workflows-total')).toHaveText('已选 0/8')
  await page.getByTestId('rent-workflows-e2e-wf-a').click()
  await page.getByTestId('rent-workflows-e2e-wf-b').click()
  await expect(page.getByTestId('rent-workflows-total')).toHaveText('已选 2/8 · 合计需下载约 3.7 GB')
  await page.getByTestId('rent-workflows-e2e-wf-a').click() // 取消
  await expect(page.getByTestId('rent-workflows-e2e-wf-a')).toHaveAttribute('aria-pressed', 'false')
  await expect(page.getByTestId('rent-workflows-total')).toHaveText('已选 1/8 · 合计需下载约 1.2 GB')

  // 换成别的应用：预装工作流区块消失，换回来选择已清空
  await page.getByTestId('rent-app').click()
  await page.getByRole('option', { name: 'JupyterLab', exact: true }).click()
  await expect(page.getByTestId('rent-workflows')).toHaveCount(0)
  await page.getByTestId('rent-app').click()
  await page.getByRole('option', { name: /^ComfyUI 图像生成/ }).click()
  await expect(page.getByTestId('rent-workflows-total')).toHaveText('已选 0/8')
  await page.getByTestId('rent-workflows-e2e-wf-b').click()
  await page.getByTestId('rent-workflows-e2e-wf-a').click()

  await page.getByTestId('rent-next').click()
  await expect(page.getByTestId('market-confirm-dialog')).toBeVisible()
  await expect(page.getByTestId('market-confirm-app')).toContainText('ComfyUI 图像生成')
  await expect(page.getByTestId('rent-confirm-workflows')).toHaveText('E2E 节点工作流 B、E2E 文生图 A（合计需下载约 3.7 GB）')
  const req = page.waitForRequest((r) => r.method() === 'POST' && new URL(r.url()).pathname === '/api/v1/orders')
  const id = await placedInstance(page, () => page.getByTestId('market-confirm-ok').click())
  ownInstance(page.request, id) // 先登记再断言：断言失败也会卸载部署
  const body = (await req).postDataJSON() as { app_id: number; app_workflow_ids: number[] }
  expect(body.app_id).toBe(apps.comfyui.id)
  expect(body.app_workflow_ids).toEqual([b.id, a.id]) // 按选择顺序
  await toast(page, '下单成功，正在开通')
  await expect(page).toHaveURL(new RegExp(`/instances/${id}$`))
  const deps = await listDeployments(page.request, id)
  expect(deps.map((d) => d.app_slug)).toEqual(['comfyui'])
  await expect(page.getByTestId('wf-row-e2e-wf-a')).toHaveAttribute('data-state', 'pending')
  await expect(page.getByTestId('wf-row-e2e-wf-b')).toHaveAttribute('data-state', 'pending')
  await hooks.agent() // 开通 → 部署 → 两个工作流都装好
  await tickUntil(page.request, id, deps[0].id, 'running')
  await expect(page.getByTestId('wf-row-e2e-wf-a')).toHaveAttribute('data-state', 'installed', { timeout: 15_000 })
  await expect(page.getByTestId('wf-row-e2e-wf-b')).toHaveAttribute('data-state', 'installed', { timeout: 15_000 })
})
