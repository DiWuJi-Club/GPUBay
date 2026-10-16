import { expect, test } from '@playwright/test'
import { loginConsole, nav, seedUser } from '../helpers/console'

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
