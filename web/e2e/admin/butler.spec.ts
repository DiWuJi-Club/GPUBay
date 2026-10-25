import { expect, test } from '@playwright/test'
import { adminApi, createAdmin, expectAdminUrl, getJSON, loginAdmin, logout } from '../helpers/admin'
import { cleanupApps } from '../helpers/apps'
import { INJECTION_APP_SCRIPT, resetButler, seedRunningInstanceWithFailedApp, sendViaApi } from '../helpers/butler'
import { loginConsole, seedUser, toast, userApi } from '../helpers/console'
import { CONSOLE_URL, hooks } from '../helpers/hooks'
import { runStamp } from '../helpers/phone'
import { cstDate } from '../helpers/time'

// AI 管家后台用例（计划 12，规格 §12 / §16.3）。用量 / 对话都由用例自己造（hooks.butlerUsage 直接记账、
// 经控制台 API 发消息走 mock 规则）；按日合计是全站的，只与接口返回值比对。A-BOT-03 改了全局设置，finally 里恢复默认。
test.beforeEach(resetButler)
test.afterEach(cleanupApps)

const count = (n: number) => String(Math.trunc(n)).replace(/\B(?=(\d{3})+(?!\d))/g, ',')

type UsageRow = { day: string; requests: number; input_tokens: number; output_tokens: number; tool_calls: number; failures: number; users: number }
type UsageReport = { from: string; to: string; items: UsageRow[]; totals: Omit<UsageRow, 'day' | 'users'> }

test('A-BOT-01 用量页：按日合计与接口一致，点某天看按用户用量', async ({ page }) => {
  const u = await seedUser()
  const now = await hooks.clock()
  const today = cstDate(now)
  const yesterday = cstDate(new Date(now.getTime() - 86400_000))
  // token 量远大于其它用例造的用量：按用户表按 token 倒序，本用户排在第一页
  await hooks.butlerUsage({ user_id: u.id, input_tokens: 9_000_000, output_tokens: 123_456, requests: 7, tool_calls: 5, failures: 2 })
  await hooks.butlerUsage({ user_id: u.id, day: yesterday, input_tokens: 8_000_000, output_tokens: 1_000, requests: 3 })
  const admin = await createAdmin('finance')
  const ctx = await adminApi(admin)
  const rep = await getJSON<UsageReport>(ctx, '/admin/api/v1/butler/usage')
  expect(rep.to).toBe(today)
  const day = rep.items.find((d) => d.day === today)!

  await loginAdmin(page, admin)
  await page.getByTestId('menu-butler').click()
  await expectAdminUrl(page, '/butler')
  const row = page.locator(`[data-testid="admin-butler-usage-row"][data-day="${today}"]`)
  await expect(row.getByTestId('admin-butler-usage-requests')).toHaveText(count(day.requests))
  await expect(row.getByTestId('admin-butler-usage-input')).toHaveText(count(day.input_tokens))
  await expect(row.getByTestId('admin-butler-usage-output')).toHaveText(count(day.output_tokens))
  await expect(row.getByTestId('admin-butler-usage-failures')).toHaveText(count(day.failures))
  await expect(row.getByTestId('admin-butler-usage-users')).toHaveText(count(day.users))
  const total = page.getByTestId('admin-butler-usage-total')
  await expect(total.getByTestId('admin-butler-total-requests')).toHaveText(count(rep.totals.requests))
  await expect(total.getByTestId('admin-butler-total-input')).toHaveText(count(rep.totals.input_tokens))
  await expect(page.getByTestId('admin-butler-usage-row').first()).toHaveAttribute('data-day', today) // 最近的在上

  // 按用户：缺省今天（服务器时钟）
  await expect(page.getByTestId('admin-butler-users-day')).toHaveValue(today)
  const mine = page.locator(`[data-testid="admin-butler-users-row"][data-user="${u.id}"]`)
  await expect(mine.locator('td')).toHaveText([String(u.id), `${u.phone.slice(0, 3)}****${u.phone.slice(7)}`, '7', '9,000,000', '123,456', '5', '2', /^¥/])
  await page.locator(`[data-testid="admin-butler-usage-row"][data-day="${yesterday}"]`).click()
  await expect(page.getByTestId('admin-butler-users-day')).toHaveValue(yesterday)
  await expect(mine.locator('td').nth(2)).toHaveText('3')
  await expect(mine.locator('td').nth(3)).toHaveText('8,000,000')
})

test('A-BOT-02 对话列表按用户筛选 → 详情抽屉显示消息、工具调用与「可疑」标记', async ({ page }) => {
  const u = await seedUser()
  const { api, id } = await seedRunningInstanceWithFailedApp(u.phone, INJECTION_APP_SCRIPT)
  // 日志里有注入文本：mock「上钩」提出释放卡（待确认，不执行）
  const { conversationId } = await sendViaApi(api, '看看部署日志', { instanceId: id })
  const admin = await createAdmin('ops')
  await loginAdmin(page, admin)
  await page.goto('/admin/butler')
  await page.getByTestId('admin-butler-tab-conversations').click()
  await page.getByTestId('admin-butler-conv-user').fill(String(u.id))
  await page.getByTestId('admin-butler-conv-search').click()
  const rows = page.getByTestId('admin-butler-conv-row')
  await expect(rows).toHaveCount(1)
  await expect(rows.first()).toHaveAttribute('data-id', conversationId)
  await expect(rows.first().getByTestId('admin-butler-conv-title')).toHaveText('看看部署日志')
  await expect(rows.first().getByTestId('admin-butler-conv-writes')).toHaveText('1')
  await rows.first().click()

  const drawer = page.getByTestId('admin-butler-conv-drawer')
  await expect(drawer.getByTestId('admin-butler-conv-id')).toHaveText(conversationId)
  const msgs = drawer.getByTestId('admin-butler-conv-msg')
  await expect(msgs.first()).toHaveAttribute('data-role', 'user')
  await expect(msgs.first()).toContainText('看看部署日志')
  await expect(drawer.getByTestId('admin-butler-suspicious').first()).toHaveText('可疑：疑似提示注入')
  await expect(drawer.locator('[data-testid="admin-butler-conv-call"][data-tool="release_instance"]')).toHaveAttribute('data-status', 'pending')
  await expect(drawer.locator('[data-testid="admin-butler-conv-call"][data-tool="list_instance_apps"]').first()).toHaveAttribute('data-status', 'executed')
  await expect(drawer.locator('[data-testid="admin-butler-conv-call"][data-tool="release_instance"]').getByTestId('admin-butler-call-args')).toContainText(id)
})

test('A-BOT-03 设置：finance 只读；super 关闭并填写公告 → 审计 butler.settings，控制台显示公告', async ({ page, browser }) => {
  const notice = `系统维护中 ${runStamp}`
  try {
    const finance = await createAdmin('finance')
    await loginAdmin(page, finance)
    await page.goto('/admin/butler?tab=settings')
    await expect(page.getByTestId('admin-butler-readonly')).toHaveText('只有超级管理员可修改')
    await expect(page.getByTestId('admin-butler-save')).toBeDisabled()
    await expect(page.getByTestId('admin-butler-enabled')).toHaveClass(/is-disabled/)
    await expect(page.getByTestId('admin-butler-notice')).toBeDisabled()
    await expect(page.getByTestId('admin-butler-daily')).toHaveValue('300000')

    await logout(page)
    const sup = await createAdmin('super')
    await loginAdmin(page, sup)
    await page.goto('/admin/butler?tab=settings')
    await expect(page.getByTestId('admin-butler-readonly')).toHaveCount(0)
    await expect(page.getByTestId('admin-butler-enabled')).toHaveClass(/is-checked/)
    await page.getByTestId('admin-butler-enabled').click()
    await expect(page.getByTestId('admin-butler-enabled')).not.toHaveClass(/is-checked/)
    await page.getByTestId('admin-butler-notice').fill(notice)
    await page.getByTestId('admin-butler-save').click()
    await toast(page, '已保存')
    const saved = await getJSON<{ enabled: boolean; notice: string; updated_by: number | null }>(await adminApi(sup), '/admin/api/v1/butler/settings')
    expect(saved).toMatchObject({ enabled: false, notice, updated_by: sup.id })

    // 审计：按操作人与动作筛选只有这一条
    await page.goto('/admin/audit')
    await page.getByTestId('audit-actor').fill(String(sup.id))
    await page.getByTestId('audit-action').click()
    await page.getByRole('option', { name: 'butler.settings' }).click()
    await page.getByTestId('audit-search').click()
    const rows = page.getByRole('row').filter({ has: page.getByTestId('audit-action-cell') })
    await expect(rows).toHaveCount(1)
    await expect(rows.first().getByTestId('audit-action-cell')).toHaveText('butler.settings')
    await expect(rows.first().getByTestId('audit-actor-cell')).toHaveText(sup.username)

    // 控制台：状态接口反映关闭，聊天区横幅显示公告、输入禁用
    const u = await seedUser()
    const uApi = await userApi(u.phone)
    expect(await getJSON<{ enabled: boolean; notice: string }>(uApi, `${CONSOLE_URL}/api/v1/butler/status`)).toMatchObject({ enabled: false, notice })
    const cctx = await browser.newContext({ baseURL: CONSOLE_URL, viewport: { width: 1280, height: 720 }, locale: 'zh-CN', timezoneId: 'Asia/Shanghai' })
    try {
      const cp = await cctx.newPage()
      await loginConsole(cp, u.phone)
      await cp.getByTestId('butler-launcher').click()
      const chat = cp.getByTestId('butler-chat').filter({ visible: true })
      await expect(chat.getByTestId('butler-banner')).toContainText(`AI 管家暂未开放：${notice}`)
      await expect(chat.getByTestId('butler-input')).toBeDisabled()
      await expect(chat.getByTestId('butler-suggest-0')).toBeDisabled()
    } finally {
      await cctx.close()
    }
  } finally {
    await hooks.butlerSettings({ defaults: true })
  }
})

test('A-BOT-04 用户详情「AI 管家」卡：对话数、近 30 天 token 与写操作，「查看对话」带上用户筛选', async ({ page }) => {
  const u = await seedUser()
  const api = await userApi(u.phone)
  await hooks.butlerReply({ text: '你好，有什么可以帮你？', usage: { input_tokens: 2000, output_tokens: 345 } })
  await sendViaApi(api, '在吗')
  await hooks.butlerUsage({ user_id: u.id, input_tokens: 10_000 })
  const admin = await createAdmin('ops')
  await loginAdmin(page, admin)
  await page.goto(`/admin/users/${u.id}`)
  const card = page.getByTestId('admin-user-butler-card')
  await expect(card.getByTestId('admin-user-butler-conversations')).toHaveText('1')
  await expect(card.getByTestId('admin-user-butler-tokens')).toHaveText('12,345')
  await expect(card.getByTestId('admin-user-butler-writes')).toHaveText('0')
  await card.getByTestId('admin-user-butler-link').click()
  await expectAdminUrl(page, '/butler', { tab: 'conversations', user_id: String(u.id) })
  await expect(page.getByTestId('admin-butler-conv-user')).toHaveValue(String(u.id))
  await expect(page.getByTestId('admin-butler-conv-row')).toHaveCount(1)
  await expect(page.getByTestId('admin-butler-conv-title')).toHaveText('在吗')
})

test('A-BOT-05 仪表盘「AI 管家（今日）」三个数字随当日用量变化', async ({ page }) => {
  type Dash = { butler_requests_today: number; butler_tokens_today: number; butler_failures_today: number }
  const admin = await createAdmin('finance')
  const ctx = await adminApi(admin)
  const before = await getJSON<Dash>(ctx, '/admin/api/v1/dashboard')
  const u = await seedUser()
  await hooks.butlerUsage({ user_id: u.id, requests: 4, input_tokens: 1000, output_tokens: 234, failures: 2 })
  await loginAdmin(page, admin)
  await expect(page.getByTestId('dash-butler-requests')).toHaveText(String(before.butler_requests_today + 4))
  await expect(page.getByTestId('dash-butler-tokens')).toHaveText((before.butler_tokens_today + 1234).toLocaleString('en-US'))
  await expect(page.getByTestId('dash-butler-failures')).toHaveText(String(before.butler_failures_today + 2))
  await page.getByTestId('dash-butler-link').click()
  await expectAdminUrl(page, '/butler')
})
