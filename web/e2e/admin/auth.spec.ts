import { expect, test, type Page } from '@playwright/test'
import { clickTwiceSync, createAdmin, drainTasks, expectAdminUrl, loginAdmin, pauseClock, recordRequests, totpNow } from '../helpers/admin'
import { settle, toast, toastLocator } from '../helpers/console'
import { hooks } from '../helpers/hooks'
import { totp } from '../helpers/totp'

async function submitPassword(page: Page, username: string, password: string) {
  await page.getByTestId('login-username').fill(username)
  await page.getByTestId('login-password').fill(password)
  await page.getByTestId('login-submit').click()
}

/** 浏览器存储的全部内容（localStorage 与 sessionStorage 的键和值拼在一起），用于断言密钥 / 恢复码没有落盘。 */
const storageDump = (page: Page) => page.evaluate(() => JSON.stringify([Object.entries(localStorage), Object.entries(sessionStorage)]))

test('A-AUTH-01 首次登录绑定动态码：二维码、密钥、恢复码、勾选后进入', async ({ page }) => {
  const admin = await createAdmin('super', false)
  const setupRequests = recordRequests(page, 'POST', '/auth/totp/setup')
  // 剪贴板开关：window.__clipboardDenied 为 true 时 writeText 失败（模拟权限被拒）
  await page.addInitScript(() => {
    const clip = navigator.clipboard
    const write = clip.writeText.bind(clip)
    clip.writeText = (t: string) => ((window as unknown as { __clipboardDenied?: boolean }).__clipboardDenied ? Promise.reject(new DOMException('denied', 'NotAllowedError')) : write(t))
  })
  const denyClipboard = (v: boolean) => page.evaluate((d) => ((window as unknown as { __clipboardDenied?: boolean }).__clipboardDenied = d), v)
  await page.goto('/admin/login?redirect=/users')
  await submitPassword(page, admin.username, admin.password)
  await expect(page.getByTestId('login-setup-qr')).toBeVisible()
  // 二维码真的画出来了：canvas 里有深色像素
  const darkPixels = await page.getByTestId('login-setup-qr').evaluate((c: HTMLCanvasElement) => {
    const d = c.getContext('2d')!.getImageData(0, 0, c.width, c.height).data
    let n = 0
    for (let i = 0; i < d.length; i += 4) if (d[i] < 128) n++
    return n
  })
  expect(darkPixels).toBeGreaterThan(1000)
  const secret = (await page.getByTestId('login-setup-secret').textContent())!.trim()
  expect(secret).toMatch(/^[A-Z2-7]{16,}$/)
  await page.getByTestId('login-setup-copy-secret').click()
  await toast(page, '密钥已复制')
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(secret)

  await test.step('剪贴板被拒：提示手动复制，不报「已复制」', async () => {
    await expect(page.locator('.el-message')).toHaveCount(0, { timeout: 15_000 })
    await denyClipboard(true)
    await page.getByTestId('login-setup-copy-secret').click()
    await toast(page, '复制失败，请手动选中复制')
    await expect(toastLocator(page, '密钥已复制')).toHaveCount(0)
    await denyClipboard(false)
  })

  await test.step('绑定时动态码错误：一条提示、只发一次请求，停留在绑定步骤', async () => {
    await page.getByTestId('login-setup-code').fill('000000')
    await page.getByTestId('login-setup-submit').click()
    await toast(page, '动态码错误或已使用过')
    await settle(page)
    await expect(toastLocator(page, '动态码错误或已使用过')).toHaveCount(1)
    expect(setupRequests).toHaveLength(1)
    await expect(page.getByTestId('login-setup-qr')).toBeVisible()
  })

  await test.step('绑定按钮极快连点两次只发一次请求', async () => {
    await page.getByTestId('login-setup-code').fill(await totpNow(secret))
    await clickTwiceSync(page.getByTestId('login-setup-submit'))
    await expect(page.getByTestId('login-recovery-code')).toHaveCount(8)
    await settle(page)
    expect(setupRequests).toHaveLength(2) // 第 1 次是上一步的错误码，这里只多了 1 次
  })
  const codes = page.getByTestId('login-recovery-code')
  await expect(codes).toHaveCount(8)
  const texts = (await codes.allTextContents()).map((t) => t.trim())
  for (const c of texts) expect(c).toMatch(/^[a-z0-9]{5}-[a-z0-9]{5}$/)
  expect(new Set(texts).size).toBe(8)
  await page.getByTestId('login-recovery-copy').click()
  await toast(page, '恢复码已复制')
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(texts.join('\n'))

  await test.step('剪贴板被拒时复制恢复码：提示手动复制', async () => {
    await expect(page.locator('.el-message')).toHaveCount(0, { timeout: 15_000 })
    await denyClipboard(true)
    await page.getByTestId('login-recovery-copy').click()
    await toast(page, '复制失败，请手动选中复制')
    await expect(toastLocator(page, '恢复码已复制')).toHaveCount(0)
    await denyClipboard(false)
  })

  await test.step('密钥与恢复码不写入浏览器存储、不出现在地址里', async () => {
    const dump = await storageDump(page)
    expect(dump).not.toContain(secret)
    for (const c of texts) expect(dump).not.toContain(c)
    expect(page.url()).not.toContain(secret)
    for (const c of texts) expect(page.url()).not.toContain(c)
    await expectAdminUrl(page, '/login', { redirect: '/users' })
  })

  await test.step('未勾选「已保存」时进入按钮禁用；取消勾选后再次禁用', async () => {
    await expect(page.getByTestId('login-enter')).toBeDisabled()
    await page.getByTestId('login-recovery-saved').click()
    await expect(page.getByTestId('login-enter')).toBeEnabled()
    await page.getByTestId('login-recovery-saved').click()
    await expect(page.getByTestId('login-enter')).toBeDisabled()
    await page.getByTestId('login-recovery-saved').click()
    await expect(page.getByTestId('login-enter')).toBeEnabled()
  })
  await page.getByTestId('login-enter').click()
  await expectAdminUrl(page, '/users') // 「进入后台」遵循 ?redirect
  await expect(page.getByTestId('layout-username')).toHaveText(admin.username)
  await page.reload()
  await expect(page.getByTestId('layout-username')).toHaveText(admin.username)
  await expect(page.getByTestId('login-recovery-codes')).toHaveCount(0)
  const dump = await storageDump(page)
  expect(dump).not.toContain(secret)
  for (const c of texts) expect(dump).not.toContain(c)

  await test.step('任一请求返回 totp_setup_required：清会话、提示一次并回登录页', async () => {
    await page.route(
      (u) => u.pathname === '/admin/api/v1/alerts',
      (r) => r.fulfill({ status: 403, contentType: 'application/json', body: JSON.stringify({ code: 'totp_setup_required', message: '请先绑定动态码', request_id: 'req-setup' }) }),
    )
    await page.reload()
    await expectAdminUrl(page, '/login')
    await toast(page, '请先绑定动态码：重新输入密码后按提示绑定')
    await settle(page)
    await expect(page.locator('.el-message')).toHaveCount(1)
    await expect(page.getByTestId('layout-side')).toHaveCount(0)
    await page.unrouteAll()
  })
})

test('A-AUTH-02 动态码登录，刷新仍登录', async ({ page }) => {
  const admin = await createAdmin('ops')
  const logins = recordRequests(page, 'POST', '/auth/login')
  const totps = recordRequests(page, 'POST', '/auth/totp')
  await page.goto('/admin/login')
  await page.getByTestId('login-username').fill(admin.username)
  await page.getByTestId('login-password').fill(admin.password)
  await clickTwiceSync(page.getByTestId('login-submit')) // 极快连点：只发一次
  await expect(page.getByTestId('login-totp-code')).toBeVisible()
  await settle(page)
  expect(logins).toHaveLength(1)
  await expect(page.getByTestId('login-setup-qr')).toHaveCount(0) // 已绑定：不再出现绑定步骤
  // 从验证器复制的「123 456」带空格：提交前去掉空白
  const code = await totpNow(admin.totp_secret!)
  await page.getByTestId('login-totp-code').fill(`${code.slice(0, 3)} ${code.slice(3)}`)
  await clickTwiceSync(page.getByTestId('login-totp-submit')) // 连点不会把同一个动态码提交两次（第二次必然「已使用过」）
  await expectAdminUrl(page, '/')
  await expect(page.getByTestId('layout-username')).toHaveText(admin.username)
  expect(totps).toHaveLength(1)
  expect(totps[0].body).toEqual({ code })
  await expect(toastLocator(page, '动态码错误或已使用过')).toHaveCount(0)
  await expect(page).toHaveTitle('仪表盘 - Spark Cloud 管理后台')
  await page.reload()
  await expect(page.getByTestId('layout-username')).toHaveText(admin.username)
  await expectAdminUrl(page, '/')

  await test.step('已登录再打开登录页直接回仪表盘；带站内 redirect 时去该页', async () => {
    await page.goto('/admin/login')
    await expectAdminUrl(page, '/')
    await expect(page.getByTestId('layout-username')).toHaveText(admin.username)
    await page.goto('/admin/login?redirect=/audit')
    await expectAdminUrl(page, '/audit')
    await expect(page).toHaveTitle('审计日志 - Spark Cloud 管理后台')
  })
})

test('A-AUTH-03 恢复码登录成功；同一恢复码再次使用失败', async ({ page }) => {
  const admin = await createAdmin('ops')
  const code = admin.recovery_codes![0]
  const recoveries = recordRequests(page, 'POST', '/auth/totp')
  const loginWithRecovery = async () => {
    await page.goto('/admin/login')
    await submitPassword(page, admin.username, admin.password)
    await page.getByTestId('login-use-recovery').click()
    await expect(page.getByTestId('login-totp-code')).toHaveCount(0)
    await page.getByTestId('login-recovery-input').fill(code)
    await clickTwiceSync(page.getByTestId('login-recovery-submit')) // 极快连点：只发一次
  }
  await loginWithRecovery()
  await expectAdminUrl(page, '/')
  expect(recoveries).toHaveLength(1)
  expect(recoveries[0].body).toEqual({ recovery_code: code })
  await expect(page.getByTestId('layout-username')).toHaveText(admin.username)
  await page.getByTestId('layout-logout').click()
  await expectAdminUrl(page, '/login')
  await loginWithRecovery()
  await toast(page, '动态码错误或已使用过')
  await expectAdminUrl(page, '/login')
  await expect(page.getByTestId('layout-username')).toHaveCount(0)

  await test.step('改用动态码可切回', async () => {
    await page.getByTestId('login-use-totp').click()
    await expect(page.getByTestId('login-recovery-input')).toHaveCount(0)
    await page.getByTestId('login-totp-code').fill(await totpNow(admin.totp_secret!))
    await page.getByTestId('login-totp-submit').click()
    await expectAdminUrl(page, '/')
    await expect(page.getByTestId('layout-username')).toHaveText(admin.username)
  })
})

test('A-AUTH-04 密码错误与动态码错误提示', async ({ page }) => {
  const admin = await createAdmin('finance')
  const logins = recordRequests(page, 'POST', '/auth/login')
  const totps = recordRequests(page, 'POST', '/auth/totp')
  await page.goto('/admin/login')

  await test.step('服务器 500：一条带请求号的提示、不重试，停留在步骤一', async () => {
    await page.route('**/admin/api/v1/auth/login', (r) =>
      r.fulfill({ status: 500, contentType: 'application/json', body: JSON.stringify({ code: 'internal', message: '服务器内部错误', request_id: 'req-e2e-500' }) }),
    )
    await submitPassword(page, admin.username, admin.password)
    await toast(page, '服务器内部错误（请求号 req-e2e-500）')
    await settle(page)
    await expect(toastLocator(page, '服务器内部错误')).toHaveCount(1)
    expect(logins).toHaveLength(1)
    await expect(page.getByTestId('login-submit')).toBeEnabled()
    await expect(page.getByTestId('login-totp-code')).toHaveCount(0)
    await page.unroute('**/admin/api/v1/auth/login')
  })

  await page.getByTestId('login-password').fill('definitely-wrong-password')
  await page.getByTestId('login-submit').click()
  await toast(page, '用户名或密码错误')
  await settle(page)
  await expect(toastLocator(page, '用户名或密码错误')).toHaveCount(1)
  expect(logins).toHaveLength(2)
  await expect(page.getByTestId('login-totp-code')).toHaveCount(0)

  await page.getByTestId('login-password').fill(admin.password)
  await page.getByTestId('login-submit').click()
  await page.getByTestId('login-totp-code').fill('000000')
  await page.getByTestId('login-totp-submit').click()
  await toast(page, '动态码错误或已使用过')
  await settle(page)
  await expect(toastLocator(page, '动态码错误或已使用过')).toHaveCount(1)
  expect(totps).toHaveLength(1)
  await expect(page.getByTestId('login-totp-hint')).toContainText('如刚用过该动态码，请等待下一个')
  await expect(page.getByTestId('login-totp-code')).toBeVisible() // 停留在动态码步骤
  await expectAdminUrl(page, '/login')
})

test('A-AUTH-05 连续 5 次失败后锁定', async ({ page }) => {
  const admin = await createAdmin('ops')
  await page.goto('/admin/login')
  for (let i = 0; i < 5; i++) {
    const resp = page.waitForResponse((r) => r.url().endsWith('/admin/api/v1/auth/login'))
    await submitPassword(page, admin.username, `wrong-password-${i}`)
    expect((await resp).status()).toBe(401)
    await toast(page, '用户名或密码错误')
  }
  const resp = page.waitForResponse((r) => r.url().endsWith('/admin/api/v1/auth/login'))
  await submitPassword(page, admin.username, admin.password)
  expect((await resp).status()).toBe(423)
  await toast(page, '登录失败次数过多，请 15 分钟后再试')
  await expect(page.getByTestId('login-totp-code')).toHaveCount(0)
})

test('A-AUTH-06 未登录访问受保护页与退出后再访问都回登录页', async ({ page }) => {
  const admin = await createAdmin('super')
  await page.goto('/admin/machines')
  await expectAdminUrl(page, '/login', { redirect: '/machines' })
  await expect(page.getByTestId('layout-side')).toHaveCount(0) // 未登录不渲染后台布局
  await submitPassword(page, admin.username, admin.password)
  await page.getByTestId('login-totp-code').fill(await totpNow(admin.totp_secret!))
  await page.getByTestId('login-totp-submit').click()
  await expectAdminUrl(page, '/machines')
  await expect(page).toHaveTitle('机器 - Spark Cloud 管理后台')
  await expect(page.getByTestId('layout-username')).toHaveText(admin.username)

  const logouts = recordRequests(page, 'POST', '/auth/logout')
  await test.step('退出请求失败（500）：保持登录并提示重试，不假装已退出', async () => {
    await page.route('**/admin/api/v1/auth/logout', (r) =>
      r.fulfill({ status: 500, contentType: 'application/json', body: JSON.stringify({ code: 'internal', message: '服务器内部错误', request_id: 'req-logout' }) }),
    )
    await page.getByTestId('layout-logout').click()
    await toast(page, '退出失败：服务器内部错误（请求号 req-logout）。服务器上的登录可能仍然有效，请重试退出；如仍失败请关闭浏览器')
    await settle(page)
    expect(logouts).toHaveLength(1)
    await expectAdminUrl(page, '/machines')
    await expect(page.getByTestId('layout-username')).toHaveText(admin.username)
    await expect(page.getByTestId('layout-logout')).toBeEnabled() // 可以再点一次重试
    expect((await page.request.get('/admin/api/v1/me')).status()).toBe(200) // 服务器会话确实还在：界面如实显示已登录
    await page.unroute('**/admin/api/v1/auth/logout')
  })

  await page.getByTestId('layout-logout').click()
  await expectAdminUrl(page, '/login')
  expect(logouts).toHaveLength(2)
  await page.goto('/admin/machines')
  await expectAdminUrl(page, '/login', { redirect: '/machines' })
  // 后端会话确实已注销：直接请求 /me 得到 401
  expect((await page.request.get('/admin/api/v1/me')).status()).toBe(401)

  await test.step('redirect 指向站外地址时忽略，登录后回仪表盘', async () => {
    await page.goto('/admin/login?redirect=//evil.example.com')
    await submitPassword(page, admin.username, admin.password)
    await page.getByTestId('login-totp-code').fill(await totpNow(admin.totp_secret!))
    await page.getByTestId('login-totp-submit').click()
    await expectAdminUrl(page, '/')
  })
})

test('A-AUTH-07 临时会话超时：6 分钟后提交动态码回到步骤一', async ({ page }) => {
  const admin = await createAdmin('ops')
  await page.goto('/admin/login?redirect=/users')
  await submitPassword(page, admin.username, admin.password)
  await expect(page.getByTestId('login-totp-code')).toBeVisible()
  const now = await hooks.advance(360)
  await page.getByTestId('login-totp-code').fill(totp(admin.totp_secret!, Math.floor(now.getTime() / 1000)))
  await page.getByTestId('login-totp-submit').click()
  await expect(page.getByTestId('login-timeout')).toHaveText('登录已超时，请重新输入密码')
  await expect(page.getByTestId('login-password')).toBeVisible()
  await expect(page.getByTestId('login-password')).toHaveValue('') // 密码已清空，需重新输入
  await expect(page.getByTestId('login-totp-code')).toHaveCount(0)
  await expect(toastLocator(page, '登录已失效')).toHaveCount(0) // 登录页的 401 不走「登录已失效」
  await expectAdminUrl(page, '/login', { redirect: '/users' })

  await test.step('重新输入密码与动态码后进入，提示消失并回到 redirect', async () => {
    await page.getByTestId('login-password').fill(admin.password)
    await page.getByTestId('login-submit').click()
    await expect(page.getByTestId('login-timeout')).toHaveCount(0)
    await page.getByTestId('login-totp-code').fill(await totpNow(admin.totp_secret!))
    await page.getByTestId('login-totp-submit').click()
    await expectAdminUrl(page, '/users')
  })
})

test('A-AUTH-08 会话过期：推进 13 小时后任意请求跳登录', async ({ page }) => {
  const admin = await createAdmin('ops')
  await page.clock.install() // 控制角标 60 秒刷新；pauseClock 之前时间照常流逝
  await loginAdmin(page, admin)

  await test.step('页面请求 401：一条「登录已失效」提示，带 redirect 回登录页', async () => {
    const changes = recordRequests(page, 'POST', '/me/password')
    await page.getByTestId('layout-settings').click()
    await expectAdminUrl(page, '/settings')
    await hooks.advance(46800)
    await page.getByTestId('settings-old').fill(admin.password)
    await page.getByTestId('settings-new').fill('expired-session-pass-1')
    await page.getByTestId('settings-confirm').fill('expired-session-pass-1')
    await page.getByTestId('settings-submit').click()
    await expectAdminUrl(page, '/login', { redirect: '/settings' })
    await toast(page, '登录已失效，请重新登录')
    await settle(page)
    await expect(page.locator('.el-message')).toHaveCount(1) // 只有这一条：页面自身的错误提示静默
    expect(changes).toHaveLength(1)
    await expect(page.getByTestId('login-username')).toBeVisible()
    await expect(page.locator('.el-message')).toHaveCount(0, { timeout: 15_000 }) // 等提示消失，免得干扰下一步计数
  })

  await test.step('后台定时请求（告警角标 60 秒刷新）401 同样只提示一次并跳登录', async () => {
    const badges = recordRequests(page, 'GET', '/alerts') // 登录前挂上：进入布局时的首次加载也计入
    await loginAdmin(page, admin)
    await expectAdminUrl(page, '/')
    await expect.poll(() => badges.length).toBe(1)
    // 仪表盘自己也每 60 秒刷新（A-DASH-01 覆盖它的 401）：换到没有页面级定时器的个人设置页，这里只让角标定时器触发 401
    await page.getByTestId('layout-settings').click()
    await expectAdminUrl(page, '/settings')
    await pauseClock(page)
    await hooks.advance(46800)
    await page.clock.runFor(60_000)
    await expectAdminUrl(page, '/login', { redirect: '/settings' })
    await toast(page, '登录已失效，请重新登录')
    await drainTasks(page)
    await expect(page.locator('.el-message')).toHaveCount(1)
    expect(badges).toHaveLength(2)
  })

  await test.step('二次验证弹框打开时会话失效：弹框关闭、等待的请求静默结束，只提示一次并跳登录', async () => {
    await page.clock.resume()
    await expect(page.locator('.el-message')).toHaveCount(0, { timeout: 15_000 })
    const fresh = await createAdmin('ops')
    await loginAdmin(page, fresh)
    await page.getByTestId('layout-settings').click()
    await expectAdminUrl(page, '/settings')
    const changes = recordRequests(page, 'POST', '/me/password')
    await page.route(
      (u) => u.pathname === '/admin/api/v1/me/password',
      (r) => r.fulfill({ status: 403, contentType: 'application/json', body: JSON.stringify({ code: 'totp_required', message: '该操作需要输入动态码', request_id: 'req-t' }) }),
    )
    await page.route(
      (u) => u.pathname === '/admin/api/v1/alerts',
      (r) => r.fulfill({ status: 401, contentType: 'application/json', body: JSON.stringify({ code: 'unauthorized', message: '请先登录', request_id: 'req-u' }) }),
    )
    await page.getByTestId('settings-old').fill(fresh.password)
    await page.getByTestId('settings-new').fill('step-up-pending-pass-1')
    await page.getByTestId('settings-confirm').fill('step-up-pending-pass-1')
    await pauseClock(page)
    await page.getByTestId('settings-submit').click()
    await expect(page.getByTestId('totp-dialog')).toBeVisible()
    await page.clock.runFor(60_000) // 角标刷新得到 401
    await expectAdminUrl(page, '/login', { redirect: '/settings' })
    await page.clock.resume()
    await expect(page.getByTestId('totp-dialog')).toBeHidden()
    await toast(page, '登录已失效，请重新登录')
    await settle(page)
    await expect(page.locator('.el-message')).toHaveCount(1)
    expect(changes).toHaveLength(1)
    await page.unrouteAll()
  })
})
