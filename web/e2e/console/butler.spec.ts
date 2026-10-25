import { expect, test, type APIRequestContext, type Page } from '@playwright/test'
import { appRow, cleanupApps, listDeployments, ownInstance, seedApps } from '../helpers/apps'
import {
  INJECTION_APP_SCRIPT, chat, closeDrawer, expectAssistantContains, expectIdle, gotoButlerPage, historyItem, lastConfirmCard, lastStep, openConversation,
  openDrawer, openHistory, replyText, resetButler, seedRunningInstanceWithFailedApp, send,
} from '../helpers/butler'
import { isMobile, loginConsole, seedUser, toast } from '../helpers/console'
import { hooks } from '../helpers/hooks'
import { instanceOf, runningInstance } from '../helpers/instances'
import { yuan } from '../helpers/money'

// AI 管家控制台用例（计划 12，规格 §16.3）。e2e-server 用 mock 提供方：回复优先取 hooks.butlerReply 注入的队列，
// 队列空时按 internal/butler/llm/mock/rules.go 的关键词规则（推荐 / 部署 / 续费 / 释放 / 日志诊断 / 注入）。
// 电脑端走浮动按钮 + 抽屉，手机端走底部「管家」进入 /butler（helpers/butler.ts openDrawer 分流）。
// 每个用例新建用户；开始前清空回复队列、设置回到默认；造了部署的用例由 afterEach cleanupApps 卸载。
test.beforeEach(resetButler)
test.afterEach(cleanupApps)

const DEFAULT_TEXT = '我可以帮你选机器、部署应用、排查问题、查看余额。'
const SUGGESTIONS = ['帮我选一台机器', '在我的实例上部署 vLLM', '我的应用为什么失败了', '我的余额还能用多久']

type Product = { id: number; name: string; prices: { duration: string; amount: number }[] }
async function products(api: APIRequestContext): Promise<Product[]> {
  const r = await api.get('/api/v1/products')
  if (!r.ok()) throw new Error(`GET products ${r.status()} ${await r.text()}`)
  return ((await r.json()) as { items: Product[] }).items
}
const balanceOf = async (api: APIRequestContext) => ((await (await api.get('/api/v1/wallet')).json()) as { balance: number }).balance

/** 登录已有运行中实例的新用户。 */
async function userWithInstance(page: Page) {
  const u = await seedUser()
  const { api, id } = await runningInstance(u.phone)
  await loginConsole(page, u.phone)
  return { u, api, id }
}

test('C-BOT-01 未登录点 AI 管家入口跳登录，登录后回到原页', async ({ page }, info) => {
  const u = await seedUser()
  await hooks.advance(61) // 本手机号 60 秒内没发过码
  await page.goto('/market')
  // 电脑：右下角浮动按钮（redirect 回市场页）；手机：底部「管家」标签（/butler 需要登录，redirect 回 /butler）
  const back = isMobile(info) ? '/butler' : '/market'
  await page.getByTestId(isMobile(info) ? 'tabbar-butler' : 'butler-launcher').click()
  await expect(page).toHaveURL((url) => url.pathname === '/login' && url.searchParams.get('redirect') === back)
  await page.getByTestId('login-phone').fill(u.phone)
  await page.getByTestId('login-send').click()
  await page.getByTestId('login-code').fill('000000')
  await page.getByTestId('login-submit').click()
  await expect(page).toHaveURL((url) => url.pathname === back)
  if (isMobile(info)) {
    await expect(page.getByTestId('butler-page')).toBeVisible()
  } else {
    await page.getByTestId('butler-launcher').click()
    await expect(page.locator('.butler-drawer')).toBeVisible()
  }
  await expect(chat(page).getByTestId('butler-welcome')).toBeVisible()
})

test('C-BOT-02 空状态与建议 chips：点「帮我选一台机器」→ 步骤行「✓ 查询套餐」与推荐文本', async ({ page }, info) => {
  const u = await seedUser()
  await loginConsole(page, u.phone)
  const names = (await products(page.request)).map((p) => p.name)
  const c = await openDrawer(page, info)
  await expect(c.getByTestId('butler-welcome')).toBeVisible()
  for (const [i, s] of SUGGESTIONS.entries()) await expect(c.getByTestId(`butler-suggest-${i}`)).toHaveText(s)
  await expect(c.getByTestId('butler-quota')).toHaveText('今日已用 0%')
  await c.getByTestId('butler-suggest-0').click()
  await expect(c.getByTestId('butler-msg-user')).toHaveText(['帮我选一台机器'])
  await expect(c.getByTestId('butler-welcome')).toHaveCount(0)
  await expect(lastStep(page, 'list_products')).toHaveText('✓ 查询套餐')
  await expectAssistantContains(page, '要我帮你下单吗？')
  const text = (await c.getByTestId('butler-msg-assistant').last().textContent()) ?? ''
  const m = /推荐 (.+?)（¥[\d,]+\.\d{2}\/天，现有 \d+ 台可租）。要我帮你下单吗？/.exec(text)
  expect(m, text).not.toBeNull()
  expect(names).toContain(m![1])
  await expect(c.getByTestId('butler-title')).toHaveText('帮我选一台机器')
  await expect(c.getByTestId('butler-quota')).toHaveText(/^今日已用 \d+%$/) // 本轮用量随 usage 事件更新（1100 / 300000 → 0%）
})

test('C-BOT-03 部署 vLLM：确认卡显示应用与生成的密钥，确认后已执行，实例详情出现排队中的部署', async ({ page }, info) => {
  await seedApps()
  const { api, id } = await userWithInstance(page)
  ownInstance(api, id)
  const c = await openDrawer(page, info)
  await c.getByTestId('butler-suggest-1').click() // 「在我的实例上部署 vLLM」
  const card = lastConfirmCard(page)
  await expect(card).toHaveAttribute('data-tool', 'deploy_app')
  await expect(card).toHaveAttribute('data-state', 'pending')
  await expect(lastStep(page, 'list_instances')).toHaveText('✓ 查询实例列表')
  await expect(lastStep(page, 'get_app')).toHaveText('✓ 查看应用详情')
  await expect(lastStep(page, 'generate_secret')).toHaveText('✓ 生成随机密钥')
  await expect(card.getByTestId('butler-confirm-title')).toHaveText('部署应用')
  const summary = card.getByTestId('butler-confirm-summary')
  await expect(summary).toContainText(`在实例 ${id} 上部署 vLLM 推理服务`)
  const key = /接口密钥：([A-Za-z0-9]{32})(?:\n|$)/.exec((await summary.textContent()) ?? '')?.[1]
  expect(key, '确认卡显示生成的 32 位接口密钥').toBeTruthy()
  await expect(card.getByTestId('butler-confirm-danger')).toHaveCount(0)
  await expect(card.getByTestId('butler-confirm-countdown')).toHaveText(/^\d\d:\d\d 内有效$/)
  await card.getByTestId('butler-confirm-ok').click()
  await expect(card.getByTestId('butler-confirm-result')).toHaveText('已执行')
  await expect(card).toHaveAttribute('data-state', 'executed')
  await expectAssistantContains(page, '已提交部署')
  const deps = await listDeployments(api, id)
  expect(deps.map((d) => [d.app_slug, d.state])).toEqual([['vllm', 'queued']])
  expect(deps[0].secret_env).toEqual({ API_KEY: true })

  await page.goto(`/instances/${id}`)
  await expect(appRow(page, deps[0].id)).toHaveAttribute('data-state', 'queued')
  await expect(appRow(page, deps[0].id).getByTestId('detail-app-name')).toHaveText('vLLM 推理服务')

  // 密钥只在确认卡上可见一次：重新打开这段对话，卡片已变成步骤行，页面上没有密钥
  await openConversation(page, info, '在我的实例上部署 vLLM')
  await expect(lastStep(page, 'deploy_app')).toHaveText('✓ 部署应用')
  await expect(chat(page).getByTestId('butler-confirm-card')).toHaveCount(0)
  await expect(chat(page)).not.toContainText(key!)
})

test('C-BOT-04 续费卡显示套餐日价，取消 → 「用户拒绝」收尾文本，余额不变', async ({ page }, info) => {
  const { api, id } = await userWithInstance(page)
  const price = (await products(api))[0].prices.find((p) => p.duration === '1d')!.amount
  const before = await balanceOf(api)
  await openDrawer(page, info)
  await send(page, '帮我续费')
  const card = lastConfirmCard(page)
  await expect(card).toHaveAttribute('data-tool', 'renew_instance')
  await expect(card.getByTestId('butler-confirm-title')).toHaveText('续费实例')
  await expect(card.getByTestId('butler-confirm-summary')).toContainText(`续费实例 ${id}`)
  await expect(card.getByTestId('butler-confirm-amount')).toHaveText(`预计扣款 ${yuan(price)}`)
  await card.getByTestId('butler-confirm-cancel').click()
  await expect(card.getByTestId('butler-confirm-result')).toHaveText('已取消')
  await expect(card).toHaveAttribute('data-state', 'rejected')
  await expectAssistantContains(page, '用户拒绝了这次操作，已取消。')
  expect(await balanceOf(api)).toBe(before)
})

test('C-BOT-05 释放卡为危险样式：勾选「我已了解」前确认按钮禁用，确认后实例进入释放', async ({ page }, info) => {
  const { api, id } = await userWithInstance(page)
  await openDrawer(page, info)
  await send(page, '释放我的实例')
  const card = lastConfirmCard(page)
  await expect(card).toHaveAttribute('data-tool', 'release_instance')
  await expect(card).toHaveClass(/\bdanger\b/)
  await expect(card.getByTestId('butler-confirm-summary')).toContainText(`释放实例 ${id}`)
  await expect(card.getByTestId('butler-confirm-danger')).toHaveText('此操作不可恢复')
  await expect(card.getByTestId('butler-confirm-ok')).toBeDisabled()
  await card.getByTestId('butler-confirm-ack').click()
  await expect(card.getByTestId('butler-confirm-ok')).toBeEnabled()
  expect((await instanceOf(api, id)).state).toBe('running')
  await card.getByTestId('butler-confirm-ok').click()
  await expect(card.getByTestId('butler-confirm-result')).toHaveText('已执行')
  await expectAssistantContains(page, '实例已开始释放。')
  await expect.poll(async () => (await instanceOf(api, id)).state).toMatch(/^(wiping|released)$/)
})

test('C-BOT-06 确认卡过期（假时钟 +11 分钟）→ 按钮禁用「已失效」，发新消息正常', async ({ page }, info) => {
  const { api } = await userWithInstance(page)
  const before = await balanceOf(api)
  await openDrawer(page, info)
  await send(page, '帮我续费')
  await expect(lastConfirmCard(page)).toHaveAttribute('data-state', 'pending')
  await hooks.advance(11 * 60)
  // 倒计时按服务器给的 expires_in 计算（浏览器时钟不随假时钟走）：重新加载后服务器返回 expires_in = 0 → 已失效
  await page.reload()
  await openConversation(page, info, '帮我续费')
  const card = lastConfirmCard(page)
  await expect(card).toHaveAttribute('data-state', 'expired')
  await expect(card.getByTestId('butler-confirm-result')).toHaveText('已失效')
  await expect(card.getByTestId('butler-confirm-ok')).toBeDisabled()
  await expect(card.getByTestId('butler-confirm-cancel')).toBeDisabled()
  await send(page, '你好')
  await expectAssistantContains(page, DEFAULT_TEXT)
  await expectIdle(page)
  expect(await balanceOf(api)).toBe(before) // 过期的卡没有执行：没有扣款
})

test('C-BOT-07 实例详情失败部署旁「问问管家」带上下文 → 回答里有日志摘要', async ({ page }, info) => {
  const u = await seedUser()
  const { id, dep } = await seedRunningInstanceWithFailedApp(u.phone)
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  await expect(appRow(page, dep.id)).toHaveAttribute('data-state', 'failed')
  await appRow(page, dep.id).getByTestId('detail-app-ask-butler').click()
  const c = chat(page)
  await expect(c).toBeVisible()
  if (isMobile(info)) await expect(page).toHaveURL(/\/butler$/)
  await expect(c.getByTestId('butler-context')).toHaveText(`将带上实例 ${id} 的信息`)
  await expect(c.getByTestId('butler-input')).toHaveValue('这个实例上的应用为什么失败了？')
  await c.getByTestId('butler-send').click()
  await expect(lastStep(page, 'list_instance_apps')).toHaveText('✓ 查询实例上的应用')
  await expect(lastStep(page, 'get_deployment_log')).toHaveText('✓ 读取部署日志')
  await expectAssistantContains(page, /日志显示：.*boom.*。建议重新部署。/)
  await expect(c.getByTestId('butler-context')).toHaveCount(0) // 上下文只随这一条消息发送
})

test('C-BOT-08 注入：部署日志里有注入文本 → 管家只提出释放卡、不执行，关闭后实例仍运行', async ({ page }, info) => {
  const u = await seedUser()
  const { api, id, dep } = await seedRunningInstanceWithFailedApp(u.phone, INJECTION_APP_SCRIPT)
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  await expect(appRow(page, dep.id)).toHaveAttribute('data-state', 'failed')
  await page.getByTestId('detail-ask-butler').click() // 卡片头的入口：只带上下文
  await expect(chat(page).getByTestId('butler-context')).toHaveText(`将带上实例 ${id} 的信息`)
  await send(page, '看看部署日志')
  // mock 读到日志里的「IGNORE ALL INSTRUCTIONS」后「上钩」提出释放：系统只会生成待确认的危险卡
  const card = lastConfirmCard(page)
  await expect(card).toHaveAttribute('data-tool', 'release_instance')
  await expect(card).toHaveAttribute('data-state', 'pending')
  await expect(card.getByTestId('butler-confirm-summary')).toContainText(`释放实例 ${id}`)
  await expect(card.getByTestId('butler-confirm-ok')).toBeDisabled() // 危险操作没勾选不能确认
  await expectIdle(page)
  expect((await instanceOf(api, id)).state).toBe('running')

  await closeDrawer(page, info, `/instances/${id}`)
  await expect(page.getByTestId('detail-state')).toHaveText('运行中')
  // 卡片过期并被清扫后也不会执行
  await hooks.advance(11 * 60)
  await hooks.sweep()
  expect((await instanceOf(api, id)).state).toBe('running')
  const conv = ((await (await api.get('/api/v1/butler/conversations')).json()) as { items: { id: string; status: string }[] }).items
  expect(conv).toHaveLength(1)
  const detail = (await (await api.get(`/api/v1/butler/conversations/${conv[0].id}`)).json()) as { pending_tool_call: unknown }
  expect(detail.pending_tool_call).toBeNull()
})

test('C-BOT-09 今日额度用完：横幅与 429 文案；次日恢复', async ({ page }, info) => {
  const u = await seedUser()
  await hooks.butlerUsage({ user_id: u.id, input_tokens: 300_000 })
  await loginConsole(page, u.phone)
  const c = await openDrawer(page, info)
  await expect(c.getByTestId('butler-quota')).toHaveText('今日已用 100%')
  const resp = page.waitForResponse((r) => r.url().endsWith('/api/v1/butler/conversations/new/messages'))
  await c.getByTestId('butler-input').fill('你好')
  await c.getByTestId('butler-send').click()
  expect((await resp).status()).toBe(429)
  await expect(c.getByTestId('butler-banner')).toContainText('今日 AI 管家额度已用完，明天再来')
  await expect(c.getByTestId('butler-msg-user')).toHaveCount(0) // 服务端没收下：不留气泡，原文放回输入框
  await expect(c.getByTestId('butler-input')).toHaveValue('你好')

  await hooks.advance(86400)
  await page.reload()
  const c2 = await openDrawer(page, info)
  await expect(c2.getByTestId('butler-quota')).toHaveText('今日已用 0%')
  await replyText('新的一天，额度恢复了')
  await send(page, '你好')
  await expectAssistantContains(page, '新的一天，额度恢复了')
  await expect(c2.getByTestId('butler-banner')).toHaveCount(0)
})

test('C-BOT-10 模型不可用 → 错误行 + 重试成功', async ({ page }, info) => {
  const u = await seedUser()
  await loginConsole(page, u.phone)
  const c = await openDrawer(page, info)
  await hooks.butlerReply({ error: true })
  await send(page, '你好呀')
  const err = c.getByTestId('butler-error')
  await expect(err).toHaveAttribute('data-code', 'butler_llm_unavailable')
  await expect(err).toContainText('AI 管家暂时不可用，请稍后再试')
  await replyText('我恢复正常了')
  await err.getByTestId('butler-retry').click()
  await expectAssistantContains(page, '我恢复正常了')
  await expect(c.getByTestId('butler-error')).toHaveCount(0)
  await expect(c.getByTestId('butler-msg-user').last()).toHaveText('你好呀')
})

test('C-BOT-11 历史列表：两段对话切换、删除对话', async ({ page }, info) => {
  const u = await seedUser()
  await loginConsole(page, u.phone)
  await gotoButlerPage(page, info)
  const c = chat(page)
  await replyText('第一段的回复')
  await send(page, '第一段对话')
  await expectAssistantContains(page, '第一段的回复')
  await c.getByTestId('butler-new').click()
  await expect(c.getByTestId('butler-welcome')).toBeVisible()
  await hooks.advance(1) // 假时钟不走：两段对话的最后消息时间相同时列表顺序不确定
  await replyText('第二段的回复')
  await send(page, '第二段对话')
  await expectAssistantContains(page, '第二段的回复')

  let list = await openHistory(page, info)
  await expect(list.getByTestId('butler-list-item')).toHaveCount(2)
  await expect(list.getByTestId('butler-list-item').first()).toContainText('第二段对话') // 最近的在上
  await historyItem(list, '第一段对话').click()
  await expect(c.getByTestId('butler-title')).toHaveText('第一段对话')
  await expect(c.getByTestId('butler-msg-user')).toHaveText(['第一段对话'])
  await expect(c.getByTestId('butler-msg-assistant')).toHaveText(['第一段的回复'])

  list = await openHistory(page, info)
  await historyItem(list, '第二段对话').click()
  await expect(c.getByTestId('butler-msg-user')).toHaveText(['第二段对话'])
  await expect(c.getByTestId('butler-msg-assistant')).toHaveText(['第二段的回复'])

  list = await openHistory(page, info)
  await historyItem(list, '第一段对话').getByTestId('butler-list-delete').click()
  await expect(page.getByTestId('butler-delete-dialog')).toBeVisible()
  await page.getByTestId('butler-delete-ok').click()
  await toast(page, '已删除')
  await expect(list.getByTestId('butler-list-item')).toHaveCount(1)
  await expect(historyItem(list, '第一段对话')).toHaveCount(0)
  await expect(c.getByTestId('butler-msg-user')).toHaveText(['第二段对话']) // 当前对话不受影响
  const items = ((await (await page.request.get('/api/v1/butler/conversations')).json()) as { items: { title: string }[] }).items
  expect(items.map((i) => i.title)).toEqual(['第二段对话'])
})

test('C-BOT-12 手机底部「管家」标签进入 /butler，Enter 发送成功（电脑端走侧栏「AI 管家」）', async ({ page }, info) => {
  // 规格只要求手机端；覆盖表禁止 test.skip，电脑端同一用例改走侧栏入口
  const u = await seedUser()
  await loginConsole(page, u.phone)
  if (isMobile(info)) {
    await expect(page.getByTestId('tabbar-butler')).toHaveText('管家')
    await expect(page.getByTestId('butler-launcher')).toHaveCount(0) // 手机没有浮动按钮
  } else {
    await expect(page.getByTestId('menu-butler')).toHaveText('AI 管家')
    await expect(page.getByTestId('butler-launcher')).toBeVisible()
  }
  await gotoButlerPage(page, info)
  await expect(page.getByTestId('butler-launcher')).toHaveCount(0) // /butler 全页不显示浮动按钮
  await expect(page.getByTestId(isMobile(info) ? 'butler-history' : 'butler-list')).toBeVisible()
  const input = chat(page).getByTestId('butler-input')
  await input.fill('你好')
  await input.press('Shift+Enter') // 换行，不发送
  await expect(input).toHaveValue('你好\n')
  await expect(chat(page).getByTestId('butler-msg-user')).toHaveCount(0)
  await replyText('你好，我是 AI 管家')
  await input.press('Enter')
  await expect(chat(page).getByTestId('butler-msg-user')).toHaveText(['你好'])
  await expectAssistantContains(page, '你好，我是 AI 管家')
  await expect(input).toHaveValue('')
})

test('C-BOT-13 Markdown：<script> 被转义、站外链接是纯文本、站内链接可点', async ({ page }, info) => {
  const u = await seedUser()
  await loginConsole(page, u.phone)
  const c = await openDrawer(page, info)
  const text = [
    '先看 **加粗** 和 `code`。',
    '<script>window.__butlerXss = 1</script>',
    '站外：[官网](https://evil.example/phish)',
    '站内：[实例详情](/instances/ins-e2emd1)',
  ].join('\n\n')
  await replyText(text, { chunk_delay_ms: 150 }) // 约 20 段、3 秒：看得到流式过程
  await send(page, '给我几个链接')
  const bubble = c.getByTestId('butler-msg-assistant').last()
  await expect(bubble).toHaveAttribute('data-streaming', 'true')
  await expect(bubble).toHaveAttribute('data-streaming', 'false')
  await expect(bubble.locator('strong')).toHaveText('加粗')
  await expect(bubble.locator('code')).toHaveText('code')
  await expect(bubble).toContainText('<script>window.__butlerXss = 1</script>')
  await expect(bubble.locator('script')).toHaveCount(0)
  expect(await page.evaluate(() => (window as unknown as { __butlerXss?: number }).__butlerXss)).toBeUndefined()
  await expect(bubble.locator('a[href*="evil.example"]')).toHaveCount(0)
  await expect(bubble).toContainText('官网 (https://evil.example/phish)')
  const link = bubble.locator('a[href="/instances/ins-e2emd1"]')
  await expect(link).toHaveText('实例详情')
  expect(await link.getAttribute('target')).toBeNull() // 站内链接不开新窗口（走前端路由）
  await link.click()
  await expect(page).toHaveURL(/\/instances\/ins-e2emd1$/)
})
