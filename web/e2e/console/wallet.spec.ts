import { expect, test } from '@playwright/test'
import { loginConsole, nav, placeOrder, seedUser, settle, toast, toastLocator, userApi } from '../helpers/console'
import { hooks } from '../helpers/hooks'
import { cstTime } from '../helpers/time'

test('C-WAL-01 余额与流水：类型中文、正负着色、变动后余额、滚动加载第二页；加载失败可重试；加载中刷新丢弃旧结果', async ({ page }, info) => {
  const u = await seedUser({ balance: 106900, ledger_rows: 25 }) // 25 条 +¥0.01、1 条 +¥1,068.75，再下单 -¥69.00 → 27 条、余额 ¥1,000.00
  const api = await userApi(u.phone)
  await hooks.machines(1)
  await placeOrder(api)
  const ledgerApi = ((await (await api.get('/api/v1/wallet/ledger?limit=100')).json()) as { items: { id: number; remark: string; created_at: string }[] }).items
  const ledgerIds = ledgerApi.map((r) => r.id)
  expect(ledgerIds).toHaveLength(27)
  expect(ledgerApi[0].remark).toMatch(/^新购 /)
  const ledgerReqs: URL[] = []
  page.on('request', (r) => {
    const url = new URL(r.url())
    if (url.pathname === '/api/v1/wallet/ledger') ledgerReqs.push(url)
  })
  await loginConsole(page, u.phone)
  await nav(page, info, 'wallet')
  await expect(page.getByTestId('wallet-balance')).toHaveText('¥1,000.00')
  await expect(page.getByTestId('ledger-amount')).toHaveCount(20)
  await settle(page) // 让哨兵的 IntersectionObserver 有机会触发，再断言确实没有自动拉第二页
  await expect(page.getByTestId('ledger-amount')).toHaveCount(20)
  expect(ledgerReqs).toHaveLength(1) // 第一页加载完、未滚动时不会自动拉第二页
  expect(ledgerReqs[0].searchParams.get('limit')).toBe('20')
  expect(ledgerReqs[0].searchParams.has('before_id')).toBe(false)
  const first = page.getByTestId('ledger-amount').first()
  await expect(first).toHaveText('-¥69.00')
  await expect(first).toHaveClass(/money-minus/)
  await expect(page.getByTestId('ledger-kind').first()).toHaveText('扣款')
  await expect(page.getByTestId('ledger-after').first()).toHaveText('¥1,000.00')
  await expect(page.getByTestId('ledger-remark').first()).toHaveText(ledgerApi[0].remark)
  await expect(page.getByTestId('ledger-time').first()).toHaveText(cstTime(ledgerApi[0].created_at))
  await expect(page.getByTestId('ledger-amount').nth(1)).toHaveText('+¥1,068.75')
  await expect(page.getByTestId('ledger-amount').nth(1)).toHaveClass(/money-plus/)
  await expect(page.getByTestId('ledger-kind').nth(1)).toHaveText('调账')
  await page.getByTestId('ledger-sentinel').scrollIntoViewIfNeeded()
  await expect(page.getByTestId('ledger-amount')).toHaveCount(27)
  await expect(page.getByTestId('ledger-end')).toHaveText('没有更多了')
  await settle(page)
  await expect(page.getByTestId('ledger-sentinel')).toHaveCount(0)
  expect(ledgerReqs).toHaveLength(2) // 第二页按游标取：before_id = 第一页最后一条
  expect(ledgerReqs[1].searchParams.get('before_id')).toBe(String(ledgerIds[19]))
  expect(ledgerReqs[1].searchParams.get('limit')).toBe('20')

  const isLedger = (url: URL) => url.pathname === '/api/v1/wallet/ledger'
  await test.step('流水加载失败：只请求一次、只提示一次，显示错误与重试，重试后加载', async () => {
    ledgerReqs.length = 0
    await page.route(isLedger, (route) =>
      route.fulfill({ status: 500, contentType: 'application/json', body: JSON.stringify({ code: 'internal', message: '服务器内部错误，请稍后重试', request_id: 'req-ledger-500' }) }),
    )
    await page.getByTestId('wallet-refresh').click()
    await toast(page, '服务器内部错误，请稍后重试（请求号 req-ledger-500）')
    await settle(page, 30)
    expect(ledgerReqs).toHaveLength(1) // 失败后哨兵不会反复触发请求
    await expect(page.getByTestId('wallet-ledger-error')).toContainText('请求号 req-ledger-500')
    await expect(page.getByTestId('wallet-balance')).toHaveText('¥1,000.00') // 余额不受影响
    await expect(page.getByTestId('ledger-sentinel')).toHaveCount(0) // 出错后不再自动加载
    await page.evaluate(() => window.scrollTo(0, document.body.scrollHeight))
    await hooks.advance(60)
    await settle(page, 30)
    expect(ledgerReqs).toHaveLength(1)
    await expect(toastLocator(page, '请求号 req-ledger-500')).toHaveCount(1)
    await page.unroute(isLedger)
    await page.getByTestId('wallet-ledger-retry').click()
    await expect(page.getByTestId('ledger-amount')).toHaveCount(20)
    await expect(page.getByTestId('wallet-ledger-error')).toHaveCount(0)
    expect(ledgerReqs).toHaveLength(2)
    expect(ledgerReqs[1].searchParams.has('before_id')).toBe(false)
  })

  await test.step('加载中点刷新：重新从第一页加载，旧请求的结果被丢弃', async () => {
    let release!: () => void
    const gate = new Promise<void>((r) => (release = r))
    let staleDone!: () => void
    const staleDelivered = new Promise<void>((r) => (staleDone = r))
    let n = 0
    await page.route(isLedger, async (route) => {
      if (++n > 1) return route.continue()
      await gate // 第一次请求挂起，直到再次点刷新
      const resp = await route.fetch()
      const body = (await resp.json()) as { items: { remark: string }[] }
      for (const it of body.items) it.remark = 'STALE'
      await route.fulfill({ response: resp, json: body })
      staleDone()
    })
    const firstReq = page.waitForRequest((r) => isLedger(new URL(r.url())))
    await page.getByTestId('wallet-refresh').click()
    await firstReq
    await expect(page.getByTestId('ledger-amount')).toHaveCount(0) // 重置后等待中
    const secondReq = page.waitForRequest((r) => isLedger(new URL(r.url())), { timeout: 5_000 }) // 远小于请求超时，必须是刷新本身发出的
    await page.getByTestId('wallet-refresh').click()
    expect(new URL((await secondReq).url()).searchParams.has('before_id')).toBe(false) // 旧请求未返回，仍重新请求第一页
    await expect(page.getByTestId('ledger-amount')).toHaveCount(20)
    await expect(page.getByTestId('ledger-remark').first()).toHaveText(ledgerApi[0].remark)
    release()
    await staleDelivered
    await settle(page)
    await expect(page.getByTestId('ledger-amount')).toHaveCount(20) // 旧请求的结果没有拼进来
    await expect(page.getByTestId('ledger-remark').filter({ hasText: 'STALE' })).toHaveCount(0)
    expect(n).toBe(2)
    await page.unroute(isLedger)
  })
})

test('C-WAL-02 订单 tab：新购 / 续费、租期、金额、状态，实例 ID 可点击', async ({ page }, info) => {
  const u = await seedUser({ balance: 100000 })
  const api = await userApi(u.phone)
  await hooks.machines(1)
  const id = await placeOrder(api)
  await hooks.agent()
  await hooks.advance(3600) // 续费单与新购单的下单时间相差 1 小时，界面上能区分
  const renew = await api.post(`/api/v1/instances/${id}/renew`, { data: { duration: '1w' }, headers: { 'Idempotency-Key': crypto.randomUUID() } })
  expect(renew.ok()).toBe(true)
  const apiOrders = ((await (await api.get('/api/v1/orders')).json()) as { items: { kind: string; created_at: string }[] }).items
  const renewAt = cstTime(apiOrders.find((o) => o.kind === 'renew')!.created_at)
  const newAt = cstTime(apiOrders.find((o) => o.kind === 'new')!.created_at)
  expect(renewAt).not.toBe(newAt)
  await loginConsole(page, u.phone)
  await nav(page, info, 'wallet')
  await page.getByRole('tab', { name: '订单' }).click()
  await expect(page).toHaveURL(/tab=orders/)
  const rows = page.getByTestId('order-row')
  await expect(rows).toHaveCount(2)
  await expect(rows.nth(0).getByTestId('order-kind')).toHaveText('续费')
  await expect(rows.nth(0).getByTestId('order-duration')).toHaveText('1 周')
  await expect(rows.nth(0).getByTestId('order-amount')).toHaveText('¥414.00')
  await expect(rows.nth(0).getByTestId('order-status')).toHaveText('已支付')
  await expect(rows.nth(0).getByTestId('order-time')).toHaveText(renewAt)
  await expect(rows.nth(0).getByTestId('order-instance')).toHaveText(id)
  await expect(rows.nth(1).getByTestId('order-kind')).toHaveText('新购')
  await expect(rows.nth(1).getByTestId('order-duration')).toHaveText('1 天')
  await expect(rows.nth(1).getByTestId('order-amount')).toHaveText('¥69.00')
  await expect(rows.nth(1).getByTestId('order-status')).toHaveText('已支付')
  await expect(rows.nth(1).getByTestId('order-time')).toHaveText(newAt)
  await rows.nth(1).getByTestId('order-instance').click()
  await expect(page).toHaveURL(new RegExp(`/instances/${id}$`))
})

test('C-WAL-03 充值记录：三种状态、待支付可继续、超过 10 条分页、?tab=recharges 刷新保持', async ({ page }, info) => {
  const u = await seedUser()
  const api = await userApi(u.phone)
  const create = async (amount = 5000) => {
    const r = await api.post('/api/v1/recharges', { data: { amount, channel: 'alipay' } })
    expect(r.status()).toBe(201)
    return ((await r.json()) as { id: string }).id
  }
  for (let i = 0; i < 9; i++) {
    // 更早的 9 笔已支付（¥10.00…¥18.00），凑满 12 笔触发分页；已支付不受同时待支付上限影响
    const id = await create(1000 + i * 100)
    expect((await api.post(`/api/v1/dev/recharges/${id}/pay`, { data: {} })).ok()).toBe(true)
    await hooks.advance(60)
  }
  const paid = await create()
  await api.post(`/api/v1/dev/recharges/${paid}/pay`, { data: {} })
  await hooks.advance(60) // 三笔充值单下单时间各不相同，列表按时间倒序的顺序确定
  const closed = await create()
  await hooks.advance(7200)
  await hooks.sweep()
  const pending = await create()
  await loginConsole(page, u.phone)
  await nav(page, info, 'wallet')
  await page.getByRole('tab', { name: '充值记录' }).click()
  await expect(page).toHaveURL(/tab=recharges/)
  const rows = page.getByTestId('recharge-row')
  await expect(rows).toHaveCount(10)
  await expect(rows.nth(0).getByTestId('recharge-status')).toHaveText('待支付')
  await expect(rows.nth(0).getByTestId('recharge-channel')).toHaveText('支付宝')
  await expect(rows.nth(0).getByTestId('recharge-amount')).toHaveText('¥50.00')
  await expect(rows.nth(1).getByTestId('recharge-status')).toHaveText('已关闭')
  await expect(rows.nth(2).getByTestId('recharge-status')).toHaveText('已支付')
  const apiRecharges = ((await (await api.get('/api/v1/recharges?page=1&page_size=10')).json()) as { items: { id: string; created_at: string }[] }).items
  expect(apiRecharges.slice(0, 3).map((r) => r.id)).toEqual([pending, closed, paid])
  for (let i = 0; i < 3; i++) await expect(rows.nth(i).getByTestId('recharge-time')).toHaveText(cstTime(apiRecharges[i].created_at))
  await expect(rows.nth(0).getByTestId('recharge-continue')).toHaveText('继续支付')
  await expect(rows.nth(1).getByTestId('recharge-continue')).toHaveCount(0)
  await expect(rows.nth(2).getByTestId('recharge-continue')).toHaveCount(0)
  await expect(rows.nth(9).getByTestId('recharge-amount')).toHaveText('¥12.00')
  const pager = page.getByTestId('wallet-recharges-pager')
  await expect(pager).toBeVisible()
  const pageReq = page.waitForRequest((r) => new URL(r.url()).pathname === '/api/v1/recharges')
  await pager.getByText('2', { exact: true }).click()
  const p2 = new URL((await pageReq).url()).searchParams
  expect([p2.get('page'), p2.get('page_size')]).toEqual(['2', '10'])
  await expect(rows).toHaveCount(2)
  await expect(rows.nth(0).getByTestId('recharge-amount')).toHaveText('¥11.00')
  await expect(rows.nth(1).getByTestId('recharge-amount')).toHaveText('¥10.00')
  await page.reload()
  await expect(page.getByRole('tab', { name: '充值记录' })).toHaveAttribute('aria-selected', 'true')
  await page.getByTestId('recharge-row').first().getByTestId('recharge-continue').click()
  await expect(page).toHaveURL(new RegExp(`/wallet/recharge/${pending}$`))
})

test('C-WAL-04 未实名用户点充值引导实名', async ({ page }, info) => {
  const u = await seedUser({ verified: false, balance: 0 })
  await loginConsole(page, u.phone)
  await nav(page, info, 'wallet')
  await page.getByTestId('wallet-recharge-btn').click()
  await expect(page.getByTestId('guide-dialog')).toHaveText('按监管要求，下单与充值前需要完成实名认证。')
  await expect(page.getByTestId('guide-action')).toHaveText('去实名')
  await page.getByTestId('guide-action').click()
  await expect(page).toHaveURL(/\/kyc\?redirect=(%2F|\/)wallet/)
})

test('C-ERR-01 服务器错误提示含请求号', async ({ page }, info) => {
  const u = await seedUser()
  await loginConsole(page, u.phone)
  await page.route('**/api/v1/wallet', (route) =>
    route.fulfill({ status: 500, contentType: 'application/json', body: JSON.stringify({ code: 'internal', message: '服务器内部错误，请稍后重试', request_id: 'req-e2e-500' }) }),
  )
  await nav(page, info, 'wallet')
  await toast(page, '服务器内部错误，请稍后重试（请求号 req-e2e-500）')
  await expect(toastLocator(page, '请求号 req-e2e-500')).toHaveCount(1)
  await expect(page.getByTestId('wallet-error')).toContainText('服务器内部错误，请稍后重试（请求号 req-e2e-500）')
  await expect(page.getByTestId('wallet-balance')).toHaveText('—')
})

test('C-ERR-02 网络错误提示', async ({ page }, info) => {
  const u = await seedUser()
  await loginConsole(page, u.phone)
  await page.route('**/api/v1/wallet', (route) => route.abort('failed'))
  await nav(page, info, 'wallet')
  await toast(page, '网络异常，请稍后重试')
  await expect(toastLocator(page, '网络异常')).toHaveCount(1)
  await expect(page.getByTestId('wallet-error')).toContainText('网络异常，请稍后重试')
})

