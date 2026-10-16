import { expect, test } from '@playwright/test'
import { loginConsole, nav, seedUser } from '../helpers/console'
import { hooks } from '../helpers/hooks'
import { uniquePhone } from '../helpers/phone'

test('C-AUTH-01 未登录访问受保护页重定向到登录，登录后回到原页', async ({ page }) => {
  const u = await seedUser()
  await page.goto('/instances')
  await expect(page).toHaveURL(/\/login\?redirect=(%2F|\/)instances$/)
  await hooks.advance(61)
  await page.getByTestId('login-phone').fill(u.phone)
  await page.getByTestId('login-send').click()
  // 从短信复制的「000 000」：输入框不截断，提交前去掉空白（Review Focus 1）
  await page.getByTestId('login-code').fill('000 000')
  await expect(page.getByTestId('login-code')).toHaveValue('000 000')
  await page.getByTestId('login-submit').click()
  await expect(page).toHaveURL(/\/instances$/)
})

test('C-AUTH-02 发送验证码：格式错误不发请求，合法号倒计时 60 秒', async ({ page }) => {
  await page.clock.install()
  let smsRequests = 0
  page.on('request', (r) => {
    if (r.url().includes('/api/v1/auth/sms-code')) smsRequests += 1
  })
  await page.goto('/login')
  await page.getByTestId('login-phone').fill('123')
  await page.getByTestId('login-send').click()
  await expect(page.getByTestId('login-phone-item')).toContainText('手机号格式不正确')
  expect(smsRequests).toBe(0)

  await hooks.advance(61)
  await page.getByTestId('login-phone').fill(uniquePhone())
  await page.getByTestId('login-send').click()
  await expect(page.getByTestId('login-send')).toBeDisabled()
  await expect(page.getByTestId('login-send')).toHaveText(/^(60|59) 秒后重发$/)
  expect(smsRequests).toBe(1)
  await page.clock.runFor(58_000)
  await expect(page.getByTestId('login-send')).toBeDisabled()
  await expect(page.getByTestId('login-send')).toHaveText(/^[12] 秒后重发$/)
  await page.clock.runFor(2_000)
  await expect(page.getByTestId('login-send')).toBeEnabled()
  await expect(page.getByTestId('login-send')).toHaveText('发送验证码')
})

test('C-AUTH-03 验证码错误停留在登录页', async ({ page }) => {
  await hooks.advance(61)
  await page.goto('/login')
  await page.getByTestId('login-phone').fill(uniquePhone())
  await page.getByTestId('login-send').click()
  await page.getByTestId('login-code').fill('111111')
  await page.getByTestId('login-submit').click()
  await expect(page.getByTestId('login-code-item')).toContainText('验证码错误或已过期')
  await expect(page).toHaveURL(/\/login$/)
})

test('C-AUTH-04 新手机号登录自动注册；刷新后会话保持', async ({ page }) => {
  const phone = uniquePhone()
  await hooks.advance(61)
  await page.goto('/login')
  await page.getByTestId('login-phone').fill(phone)
  await page.getByTestId('login-send').click()
  // 输入法打出的全角数字：转半角后提交（Review Focus 1）
  await page.getByTestId('login-code').fill('００００００')
  await page.getByTestId('login-submit').click()
  await expect(page).toHaveURL(/\/market$/)
  await expect(page.getByTestId('layout-phone')).toHaveText(`${phone.slice(0, 3)}****${phone.slice(7)}`)
  await page.reload()
  await expect(page.getByTestId('layout-phone')).toHaveText(`${phone.slice(0, 3)}****${phone.slice(7)}`)
})

test('C-AUTH-05 退出登录后回到登录页，再访问受保护页被重定向', async ({ page }, info) => {
  const u = await seedUser()
  await loginConsole(page, u.phone)
  await nav(page, info, 'account')
  await page.getByTestId('account-logout').click()
  await expect(page).toHaveURL(/\/login$/)
  await page.goto('/wallet')
  await expect(page).toHaveURL(/\/login\?redirect=(%2F|\/)wallet$/)
})
