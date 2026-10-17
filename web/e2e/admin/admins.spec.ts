import { expect, test, type APIRequestContext, type Browser, type Page } from '@playwright/test'
import {
  adminApi, clickTwiceSync, createAdmin, drainTasks, enterTotpDialog, expectAdminUrl, expectStillOpen, failOnce, getJSON, isPath, loginAdmin, logout, nextUsername, pick, recordRequests, type SeededAdmin, totpNow, trackToasts,
} from '../helpers/admin'
import { toast } from '../helpers/console'
import { ADMIN_URL } from '../helpers/hooks'

const USERNAME_RULE = '用户名需为 3～32 位小写字母、数字或 _ . -，并以字母开头'
const PASSWORD_RULE = '密码长度需为 12～128 个字符'
const LAST_SUPER = '不能停用或降级最后一个启用中的超级管理员'

const adminRow = (page: Page, username: string) => page.getByRole('row').filter({ has: page.getByTestId('adm-username').getByText(username, { exact: true }) })
const listAdmins = async (ctx: APIRequestContext) => (await getJSON<{ items: { username: string; role: string; status: string; totp_confirmed: boolean }[] }>(ctx, '/admin/api/v1/admins')).items

async function submitPassword(page: Page, username: string, password: string) {
  await page.goto('/admin/login')
  await page.getByTestId('login-username').fill(username)
  await page.getByTestId('login-password').fill(password)
  await page.getByTestId('login-submit').click()
}

/** 首次登录 / 重置后登录：出现绑定步骤，用页面给出的密钥完成绑定，拿到 8 个恢复码后进入后台。 */
async function bindOnLogin(page: Page, username: string, password: string) {
  await submitPassword(page, username, password)
  await expect(page.getByTestId('login-setup-qr')).toBeVisible()
  await expect(page.getByTestId('login-totp-code')).toHaveCount(0)
  const secret = (await page.getByTestId('login-setup-secret').textContent())!.trim()
  await page.getByTestId('login-setup-code').fill(await totpNow(secret))
  await page.getByTestId('login-setup-submit').click()
  await expect(page.getByTestId('login-recovery-code')).toHaveCount(8)
  await page.getByTestId('login-recovery-saved').click()
  await page.getByTestId('login-enter').click()
  await expect(page.getByTestId('layout-username')).toHaveText(username)
}

/** 目标管理员在另一个浏览器上下文里保持登录（用来观察角色 / 停用 / 重置对其现有会话的影响）。 */
async function otherSession(browser: Browser, admin: SeededAdmin) {
  const ctx = await browser.newContext({ baseURL: ADMIN_URL, viewport: { width: 1440, height: 900 }, locale: 'zh-CN', timezoneId: 'Asia/Shanghai' })
  const page = await ctx.newPage()
  await loginAdmin(page, admin)
  return { page, close: () => ctx.close() }
}

test('A-ADM-01 新增管理员：本地校验、动态码后原请求带 totp 只重发一次、重复用户名只提示一次并保留输入、新管理员首次登录绑定动态码；首次加载失败可重试', async ({ page }) => {
  const root = await createAdmin('super')
  const ctx = await adminApi(root)
  const toasts = await trackToasts(page)
  const gets = recordRequests(page, 'GET', '/admins')
  const posts = recordRequests(page, 'POST', '/admins')
  const username = nextUsername()
  const password = 'initial-password-123456'
  const body = { username, password, role: 'ops' }
  await loginAdmin(page, root)

  await test.step('首次加载失败：页内错误 + 重试，不自动重试', async () => {
    await failOnce(page, '/admins', 'req-adm-1')
    await page.getByTestId('menu-admins').click()
    await expectAdminUrl(page, '/admins')
    await expect(page.getByTestId('admins-error')).toContainText('服务器内部错误（请求号 req-adm-1）')
    await expect(page.getByTestId('adm-username')).toHaveCount(0)
    await drainTasks(page)
    expect(gets).toHaveLength(1)
    expect(await toasts()).toEqual(['服务器内部错误（请求号 req-adm-1）'])
    await page.getByTestId('admins-retry').click()
    await expect(page.getByTestId('admins-error')).toHaveCount(0)
    await expect(adminRow(page, root.username).getByTestId('adm-role')).toHaveText('超级管理员')
    await expect(adminRow(page, root.username).getByTestId('adm-totp')).toHaveText('已绑定')
    await expect(adminRow(page, root.username).getByTestId('adm-last-ip')).toHaveText('127.0.0.1')
    await expect(adminRow(page, root.username).getByTestId('adm-last-login')).toHaveText(/^\d{4}-\d{2}-\d{2} \d{2}:\d{2}$/)
    expect(gets).toHaveLength(2)
  })

  await test.step('用户名 / 密码不合规：页内提示，不发请求', async () => {
    await page.getByTestId('admins-create-btn').click()
    for (const bad of ['', 'ab', 'Abc_1', '1abc', 'abc def', `a${'b'.repeat(32)}`]) {
      await page.getByTestId('adm-create-username').fill(bad)
      await page.getByTestId('adm-create-submit').click()
      await expect(page.getByTestId('adm-create-error')).toHaveText(USERNAME_RULE)
    }
    await page.getByTestId('adm-create-username').fill(username)
    for (const bad of ['', 'short-pass1', 'x'.repeat(129)]) {
      await page.getByTestId('adm-create-password').fill(bad)
      await page.getByTestId('adm-create-submit').click()
      await expect(page.getByTestId('adm-create-error')).toHaveText(PASSWORD_RULE)
    }
    await drainTasks(page)
    expect(posts).toHaveLength(0)
  })

  await test.step('取消动态码：只发出不带 totp 的一次，无提示，弹窗与输入保留，未创建', async () => {
    await page.getByTestId('adm-create-password').fill(password)
    await pick(page, 'adm-create-role', '运维')
    const before = (await toasts()).length
    await page.getByTestId('adm-create-submit').click()
    await expect(page.getByTestId('adm-create-error')).toHaveCount(0)
    await expect(page.getByTestId('totp-dialog')).toBeVisible()
    await page.getByTestId('totp-cancel').click()
    await expect(page.getByTestId('totp-dialog')).toBeHidden()
    await expect(page.getByTestId('adm-create-dialog')).toBeVisible()
    await expect(page.getByTestId('adm-create-username')).toHaveValue(username)
    await expect(page.getByTestId('adm-create-password')).toHaveValue(password)
    await drainTasks(page)
    expect(posts.map((p) => p.body)).toEqual([body])
    expect((await toasts()).slice(before)).toEqual([])
    expect((await listAdmins(ctx)).some((a) => a.username === username)).toBe(false)
  })

  await test.step('连点创建：弹动态码，原请求带 totp 只重发一次；列表显示运维、正常、未绑定', async () => {
    await clickTwiceSync(page.getByTestId('adm-create-submit'))
    await enterTotpDialog(page, root.totp_secret!)
    await toast(page, '管理员已创建，首次登录时绑定动态码')
    await expect(page.getByTestId('adm-create-dialog')).toBeHidden()
    await drainTasks(page)
    expect(posts.slice(1).map((p) => p.body)).toEqual([body, { ...body, totp: expect.stringMatching(/^\d{6}$/) }])
    const row = adminRow(page, username)
    await expect(row.getByTestId('adm-role')).toHaveText('运维')
    await expect(row.getByTestId('adm-status')).toHaveText('正常')
    await expect(row.getByTestId('adm-totp')).toHaveText('未绑定')
    await expect(row.getByTestId('adm-last-login')).toHaveText('—')
    await expect(row.getByTestId('adm-last-ip')).toHaveText('—')
  })

  await test.step('重复用户名（5 分钟内免动态码）：只提示一次，弹窗与输入保留', async () => {
    await page.getByTestId('admins-create-btn').click()
    await expect(page.getByTestId('adm-create-username')).toHaveValue('')
    await expect(page.getByTestId('adm-create-password')).toHaveValue('') // 创建成功后初始密码已清空
    await page.getByTestId('adm-create-username').fill(username)
    await page.getByTestId('adm-create-password').fill(password)
    const before = (await toasts()).length
    await page.getByTestId('adm-create-submit').click()
    await toast(page, '用户名已存在')
    await expectStillOpen(page, 'adm-create-dialog', '用户名已存在')
    await expect(page.getByTestId('adm-create-username')).toHaveValue(username)
    expect((await toasts()).slice(before)).toEqual(['用户名已存在'])
    expect(posts).toHaveLength(4)
    await expect(page.getByTestId('totp-dialog')).toBeHidden()
    await page.keyboard.press('Escape')
    await expect(page.getByTestId('adm-create-dialog')).toBeHidden()
    await page.getByTestId('admins-create-btn').click()
    await expect(page.getByTestId('adm-create-password')).toHaveValue('') // 关闭弹窗即清空初始密码
    await page.keyboard.press('Escape')
    await expect(page.getByTestId('adm-create-dialog')).toBeHidden()
  })

  await test.step('新管理员首次登录必须绑定动态码，绑定后按运维角色进入', async () => {
    await page.getByTestId('layout-logout').click()
    await bindOnLogin(page, username, password)
    await expect(page.getByTestId('layout-role')).toHaveText('运维')
    await expect(page.getByTestId('menu-machines')).toBeVisible()
    await expect(page.getByTestId('menu-admins')).toHaveCount(0)
    expect((await listAdmins(ctx)).find((a) => a.username === username)?.totp_confirmed).toBe(true)
  })
})

test('A-ADM-02 修改角色后对方下次加载菜单变化；停用后对方会话 401、无法登录；最后一个超管的保护提示；运维 / 财务无菜单且 403', async ({ page, browser }) => {
  await test.step('运维、财务：没有「管理员」菜单，直接访问显示 403', async () => {
    for (const role of ['ops', 'finance'] as const) {
      await loginAdmin(page, await createAdmin(role))
      await expect(page.getByTestId('menu-audit')).toBeVisible()
      await expect(page.getByTestId('menu-admins')).toHaveCount(0)
      await page.goto('/admin/admins')
      await expect(page.getByTestId('forbidden-page')).toBeVisible()
      await expectAdminUrl(page, '/403', { from: '/admins' })
      await logout(page)
    }
  })

  const root = await createAdmin('super')
  const target = await createAdmin('ops')
  const other = await otherSession(browser, target)
  await expect(other.page.getByTestId('menu-machines')).toBeVisible()
  await expect(other.page.getByTestId('menu-orders')).toHaveCount(0)
  const toasts = await trackToasts(page)
  const puts = recordRequests(page, 'PUT', `/admins/${target.id}`)
  await loginAdmin(page, root)
  await page.getByTestId('menu-admins').click()
  const row = adminRow(page, target.username)
  await expect(row.getByTestId('adm-role')).toHaveText('运维')

  await test.step('改为财务：连点保存，动态码后原请求带 totp 只重发一次；对方刷新后菜单变为财务', async () => {
    await row.getByTestId('adm-edit').click()
    await pick(page, 'adm-form-role', '财务')
    await expect(page.getByTestId('adm-self-warning')).toHaveCount(0) // 不是自己：没有额外警告
    await clickTwiceSync(page.getByTestId('adm-form-submit'))
    await enterTotpDialog(page, root.totp_secret!)
    await toast(page, '已保存')
    await expect(page.getByTestId('adm-edit-dialog')).toBeHidden()
    await expect(row.getByTestId('adm-role')).toHaveText('财务')
    await drainTasks(page)
    expect(puts.map((p) => p.body)).toEqual([
      { role: 'finance', status: 'active' },
      { role: 'finance', status: 'active', totp: expect.stringMatching(/^\d{6}$/) },
    ])
    await other.page.reload()
    await expect(other.page.getByTestId('layout-role')).toHaveText('财务')
    await expect(other.page.getByTestId('menu-orders')).toBeVisible()
    await expect(other.page.getByTestId('menu-machines')).toHaveCount(0)
  })

  await test.step('后端拒绝（最后一个启用中的超级管理员）：只提示一次，弹窗与所选值保留', async () => {
    const isSelf = isPath(`/admins/${root.id}`)
    await page.route(isSelf, (r) => r.fulfill({ status: 409, contentType: 'application/json', body: JSON.stringify({ code: 'last_super', message: LAST_SUPER }) }), { times: 1 })
    const selfPuts = recordRequests(page, 'PUT', `/admins/${root.id}`)
    await adminRow(page, root.username).getByTestId('adm-edit').click()
    await expect(page.getByTestId('adm-edit-dialog')).toBeVisible()
    await expect(page.getByTestId('adm-self-warning')).toHaveCount(0) // 未降级 / 停用时不提示
    await pick(page, 'adm-form-status', '已停用')
    await expect(page.getByTestId('adm-self-warning')).toHaveText('这是你自己的账号：保存后你将立即失去超级管理员权限或被强制下线。')
    await pick(page, 'adm-form-status', '正常')
    await pick(page, 'adm-form-role', '运维')
    await expect(page.getByTestId('adm-self-warning')).toBeVisible()
    await pick(page, 'adm-form-role', '超级管理员')
    await expect(page.getByTestId('adm-self-warning')).toHaveCount(0)
    await pick(page, 'adm-form-status', '已停用')
    const before = (await toasts()).length
    await page.getByTestId('adm-form-submit').click()
    await toast(page, LAST_SUPER)
    await expectStillOpen(page, 'adm-edit-dialog', LAST_SUPER)
    await expect(page.getByTestId('adm-form-status')).toContainText('已停用')
    expect((await toasts()).slice(before)).toEqual([LAST_SUPER])
    expect(selfPuts.map((p) => p.body)).toEqual([{ role: 'super', status: 'disabled' }])
    await page.keyboard.press('Escape')
    await expect(page.getByTestId('adm-edit-dialog')).toBeHidden()
    await expect(adminRow(page, root.username).getByTestId('adm-status')).toHaveText('正常')
  })

  await test.step('停用（5 分钟内免动态码）：对方下一次请求 401 回登录页，之后无法登录', async () => {
    await row.getByTestId('adm-edit').click()
    await pick(page, 'adm-form-status', '已停用')
    await page.getByTestId('adm-form-submit').click()
    await toast(page, '已保存')
    await expect(page.getByTestId('totp-dialog')).toBeHidden()
    await expect(row.getByTestId('adm-status')).toHaveText('已停用')
    await drainTasks(page)
    expect(puts.slice(2).map((p) => p.body)).toEqual([{ role: 'finance', status: 'disabled' }])

    const orders = other.page.waitForResponse((r) => new URL(r.url()).pathname === '/admin/api/v1/orders')
    await other.page.getByTestId('menu-orders').click()
    expect((await orders).status()).toBe(401)
    await toast(other.page, '登录已失效，请重新登录')
    await expectAdminUrl(other.page, '/login', { redirect: '/orders' })
    await other.page.getByTestId('login-username').fill(target.username)
    await other.page.getByTestId('login-password').fill(target.password)
    await other.page.getByTestId('login-submit').click()
    await toast(other.page, '用户名或密码错误')
    await expect(other.page.getByTestId('login-totp-code')).toHaveCount(0)
  })
  await other.close()
})

test('A-ADM-03 重置他人动态码：确认文案、取消不发请求、失败只提示一次、动态码后原请求带 totp 只重发一次；对方被下线并在下次登录重新绑定', async ({ page, browser }) => {
  const root = await createAdmin('super')
  const target = await createAdmin('ops')
  const other = await otherSession(browser, target)
  const toasts = await trackToasts(page)
  const posts = recordRequests(page, 'POST', `/admins/${target.id}/reset-totp`)
  await loginAdmin(page, root)
  await page.getByTestId('menu-admins').click()
  const row = adminRow(page, target.username)
  await expect(row.getByTestId('adm-totp')).toHaveText('已绑定')

  await test.step('确认框文案；取消不发请求', async () => {
    await row.getByTestId('adm-reset-totp').click()
    await expect(page.getByTestId('adm-reset-text')).toHaveText('该管理员将被强制下线并在下次登录时重新绑定。')
    await expect(page.getByTestId('adm-self-warning')).toHaveCount(0) // 不是自己：没有额外警告
    await page.getByTestId('adm-reset-cancel').click()
    await expect(page.getByTestId('adm-reset-dialog')).toBeHidden()
    await drainTasks(page)
    expect(posts).toHaveLength(0)
  })

  await test.step('重置自己的动态码：确认框额外警告会被立即下线；取消不发请求', async () => {
    const selfPosts = recordRequests(page, 'POST', `/admins/${root.id}/reset-totp`)
    await adminRow(page, root.username).getByTestId('adm-reset-totp').click()
    await expect(page.getByTestId('adm-self-warning')).toHaveText('这是你自己的账号：重置后你将立即被下线，需要重新登录并绑定动态码。')
    await page.getByTestId('adm-reset-cancel').click()
    await expect(page.getByTestId('adm-reset-dialog')).toBeHidden()
    await drainTasks(page)
    expect(selfPosts).toHaveLength(0)
  })

  await test.step('失败（500）：只提示一次，确认框保留，对方仍已绑定', async () => {
    await failOnce(page, `/admins/${target.id}/reset-totp`, 'req-reset-1')
    await row.getByTestId('adm-reset-totp').click()
    await page.getByTestId('adm-reset-ok').click()
    await toast(page, '服务器内部错误（请求号 req-reset-1）')
    await expectStillOpen(page, 'adm-reset-dialog', '服务器内部错误（请求号 req-reset-1）')
    expect(await toasts()).toEqual(['服务器内部错误（请求号 req-reset-1）'])
    expect(posts.map((p) => p.body)).toEqual([{}])
    await expect(row.getByTestId('adm-totp')).toHaveText('已绑定')
  })

  await test.step('连点确定：动态码后原请求带 totp 只重发一次；列表变为未绑定', async () => {
    await clickTwiceSync(page.getByTestId('adm-reset-ok'))
    await enterTotpDialog(page, root.totp_secret!)
    await toast(page, '已重置动态码')
    await expect(page.getByTestId('adm-reset-dialog')).toBeHidden()
    await expect(row.getByTestId('adm-totp')).toHaveText('未绑定')
    await drainTasks(page)
    expect(posts.slice(1).map((p) => p.body)).toEqual([{}, { totp: expect.stringMatching(/^\d{6}$/) }])
  })

  await test.step('对方会话已失效；再登录时直接进入绑定步骤（不再要动态码），重新绑定后进入', async () => {
    await other.page.reload()
    await expect(other.page.getByTestId('login-username')).toBeVisible()
    await expect(other.page.getByTestId('layout-username')).toHaveCount(0)
    await bindOnLogin(other.page, target.username, target.password)
    await expect(other.page.getByTestId('layout-role')).toHaveText('运维')
  })
  await other.close()
})
