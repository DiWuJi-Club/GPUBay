import { expect, request, test, type Page } from '@playwright/test'
import { isMobile, loginConsole, nav, seedUser, toast, userApi } from '../helpers/console'
import { CONSOLE_URL, hooks } from '../helpers/hooks'
import { callChat, cleanupLLM, createKey, createPoolMachineAndNode, seedLLMModel, sweep, waitGatewayModel } from '../helpers/llm'
import { yuan } from '../helpers/money'
import { cstDate } from '../helpers/time'

// Token 商城控制台（计划 9 规格 §9.2、§16.3）。模型 / 节点是全局状态：每个用例自己建模型（唯一 slug）、自己的池机器与钩子节点，
// afterEach 的 cleanupLLM 把模型下线、节点推进到 removed、机器移出池。
// 「贵」模型：FakeVLLM 缺省回 usage 100 / 50 → 100 × 500,000 + 50 × 1,000,000 = 10^8 微分 = ¥1.00 一次，
// 达到结算阈值（¥1），清扫一轮即结算，不必推进 10 分钟。
const PRICY = { input_price: 500_000, output_price: 1_000_000 }

test.afterEach(cleanupLLM)

type PublicModels = { base_url: string; items: { slug: string; kind: string; input_price: number }[] }
async function publicModels(): Promise<PublicModels> {
  const ctx = await request.newContext({ baseURL: CONSOLE_URL })
  try {
    const r = await ctx.get('/api/v1/llm/models')
    if (!r.ok()) throw new Error(`GET /llm/models ${r.status()} ${await r.text()}`)
    return (await r.json()) as PublicModels
  } finally {
    await ctx.dispose()
  }
}

/** 当前可见的那个 Tab 面板（未登录时 Keys / 用量 / 日志三个面板都渲染了登录引导）。 */
const pane = (page: Page, name: string) => page.locator(`#pane-${name}`)

/** 页面日期（请求日志默认最近 7 天）按浏览器时间算：把页面时钟对齐到服务器假时钟（之前的用例已推进了若干天）。 */
async function alignPageClock(page: Page) {
  await page.clock.install({ time: await hooks.clock() })
}

test('C-TOK-01 导航「Token 商城」入口与市场横幅（最低输入价）', async ({ page }, info) => {
  await seedLLMModel({ input_price: 37, output_price: 99 })
  const u = await seedUser()
  await loginConsole(page, u.phone)
  // 横幅价 = 全部已上线模型里最低的输入价（同一轮里不应有别的已上线模型，但按接口算，不依赖这一点）
  const min = Math.min(...(await publicModels()).items.map((m) => m.input_price))
  expect(min).toBeLessThanOrEqual(37)
  await expect(page.getByTestId('market-tokens-banner')).toBeVisible()
  await expect(page.getByTestId('market-tokens-banner-price')).toHaveText(yuan(min))
  await page.getByTestId('market-tokens-banner').click()
  await expect(page).toHaveURL(/\/tokens$/)
  await expect(page).toHaveTitle('Token 商城 - Spark Cloud')
  await page.goto('/market')
  await nav(page, info, 'tokens')
  await expect(page).toHaveURL(/\/tokens$/)
  await expect(page.getByTestId(isMobile(info) ? 'tabbar-tokens' : 'menu-tokens')).toHaveClass(/active/)
  await expect(page.getByTestId('tokens-models-intro')).toBeVisible()
})

test('C-TOK-02 模型卡片：价格（元 / 百万 token）、上下文、标签，「暂无节点」→ 建节点后「可用」；未登录其它 Tab 显示登录引导', async ({ page }) => {
  const m = await seedLLMModel({ input_price: 50, output_price: 150, features: ['tools'] })
  await page.goto('/tokens') // 未登录：模型 Tab 照常可见
  const card = page.getByTestId(`tokens-model-card-${m.slug}`)
  await expect(card.getByTestId('tokens-model-name')).toHaveText(m.name)
  await expect(card.getByTestId('tokens-model-slug')).toHaveText(m.slug)
  await expect(card.getByTestId('tokens-model-input-price')).toHaveText('¥0.50')
  await expect(card.getByTestId('tokens-model-output-price')).toHaveText('¥1.50')
  await expect(card.getByTestId('tokens-model-context')).toHaveText('32K')
  await expect(card.getByTestId('tokens-model-feature')).toHaveText(['工具调用'])
  await expect(card.getByTestId('tokens-model-available')).toHaveText('暂无节点')

  await page.getByTestId('tokens-tab-keys').click()
  await expect(pane(page, 'keys').getByTestId('tokens-login-prompt')).toBeVisible()
  await page.getByTestId('tokens-tab-usage').click()
  await expect(pane(page, 'usage').getByTestId('tokens-login-prompt')).toBeVisible()
  await page.getByTestId('tokens-tab-logs').click()
  await expect(pane(page, 'logs').getByTestId('tokens-login-prompt')).toBeVisible()
  await pane(page, 'logs').getByTestId('tokens-login-btn').click()
  await expect(page).toHaveURL((u) => u.pathname === '/login' && u.searchParams.get('redirect') === '/tokens?tab=logs')

  await createPoolMachineAndNode(m)
  await page.goto('/tokens')
  await expect(card.getByTestId('tokens-model-available')).toHaveText('可用')
})

test('C-TOK-03 未实名：「创建 Key」禁用并引导去实名，接口同样拒绝', async ({ page }) => {
  const u = await seedUser({ verified: false })
  const api = await userApi(u.phone)
  const r = await api.post('/api/v1/llm/keys', { data: { name: 'x' } })
  expect(r.status()).toBe(403)
  expect(((await r.json()) as { code: string }).code).toBe('kyc_required')
  await loginConsole(page, u.phone)
  await page.goto('/tokens?tab=keys')
  await expect(page.getByTestId('tokens-key-create')).toBeDisabled()
  await expect(page.getByTestId('tokens-keys-empty')).toBeVisible()
  await page.getByTestId('tokens-key-kyc-link').click()
  await expect(page).toHaveURL((x) => x.pathname === '/kyc' && x.searchParams.get('redirect') === '/tokens?tab=keys')
})

test('C-TOK-04 创建 Key：明文只显示一次、可复制，关闭需确认；之后只显示提示，Key 可用于网关', async ({ page }) => {
  const u = await seedUser()
  await loginConsole(page, u.phone)
  await page.goto('/tokens?tab=keys')
  await expect(page.getByTestId('tokens-keys-empty')).toBeVisible()
  await expect(page.getByTestId('tokens-key-count')).toHaveText('0 / 20')
  await page.getByTestId('tokens-key-create').click()
  await page.getByTestId('tokens-key-name').fill('生产环境')
  await page.getByTestId('tokens-key-submit').click()
  await expect(page.getByTestId('tokens-key-warning')).toBeVisible()
  const plain = (await page.getByTestId('tokens-key-plain').textContent())?.trim() ?? ''
  expect(plain).toMatch(/^sk-spark-[0-9A-Za-z]{40}$/)
  await page.getByTestId('tokens-key-copy').click()
  await toast(page, '已复制')
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(plain)

  // 关闭要二次确认：先「返回」，明文仍在；再确认关闭
  await page.getByTestId('tokens-key-done').click()
  await page.locator('.el-message-box').getByRole('button', { name: '返回' }).click()
  await expect(page.getByTestId('tokens-key-plain')).toHaveText(plain)
  await page.getByTestId('tokens-key-done').click()
  await page.locator('.el-message-box').getByRole('button', { name: '已保存，关闭' }).click()
  await expect(page.getByTestId('tokens-key-dialog')).toBeHidden()

  await expect(page.getByTestId('tokens-key-count')).toHaveText('1 / 20')
  await expect(page.getByTestId('tokens-key-hint')).toHaveText(`sk-spark-…${plain.slice(-4)}`)
  await expect(page.locator('body')).not.toContainText(plain)
  await page.reload()
  await expect(page.getByTestId('tokens-key-hint')).toHaveText(`sk-spark-…${plain.slice(-4)}`)
  await expect(page.locator('body')).not.toContainText(plain)

  // 网关认这把 Key：鉴权通过，才轮到模型校验（404）；随便一个格式正确的 Key 是 401
  const ok = await callChat(plain, 'no-such-model')
  expect(ok.status).toBe(404)
  expect((ok.json?.error as { code: string }).code).toBe('model_not_found')
  const bad = await callChat(`sk-spark-${'A'.repeat(40)}`, 'no-such-model')
  expect(bad.status).toBe(401)
  expect((bad.json?.error as { code: string }).code).toBe('invalid_api_key')
})

test('C-TOK-05 停用 / 启用 / 撤销 Key，网关随之拒绝或放行', async ({ page }) => {
  const u = await seedUser()
  const api = await userApi(u.phone)
  const k = await createKey(api, '测试 Key')
  await loginConsole(page, u.phone)
  await page.goto('/tokens?tab=keys')
  const status = page.getByTestId(`tokens-key-status-${k.id}`)
  const toggle = page.getByTestId(`tokens-key-disable-${k.id}`)
  await expect(status).toHaveText('启用')
  await expect(page.getByTestId(`tokens-key-row-${k.id}`)).toContainText('测试 Key')

  await toggle.click()
  await toast(page, '已停用')
  await expect(status).toHaveText('已停用')
  await expect(toggle).toHaveText('启用')
  expect((await callChat(k.key, 'no-such-model')).status).toBe(401)

  await toggle.click()
  await toast(page, '已启用')
  await expect(status).toHaveText('启用')
  expect((await callChat(k.key, 'no-such-model')).status).toBe(404)

  await page.getByTestId(`tokens-key-revoke-${k.id}`).click()
  await page.locator('.el-message-box').getByRole('button', { name: '撤销', exact: true }).click()
  await toast(page, '已撤销')
  await expect(status).toHaveText('已撤销')
  await expect(page.getByTestId(`tokens-key-disable-${k.id}`)).toHaveCount(0)
  await expect(page.getByTestId(`tokens-key-revoke-${k.id}`)).toHaveCount(0)
  await expect(page.getByTestId('tokens-key-count')).toHaveText('0 / 20')
  const gone = await callChat(k.key, 'no-such-model')
  expect(gone.status).toBe(401)
  expect((gone.json?.error as { code: string }).code).toBe('invalid_api_key')
})

test('C-TOK-06 用量 Tab：一次非流式 + 一次流式调用后今日 / 本月 / 未结算与按天表，清扫结算后未结算清零', async ({ page }) => {
  const m = await seedLLMModel(PRICY)
  await createPoolMachineAndNode(m)
  const u = await seedUser()
  const api = await userApi(u.phone)
  const k = await createKey(api)
  await waitGatewayModel(k.key, m.slug)
  const plain = await callChat(k.key, m.slug)
  expect(plain.status).toBe(200)
  expect(plain.json?.usage).toMatchObject({ prompt_tokens: 100, completion_tokens: 50 })
  expect(plain.requestId).toMatch(/^req-[a-z0-9]{16}$/)
  const stream = await callChat(k.key, m.slug, { stream: true })
  expect(stream.status).toBe(200)
  expect(stream.text).toContain('"usage"') // 网关注入 include_usage，末尾带 usage 块
  expect(stream.text.trimEnd().endsWith('data: [DONE]')).toBe(true)
  const today = cstDate(await hooks.clock())

  await loginConsole(page, u.phone)
  await page.goto('/tokens?tab=usage')
  await expect(page.getByTestId('tokens-stat-today')).toHaveText(yuan(200))
  await expect(page.getByTestId('tokens-stat-month')).toHaveText(yuan(200))
  await expect(page.getByTestId('tokens-stat-tokens')).toHaveText('300')
  await expect(page.getByTestId('tokens-stat-unsettled')).toHaveText(yuan(200))
  await expect(page.getByTestId('tokens-stat-debt')).toHaveText(yuan(0))
  await expect(page.getByTestId('tokens-daily-row')).toHaveText([today])
  const dayRow = page.getByTestId('tokens-daily-table').getByRole('row').filter({ has: page.getByTestId('tokens-daily-row') })
  await expect(dayRow).toContainText(m.slug)
  await expect(dayRow.getByTestId('tokens-daily-cost')).toHaveText('¥2.00')
  await expect(page.getByTestId('tokens-settlements-empty')).toBeVisible()

  await sweep() // ¥2.00 ≥ ¥1：本轮结算
  await page.reload()
  await expect(page.getByTestId('tokens-stat-unsettled')).toHaveText(yuan(0))
  await expect(page.getByTestId('tokens-stat-today')).toHaveText(yuan(200))
  await expect(page.getByTestId('tokens-stat-month')).toHaveText(yuan(200))
  await expect(page.getByTestId('tokens-daily-row')).toHaveText([today])
  await expect(dayRow.getByTestId('tokens-daily-cost')).toHaveText('¥2.00')
  await expect(page.getByTestId('tokens-settlements-table').getByText(yuan(200), { exact: true })).toBeVisible()
})

test('C-TOK-07 欠费：提示条显示欠费金额、「去充值」跳转充值页，网关返回 402', async ({ page }) => {
  const m = await seedLLMModel(PRICY)
  await createPoolMachineAndNode(m)
  const u = await seedUser({ balance: 50 }) // ¥0.50，一次调用 ¥1.00：预检按 max_tokens=1 估算只要几分钱，放行
  const k = await createKey(await userApi(u.phone))
  await waitGatewayModel(k.key, m.slug)
  expect((await callChat(k.key, m.slug, { max_tokens: 1 })).status).toBe(200)
  await sweep() // 结算：实扣 ¥0.50，¥0.50 计入欠费
  const denied = await callChat(k.key, m.slug, { max_tokens: 1 })
  expect(denied.status).toBe(402)
  expect((denied.json?.error as { code: string }).code).toBe('insufficient_quota')

  await loginConsole(page, u.phone)
  await page.goto('/tokens')
  await expect(page.getByTestId('tokens-debt-alert')).toBeVisible()
  await expect(page.getByTestId('tokens-debt-amount')).toHaveText(yuan(50))
  await page.getByTestId('tokens-tab-usage').click()
  await expect(page.getByTestId('tokens-stat-debt')).toHaveText(yuan(50))
  await expect(page.getByTestId('tokens-settlements-table')).toContainText('余额不足，¥0.50 计入欠费')
  await page.getByTestId('tokens-debt-recharge').click()
  await expect(page).toHaveURL(/\/wallet\/recharge$/)
})

test('C-TOK-08 请求日志：分页（每页 20 条）、状态中文与 HTTP 码、请求 ID', async ({ page }) => {
  const m = await seedLLMModel()
  await createPoolMachineAndNode(m)
  const u = await seedUser()
  const k = await createKey(await userApi(u.phone), '日志 Key')
  await waitGatewayModel(k.key, m.slug)
  const ids: string[] = []
  for (let i = 0; i < 21; i++) {
    const r = await callChat(k.key, m.slug)
    expect(r.status).toBe(200)
    ids.push(r.requestId)
  }
  await hooks.llmUpstream({ fail_first_n: 1 }) // 唯一的节点回 503，没有别的节点可转移 → 502、记一行「上游错误」
  const failed = await callChat(k.key, m.slug)
  expect(failed.status).toBe(502)
  await hooks.llmUpstream() // 恢复正常的上游（清理时不受影响）

  await alignPageClock(page)
  await loginConsole(page, u.phone)
  await page.goto('/tokens?tab=logs')
  await expect(page.getByTestId('tokens-logs-total')).toHaveText('共 22 条')
  const rows = page.locator('[data-testid^="tokens-log-row-"]')
  await expect(rows).toHaveCount(20)
  // 新的在前：第一行是失败的那次
  await expect(page.getByTestId(`tokens-log-row-${failed.requestId}`)).toBeVisible()
  await expect(page.getByTestId('tokens-log-status').first()).toHaveText('上游错误')
  await expect(page.getByTestId('tokens-log-http').first()).toHaveText('502')
  await expect(page.getByTestId('tokens-log-request-id').first()).toHaveText(failed.requestId)
  await expect(page.getByTestId('tokens-log-status').nth(1)).toHaveText('成功')
  await expect(page.getByTestId('tokens-log-key').first()).toHaveText(`日志 Key（sk-spark-…${k.hint.slice(-4)}）`)
  await expect(page.getByTestId('tokens-log-tokens').nth(1)).toHaveText('100 / 50')
  await expect(page.getByTestId('tokens-log-row-' + ids[0])).toHaveCount(0) // 最早的两条在第 2 页
  await page.getByTestId('tokens-logs-pager').getByText('2', { exact: true }).click()
  await expect(rows).toHaveCount(2)
  await expect(page.getByTestId(`tokens-log-row-${ids[0]}`)).toBeVisible()
  await expect(page.getByTestId(`tokens-log-row-${ids[1]}`)).toBeVisible()
})

test('C-TOK-09 结算记录：金额与明细，「查看」链接到钱包流水并定位到这条「Token 消费」', async ({ page }) => {
  const m = await seedLLMModel(PRICY)
  await createPoolMachineAndNode(m)
  const u = await seedUser()
  const api = await userApi(u.phone)
  const k = await createKey(api)
  await waitGatewayModel(k.key, m.slug)
  expect((await callChat(k.key, m.slug)).status).toBe(200)
  await sweep()
  const s = ((await (await api.get('/api/v1/llm/settlements')).json()) as { items: { id: number; charged: number; wallet_ledger_id: number | null }[] }).items
  expect(s).toHaveLength(1)
  expect(s[0].charged).toBe(100)
  expect(s[0].wallet_ledger_id).not.toBeNull()

  await loginConsole(page, u.phone)
  await page.goto('/tokens?tab=usage')
  await expect(page.getByTestId(`tokens-settlement-charged-${s[0].id}`)).toHaveText(yuan(100))
  await expect(page.getByTestId(`tokens-settlement-detail-${s[0].id}`)).toHaveText(`用量 ${yuan(100)}`)
  await page.getByTestId(`tokens-settlement-ledger-${s[0].id}`).click()
  await expect(page).toHaveURL((x) => x.pathname === '/wallet' && x.searchParams.get('ledger') === String(s[0].wallet_ledger_id))
  await expect(page.getByTestId('ledger-focus')).toContainText(`已定位到流水 #${s[0].wallet_ledger_id}`)
  await expect(page.getByTestId('ledger-kind').first()).toHaveText('Token 消费')
  await expect(page.getByTestId('ledger-amount').first()).toHaveText(`-${yuan(100)}`)
})

test('C-TOK-10 快速开始：base_url 与 curl / Python 示例含已上线的对话模型 slug，默认限额', async ({ page }) => {
  const m = await seedLLMModel({ sort: -10000 }) // 排在最前：示例取第一个对话模型
  const pub = await publicModels()
  expect(pub.items.find((x) => x.kind === 'chat')?.slug).toBe(m.slug)
  await page.goto('/tokens?tab=quickstart')
  await expect(page.getByTestId('tokens-quickstart-base-url')).toHaveText(pub.base_url)
  await expect(page.getByTestId('tokens-quickstart-model')).toHaveText(m.slug)
  const base = pub.base_url.replace(/\/+$/, '')
  await expect(page.getByTestId('tokens-quickstart-curl')).toContainText(`curl ${base}/chat/completions`)
  await expect(page.getByTestId('tokens-quickstart-curl')).toContainText(`"model":"${m.slug}"`)
  await expect(page.getByTestId('tokens-quickstart-python')).toContainText(base)
  await expect(page.getByTestId('tokens-quickstart-python')).toContainText(m.slug)
  await expect(page.getByTestId('tokens-quickstart-note-limits')).toContainText('每分钟 60 次请求')
  await page.getByTestId('tokens-quickstart-copy-base-url').click()
  await toast(page, '已复制')
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(pub.base_url)
})
