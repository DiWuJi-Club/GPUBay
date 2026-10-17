import { expect, test } from '@playwright/test'
import { loginConsole, nav, placeOrder, seedUser, toast, userApi } from '../helpers/console'
import { hooks } from '../helpers/hooks'
import { cstTime } from '../helpers/time'

test('C-WAL-01 余额与流水：类型中文、正负着色、变动后余额、滚动加载第二页', async ({ page }, info) => {
  const u = await seedUser({ balance: 106900, ledger_rows: 25 }) // 25 条 +¥0.01、1 条 +¥1,068.75，再下单 -¥69.00 → 27 条、余额 ¥1,000.00
  const api = await userApi(u.phone)
  await hooks.machines(1)
  await placeOrder(api)
  const ledgerIds = ((await (await api.get('/api/v1/wallet/ledger?limit=100')).json()) as { items: { id: number }[] }).items.map((r) => r.id)
  expect(ledgerIds).toHaveLength(27)
  const ledgerReqs: URL[] = []
  page.on('request', (r) => {
    const url = new URL(r.url())
    if (url.pathname === '/api/v1/wallet/ledger') ledgerReqs.push(url)
  })
  await loginConsole(page, u.phone)
  await nav(page, info, 'wallet')
  await expect(page.getByTestId('wallet-balance')).toHaveText('¥1,000.00')
  await expect(page.getByTestId('ledger-amount')).toHaveCount(20)
  expect(ledgerReqs).toHaveLength(1) // 第一页加载完、未滚动时不会自动拉第二页
  expect(ledgerReqs[0].searchParams.get('limit')).toBe('20')
  expect(ledgerReqs[0].searchParams.has('before_id')).toBe(false)
  const first = page.getByTestId('ledger-amount').first()
  await expect(first).toHaveText('-¥69.00')
  await expect(first).toHaveClass(/money-minus/)
  await expect(page.getByTestId('ledger-kind').first()).toHaveText('扣款')
  await expect(page.getByTestId('ledger-after').first()).toHaveText('¥1,000.00')
  await expect(page.getByTestId('ledger-amount').nth(1)).toHaveText('+¥1,068.75')
  await expect(page.getByTestId('ledger-amount').nth(1)).toHaveClass(/money-plus/)
  await expect(page.getByTestId('ledger-kind').nth(1)).toHaveText('调账')
  await page.getByTestId('ledger-sentinel').scrollIntoViewIfNeeded()
  await expect(page.getByTestId('ledger-amount')).toHaveCount(27)
  await expect(page.getByTestId('ledger-end')).toHaveText('没有更多了')
  await expect(page.getByTestId('ledger-sentinel')).toHaveCount(0)
  expect(ledgerReqs).toHaveLength(2) // 第二页按游标取：before_id = 第一页最后一条
  expect(ledgerReqs[1].searchParams.get('before_id')).toBe(String(ledgerIds[19]))
  expect(ledgerReqs[1].searchParams.get('limit')).toBe('20')
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

test('C-WAL-03 充值记录：三种状态、待支付可继续、?tab=recharges 刷新保持', async ({ page }, info) => {
  const u = await seedUser()
  const api = await userApi(u.phone)
  const create = async () => ((await (await api.post('/api/v1/recharges', { data: { amount: 5000, channel: 'alipay' } })).json()) as { id: string }).id
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
  await expect(rows).toHaveCount(3)
  await expect(rows.nth(0).getByTestId('recharge-status')).toHaveText('待支付')
  await expect(rows.nth(0).getByTestId('recharge-channel')).toHaveText('支付宝')
  await expect(rows.nth(0).getByTestId('recharge-amount')).toHaveText('¥50.00')
  await expect(rows.nth(1).getByTestId('recharge-status')).toHaveText('已关闭')
  await expect(rows.nth(2).getByTestId('recharge-status')).toHaveText('已支付')
  await expect(rows.nth(1).getByTestId('recharge-continue')).toHaveCount(0)
  await page.reload()
  await expect(page.getByRole('tab', { name: '充值记录' })).toHaveAttribute('aria-selected', 'true')
  await page.getByTestId('recharge-row').first().getByTestId('recharge-continue').click()
  await expect(page).toHaveURL(new RegExp(`/wallet/recharge/${pending}$`))
  void closed
})

test('C-WAL-04 未实名用户点充值引导实名', async ({ page }, info) => {
  const u = await seedUser({ verified: false, balance: 0 })
  await loginConsole(page, u.phone)
  await nav(page, info, 'wallet')
  await page.getByTestId('wallet-recharge-btn').click()
  await expect(page.getByRole('dialog')).toContainText('请先完成实名认证')
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
  await expect(page.getByTestId('wallet-error')).toContainText('请求号 req-e2e-500')
})

test('C-ERR-02 网络错误提示', async ({ page }, info) => {
  const u = await seedUser()
  await loginConsole(page, u.phone)
  await page.route('**/api/v1/wallet', (route) => route.abort('failed'))
  await nav(page, info, 'wallet')
  await toast(page, '网络异常，请稍后重试')
})
