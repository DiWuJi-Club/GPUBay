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

const FAIL_TEXT = '服务器内部错误（请求号 req-dash）'
const failBody = { status: 500, contentType: 'application/json', body: JSON.stringify({ code: 'internal', message: '服务器内部错误', request_id: 'req-dash' }) }
const json = (v: unknown) => ({ status: 200, contentType: 'application/json', body: JSON.stringify(v) })

test('A-DASH-01 仪表盘数字与接口一致、金额格式、30 根柱', async ({ page }) => {
  await page.clock.install() // 60 秒刷新用；pauseClock 之前时间照常流逝
  const toasts = await trackToasts(page)
  const dashRequests = recordRequests(page, 'GET', '/dashboard')
  // 接管 /dashboard：pass 走真实后端；fail 返回 500；hold 挂起（之后手动放行，模拟慢响应）；mock 返回构造的数据
  let mode: 'pass' | 'fail' | 'hold' | 'mock' = 'pass'
  let mock: Dash | null = null
  const held: Route[] = []
  await page.route(
    (u) => u.pathname === '/admin/api/v1/dashboard',
    (r) => (mode === 'pass' ? r.fallback() : mode === 'fail' ? r.fulfill(failBody) : mode === 'hold' ? void held.push(r) : r.fulfill(json(mock))),
  )
  const admin = await createAdmin('finance')
  const ctx = await adminApi(admin)
  const d = (await (await ctx.get('/admin/api/v1/dashboard')).json()) as Dash
  const total = page.getByTestId('dash-machines-total')
  const bars = page.getByTestId('dash-chart').getByTestId('dash-bar')

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
    await expect(bars).toHaveCount(30)
    const last = d.utilization[29]
    await expect(bars.nth(29)).toHaveAttribute('data-date', last.date)
    await expect(bars.nth(29).locator('title')).toHaveText(`${last.date} 出租率 ${(last.rate * 100).toFixed(1)}%`)
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
    mode = 'fail'
    await page.clock.runFor(60_000)
    await expect.poll(() => dashRequests.length).toBe(3)
    await expect.poll(toasts).toEqual([FAIL_TEXT])
    await page.clock.runFor(60_000)
    await expect.poll(() => dashRequests.length).toBe(4)
    await drainTasks(page)
    expect(await toasts()).toEqual([FAIL_TEXT])
    await expect(total).toHaveText(String(d.machines.total + 1))
    await expect(page.getByTestId('dash-error')).toHaveCount(0)
    mode = 'pass'
    await page.clock.runFor(60_000)
    await expect.poll(() => dashRequests.length).toBe(5)
    await drainTasks(page)
    expect(await toasts()).toEqual([FAIL_TEXT])
    mode = 'fail'
    await page.clock.runFor(60_000)
    await expect.poll(() => dashRequests.length).toBe(6)
    await expect.poll(toasts).toEqual([FAIL_TEXT, FAIL_TEXT])
  })

  await test.step('首次加载失败：提示一次并显示占位与重试，不连续重试；定时刷新恢复前不再提示；手动重试失败再提示', async () => {
    // mode 仍是 fail；整页重新加载后提示记录从零开始
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
  })

  await test.step('定时刷新与手动重试同时在途：先发后到的旧失败被丢弃（不提示、不回到错误占位）', async () => {
    mode = 'hold'
    await page.clock.runFor(60_000) // 定时刷新发出后挂起
    await expect.poll(() => held.length).toBe(1)
    mode = 'pass'
    await page.getByTestId('dash-retry').click() // 后发的重试先成功
    await expect(total).toHaveText(String(d.machines.total + 1))
    await expect(page.getByTestId('dash-error')).toHaveCount(0)
    await held.shift()!.fulfill(failBody) // 旧请求这时才失败
    await drainTasks(page)
    expect(dashRequests).toHaveLength(11)
    expect(await toasts()).toEqual([FAIL_TEXT, FAIL_TEXT])
    await expect(page.getByTestId('dash-error')).toHaveCount(0)
    await expect(total).toHaveText(String(d.machines.total + 1))
  })

  await test.step('定时刷新与手动重试同时在途：先发后到的旧数据不覆盖重试拿到的新数据', async () => {
    // 请求超时是 30 秒（AbortSignal.timeout），两次 60 秒定时刷新不会重叠；会重叠的是慢的定时刷新与手动重试
    mode = 'fail'
    await page.clock.resume()
    await page.reload()
    await expect(page.getByTestId('dash-error')).toBeVisible()
    await pauseClock(page)
    await drainTasks(page)
    expect(dashRequests).toHaveLength(12)
    mode = 'hold'
    await page.clock.runFor(60_000)
    await expect.poll(() => held.length).toBe(1)
    mode = 'pass'
    await hooks.machines(1)
    await page.getByTestId('dash-retry').click()
    await expect(total).toHaveText(String(d.machines.total + 2))
    await held.shift()!.fulfill(json({ ...d, machines: { ...d.machines, total: 99999 } })) // 旧的定时刷新这时才返回
    await drainTasks(page)
    expect(dashRequests).toHaveLength(14)
    await expect(total).toHaveText(String(d.machines.total + 2))
    expect(await toasts()).toEqual([FAIL_TEXT])
  })

  await test.step('首次加载失败后由 60 秒定时刷新自动恢复（不点重试、不再提示）', async () => {
    mode = 'fail'
    await page.clock.resume()
    await page.reload()
    await expect(page.getByTestId('dash-error')).toContainText(FAIL_TEXT)
    await expect.poll(toasts).toEqual([FAIL_TEXT])
    await pauseClock(page)
    await drainTasks(page)
    expect(dashRequests).toHaveLength(15)
    mode = 'pass'
    await page.clock.runFor(60_000)
    await expect(total).toHaveText(String(d.machines.total + 2))
    await expect(page.getByTestId('dash-error')).toHaveCount(0)
    expect(dashRequests).toHaveLength(16)
    expect(await toasts()).toEqual([FAIL_TEXT])
  })

  await test.step('柱状图几何：出租率 0 / 50% / 100% 及越界值的高度、y（柱底贴底线 160）与提示文字', async () => {
    const rates = [0, 0.5, 1, 1.2, -0.1, 0.123]
    const utilization = Array.from({ length: 30 }, (_, i) => ({ date: `2026-09-${String(i + 1).padStart(2, '0')}`, rate: rates[i] ?? 0.25 }))
    mock = { ...d, utilization }
    mode = 'mock'
    await page.clock.runFor(60_000)
    await expect(bars.nth(0)).toHaveAttribute('data-date', '2026-09-01')
    const expected: [number, string, string][] = [
      [0, '0', '0.0'],
      [1, '70', '50.0'],
      [2, '140', '100.0'],
      [3, '140', '100.0'], // 超过 1 按 100% 画，不出坐标系
      [4, '0', '0.0'], // 负数按 0
      [5, '17', '12.3'],
      [6, '35', '25.0'],
    ]
    for (const [i, h, pct] of expected) {
      await expect(bars.nth(i)).toHaveAttribute('height', h)
      await expect(bars.nth(i)).toHaveAttribute('y', String(160 - Number(h)))
      await expect(bars.nth(i)).toHaveAttribute('x', String(i * 20 + 4))
      await expect(bars.nth(i).locator('title')).toHaveText(`${utilization[i].date} 出租率 ${pct}%`)
    }
    await expect(bars).toHaveCount(30)
    await expect(page.getByTestId('dash-chart-empty')).toHaveCount(0)
  })

  await test.step('出租率数据为空：显示「暂无出租率数据」，不画空坐标', async () => {
    mock = { ...d, utilization: [] }
    await page.clock.runFor(60_000)
    await expect(page.getByTestId('dash-chart-empty')).toContainText('暂无出租率数据')
    await expect(page.getByTestId('dash-chart')).toHaveCount(0)
    mode = 'pass'
    await page.clock.runFor(60_000)
    await expect(bars).toHaveCount(30)
    await expect(page.getByTestId('dash-chart-empty')).toHaveCount(0)
    expect(dashRequests).toHaveLength(19)
  })

  await test.step('会话过期时定时刷新收到 401：仪表盘不另行提示，只有一条「登录已失效」并跳登录', async () => {
    await hooks.advance(46800) // 超过会话 12 小时绝对时长
    await page.clock.runFor(60_000) // 仪表盘与角标的 60 秒定时器都触发，两个请求都是 401
    await expectAdminUrl(page, '/login', { redirect: '/' })
    await expect.poll(toasts).toEqual([FAIL_TEXT, '登录已失效，请重新登录'])
    await drainTasks(page)
    expect(await toasts()).toEqual([FAIL_TEXT, '登录已失效，请重新登录'])
    expect(dashRequests).toHaveLength(20)
  })
})
