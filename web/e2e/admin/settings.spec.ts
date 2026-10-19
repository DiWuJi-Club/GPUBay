import { expect, test, type Page } from '@playwright/test'
import { createAdmin, expectAdminUrl, loginAdmin, pauseClock, recordRequests } from '../helpers/admin'
import { settle, toast, toastLocator } from '../helpers/console'

type Reply = { status: number; body: Record<string, unknown> }
const totpRequired: Reply = { status: 403, body: { code: 'totp_required', message: '该操作需要输入动态码', request_id: 'req-totp' } }
const invalidTotp: Reply = { status: 400, body: { code: 'invalid_totp', message: '动态码错误或已使用过', request_id: 'req-bad' } }
const ok: Reply = { status: 200, body: { ok: true } }

/** 接管一个接口：按顺序给出 replies 里的响应（用完后再来的请求原样放行到后端）。 */
async function scripted(page: Page, path: string, replies: Reply[]) {
  const queue = [...replies]
  await page.route(
    (u) => u.pathname === `/admin/api/v1${path}`,
    (r) => {
      const next = queue.shift()
      return next ? r.fulfill({ status: next.status, contentType: 'application/json', body: JSON.stringify(next.body) }) : r.fallback()
    },
  )
}

async function fillPasswords(page: Page, old: string, next: string, confirm = next) {
  await page.getByTestId('settings-old').fill(old)
  await page.getByTestId('settings-new').fill(next)
  await page.getByTestId('settings-confirm').fill(confirm)
}

test('A-SET-01 修改密码：原密码错误提示；成功后用新密码登录', async ({ page }) => {
  await page.clock.install() // 并发二次验证一步用它触发角标刷新；pauseClock 之前时间照常流逝
  const admin = await createAdmin('ops')
  await loginAdmin(page, admin)
  await page.getByTestId('layout-settings').click()
  await expectAdminUrl(page, '/settings')
  await expect(page).toHaveTitle('个人设置 - Spark Cloud 管理后台')
  const changes = recordRequests(page, 'POST', '/me/password')
  const newPassword = 'new-e2e-password-7890'

  await test.step('前端校验：新密码不足 12 位、两次不一致，不发请求', async () => {
    await fillPasswords(page, 'not-the-old-password', 'short-pass1', 'short-pass2')
    await page.getByTestId('settings-submit').click()
    await expect(page.getByTestId('settings-new-error')).toHaveText('新密码至少 12 位')
    await expect(page.getByTestId('settings-confirm-error')).toHaveText('两次输入的新密码不一致')
    await fillPasswords(page, 'not-the-old-password', newPassword, newPassword + 'x')
    await page.getByTestId('settings-submit').click()
    await expect(page.getByTestId('settings-confirm-error')).toHaveText('两次输入的新密码不一致')
    await expect(page.getByTestId('settings-new-error')).toHaveCount(0)
    await settle(page)
    expect(changes).toHaveLength(0)
  })

  await page.getByTestId('settings-confirm').fill(newPassword)
  await page.getByTestId('settings-submit').click()
  await toast(page, '原密码不正确')
  await expect(page.getByTestId('settings-confirm-error')).toHaveCount(0)
  await settle(page)
  await expect(toastLocator(page, '原密码不正确')).toHaveCount(1)
  expect(changes).toHaveLength(1)

  await test.step('二次验证弹框：取消与 Esc 都让请求结束、不重发、不报错', async () => {
    await scripted(page, '/me/password', [totpRequired, totpRequired])
    for (const close of ['cancel', 'escape'] as const) {
      const before = changes.length
      await page.getByTestId('settings-submit').click()
      await expect(page.getByTestId('totp-dialog')).toBeVisible()
      await expect(page.getByTestId('totp-message')).toHaveCount(0)
      await expect(page.getByTestId('totp-ok')).toBeDisabled()
      await page.getByTestId('totp-input').fill('12345')
      await expect(page.getByTestId('totp-ok')).toBeDisabled() // 不足 6 位
      if (close === 'cancel') await page.getByTestId('totp-cancel').click()
      else await page.keyboard.press('Escape')
      await expect(page.getByTestId('totp-dialog')).toBeHidden()
      await expect(page.getByTestId('settings-submit')).toBeEnabled() // 请求已结束，不再 loading
      await settle(page)
      expect(changes.length - before).toBe(1)
      await expect(page.locator('.el-message').filter({ hasText: /已取消|动态码/ })).toHaveCount(0)
    }
    await page.unrouteAll()
  })

  await test.step('二次验证弹框：动态码错误时带提示再次弹出，输入正确后带 totp 重发原请求', async () => {
    await scripted(page, '/me/password', [totpRequired, invalidTotp, ok])
    const before = changes.length
    await page.getByTestId('settings-submit').click()
    await page.getByTestId('totp-input').fill('111111')
    await page.getByTestId('totp-ok').click()
    await expect(page.getByTestId('totp-message')).toHaveText('动态码错误或已使用过')
    await expect(page.getByTestId('totp-dialog')).toBeVisible()
    await expect(page.getByTestId('totp-input')).toHaveValue('')
    await page.getByTestId('totp-input').fill('222 222')
    await page.getByTestId('totp-ok').click()
    await toast(page, '密码已修改')
    await expect(page.getByTestId('totp-dialog')).toBeHidden()
    expect(changes.slice(before).map((c) => c.body)).toEqual([
      { old: 'not-the-old-password', new: newPassword },
      { old: 'not-the-old-password', new: newPassword, totp: '111111' },
      { old: 'not-the-old-password', new: newPassword, totp: '222222' },
    ])
    await page.unrouteAll()
  })

  await test.step('二次验证弹框：回车在不足 6 位时不提交，满 6 位时提交', async () => {
    await expect(page.locator('.el-message')).toHaveCount(0, { timeout: 15_000 })
    await fillPasswords(page, admin.password, newPassword)
    await scripted(page, '/me/password', [totpRequired, ok])
    const before = changes.length
    await page.getByTestId('settings-submit').click()
    await expect(page.getByTestId('totp-dialog')).toBeVisible()
    await page.getByTestId('totp-input').fill('12345')
    await page.getByTestId('totp-input').press('Enter')
    await settle(page)
    await expect(page.getByTestId('totp-dialog')).toBeVisible()
    await expect(page.getByTestId('totp-input')).toHaveValue('12345')
    expect(changes.length - before).toBe(1) // 没有带 totp 的重发
    await page.getByTestId('totp-input').fill('123456')
    await page.getByTestId('totp-input').press('Enter')
    await toast(page, '密码已修改')
    await expect(page.getByTestId('totp-dialog')).toBeHidden()
    expect(changes.slice(before).map((c) => c.body?.totp)).toEqual([undefined, '123456'])
    await page.unrouteAll()
  })

  await test.step('二次验证弹框是单例：两个请求同时要求动态码只弹一次，输入后都完成', async () => {
    await expect(page.locator('.el-message')).toHaveCount(0, { timeout: 15_000 })
    await fillPasswords(page, admin.password, newPassword)
    await scripted(page, '/me/password', [totpRequired, ok])
    await scripted(page, '/alerts', [totpRequired, { status: 200, body: { items: [], total: 3 } }])
    const badgeRequests = recordRequests(page, 'GET', '/alerts')
    const before = changes.length
    await pauseClock(page)
    await page.getByTestId('settings-submit').click()
    await expect(page.getByTestId('totp-dialog')).toBeVisible()
    await page.getByTestId('totp-input').fill('333')
    await page.clock.runFor(60_000) // 角标刷新也收到 totp_required
    await expect.poll(() => badgeRequests.length).toBe(1)
    await page.clock.resume()
    await settle(page)
    // 没有第二次弹框：再弹一次会清空输入（promptTotp 重置 code）
    await expect(page.getByTestId('totp-dialog')).toBeVisible()
    await expect(page.getByTestId('totp-input')).toHaveValue('333')
    await page.getByTestId('totp-input').fill('333333')
    await page.getByTestId('totp-ok').click()
    await toast(page, '密码已修改')
    await expect(page.getByTestId('menu-alerts-badge')).toHaveText('3')
    await expect(page.getByTestId('totp-dialog')).toBeHidden()
    await settle(page)
    await expect(page.getByTestId('totp-dialog')).toBeHidden() // 没有第二次弹出
    expect(changes.slice(before).map((c) => c.body?.totp)).toEqual([undefined, '333333'])
    expect(badgeRequests).toHaveLength(2) // 等第一个验证完成后原样重发一次
    await page.unrouteAll()
  })

  await expect(page.locator('.el-message')).toHaveCount(0, { timeout: 15_000 })
  await fillPasswords(page, admin.password, newPassword)
  const resp = page.waitForResponse((r) => r.url().endsWith('/admin/api/v1/me/password'))
  await page.getByTestId('settings-submit').click()
  expect((await resp).status()).toBe(200)
  await toast(page, '密码已修改')
  await expect(page.getByTestId('settings-old')).toHaveValue('') // 成功后清空表单
  await page.getByTestId('layout-logout').click()
  await expectAdminUrl(page, '/login')

  await test.step('旧密码不能再登录', async () => {
    await page.getByTestId('login-username').fill(admin.username)
    await page.getByTestId('login-password').fill(admin.password)
    await page.getByTestId('login-submit').click()
    await toast(page, '用户名或密码错误')
  })
  await loginAdmin(page, { ...admin, password: newPassword })
})
