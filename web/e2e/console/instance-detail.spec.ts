import { expect, test, type Page } from '@playwright/test'
import { dialog, loginConsole, placeOrder, seedUser, toast, userApi } from '../helpers/console'
import { hooks } from '../helpers/hooks'
import { instanceOf, runningInstance } from '../helpers/instances'

const clipboard = (page: Page) => page.evaluate(() => navigator.clipboard.readText())

test('C-DET-01 SSH 命令显示与复制；非运行中显示提示', async ({ page }) => {
  const u = await seedUser()
  const { api, id } = await runningInstance(u.phone)
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  await expect(page.getByTestId('detail-ssh-command')).toHaveText(`ssh ${id}@ssh.spark.test`)
  await page.getByTestId('detail-ssh-copy').click()
  await toast(page, '已复制')
  expect(await clipboard(page)).toBe(`ssh ${id}@ssh.spark.test`)
  await hooks.machines(1)
  const creating = await placeOrder(api)
  await page.goto(`/instances/${creating}`)
  await expect(page.getByTestId('detail-ssh-hint')).toHaveText('实例运行中才能连接')
  await expect(page.getByTestId('detail-ssh-command')).toHaveCount(0)
})

test('C-DET-02 密码默认隐藏，点显示才请求，再次进入仍隐藏', async ({ page }) => {
  const u = await seedUser()
  const { id } = await runningInstance(u.phone)
  let passwordRequests = 0
  page.on('request', (r) => {
    if (r.url().endsWith(`/api/v1/instances/${id}/password`)) passwordRequests += 1
  })
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  await expect(page.getByTestId('detail-password')).toHaveText('••••••••')
  expect(passwordRequests).toBe(0)
  await page.getByTestId('detail-password-show').click()
  await expect(page.getByTestId('detail-password')).toHaveText(/^[A-Za-z0-9!@#$%^&*_+\-=?]{16}$/)
  expect(passwordRequests).toBe(1)
  const pw = await page.getByTestId('detail-password').textContent()
  await page.getByTestId('detail-password-copy').click()
  expect(await clipboard(page)).toBe(pw)
  await page.reload()
  await expect(page.getByTestId('detail-password')).toHaveText('••••••••')
  expect(passwordRequests).toBe(1)
})

test('C-DET-03 运行中续费 1 周：价格、剩余时长、余额与续费订单', async ({ page }) => {
  const u = await seedUser({ balance: 100000 })
  const { api, id } = await runningInstance(u.phone)
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  await page.getByTestId('detail-renew-btn').click()
  await expect(dialog(page)).toBeVisible()
  await page.getByTestId('detail-renew-duration-1w').click()
  await expect(page.getByTestId('detail-renew-price')).toHaveText('¥414.00')
  await expect(page.getByTestId('detail-renew-balance')).toHaveText('¥931.00')
  await expect(page.getByTestId('detail-renew-after')).toHaveText('¥517.00')
  // 续费请求必须带 Idempotency-Key（crypto.randomUUID，36 字符）
  const renewReq = page.waitForRequest((r) => r.method() === 'POST' && r.url().endsWith(`/api/v1/instances/${id}/renew`))
  await page.getByTestId('detail-renew-ok').click()
  const key = (await renewReq).headers()['idempotency-key']
  expect(key).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/)
  await toast(page, '续费成功')
  const remaining = Number(await page.getByTestId('detail-remaining').getAttribute('data-remaining'))
  expect(remaining).toBeGreaterThan(7 * 86400 + 23 * 3600)
  expect(remaining).toBeLessThanOrEqual(8 * 86400)
  expect((await (await api.get('/api/v1/wallet')).json()).balance).toBe(100000 - 6900 - 41400)
  const orders = (await (await api.get('/api/v1/orders')).json()).items as { kind: string; instance_id: string; amount: number }[]
  expect(orders.some((o) => o.kind === 'renew' && o.instance_id === id && o.amount === 41400)).toBe(true)
})

test('C-DET-04 停机实例续费后提示重新启动，Agent 完成后运行中', async ({ page }) => {
  const u = await seedUser()
  const { api, id } = await runningInstance(u.phone)
  await hooks.advance(86400)
  await hooks.sweep()
  await hooks.agent()
  expect((await instanceOf(api, id)).state).toBe('stopped')
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  await expect(page.getByTestId('detail-state')).toHaveText('已到期停机')
  await page.getByTestId('detail-renew-btn').click()
  await page.getByTestId('detail-renew-duration-1d').click()
  await page.getByTestId('detail-renew-ok').click()
  await toast(page, '正在重新启动')
  await expect(page.getByTestId('detail-pending')).toHaveText('1 天')
  await hooks.agent()
  await page.reload()
  await expect(page.getByTestId('detail-state')).toHaveText('运行中')
})

test('C-DET-05 续费余额不足时按钮变为去充值', async ({ page }) => {
  const u = await seedUser({ balance: 10000 })
  const { id } = await runningInstance(u.phone) // 扣 ¥69 后余额 ¥31
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  await page.getByTestId('detail-renew-btn').click()
  await page.getByTestId('detail-renew-duration-1d').click()
  await expect(page.getByTestId('detail-renew-after')).toContainText('余额不足')
  await expect(page.getByTestId('detail-renew-ok')).toHaveText('去充值')
  await page.getByTestId('detail-renew-ok').click()
  await expect(page).toHaveURL(/\/wallet\/recharge\?redirect=(%2F|\/)instances(%2F|\/)ins-/)
})

test('C-DET-06 释放确认：取消不发请求，确认后洗机中；双击只发一个请求', async ({ page }) => {
  const u = await seedUser()
  const { id } = await runningInstance(u.phone)
  let releaseRequests = 0
  page.on('request', (r) => {
    if (r.method() === 'POST' && r.url().endsWith(`/api/v1/instances/${id}/release`)) releaseRequests += 1
  })
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  await page.getByTestId('detail-release-btn').click()
  await expect(page.getByTestId('detail-release-text')).toHaveText('释放后数据将被清除，已付费用不退，确定释放吗？')
  await expect(page.getByTestId('detail-release-warning')).toHaveCount(0)
  await expect(page.getByTestId('detail-release-ok')).toHaveText('确定释放')
  await page.getByTestId('detail-release-cancel').click()
  await expect(dialog(page)).toBeHidden()
  expect(releaseRequests).toBe(0)
  await expect(page.getByTestId('detail-state')).toHaveText('运行中')

  await page.getByTestId('detail-release-btn').click()
  await page.getByTestId('detail-release-ok').dblclick() // Review Focus 5：请求进行中按钮 loading 禁用
  await toast(page, '已提交释放')
  await expect(page.getByTestId('detail-state')).toHaveText('洗机中')
  expect(releaseRequests).toBe(1)
  await expect(page.getByTestId('detail-renew-btn')).toHaveCount(0)
  await expect(page.getByTestId('detail-release-btn')).toHaveCount(0)
})

test('C-DET-07 停机且有待开始续费时，释放确认框追加作废提示', async ({ page }) => {
  const u = await seedUser()
  const { api, id } = await runningInstance(u.phone)
  await hooks.advance(86400)
  await hooks.sweep()
  await hooks.agent()
  const renew = await api.post(`/api/v1/instances/${id}/renew`, { data: { duration: '1w' }, headers: { 'Idempotency-Key': crypto.randomUUID() } })
  expect(renew.ok()).toBe(true)
  expect((await instanceOf(api, id)).pending_seconds).toBe(7 * 86400)
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  await page.getByTestId('detail-release-btn').click()
  await expect(page.getByTestId('detail-release-warning')).toHaveText('该实例有已付费但尚未开始计时的 7 天，释放后这部分时长也会作废')
})

test('C-DET-08 开通失败：提示退款、余额恢复、无操作按钮', async ({ page }) => {
  const u = await seedUser({ balance: 100000 })
  const api = await userApi(u.phone)
  await hooks.machines(1)
  const id = await placeOrder(api)
  await hooks.agent({ fail: true })
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  await expect(page.getByTestId('detail-state')).toHaveText('开通失败')
  await expect(page.getByTestId('detail-failed-hint')).toContainText('费用已原路退回余额')
  await expect(page.getByTestId('detail-renew-btn')).toHaveCount(0)
  await expect(page.getByTestId('detail-release-btn')).toHaveCount(0)
  expect((await (await api.get('/api/v1/wallet')).json()).balance).toBe(100000)
})

test('C-DET-09 按钮与状态匹配', async ({ page }) => {
  const u = await seedUser({ balance: 1_000_000 })
  const api = await userApi(u.phone)
  await hooks.machines(6)
  const stopped = await placeOrder(api)
  await hooks.agent()
  await hooks.advance(86400)
  await hooks.sweep()
  await hooks.agent()
  const released = await placeOrder(api)
  await hooks.agent()
  await api.post(`/api/v1/instances/${released}/release`, { data: {} })
  await hooks.agent()
  const failed = await placeOrder(api)
  await hooks.agent({ fail: true })
  const running = await placeOrder(api)
  await hooks.agent()
  const wiping = await placeOrder(api)
  await hooks.agent()
  await api.post(`/api/v1/instances/${wiping}/release`, { data: {} })
  const creating = await placeOrder(api)

  await loginConsole(page, u.phone)
  const matrix: [string, boolean, boolean][] = [
    [creating, true, false], [running, true, true], [stopped, true, true], [wiping, false, false], [released, false, false], [failed, false, false],
  ]
  for (const [id, renew, release] of matrix) {
    await page.goto(`/instances/${id}`)
    await expect(page.getByTestId('detail-id')).toHaveText(id)
    await expect(page.getByTestId('detail-renew-btn')).toHaveCount(renew ? 1 : 0)
    await expect(page.getByTestId('detail-release-btn')).toHaveCount(release ? 1 : 0)
  }
})
