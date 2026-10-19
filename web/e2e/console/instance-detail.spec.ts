import { expect, test, type Page } from '@playwright/test'
import { drainTasks, fail500, trackToasts } from '../helpers/admin'
import { pauseClock } from '../helpers/clock'
import { dialog, loginConsole, placeOrder, seedUser, toast, userApi } from '../helpers/console'
import { hooks } from '../helpers/hooks'
import { advanceToProvision, bookViaApi, cancelBookingsAndDispose, privateGroup, startAfter } from '../helpers/booking'
import { instanceOf, runningInstance } from '../helpers/instances'
import { freshPublicKey, XSS, xssFired } from '../helpers/sshkeys'
import { cstTime } from '../helpers/time'

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

test('C-DET-10 详情页自动刷新：开通中 / 洗机中 10 秒轮询，倒计时归零刷新一次', async ({ page }) => {
  const u = await seedUser({ balance: 1_000_000 })
  const api = await userApi(u.phone)
  await hooks.machines(2)
  const creating = await placeOrder(api)
  const toasts = await trackToasts(page)
  await page.clock.install()
  await loginConsole(page, u.phone)

  // 记录本页对某个实例详情接口的 GET 次数，以及主文档导航次数（证明变化不是整页刷新带来的）
  const gets = new Map<string, number>()
  page.on('request', (r) => {
    const path = new URL(r.url()).pathname
    if (r.method() === 'GET' && path.startsWith('/api/v1/instances/')) gets.set(path, (gets.get(path) ?? 0) + 1)
  })
  const getsOf = (id: string) => gets.get(`/api/v1/instances/${id}`) ?? 0
  let navigations = 0
  page.on('framenavigated', (f) => {
    if (f === page.mainFrame()) navigations += 1
  })

  await test.step('开通中每 10 秒轮询，Agent 开通后不刷新页面即变为运行中，之后停止轮询', async () => {
    await page.goto(`/instances/${creating}`)
    await expect(page.getByTestId('detail-state')).toHaveText('开通中')
    await expect(page.getByTestId('detail-ssh-hint')).toBeVisible()
    await pauseClock(page)
    const navBase = navigations
    await hooks.agent()
    await drainTasks(page)
    await expect(page.getByTestId('detail-state')).toHaveText('开通中') // 后端已开通但页面尚未轮询：下面的变化只能来自 10 秒轮询
    const base = getsOf(creating)
    await page.clock.runFor(9_000)
    await drainTasks(page)
    expect(getsOf(creating)).toBe(base)
    await page.clock.runFor(1_000)
    await expect(page.getByTestId('detail-state')).toHaveText('运行中')
    await expect(page.getByTestId('detail-ssh-command')).toHaveText(`ssh ${creating}@ssh.spark.test`)
    expect(getsOf(creating)).toBe(base + 1)
    await page.clock.runFor(30_000) // 运行中不再轮询
    await drainTasks(page)
    expect(getsOf(creating)).toBe(base + 1)
    expect(navigations).toBe(navBase)
  })

  await test.step('洗机中同样轮询：刷新失败只提示一次且保留数据，Agent 洗完后变为已释放并停止轮询', async () => {
    await page.clock.resume() // 确认框的打开 / 关闭动画依赖页面定时器
    await page.getByTestId('detail-release-btn').click()
    await page.getByTestId('detail-release-ok').click()
    await expect(page.getByTestId('detail-state')).toHaveText('洗机中')
    await pauseClock(page)
    const navBase = navigations
    const detail = (url: URL) => url.pathname === `/api/v1/instances/${creating}`
    await page.route(detail, fail500('rid-det-10'), { times: 2 })
    const base = getsOf(creating)
    const failToasts = async () => (await toasts()).filter((t) => t.includes('服务器内部错误（请求号 rid-det-10）'))
    await page.clock.runFor(10_000)
    await expect.poll(failToasts).toHaveLength(1)
    await page.clock.runFor(10_000) // 同一次故障第二次失败不再提示
    await expect.poll(() => getsOf(creating)).toBe(base + 2)
    await drainTasks(page)
    expect(await failToasts()).toHaveLength(1)
    await expect(page.getByTestId('detail-id')).toHaveText(creating)
    await expect(page.getByTestId('detail-state')).toHaveText('洗机中') // 已显示的数据保留
    await hooks.agent()
    await page.clock.runFor(10_000)
    await expect(page.getByTestId('detail-state')).toHaveText('已释放')
    expect(getsOf(creating)).toBe(base + 3)
    await page.clock.runFor(30_000) // 已释放不再轮询
    await drainTasks(page)
    expect(getsOf(creating)).toBe(base + 3)
    expect(navigations).toBe(navBase)
  })

  await test.step('运行中倒计时归零只刷新一次，不刷新页面即显示已到期停机', async () => {
    const running = await placeOrder(api)
    await hooks.agent()
    const left = (await instanceOf(api, running)).remaining_seconds
    await hooks.advance(left - 20) // 只剩 20 秒
    await page.clock.resume() // 上一步冻结了页面时间：恢复后再进入新页面，避免启动阶段的定时器被卡住
    await page.goto(`/instances/${running}`)
    await expect(page.getByTestId('detail-state')).toHaveText('运行中')
    await expect(page.getByTestId('detail-remaining')).toHaveAttribute('data-remaining', /^(20|19|18)$/)
    await pauseClock(page)
    const shown = Number(await page.getByTestId('detail-remaining').getAttribute('data-remaining')) // 冻结前可能已自然走了一两秒
    const navBase = navigations
    await hooks.advance(20)
    await hooks.sweep()
    await hooks.agent()
    expect((await instanceOf(api, running)).state).toBe('stopped')
    const base = getsOf(running)
    await page.clock.runFor((shown - 1) * 1000)
    await drainTasks(page)
    await expect(page.getByTestId('detail-remaining')).toHaveAttribute('data-remaining', '1')
    await expect(page.getByTestId('detail-state')).toHaveText('运行中')
    expect(getsOf(running)).toBe(base)
    await page.clock.runFor(1_000)
    await expect(page.getByTestId('detail-state')).toHaveText('已到期停机')
    await expect(page.getByTestId('detail-ssh-hint')).toHaveText('实例运行中才能连接')
    await expect(page.getByTestId('detail-retain')).toBeVisible()
    expect(getsOf(running)).toBe(base + 1)
    await page.clock.runFor(30_000) // 停机不轮询，归零只触发一次
    await drainTasks(page)
    expect(getsOf(running)).toBe(base + 1)
    expect(navigations).toBe(navBase)
  })

  await test.step('归零时服务器尚未清扫（仍是运行中、剩余 0 秒）：每 10 秒再查，清扫停机后显示已到期停机并停止轮询', async () => {
    await hooks.machines(1)
    const late = await placeOrder(api)
    await hooks.agent()
    const left = (await instanceOf(api, late)).remaining_seconds
    await hooks.advance(left - 20)
    await page.clock.resume()
    await page.goto(`/instances/${late}`)
    await expect(page.getByTestId('detail-state')).toHaveText('运行中')
    await expect(page.getByTestId('detail-remaining')).toHaveAttribute('data-remaining', /^(20|19|18)$/)
    await pauseClock(page)
    const shown = Number(await page.getByTestId('detail-remaining').getAttribute('data-remaining'))
    const navBase = navigations
    await hooks.advance(20) // 到期但不清扫：后端仍报 running、remaining_seconds 0
    const atZero = await instanceOf(api, late)
    expect([atZero.state, atZero.remaining_seconds]).toEqual(['running', 0])
    const base = getsOf(late)
    await page.clock.runFor(shown * 1000)
    await expect.poll(() => getsOf(late)).toBeGreaterThanOrEqual(base + 1) // 归零刷新一次
    await drainTasks(page)
    await expect(page.getByTestId('detail-state')).toHaveText('运行中')
    await expect(page.getByTestId('detail-remaining')).toHaveAttribute('data-remaining', '0')
    // 轮询定时器从进入页面起每 10 秒一跳，归零后同一次 runFor 的零头里可能已经跳过一次：以此刻为基准
    const c0 = getsOf(late)
    expect(c0 - base).toBeLessThanOrEqual(2)
    await page.clock.runFor(10_000) // 仍是运行中 + 0 秒：任意 10 秒内恰好再查一次（不会更快）
    await expect.poll(() => getsOf(late)).toBe(c0 + 1)
    await drainTasks(page)
    expect(getsOf(late)).toBe(c0 + 1)
    await expect(page.getByTestId('detail-state')).toHaveText('运行中')
    await hooks.sweep()
    await hooks.agent()
    expect((await instanceOf(api, late)).state).toBe('stopped')
    await page.clock.runFor(10_000)
    await expect(page.getByTestId('detail-state')).toHaveText('已到期停机')
    await expect(page.getByTestId('detail-ssh-hint')).toHaveText('实例运行中才能连接')
    expect(getsOf(late)).toBe(c0 + 2)
    await page.clock.runFor(30_000) // 停机后不再轮询
    await drainTasks(page)
    expect(getsOf(late)).toBe(c0 + 2)
    expect(navigations).toBe(navBase)
  })
})

test('C-DET-11 重置密码：新密码只在确认框显示一次、关闭即清除；新密码能登录，旧密码不能', async ({ page }) => {
  const u = await seedUser()
  const { id } = await runningInstance(u.phone)
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  await page.getByTestId('detail-password-show').click()
  await expect(page.getByTestId('detail-password')).toHaveText(/^[A-Za-z0-9]{16}$/)
  const old = (await page.getByTestId('detail-password').textContent())!
  expect((await hooks.ssh({ instance_id: id, password: old, command: 'echo ok' })).output).toBe('ok\n')
  await page.getByTestId('detail-password-reset').click()
  await expect(page.getByTestId('detail-reset-password')).toHaveCount(0) // 确认前只有说明文字
  await page.getByTestId('detail-reset-ok').click()
  await toast(page, '密码已重置，请复制保存')
  await expect(page.getByTestId('detail-reset-password')).toHaveText(/^[A-Za-z0-9]{16}$/)
  const fresh = (await page.getByTestId('detail-reset-password').textContent())!
  expect(fresh).not.toBe(old)
  await expect(page.getByTestId('detail-password')).toHaveText('••••••••') // 页面上显示过的旧密码已清掉
  await page.getByTestId('detail-reset-done').click()
  await expect(page.getByTestId('detail-reset-password')).toHaveCount(0)
  // 再次打开：回到确认步骤，上次的新密码不再出现
  await page.getByTestId('detail-password-reset').click()
  await expect(page.getByTestId('detail-reset-ok')).toBeVisible()
  await expect(page.getByTestId('detail-reset-password')).toHaveCount(0)
  await page.getByTestId('detail-reset-cancel').click()
  await expect(page.getByTestId('detail-reset-ok')).toHaveCount(0)
  expect((await hooks.ssh({ instance_id: id, password: fresh, command: 'echo ok' })).output).toBe('ok\n')
  const denied = await hooks.ssh({ instance_id: id, password: old, command: 'echo ok' })
  expect(denied.error).not.toBe('')
  // 「显示」取到的是新密码
  await page.getByTestId('detail-password-show').click()
  await expect(page.getByTestId('detail-password')).toHaveText(fresh)
})

test('C-DET-12 关闭密码登录：无公钥时提示且开关回弹；有公钥后关闭成功且密码被拒', async ({ page }) => {
  const u = await seedUser()
  const { api, id } = await runningInstance(u.phone)
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  const sw = page.getByTestId('detail-password-auth').getByRole('switch')
  await expect(sw).toHaveAttribute('aria-checked', 'true')
  await page.getByTestId('detail-password-auth').click()
  await toast(page, '关闭密码登录前请先在「我的 → SSH 公钥」添加公钥')
  await expect(sw).toHaveAttribute('aria-checked', 'true')
  expect((await instanceOf(api, id)).password_auth).toBe(true)
  const added = await api.post('/api/v1/ssh-keys', { data: { public_key: freshPublicKey('e2e') } })
  expect(added.status()).toBe(201)
  await page.getByTestId('detail-password-show').click()
  await expect(page.getByTestId('detail-password')).toHaveText(/^[A-Za-z0-9]{16}$/)
  const pw = (await page.getByTestId('detail-password').textContent())!
  await page.getByTestId('detail-password-auth').click()
  await toast(page, '已关闭密码登录')
  await expect(sw).toHaveAttribute('aria-checked', 'false')
  const denied = await hooks.ssh({ instance_id: id, password: pw, command: 'echo ok' })
  expect(denied.error).not.toBe('')
  // 再打开：密码又能登录
  await page.getByTestId('detail-password-auth').click()
  await toast(page, '已打开密码登录')
  await expect(sw).toHaveAttribute('aria-checked', 'true')
  expect((await hooks.ssh({ instance_id: id, password: pw, command: 'echo ok' })).output).toBe('ok\n')
})

test('C-DET-13 添加端口显示网址与备注（HTML 按文本显示）；非法端口与 22 被拦截', async ({ page }) => {
  const u = await seedUser()
  const { id } = await runningInstance(u.phone)
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  await page.getByTestId('detail-port-input').fill('22')
  await page.getByTestId('detail-port-add').click()
  await expect(page.getByTestId('detail-port-error')).toContainText('22 端口')
  await page.getByTestId('detail-port-input').fill('70000')
  await page.getByTestId('detail-port-add').click()
  await expect(page.getByTestId('detail-port-error')).toHaveText('端口必须是 1～65535 之间的整数')
  await expect(page.getByTestId('detail-port-row')).toHaveCount(0)
  await page.getByTestId('detail-port-input').fill('8888')
  await page.getByTestId('detail-port-note').fill(XSS)
  await page.getByTestId('detail-port-add').click()
  await expect(page.getByTestId('detail-port-row')).toHaveCount(1)
  await expect(page.getByTestId('detail-port-error')).toHaveCount(0)
  await expect(page.getByTestId('detail-port-url')).toHaveText(`http://${id}-8888.spark.test:18080/`)
  await expect(page.getByTestId('detail-port-visibility')).toHaveText('私有')
  await expect(page.getByTestId('detail-port-row')).toContainText(XSS)
  await expect(page.getByTestId('detail-port-row').locator('img')).toHaveCount(0)
  expect(await xssFired(page)).toBe(false)
  await expect(page.getByTestId('detail-port-public-warn')).toHaveCount(0)
  // 重复添加：后端拒绝，原因显示在输入框下
  await page.getByTestId('detail-port-input').fill('8888')
  await page.getByTestId('detail-port-add').click()
  await expect(page.getByTestId('detail-port-error')).toHaveText('该端口已添加')
})

test('C-DET-14 设为公开需二次确认（含警告文案）；取消不改；公开后显示公开警告', async ({ page }) => {
  const u = await seedUser()
  const { api, id } = await runningInstance(u.phone)
  await api.post(`/api/v1/instances/${id}/ports`, { data: { port: 8888 } })
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  await page.getByTestId('detail-port-toggle').click()
  await expect(page.getByTestId('detail-port-public-text')).toContainText('任何人')
  await page.getByTestId('detail-port-public-cancel').click()
  await expect(page.getByTestId('detail-port-visibility')).toHaveText('私有')
  await expect(page.getByTestId('detail-port-public-warn')).toHaveCount(0)
  await page.getByTestId('detail-port-toggle').click()
  await page.getByTestId('detail-port-public-ok').click()
  await expect(page.getByTestId('detail-port-visibility')).toHaveText('公开')
  await expect(page.getByTestId('detail-port-toggle')).toHaveText('设为私有')
  await expect(page.getByTestId('detail-port-public-warn')).toContainText('任何人都能访问')
  // 改回私有不需要确认
  await page.getByTestId('detail-port-toggle').click()
  await expect(page.getByTestId('detail-port-visibility')).toHaveText('私有')
  await expect(page.getByTestId('detail-port-public-warn')).toHaveCount(0)
})

test('C-DET-15 端口上限 5 个：输入框消失并提示', async ({ page }) => {
  const u = await seedUser()
  const { api, id } = await runningInstance(u.phone)
  for (const p of [8001, 8002, 8003, 8004, 8005]) expect((await api.post(`/api/v1/instances/${id}/ports`, { data: { port: p } })).status()).toBe(201)
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  await expect(page.getByTestId('detail-port-row')).toHaveCount(5)
  await expect(page.getByTestId('detail-port-input')).toHaveCount(0)
  await expect(page.getByTestId('detail-port-limit')).toHaveText('每个实例最多开放 5 个端口')
})

test('C-DET-16 删除端口', async ({ page }) => {
  const u = await seedUser()
  const { api, id } = await runningInstance(u.phone)
  await api.post(`/api/v1/instances/${id}/ports`, { data: { port: 8888 } })
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  await expect(page.getByTestId('detail-port-row')).toHaveCount(1)
  await page.getByTestId('detail-port-delete').click()
  await expect(page.getByTestId('detail-port-row')).toHaveCount(0)
  const list = (await (await api.get(`/api/v1/instances/${id}/ports`)).json()) as { items: unknown[] }
  expect(list.items).toHaveLength(0)
})

// ---- 计划 6：预订、取消、续费上限、自动续费、时间线（预订都在本用例自己的机组里，见 helpers/booking.ts privateGroup）----

const HOUR = 3600e3
const at = (d: Date, offsetMs: number) => cstTime(new Date(d.getTime() + offsetMs).toISOString())

/** 在自己的机组里即时下单一台并让 Agent 开通成功（钉在该组唯一的机器上）。 */
async function runningInGroup(phone: string) {
  const g = await privateGroup(1)
  const api = await userApi(phone)
  const { ids } = await bookViaApi(api, { groupId: g.id, machineId: g.machineIds[0] })
  await hooks.agent()
  return { api, id: ids[0], g }
}

test('C-DET-17 预订详情：机组 / 机器 / 开始 / 结束 / 批次，无释放按钮', async ({ page }) => {
  const g = await privateGroup(1)
  const u = await seedUser({ balance: 1_000_000 })
  const api = await userApi(u.phone)
  const s = await startAfter(30)
  const { ids, batchId } = await bookViaApi(api, { startAt: s.iso, groupId: g.id })
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${ids[0]}`)
  await expect(page.getByTestId('detail-state')).toHaveText('已预订')
  await expect(page.getByTestId('detail-group')).toHaveText(g.name)
  await expect(page.getByTestId('detail-machine')).toHaveText(`机器 #${g.machineIds[0]}`)
  await expect(page.getByTestId('detail-start')).toHaveText(cstTime(s.iso))
  await expect(page.getByTestId('detail-end')).toHaveText(at(s.date, 24 * HOUR))
  await expect(page.getByTestId('detail-batch')).toHaveText(batchId!)
  await expect(page.getByTestId('detail-remaining')).toHaveText('—')
  await expect(page.getByTestId('detail-release-btn')).toHaveCount(0)
  await expect(page.getByTestId('detail-cancel-btn')).toBeVisible()
  await expect(page.getByTestId('detail-renew-btn')).toHaveText('续费（加时）')
})

test('C-DET-18 取消（距开始 ≥ 24h，已过冷静期）：全额退款', async ({ page }) => {
  const g = await privateGroup(1)
  const u = await seedUser({ balance: 100000 })
  const api = await userApi(u.phone)
  const s = await startAfter(40)
  const { ids } = await bookViaApi(api, { startAt: s.iso, groupId: g.id })
  await hooks.advance(16 * 60) // 过冷静期，仍 ≥ 24h
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${ids[0]}`)
  await page.getByTestId('detail-cancel-btn').click()
  await expect(page.getByTestId('detail-cancel-text')).toHaveText(`将退款 ¥69.00（100%），可取消至 ${at(s.date, -HOUR)}。开通后将不能取消。`)
  await page.getByTestId('detail-cancel-ok').click()
  await toast(page, '已取消预订，退款已退回余额')
  await expect(page.getByTestId('detail-state')).toHaveText('已取消')
  await expect(page.getByTestId('detail-cancelled-hint')).toBeVisible()
  await expect(page.getByTestId('detail-cancel-btn')).toHaveCount(0)
  await expect(page.getByTestId('detail-event').filter({ hasText: '已取消预订，已退款 ¥69.00' })).toHaveAttribute('data-kind', 'cancelled')
  expect((await (await api.get('/api/v1/wallet')).json()).balance).toBe(100000)
})

test('C-DET-19 取消（距开始 < 24h，已过冷静期）：退 50%', async ({ page }) => {
  const g = await privateGroup(1)
  const u = await seedUser({ balance: 100000 })
  const api = await userApi(u.phone)
  const s = await startAfter(3)
  const { ids } = await bookViaApi(api, { startAt: s.iso, groupId: g.id })
  await hooks.advance(16 * 60)
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${ids[0]}`)
  await page.getByTestId('detail-cancel-btn').click()
  await expect(page.getByTestId('detail-cancel-text')).toHaveText(`将退款 ¥34.50（50%），可取消至 ${at(s.date, -HOUR)}。开通后将不能取消。`)
  await page.getByTestId('detail-cancel-ok').click()
  await toast(page, '已取消预订，退款已退回余额')
  await expect(page.getByTestId('detail-state')).toHaveText('已取消')
  expect((await (await api.get('/api/v1/wallet')).json()).balance).toBe(100000 - 6900 + 3450)
})

test('C-DET-20 开通后取消按钮消失，续费仍可用', async ({ page }) => {
  const g = await privateGroup(1)
  const u = await seedUser({ balance: 1_000_000 })
  const api = await userApi(u.phone)
  const s = await startAfter(3)
  const { ids } = await bookViaApi(api, { startAt: s.iso, groupId: g.id })
  await advanceToProvision(s.date)
  await hooks.sweep()
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${ids[0]}`)
  await expect(page.getByTestId('detail-state')).toHaveText('开通中')
  await expect(page.getByTestId('detail-cancel-btn')).toHaveCount(0)
  await expect(page.getByTestId('detail-renew-btn')).toHaveText('续费')
  await hooks.agent()
  await page.reload()
  await expect(page.getByTestId('detail-state')).toHaveText('运行中')
  await expect(page.getByTestId('detail-cancel-btn')).toHaveCount(0)
  await expect(page.getByTestId('detail-release-btn')).toBeVisible()
})

test('C-DET-21 续费表单显示可续至并禁用超限档位；接口提交超限返回 renew_blocked', async ({ page }) => {
  const u = await seedUser({ balance: 1_000_000 })
  const { api, id, g } = await runningInGroup(u.phone)
  const s = await startAfter(72)
  const { ids } = await bookViaApi(api, { startAt: s.iso, groupId: g.id, machineId: g.machineIds[0] })
  const until = at(s.date, -8 * HOUR) // 下一预订开通前 1 小时、再留 7 小时保留 + 洗机
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  await page.getByTestId('detail-renew-btn').click()
  await expect(page.getByTestId('detail-renew-until')).toHaveText(`可续至 ${until}（机器之后已有预订）`)
  // testid 在 el-radio-button 的 <label> 上，disabled 落在内部 <input>
  await expect(page.getByTestId('detail-renew-duration-1w').locator('input')).toBeDisabled()
  await expect(page.getByTestId('detail-renew-duration-1w')).toContainText('超出可续时间')
  await expect(page.getByTestId('detail-renew-duration-1m').locator('input')).toBeDisabled()
  await expect(page.getByTestId('detail-renew-duration-1d').locator('input')).toBeEnabled()
  await expect(page.getByTestId('detail-renew-duration-1d').locator('input')).toBeChecked()
  const r = await api.post(`/api/v1/instances/${id}/renew`, { data: { duration: '1w', quantity: 1 }, headers: { 'Idempotency-Key': crypto.randomUUID() } })
  expect(r.status()).toBe(409)
  expect((await r.json()) as { code: string; message: string }).toMatchObject({
    code: 'renew_blocked',
    message: `该机器自 ${cstTime(s.iso)} 起已有预订，本次最多可续至 ${until}`,
  })
  await cancelBookingsAndDispose(api, ids)
})

test('C-DET-22 自动续费开关；余额不足时失败提示', async ({ page }) => {
  const u = await seedUser({ balance: 7000 })
  const { api, id } = await runningInGroup(u.phone) // 余额剩 1 元
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  await page.getByTestId('detail-auto-renew').click()
  await toast(page, '已开启自动续费')
  expect((await instanceOf(api, id)).auto_renew).toBe(true)
  await hooks.advance(23 * 3600)
  await hooks.sweep()
  await page.reload()
  await expect(page.getByTestId('detail-auto-renew-failed')).toContainText('自动续费失败：余额不足，请充值后系统会自动重试')
  await expect(page.getByTestId('detail-state')).toHaveText('运行中')
  await expect(page.getByTestId('detail-event').filter({ hasText: '自动续费失败：余额不足' })).toHaveAttribute('data-kind', 'auto_renew_failed')
  expect((await (await api.get('/api/v1/wallet')).json()).balance).toBe(100)
})

test('C-DET-23 自动续费被预订挡住时降档，动态显示实际档位', async ({ page }) => {
  const u = await seedUser({ balance: 1_000_000 })
  const { api, id, g } = await runningInGroup(u.phone) // expires ≈ now + 1d
  const put = await api.put(`/api/v1/instances/${id}/auto-renew`, { data: { enabled: true, duration: '1m' } })
  expect(put.status()).toBe(200)
  // 下一预订开始 = now + 1d + 8d + 8h（整点）→ 可续至 ≈ now + 1d + 8d：1 月放不下、1 周放得下
  const s = await startAfter(24 + 8 * 24 + 8)
  const { ids } = await bookViaApi(api, { startAt: s.iso, groupId: g.id, machineId: g.machineIds[0] })
  const before = (await (await api.get('/api/v1/wallet')).json()).balance as number
  await hooks.advance(23 * 3600)
  await hooks.sweep()
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  await expect(page.getByTestId('detail-event').filter({ hasText: '自动续费成功（1 周）' })).toHaveAttribute('data-kind', 'auto_renew')
  expect(before - ((await (await api.get('/api/v1/wallet')).json()).balance as number)).toBe(41400) // 按 1 周扣款
  await cancelBookingsAndDispose(api, ids)
})

test('C-DET-24 时间线：预订下单、开始开通、开通成功（新到旧）', async ({ page }) => {
  const g = await privateGroup(1)
  const u = await seedUser({ balance: 1_000_000 })
  const api = await userApi(u.phone)
  const s = await startAfter(3)
  const { ids } = await bookViaApi(api, { startAt: s.iso, groupId: g.id })
  await advanceToProvision(s.date)
  await hooks.sweep()
  await hooks.agent()
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${ids[0]}`)
  await expect(page.getByTestId('detail-state')).toHaveText('运行中')
  const events = page.getByTestId('detail-event')
  await expect(events).toHaveCount(3)
  await expect(events.nth(0)).toHaveAttribute('data-kind', 'created')
  await expect(events.nth(0)).toHaveText('开通成功')
  await expect(events.nth(1)).toHaveAttribute('data-kind', 'provision')
  await expect(events.nth(1)).toHaveText('开始开通')
  await expect(events.nth(2)).toHaveAttribute('data-kind', 'order')
  await expect(events.nth(2)).toContainText('预订下单')
})

test('C-DET-25 续费数量清空后回到 1，按 1 个单位扣款', async ({ page }) => {
  const u = await seedUser({ balance: 1_000_000 })
  const { api, id } = await runningInGroup(u.phone)
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  await page.getByTestId('detail-renew-btn').click()
  const qty = page.getByTestId('detail-renew-quantity').locator('input')
  await qty.fill('3')
  await qty.blur()
  await expect(page.getByTestId('detail-renew-price')).toHaveText('¥207.00')
  await qty.fill('')
  await qty.blur()
  await expect(qty).toHaveValue('1')
  await expect(page.getByTestId('detail-renew-price')).toHaveText('¥69.00')
  await expect(page.getByTestId('detail-renew-ok')).toBeEnabled()
  const before = (await (await api.get('/api/v1/wallet')).json()).balance as number
  await page.getByTestId('detail-renew-ok').click()
  await toast(page, '续费成功')
  expect(before - ((await (await api.get('/api/v1/wallet')).json()).balance as number)).toBe(6900)
})
