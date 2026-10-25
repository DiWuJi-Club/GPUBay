import { expect, test, type Route } from '@playwright/test'
import { createAdmin, drainTasks, expectAdminUrl, loginAdmin, pauseClock, recordRequests } from '../helpers/admin'
import type { Role } from '../helpers/hooks'

const ROLE_LABEL: Record<Role, string> = { super: '超级管理员', ops: '运维', finance: '财务' }

test('A-NAV-01 菜单按角色过滤', async ({ page }) => {
  await page.clock.install() // 角标 60 秒刷新用；pauseClock 之前时间照常流逝
  // 计划 7 增加「应用」「应用部署」两项（全部角色可见）；计划 11 加「工作流」、计划 9 加「Token 商城」（都是全部角色），与 admin/src/__tests__/menu.spec.ts 一致
  const cases: [Role, number, string[]][] = [
    ['super', 20, []], // 计划 7 加「应用」「应用部署」（全部角色）；计划 8 加「邀请返利」（super / finance）；计划 11 加「工作流」；计划 9 加「Token 商城」
    ['ops', 14, ['orders', 'ledger', 'recharges', 'referral', 'catalog', 'admins']],
    ['finance', 14, ['machines', 'pdus', 'groups', 'kyc', 'catalog', 'admins']],
  ]
  for (const [role, count, missing] of cases) {
    const admin = await createAdmin(role)
    await loginAdmin(page, admin)
    await expect(page.getByTestId('layout-role')).toHaveText(ROLE_LABEL[role])
    await expect(page.getByTestId(/^menu-(?!alerts-badge)/)).toHaveCount(count)
    for (const key of missing) await expect(page.getByTestId(`menu-${key}`)).toHaveCount(0)
    await expect(page.getByTestId('menu-llm')).toHaveText('Token 商城')
    await expect(page.getByTestId('menu-workflows')).toHaveText('工作流')
    // 菜单项可用：点「审计日志」进入对应页面
    await page.getByTestId('menu-audit').click()
    await expectAdminUrl(page, '/audit')
    await expect(page).toHaveTitle('审计日志 - Spark Cloud 管理后台')
    await page.getByTestId('layout-logout').click()
    await expectAdminUrl(page, '/login')
  }

  const admin = await createAdmin('super')
  await test.step('侧边栏折叠：只存 sc_admin_sidebar_collapsed，刷新后保持', async () => {
    await loginAdmin(page, admin)
    await expect(page.getByTestId('menu-catalog')).toHaveText('价格与镜像')
    await page.getByTestId('layout-collapse').click()
    await expect(page.getByTestId('menu-catalog')).toHaveText('价格')
    // 计划 8：共享请求层给每个请求带 X-Device-Id，设备 ID（v4 UUID）存在 sc_device；除此之外只存折叠状态
    const storage = async () => {
      const s = await page.evaluate(() => ({ local: Object.entries(localStorage), session: Object.entries(sessionStorage) }))
      const device = s.local.filter(([k]) => k === 'sc_device')
      expect(device.every(([, v]) => /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(v))).toBe(true)
      return { local: s.local.filter(([k]) => k !== 'sc_device'), session: s.session }
    }
    expect(await storage()).toEqual({ local: [['sc_admin_sidebar_collapsed', '1']], session: [] })
    await page.reload()
    await expect(page.getByTestId('layout-username')).toHaveText(admin.username)
    await expect(page.getByTestId('menu-catalog')).toHaveText('价格')
    await page.getByTestId('layout-collapse').click()
    await expect(page.getByTestId('menu-catalog')).toHaveText('价格与镜像')
    expect(await storage()).toEqual({ local: [['sc_admin_sidebar_collapsed', '0']], session: [] })

    // localStorage 不可用（隐私模式 / 被禁用）：读写都抛错时布局照常渲染、折叠照常切换
    const p2 = await page.context().newPage()
    await p2.addInitScript(() => {
      const w = window as unknown as { __lsThrows: { get: number; set: number } }
      w.__lsThrows = { get: 0, set: 0 }
      Storage.prototype.getItem = function () {
        w.__lsThrows.get++
        throw new DOMException('denied', 'SecurityError')
      }
      Storage.prototype.setItem = function () {
        w.__lsThrows.set++
        throw new DOMException('denied', 'SecurityError')
      }
    })
    const pageErrors: string[] = []
    p2.on('pageerror', (e) => pageErrors.push(e.message))
    p2.on('console', (m) => {
      if (m.type() === 'error') pageErrors.push(m.text()) // Vue 捕获事件处理器里的异常后只打到 console.error
    })
    await p2.goto('/admin/')
    await expect(p2.getByTestId('layout-username')).toHaveText(admin.username)
    await expect(p2.getByTestId('menu-catalog')).toHaveText('价格与镜像')
    await p2.getByTestId('layout-collapse').click()
    await expect(p2.getByTestId('menu-catalog')).toHaveText('价格')
    await p2.getByTestId('layout-collapse').click()
    await expect(p2.getByTestId('menu-catalog')).toHaveText('价格与镜像')
    const throws = await p2.evaluate(() => (window as unknown as { __lsThrows: { get: number; set: number } }).__lsThrows)
    expect(throws.get).toBeGreaterThan(0)
    expect(throws.set).toBe(2)
    expect(pageErrors).toEqual([])
    await p2.close()

    await page.getByTestId('layout-logout').click()
    await expectAdminUrl(page, '/login')
  })

  // 告警角标：GET /alerts?resolved=false&page_size=1 的 total；这里接管该接口以构造数量与失败
  let alerts: { status: number; total: number } = { status: 500, total: 0 }
  await page.route(
    (u) => u.pathname === '/admin/api/v1/alerts',
    (r: Route) =>
      alerts.status === 200
        ? r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ items: [], total: alerts.total }) })
        : r.fulfill({ status: 500, contentType: 'application/json', body: JSON.stringify({ code: 'internal', message: '服务器内部错误', request_id: 'req-badge' }) }),
  )
  const badgeRequests = recordRequests(page, 'GET', '/alerts')
  const badge = page.getByTestId('menu-alerts-badge')

  await test.step('角标首次加载失败：不提示、不显示角标、不立即重试，60 秒后才再取', async () => {
    await loginAdmin(page, admin)
    await expect.poll(() => badgeRequests.length).toBe(1)
    expect(badgeRequests[0].search).toBe('?resolved=false&page_size=1')
    await pauseClock(page)
    await drainTasks(page)
    expect(badgeRequests).toHaveLength(1)
    await expect(badge).toHaveCount(0)
    await expect(page.locator('.el-message')).toHaveCount(0)
    await page.clock.runFor(59_000)
    await drainTasks(page)
    expect(badgeRequests).toHaveLength(1)
    await page.clock.runFor(1_000)
    await expect.poll(() => badgeRequests.length).toBe(2)
    await drainTasks(page)
    expect(badgeRequests).toHaveLength(2)
    await expect(page.locator('.el-message')).toHaveCount(0)
  })

  await test.step('恢复后显示未处理告警数，每 60 秒刷新；超过 99 显示 99+；失败保留上次的数；为 0 时隐藏', async () => {
    alerts = { status: 200, total: 7 }
    await page.clock.runFor(60_000)
    await expect(badge).toHaveText('7')
    alerts = { status: 200, total: 120 }
    await page.clock.runFor(60_000)
    await expect(badge).toHaveText('99+')
    alerts = { status: 500, total: 0 }
    await page.clock.runFor(60_000)
    await expect.poll(() => badgeRequests.length).toBe(5)
    await drainTasks(page)
    await expect(badge).toHaveText('99+')
    await expect(page.locator('.el-message')).toHaveCount(0)
    alerts = { status: 200, total: 0 }
    await page.clock.runFor(60_000)
    await expect(badge).toHaveCount(0)
    expect(badgeRequests).toHaveLength(6)
  })
})

test('A-NAV-02 无权限路由显示 403 页', async ({ page }) => {
  const admin = await createAdmin('finance')
  await loginAdmin(page, admin)
  await page.goto('/admin/machines')
  await expect(page.getByTestId('forbidden-page')).toContainText('无权访问该页面')
  await expectAdminUrl(page, '/403', { from: '/machines' })
  await expect(page).toHaveTitle('无权访问 - Spark Cloud 管理后台')
  await page.getByTestId('forbidden-home').click()
  await expectAdminUrl(page, '/')
  await expect(page.getByTestId('layout-username')).toHaveText(admin.username)

  await test.step('finance 访问运维与超管页面同样 403；允许的页面正常打开', async () => {
    for (const path of ['/pdus', '/kyc', '/catalog', '/admins', '/machines/1']) {
      await page.goto(`/admin${path}`)
      await expect(page.getByTestId('forbidden-page')).toBeVisible()
      await expectAdminUrl(page, '/403', { from: path })
    }
    await page.goto('/admin/orders')
    await expectAdminUrl(page, '/orders')
    await expect(page.getByTestId('forbidden-page')).toHaveCount(0)
    await expect(page).toHaveTitle('订单 - Spark Cloud 管理后台')
  })

  await test.step('ops 访问财务与超管页面 403；super 可访问全部', async () => {
    await page.getByTestId('layout-logout').click()
    await loginAdmin(page, await createAdmin('ops'))
    for (const path of ['/orders', '/ledger', '/recharges', '/catalog', '/admins']) {
      await page.goto(`/admin${path}`)
      await expect(page.getByTestId('forbidden-page')).toBeVisible()
      await expectAdminUrl(page, '/403', { from: path })
    }
    await page.goto('/admin/machines')
    await expect(page).toHaveTitle('机器 - Spark Cloud 管理后台')
    await expect(page.getByTestId('forbidden-page')).toHaveCount(0)
    await page.getByTestId('layout-logout').click()
    await loginAdmin(page, await createAdmin('super'))
    for (const path of ['/catalog', '/admins', '/machines', '/orders']) {
      await page.goto(`/admin${path}`)
      await expectAdminUrl(page, path)
      await expect(page.getByTestId('layout-username')).toBeVisible()
      await expect(page.getByTestId('forbidden-page')).toHaveCount(0)
    }
  })

  await test.step('不存在的地址显示「页面不存在」', async () => {
    await page.goto('/admin/no-such-page')
    await expect(page.getByTestId('admin-notfound')).toContainText('页面不存在')
    await expect(page).toHaveTitle('页面不存在 - Spark Cloud 管理后台')
  })
})
