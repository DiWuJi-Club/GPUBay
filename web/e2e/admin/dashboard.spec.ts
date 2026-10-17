import { expect, test, type Route } from '@playwright/test'
import { adminApi, createAdmin, drainTasks, expectAdminUrl, loginAdmin, pauseClock, recordRequests, trackToasts } from '../helpers/admin'
import { hooks } from '../helpers/hooks'
import { yuan } from '../helpers/money'

type Dash = {
  machines: { by_state: Record<string, number>; online: number; total: number }
  instances: Record<string, number>
  recharge_today: number
  recharge_month: number
  order_today: number
  order_month: number
  utilization: { date: string; rate: number }[]
}

const fail500 = (r: Route) => r.fulfill({ status: 500, contentType: 'application/json', body: JSON.stringify({ code: 'internal', message: '服务器内部错误', request_id: 'req-dash' }) })
const FAIL_TEXT = '服务器内部错误（请求号 req-dash）'
const isDashboard = (u: URL) => u.pathname === '/admin/api/v1/dashboard'

test('A-DASH-01 仪表盘数字与接口一致、金额格式、30 根柱', async ({ page }) => {
  await page.clock.install() // 60 秒刷新用；pauseClock 之前时间照常流逝
  const toasts = await trackToasts(page)
  const dashRequests = recordRequests(page, 'GET', '/dashboard')
  const admin = await createAdmin('finance')
  const ctx = await adminApi(admin)
  const d = (await (await ctx.get('/admin/api/v1/dashboard')).json()) as Dash
  const total = page.getByTestId('dash-machines-total')

  await test.step('数字与接口一致，金额按元格式，近 30 天出租率 30 根柱', async () => {
    await loginAdmin(page, admin)
    await expect(total).toHaveText(String(d.machines.total))
    await expect(page.getByTestId('dash-machines-online')).toHaveText(String(d.machines.online))
    for (const s of ['idle', 'allocated', 'maintenance', 'faulty']) await expect(page.getByTestId(`dash-machine-${s}`)).toHaveText(String(d.machines.by_state[s] ?? 0))
    for (const s of ['creating', 'running', 'stopped', 'wiping', 'released', 'failed']) await expect(page.getByTestId(`dash-instance-${s}`)).toHaveText(String(d.instances[s] ?? 0))
    await expect(page.getByTestId('dash-recharge-today')).toHaveText(yuan(d.recharge_today))
    await expect(page.getByTestId('dash-recharge-month')).toHaveText(yuan(d.recharge_month))
    await expect(page.getByTestId('dash-order-today')).toHaveText(yuan(d.order_today))
    await expect(page.getByTestId('dash-order-month')).toHaveText(yuan(d.order_month))
    expect(d.utilization).toHaveLength(30)
    const bars = page.getByTestId('dash-chart').getByTestId('dash-bar')
    await expect(bars).toHaveCount(30)
    for (const i of [0, 29]) {
      const u = d.utilization[i]
      await expect(bars.nth(i)).toHaveAttribute('data-date', u.date)
      await expect(bars.nth(i).locator('title')).toHaveText(`${u.date} 出租率 ${(u.rate * 100).toFixed(1)}%`)
      await expect(bars.nth(i)).toHaveAttribute('height', String(Math.round(u.rate * 140)))
    }
    expect(dashRequests.map((r) => r.search)).toEqual([''])
  })

  await test.step('每 60 秒刷新一次：新登记的机器在下一次刷新后出现', async () => {
    await pauseClock(page)
    await drainTasks(page)
    expect(dashRequests).toHaveLength(1)
    await hooks.machines(1)
    await page.clock.runFor(59_000)
    await drainTasks(page)
    expect(dashRequests).toHaveLength(1)
    await expect(total).toHaveText(String(d.machines.total))
    await page.clock.runFor(1_000)
    await expect.poll(() => dashRequests.length).toBe(2)
    await expect(total).toHaveText(String(d.machines.total + 1))
    expect(await toasts()).toEqual([])
  })

  await test.step('已有数据时刷新失败：保留旧数据，同一次故障只提示一次；恢复后再故障重新提示', async () => {
    await page.route(isDashboard, fail500)
    await page.clock.runFor(60_000)
    await expect.poll(() => dashRequests.length).toBe(3)
    await expect.poll(toasts).toEqual([FAIL_TEXT])
    await page.clock.runFor(60_000)
    await expect.poll(() => dashRequests.length).toBe(4)
    await drainTasks(page)
    expect(await toasts()).toEqual([FAIL_TEXT])
    await expect(total).toHaveText(String(d.machines.total + 1))
    await expect(page.getByTestId('dash-error')).toHaveCount(0)
    await page.unroute(isDashboard)
    await page.clock.runFor(60_000)
    await expect.poll(() => dashRequests.length).toBe(5)
    await drainTasks(page)
    expect(await toasts()).toEqual([FAIL_TEXT])
    await page.route(isDashboard, fail500)
    await page.clock.runFor(60_000)
    await expect.poll(() => dashRequests.length).toBe(6)
    await expect.poll(toasts).toEqual([FAIL_TEXT, FAIL_TEXT])
  })

  await test.step('首次加载失败：提示一次并显示占位与重试，不连续重试；定时刷新恢复前不再提示；手动重试失败再提示，成功后显示数据', async () => {
    // route 仍然是 500
    await page.clock.resume()
    await page.reload()
    await expect(page.getByTestId('dash-error')).toContainText(FAIL_TEXT)
    await expect(page.getByTestId('dash-retry')).toBeVisible()
    await expect.poll(toasts).toEqual([FAIL_TEXT])
    await pauseClock(page)
    await drainTasks(page)
    expect(dashRequests).toHaveLength(7)
    await expect(total).toHaveCount(0)
    await page.clock.runFor(60_000)
    await expect.poll(() => dashRequests.length).toBe(8)
    await drainTasks(page)
    expect(await toasts()).toEqual([FAIL_TEXT])
    await page.getByTestId('dash-retry').click()
    await expect.poll(() => dashRequests.length).toBe(9)
    await expect.poll(toasts).toEqual([FAIL_TEXT, FAIL_TEXT])
    await expect(page.getByTestId('dash-error')).toBeVisible()
    await page.unroute(isDashboard)
    await page.getByTestId('dash-retry').click()
    await expect(total).toHaveText(String(d.machines.total + 1))
    await expect(page.getByTestId('dash-error')).toHaveCount(0)
    expect(dashRequests).toHaveLength(10)
    expect(await toasts()).toEqual([FAIL_TEXT, FAIL_TEXT])
  })

  await test.step('会话过期时定时刷新收到 401：仪表盘不另行提示，只有一条「登录已失效」并跳登录', async () => {
    await hooks.advance(46800) // 超过会话 12 小时绝对时长
    await page.clock.runFor(60_000) // 仪表盘与角标的 60 秒定时器都触发，两个请求都是 401
    await expectAdminUrl(page, '/login', { redirect: '/' })
    await expect.poll(toasts).toEqual([FAIL_TEXT, FAIL_TEXT, '登录已失效，请重新登录'])
    await drainTasks(page)
    expect(await toasts()).toEqual([FAIL_TEXT, FAIL_TEXT, '登录已失效，请重新登录'])
    expect(dashRequests).toHaveLength(11)
  })
})
