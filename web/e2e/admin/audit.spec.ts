import { expect, test, type Route } from '@playwright/test'
import { adminApi, adminPost, createAdmin, drainTasks, expectAdminUrl, loginAdmin, recordRequests, trackToasts } from '../helpers/admin'
import { seedUser } from '../helpers/console'
import { hooks } from '../helpers/hooks'
import { cstDate, cstTime } from '../helpers/time'

const isAudit = (u: URL) => u.pathname === '/admin/api/v1/audit'

test('A-AUD-01 审计日志按操作人 / 动作 / 对象 / 时间筛选，操作人显示用户名，对象可点击，展开 diff', async ({ page }) => {
  // 用 user.ban 造记录：它的 diff 非空（{reason}），对象类型 user 可跳转；alert.resolve 的 diff 是 null，格式化后只有「{}」，测不出两空格缩进
  const u = await seedUser()
  const admin = await createAdmin('super')
  const ctx = await adminApi(admin)
  await adminPost(ctx, admin, `/admin/api/v1/users/${u.id}/ban`, { reason: 'e2e 审计用例' })
  const today = cstDate(await hooks.clock())
  const toasts = await trackToasts(page)
  const auditRequests = recordRequests(page, 'GET', '/audit')
  const searches = () => auditRequests.map((r) => r.search)
  const rows = page.getByRole('row').filter({ has: page.getByTestId('audit-action-cell') })
  let v: { id: number; phone: string } // 翻页用的第二个用户（22+ 条审计）

  await test.step('按操作人 / 动作 / 对象 / 时间筛选：条件原样作为查询参数发出，结果只有这一条', async () => {
    await loginAdmin(page, admin)
    await page.getByTestId('menu-audit').click()
    await expectAdminUrl(page, '/audit')
    await expect.poll(searches).toEqual(['?page=1&page_size=20'])
    await page.getByTestId('audit-actor').fill(String(admin.id))
    await page.getByTestId('audit-action').click()
    await page.getByRole('option', { name: 'user.ban' }).click()
    await page.getByTestId('audit-target-type').fill('user')
    await page.getByTestId('audit-target-id').fill(String(u.id))
    await page.getByTestId('audit-from').fill(today)
    await page.getByTestId('audit-to').fill(today)
    await page.getByTestId('audit-search').click()
    await expect.poll(() => searches().at(-1)).toBe(`?actor_id=${admin.id}&action=user.ban&target_type=user&target_id=${u.id}&from=${today}&to=${today}&page=1&page_size=20`)
    expect(auditRequests).toHaveLength(2)
    await expect(rows).toHaveCount(1)
    const entry = rows.first()
    await expect(entry.getByTestId('audit-actor-cell')).toHaveText(admin.username)
    await expect(entry.getByTestId('audit-action-cell')).toHaveText('user.ban')
    await expect(entry.getByTestId('audit-target')).toContainText(`user ${u.id}`)
    await expect(entry.getByTestId('audit-target').locator('a')).toHaveAttribute('href', `/admin/users/${u.id}`)
    await expect(entry.getByTestId('audit-ip')).toHaveText(/^\S+$/)
    const logged = (await (await ctx.get(`/admin/api/v1/audit?action=user.ban&target_type=user&target_id=${u.id}`)).json()) as { items: { created_at: string; ip: string }[] }
    await expect(entry.getByTestId('audit-time')).toHaveText(cstTime(logged.items[0].created_at))
    await expect(entry.getByTestId('audit-ip')).toHaveText(logged.items[0].ip)
  })

  await test.step('日期范围：结束日期不能早于开始日期（to 的 min = from，from 的 max = to）', async () => {
    await expect(page.getByTestId('audit-to')).toHaveAttribute('min', today)
    await expect(page.getByTestId('audit-from')).toHaveAttribute('max', today)
  })

  await test.step('展开行显示两空格缩进的 diff JSON', async () => {
    await rows.first().locator('.el-table__expand-icon').click() // Element Plus 的展开控件（UI 控件，不是业务元素）
    const diff = page.getByTestId('audit-diff')
    await expect(diff).toBeVisible()
    const text = (await diff.textContent())!
    expect(JSON.parse(text)).toEqual({ reason: 'e2e 审计用例' })
    expect(text).toBe('{\n  "reason": "e2e 审计用例"\n}') // 两空格缩进的格式化 JSON
  })

  await test.step('操作人换成另一个管理员 → 空；日期范围换成 2000 年 → 空', async () => {
    const other = await createAdmin('ops')
    await page.getByTestId('audit-actor').fill(String(other.id))
    await page.getByTestId('audit-search').click()
    await expect.poll(() => searches().at(-1)).toBe(`?actor_id=${other.id}&action=user.ban&target_type=user&target_id=${u.id}&from=${today}&to=${today}&page=1&page_size=20`)
    await expect(rows).toHaveCount(0)
    await page.getByTestId('audit-actor').fill(String(admin.id))
    await page.getByTestId('audit-from').fill('2000-01-01')
    await page.getByTestId('audit-to').fill('2000-01-02')
    await expect(page.getByTestId('audit-to')).toHaveAttribute('min', '2000-01-01')
    await page.getByTestId('audit-search').click()
    await expect.poll(() => searches().at(-1)).toBe(`?actor_id=${admin.id}&action=user.ban&target_type=user&target_id=${u.id}&from=2000-01-01&to=2000-01-02&page=1&page_size=20`)
    await expect(rows).toHaveCount(0)
  })

  await test.step('动作可以输入列表外的值（allow-create）：原样作为 action 查询参数发出', async () => {
    await page.getByTestId('audit-action').click()
    await page.keyboard.type('custom.e2e_only')
    await page.keyboard.press('Enter')
    await expect(page.getByTestId('audit-action')).toContainText('custom.e2e_only')
    await page.getByTestId('audit-search').click()
    await expect.poll(() => searches().at(-1)).toBe(`?actor_id=${admin.id}&action=custom.e2e_only&target_type=user&target_id=${u.id}&from=2000-01-01&to=2000-01-02&page=1&page_size=20`)
    await expect(rows).toHaveCount(0)
  })

  await test.step('重置：清空全部条件，只发一次不带筛选的请求', async () => {
    const before = auditRequests.length
    await page.getByTestId('audit-reset').click()
    await expect.poll(() => auditRequests.length).toBe(before + 1)
    expect(searches().at(-1)).toBe('?page=1&page_size=20')
    for (const id of ['audit-actor', 'audit-target-type', 'audit-target-id', 'audit-from', 'audit-to']) await expect(page.getByTestId(id)).toHaveValue('')
    await expect(page.getByTestId('audit-action')).not.toContainText('user.ban')
    await expect(page.getByTestId('audit-to')).not.toHaveAttribute('min')
    await drainTasks(page)
    expect(auditRequests).toHaveLength(before + 1)
  })

  await test.step('对象链接跳到用户详情', async () => {
    await page.getByTestId('audit-target-type').fill('user')
    await page.getByTestId('audit-target-id').fill(String(u.id))
    await page.getByTestId('audit-search').click()
    await expect.poll(() => searches().at(-1)).toBe(`?target_type=user&target_id=${u.id}&page=1&page_size=20`)
    await rows.first().getByTestId('audit-target').locator('a').click()
    await expectAdminUrl(page, `/users/${u.id}`)
  })

  await test.step('超过一页：翻到第 2 页带 page=2，换条件查询回到第 1 页', async () => {
    v = await seedUser()
    for (let i = 0; i < 11; i++) {
      await adminPost(ctx, admin, `/admin/api/v1/users/${v.id}/ban`, { reason: `翻页 ${i}` })
      await adminPost(ctx, admin, `/admin/api/v1/users/${v.id}/unban`, {})
    }
    await page.getByTestId('menu-audit').click()
    await page.getByTestId('audit-target-type').fill('user')
    await page.getByTestId('audit-target-id').fill(String(v.id))
    await page.getByTestId('audit-search').click()
    await expect.poll(() => searches().at(-1)).toBe(`?target_type=user&target_id=${v.id}&page=1&page_size=20`)
    await expect(rows).toHaveCount(20)
    // 造数据时 seedUser 的充值也会写审计：总数以接口为准（≥ 22）
    const page2 = (await (await ctx.get(`/admin/api/v1/audit?target_type=user&target_id=${v.id}&page=2&page_size=20`)).json()) as { items: { id: number; action: string }[]; total: number }
    expect(page2.total).toBeGreaterThanOrEqual(22)
    await expect(page.getByTestId('audit-pager')).toContainText(String(page2.total))
    await page.getByTestId('audit-pager').getByText('2', { exact: true }).click()
    await expect.poll(() => searches().at(-1)).toBe(`?target_type=user&target_id=${v.id}&page=2&page_size=20`)
    await expect(rows).toHaveCount(page2.total - 20)
    await expect(rows.getByTestId('audit-action-cell')).toHaveText(page2.items.map((e) => e.action))
    await page.getByTestId('audit-action').click()
    await page.getByRole('option', { name: 'user.unban' }).click()
    const before = auditRequests.length
    await page.getByTestId('audit-search').click()
    await expect.poll(() => searches().at(-1)).toBe(`?action=user.unban&target_type=user&target_id=${v.id}&page=1&page_size=20`)
    await expect(rows).toHaveCount(11)
    await drainTasks(page)
    expect(auditRequests).toHaveLength(before + 1) // 回到第 1 页只发一次请求
  })

  await test.step('改条件后查询失败：清空表格（不留与新条件不符的旧行），显示占位与重试；重试按新条件请求', async () => {
    // 此时表格显示 user.unban 的 11 行
    await expect(rows).toHaveCount(11)
    const toastsBefore = (await toasts()).length
    await page.route(isAudit, (r: Route) => r.fulfill({ status: 500, contentType: 'application/json', body: JSON.stringify({ code: 'internal', message: '服务器内部错误', request_id: 'req-audit-2' }) }))
    await page.getByTestId('audit-action').click()
    await page.getByRole('option', { name: 'user.ban', exact: true }).click()
    await page.getByTestId('audit-search').click()
    await expect(page.getByTestId('audit-error')).toContainText('服务器内部错误（请求号 req-audit-2）')
    await expect(rows).toHaveCount(0)
    expect((await toasts()).slice(toastsBefore)).toEqual(['服务器内部错误（请求号 req-audit-2）'])
    await page.unroute(isAudit)
    await page.getByTestId('audit-retry').click()
    await expect.poll(() => searches().at(-1)).toBe(`?action=user.ban&target_type=user&target_id=${v.id}&page=1&page_size=20`)
    await expect(rows).toHaveCount(11)
    await expect(rows.getByTestId('audit-action-cell')).toHaveText(Array(11).fill('user.ban'))
    await expect(page.getByTestId('audit-error')).toHaveCount(0)
  })

  await test.step('操作人：系统显示「系统」，用户名为空显示 #ID；对象类型不可跳转的只显示 ID', async () => {
    await page.route(isAudit, (r: Route) =>
      r.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          total: 3,
          items: [
            { id: 9001, actor_type: 'system', actor_id: 0, actor_name: '', action: 'instance.expire', target_type: 'instance', target_id: 'i-mock', diff: null, ip: '', created_at: '2026-10-17T02:00:00Z' },
            { id: 9002, actor_type: 'admin', actor_id: 987654, actor_name: '', action: 'machine.maintenance', target_type: 'machine', target_id: '42', diff: { on: true }, ip: '10.0.0.9', created_at: '2026-10-17T01:00:00Z' },
            { id: 9003, actor_type: 'admin', actor_id: 1, actor_name: 'root', action: 'catalog.update', target_type: 'product', target_id: '7', diff: {}, ip: '10.0.0.8', created_at: '2026-10-17T00:00:00Z' },
          ],
        }),
      }),
    )
    await page.getByTestId('audit-reset').click()
    await expect(rows).toHaveCount(3)
    await expect(rows.nth(0).getByTestId('audit-actor-cell')).toHaveText('系统')
    await expect(rows.nth(0).getByTestId('audit-target').locator('a')).toHaveAttribute('href', '/admin/instances/i-mock')
    await expect(rows.nth(1).getByTestId('audit-actor-cell')).toHaveText('#987654')
    await expect(rows.nth(1).getByTestId('audit-target').locator('a')).toHaveAttribute('href', '/admin/machines/42') // super 可进机器详情
    await expect(rows.nth(2).getByTestId('audit-actor-cell')).toHaveText('root')
    await expect(rows.nth(2).getByTestId('audit-target')).toHaveText('product 7')
    await expect(rows.nth(2).getByTestId('audit-target').locator('a')).toHaveCount(0)
  })

  await test.step('财务没有机器详情权限：机器对象只显示 ID，实例仍可点', async () => {
    await page.getByTestId('layout-logout').click()
    await expectAdminUrl(page, '/login')
    const finance = await createAdmin('finance')
    await loginAdmin(page, finance)
    await page.getByTestId('menu-audit').click()
    await expect(rows).toHaveCount(3)
    await expect(rows.nth(1).getByTestId('audit-target')).toHaveText('machine 42')
    await expect(rows.nth(1).getByTestId('audit-target').locator('a')).toHaveCount(0)
    await expect(rows.nth(0).getByTestId('audit-target').locator('a')).toHaveAttribute('href', '/admin/instances/i-mock')
    await page.unroute(isAudit)
  })

  await test.step('首次加载失败：提示一次、显示占位与重试，不自动重试；重试成功后显示列表', async () => {
    await page.getByTestId('menu-dashboard').click()
    await expectAdminUrl(page, '/')
    await page.route(isAudit, (r: Route) => r.fulfill({ status: 500, contentType: 'application/json', body: JSON.stringify({ code: 'internal', message: '服务器内部错误', request_id: 'req-audit' }) }))
    const before = auditRequests.length
    const toastsBefore = (await toasts()).length
    await page.getByTestId('menu-audit').click()
    await expect(page.getByTestId('audit-error')).toContainText('服务器内部错误（请求号 req-audit）')
    await drainTasks(page)
    expect(auditRequests).toHaveLength(before + 1)
    expect((await toasts()).slice(toastsBefore)).toEqual(['服务器内部错误（请求号 req-audit）'])
    await expect(rows).toHaveCount(0)
    await page.unroute(isAudit)
    await page.getByTestId('audit-retry').click()
    await expect(rows).toHaveCount(20)
    await expect(page.getByTestId('audit-error')).toHaveCount(0)
    expect(auditRequests).toHaveLength(before + 2)
  })
})
