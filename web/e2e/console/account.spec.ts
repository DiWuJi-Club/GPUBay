import { expect, test } from '@playwright/test'
import { loginConsole, nav, placeOrder, seedUser, userApi } from '../helpers/console'
import { hooks } from '../helpers/hooks'
import { yuan } from '../helpers/money'
import { DAY, inviteeOf, inviterWithCode } from '../helpers/referral'

test('C-ACC-01 我的页显示手机号、实名状态链接与退出', async ({ page }, info) => {
  const u = await seedUser({ verified: true })
  await loginConsole(page, u.phone)
  await nav(page, info, 'account')
  await expect(page).toHaveURL(/\/account$/)
  await expect(page.getByTestId('account-phone')).toHaveText(u.phone)
  await expect(page.getByTestId('account-kyc')).toHaveText('已认证')
  await expect(page.getByTestId('account-kyc')).toHaveAttribute('href', '/kyc')
  await expect(page.getByTestId('account-created')).toHaveText(/^\d{4}-\d{2}-\d{2} \d{2}:\d{2}$/)
  await expect(page.getByTestId('account-logout')).toBeVisible()
  await expect(page.getByTestId('account-help')).toHaveAttribute('href', '/help')
})

test('C-ACC-02 帮助页未登录可访问，含三节标题', async ({ page }) => {
  await page.goto('/help')
  await expect(page).toHaveURL(/\/help$/)
  await expect(page.getByTestId('help-ssh')).toHaveText('怎么用 SSH 连接')
  await expect(page.getByTestId('help-retention')).toHaveText('数据保留规则')
  await expect(page.getByTestId('help-billing')).toHaveText('计费规则')
  await expect(page.getByTestId('help-page')).toContainText('如遇换机，SSH 密码会变化')
  await expect(page.getByTestId('help-page')).toContainText('保留 6 小时')
})

test('C-ACC-03 我的页「邀请返利」入口与本月金额', async ({ page }, info) => {
  const inv = await inviterWithCode({ balance: 0 })
  const b = await inviteeOf(inv.code, { balance: 100_000 })
  await hooks.machines(1)
  await placeOrder(await userApi(b.phone))
  await hooks.agent() // 实例进入运行中（停在开通中会超时整单退款、返利作废）
  await hooks.sweep()
  await loginConsole(page, inv.phone)
  await nav(page, info, 'account')
  await expect(page.getByTestId('account-invite')).toBeVisible()
  await expect(page.getByTestId('account-invite')).toHaveAttribute('href', '/invite')
  await expect(page.getByTestId('account-invite-month')).toHaveCount(0) // 未发放
  await hooks.advance(7 * DAY)
  await hooks.sweep()
  await page.reload()
  await expect(page.getByTestId('account-invite-month')).toHaveText(`本月返利 ${yuan(690)}`)
})
