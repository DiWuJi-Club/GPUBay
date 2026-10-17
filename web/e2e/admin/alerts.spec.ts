import { expect, test, type Route } from '@playwright/test'
import { adminApi, clickTwiceSync, createAdmin, drainTasks, expectAdminUrl, loginAdmin, pauseClock, recordRequests, trackToasts } from '../helpers/admin'
import { placeOrder, seedUser, toast, userApi } from '../helpers/console'
import { hooks } from '../helpers/hooks'
import { cstTime } from '../helpers/time'

type AlertRow = { id: number; kind: string; machine_id: number | null; instance_id: string; message: string; created_at: string; resolved_at: string | null }
type Page_ = { items: AlertRow[]; total: number }

const fail500 = (id: string) => (r: Route) =>
  r.fulfill({ status: 500, contentType: 'application/json', body: JSON.stringify({ code: 'internal', message: '服务器内部错误', request_id: id }) })

test('A-ALT-01 开通失败告警：未处理列表、角标、标记已处理、全部可见', async ({ page }) => {
  // 先造一批机器失联告警（> 每页 20 条，测翻页），再造开通失败告警：列表按时间倒序，它在第一页最上面
  await hooks.machines(21)
  await hooks.advance(301)
  await hooks.sweep({ heartbeat: false })
  await hooks.sweep() // 恢复心跳，免得影响后续用例
  const u = await seedUser()
  const api = await userApi(u.phone)
  await hooks.machines(1)
  const instanceId = await placeOrder(api)
  await hooks.agent({ fail: true })
  const admin = await createAdmin('ops')
  const ctx = await adminApi(admin)
  const get = async (q: string) => (await (await ctx.get(`/admin/api/v1/alerts?${q}`)).json()) as Page_
  const total = (await get('resolved=false&page_size=1')).total
  expect(total).toBeGreaterThan(21)
  const failed = (await get('resolved=false&page_size=100')).items.find((a) => a.instance_id === instanceId)!
  expect(failed.machine_id).not.toBeNull()

  await page.clock.install() // 冻结后角标的 60 秒定时刷新不会触发，「标记已处理」后的角标变化只能来自立即刷新
  const toasts = await trackToasts(page)
  const listRequests = recordRequests(page, 'GET', '/alerts') // 含角标请求（?resolved=false&page_size=1）
  const listSearches = () => listRequests.map((r) => r.search).filter((s) => s.includes('page='))
  const badgeCount = () => listRequests.filter((r) => r.search === '?resolved=false&page_size=1').length
  const badge = page.getByTestId('menu-alerts-badge')
  const rows = page.getByRole('row').filter({ has: page.getByTestId('alert-kind') })
  const row = page.getByRole('row').filter({ has: page.getByTestId('alert-instance').filter({ hasText: instanceId }) })

  await test.step('角标显示未处理数；列表显示级别、类型、内容、机器 / 实例链接、时间', async () => {
    await loginAdmin(page, admin)
    await expect(badge).toContainText(total > 99 ? '99+' : String(total))
    await page.getByTestId('menu-alerts').click()
    await expectAdminUrl(page, '/alerts')
    await expect(row.getByTestId('alert-kind')).toHaveText('开通失败')
    await expect(row.getByTestId('alert-level')).toHaveText('严重')
    await expect(row.getByTestId('alert-message')).toHaveText(failed.message)
    await expect(row.getByTestId('alert-time')).toHaveText(cstTime(failed.created_at))
    await expect(row.getByTestId('alert-machine')).toHaveAttribute('href', `/admin/machines/${failed.machine_id}`)
    await expect(row.getByTestId('alert-instance')).toHaveAttribute('href', `/admin/instances/${instanceId}`)
    await expect(rows).toHaveCount(20)
    expect(listSearches()).toEqual(['?resolved=false&page=1&page_size=20'])
  })

  await test.step('翻到第 2 页：请求 page=2，显示第 21 条起', async () => {
    const page2 = await get('resolved=false&page=2&page_size=20')
    await page.getByTestId('alerts-pager').getByText('2', { exact: true }).click()
    await expect.poll(listSearches).toEqual(['?resolved=false&page=1&page_size=20', '?resolved=false&page=2&page_size=20'])
    await expect(rows).toHaveCount(page2.items.length)
    await expect(rows.first().getByTestId('alert-message')).toHaveText(page2.items[0].message)
    // 第 2 页第一条是本用例造的机器失联告警：没有实例，实例列显示「—」且不是链接
    expect(page2.items[0]).toMatchObject({ instance_id: '' })
    expect(page2.items[0].machine_id).not.toBeNull()
    await expect(rows.first().getByTestId('alert-instance')).toHaveText('—')
    await expect(rows.first().getByTestId('alert-instance')).not.toHaveAttribute('href')
    await expect(rows.first().getByTestId('alert-machine')).toHaveAttribute('href', `/admin/machines/${page2.items[0].machine_id}`)
    await expect(row).toHaveCount(0)
    await page.getByTestId('alerts-pager').getByText('1', { exact: true }).click()
    await expect(row).toHaveCount(1)
  })

  await test.step('标记已处理（双击只发一次）：提示、从未处理列表消失、角标立即减一', async () => {
    const resolves = recordRequests(page, 'POST', `/alerts/${failed.id}/resolve`)
    await pauseClock(page)
    const badgeBefore = badgeCount()
    await clickTwiceSync(row.getByTestId('alert-resolve'))
    await toast(page, '已标记为已处理')
    await expect(row).toHaveCount(0)
    const left = total - 1
    await expect(badge).toHaveCount(left > 0 ? 1 : 0)
    if (left > 0) await expect(badge).toContainText(left > 99 ? '99+' : String(left))
    await drainTasks(page)
    expect(resolves).toHaveLength(1)
    expect(badgeCount()).toBe(badgeBefore + 1) // 时钟已冻结：这次角标请求来自「标记已处理」后的立即刷新
    await page.clock.resume()
  })

  await test.step('切到「全部」：回到第一页、不带 resolved，已处理的显示处理时间', async () => {
    await page.getByTestId('alerts-filter-all').click()
    await expect.poll(() => listSearches().at(-1)).toBe('?page=1&page_size=20')
    const resolved = (await get('page=1&page_size=100')).items.find((a) => a.id === failed.id)!
    await expect(row.getByTestId('alert-resolved-at')).toHaveText(`${cstTime(resolved.resolved_at!)} 已处理`)
    await expect(row.getByTestId('alert-resolve')).toHaveCount(0)
    await page.getByTestId('alerts-filter-unresolved').click()
    await expect.poll(() => listSearches().at(-1)).toBe('?resolved=false&page=1&page_size=20')
  })

  await test.step('标记已处理失败：提示原文（带请求号），告警仍在列表里', async () => {
    await expect(rows.getByTestId('alert-resolved-at')).toHaveCount(0) // 已切回未处理列表
    const first = (await get('resolved=false&page_size=1')).items[0]
    const target = rows.first()
    await expect(target.getByTestId('alert-machine')).toHaveText(String(first.machine_id ?? '—'))
    await expect(target.getByTestId('alert-time')).toHaveText(cstTime(first.created_at))
    const isResolve = (u: URL) => u.pathname === `/admin/api/v1/alerts/${first.id}/resolve`
    const posts = recordRequests(page, 'POST', `/alerts/${first.id}/resolve`)
    await page.route(isResolve, fail500('req-resolve'))
    await target.getByTestId('alert-resolve').click()
    await toast(page, '服务器内部错误（请求号 req-resolve）')
    await expect(target.getByTestId('alert-resolve')).toBeEnabled()
    await expect(target.getByTestId('alert-machine')).toHaveText(String(first.machine_id ?? '—'))
    await expect(target.getByTestId('alert-time')).toHaveText(cstTime(first.created_at))
    await expect(rows).toHaveCount(20)
    expect(posts).toHaveLength(1)
    expect((await get('resolved=false&page_size=1')).total).toBe(total - 1)
    await page.unroute(isResolve)
  })

  await test.step('首次加载失败：提示一次、显示占位与重试，不自动重试；重试成功后显示列表', async () => {
    const isList = (u: URL) => u.pathname === '/admin/api/v1/alerts' && u.searchParams.has('page')
    await page.getByTestId('menu-dashboard').click()
    await expectAdminUrl(page, '/')
    await page.route(isList, fail500('req-alerts'))
    const before = listSearches().length
    const toastsBefore = (await toasts()).length
    await page.getByTestId('menu-alerts').click()
    await expect(page.getByTestId('alerts-error')).toContainText('服务器内部错误（请求号 req-alerts）')
    await pauseClock(page)
    await drainTasks(page)
    expect(listSearches().length).toBe(before + 1)
    expect((await toasts()).slice(toastsBefore)).toEqual(['服务器内部错误（请求号 req-alerts）'])
    await expect(rows).toHaveCount(0)
    await page.unroute(isList)
    await page.getByTestId('alerts-retry').click()
    await expect(rows).toHaveCount(20)
    await expect(page.getByTestId('alerts-error')).toHaveCount(0)
    expect(listSearches().length).toBe(before + 2)
    await page.clock.resume()
  })

  await test.step('财务没有机器详情权限：机器 ID 只显示不可点，实例仍可点', async () => {
    await page.getByTestId('layout-logout').click()
    await expectAdminUrl(page, '/login')
    const finance = await createAdmin('finance')
    await loginAdmin(page, finance)
    await page.getByTestId('menu-alerts').click()
    await page.getByTestId('alerts-filter-all').click()
    await expect(row.getByTestId('alert-machine')).toHaveText(String(failed.machine_id))
    await expect(row.getByTestId('alert-machine')).not.toHaveAttribute('href')
    await expect(row.locator('a[href^="/admin/machines/"]')).toHaveCount(0)
    await expect(row.getByTestId('alert-instance')).toHaveAttribute('href', `/admin/instances/${instanceId}`)
  })
})
