import { expect, test } from '@playwright/test'
import { adminApi, adminPost, createAdmin } from '../helpers/admin'
import { advanceToProvision, bookViaApi, privateGroup, startAfter } from '../helpers/booking'
import { loginConsole, nav, placeOrder, seedUser, toast, userApi } from '../helpers/console'
import { hooks } from '../helpers/hooks'
import { instanceOf, instanceRow, runningInstance, stateLabels } from '../helpers/instances'
import { cstTime } from '../helpers/time'

test('C-INS-01 六种状态同时展示，标签文案与颜色正确，按创建时间倒序', async ({ page }, info) => {
  const u = await seedUser({ balance: 1_000_000 })
  const api = await userApi(u.phone)
  await hooks.machines(6)
  // 每一步之间推进 1 秒，让 created_at 严格递增；agent() 作用于全部机器，所以按「不会被后续 agent 打扰」的顺序构造
  const next = async () => {
    await hooks.advance(1)
    return placeOrder(api)
  }
  const stopped = await next()
  await hooks.agent()
  await hooks.advance(86400)
  await hooks.sweep()
  await hooks.agent() // 完成停机指令
  const released = await next()
  await hooks.agent()
  await api.post(`/api/v1/instances/${released}/release`, { data: {} })
  await hooks.agent()
  const failed = await next()
  await hooks.agent({ fail: true })
  const running = await next()
  await hooks.agent()
  const wiping = await next()
  await hooks.agent()
  await api.post(`/api/v1/instances/${wiping}/release`, { data: {} })
  const creating = await next()

  // 首次加载失败：显示重试（无轮询兜底）；连续失败只弹一次错误提示；故障恢复后点重试出现列表
  let fail = true
  await page.route('**/api/v1/instances', (route) => (fail ? route.fulfill({ status: 500, contentType: 'application/json', body: '{"code":"internal","message":"boom"}' }) : route.fallback()))
  await loginConsole(page, u.phone)
  await nav(page, info, 'instances')
  await expect(page.getByTestId('instances-error')).toBeVisible()
  await page.getByTestId('instances-retry').click()
  await expect(page.getByTestId('instances-error')).toBeVisible()
  await expect(page.locator('.el-message--error')).toHaveCount(1)
  fail = false
  await page.getByTestId('instances-retry').click()
  await expect(page.getByTestId('instances-error')).toHaveCount(0)
  const expectedOrder = [creating, wiping, running, failed, released, stopped]
  await expect(page.getByTestId('instance-id')).toHaveText(expectedOrder)
  const states = { creating, wiping, running, failed, released, stopped }
  for (const [state, id] of Object.entries(states)) {
    const row = instanceRow(page, info, id)
    await expect(row.getByTestId('instance-state')).toHaveText(stateLabels[state].text)
    await expect(row.getByTestId('instance-state')).toHaveClass(new RegExp(`el-tag--${stateLabels[state].type}`))
  }
  await expect(instanceRow(page, info, creating).getByTestId('instance-time')).toHaveText('已付 1 天 待开始')
  await expect(instanceRow(page, info, failed).getByTestId('instance-time')).toHaveText('费用已原路退回余额')
  await expect(instanceRow(page, info, stopped).getByTestId('instance-time')).toHaveText(/^数据保留至 \d{4}-\d{2}-\d{2} \d{2}:\d{2}$/)
  if (info.project.name === 'console-mobile') await expect(page.getByTestId('instances-cards')).toBeVisible()
  else await expect(page.getByTestId('instances-table')).toBeVisible()
})

test('C-INS-02 开通中实例每 10 秒自动刷新为运行中', async ({ page }, info) => {
  const u = await seedUser()
  const api = await userApi(u.phone)
  await hooks.machines(1)
  const id = await placeOrder(api)
  await page.clock.install()
  await loginConsole(page, u.phone)
  await nav(page, info, 'instances')
  const row = instanceRow(page, info, id)
  await expect(row.getByTestId('instance-state')).toHaveText('开通中')
  await hooks.agent()
  await expect(row.getByTestId('instance-state')).toHaveText('开通中') // 后端已开通但页面尚未轮询，证明下面的变化来自 10 秒轮询
  await page.clock.runFor(10_000)
  await expect(row.getByTestId('instance-state')).toHaveText('运行中')
  await expect(row.getByTestId('instance-time')).toHaveText(/^剩余 /)
})

test('C-INS-03 机器失联后列表与详情显示计时已暂停', async ({ page }, info) => {
  const u = await seedUser()
  const { id } = await runningInstance(u.phone)
  await hooks.advance(360)
  await hooks.sweep({ heartbeat: false })
  await loginConsole(page, u.phone)
  await nav(page, info, 'instances')
  await expect(instanceRow(page, info, id).getByTestId('instance-suspended')).toHaveText('计时已暂停（机器维护中）')
  await page.goto(`/instances/${id}`)
  await expect(page.getByTestId('detail-suspended')).toBeVisible()
  await hooks.sweep() // 心跳恢复，避免影响后续用例
})

test('C-INS-04 运行中倒计时每秒递减；停机显示北京时间的数据保留截止', async ({ page }, info) => {
  const u = await seedUser({ balance: 200000 })
  const { api, id: stoppedId } = await runningInstance(u.phone)
  await hooks.advance(86400)
  await hooks.sweep()
  await hooks.agent()
  await hooks.machines(1)
  const runningId = await placeOrder(api)
  await hooks.agent()
  const stopped = await instanceOf(api, stoppedId)
  expect(stopped.retain_until).not.toBeNull()

  await loginConsole(page, u.phone)
  await nav(page, info, 'instances')
  const runningRow = instanceRow(page, info, runningId)
  // 下单后 loginConsole 已把假时钟推进了 61 秒（发码限流），所以此时是「23 小时 58 分」量级，不是整整 1 天
  await expect(runningRow.getByTestId('instance-time')).toHaveText(/^剩余 23 小时 5\d 分$/)
  const first = Number(await runningRow.getByTestId('instance-time').getAttribute('data-remaining'))
  await expect.poll(async () => Number(await runningRow.getByTestId('instance-time').getAttribute('data-remaining')), { timeout: 5000 }).toBeLessThan(first)
  await expect(instanceRow(page, info, stoppedId).getByTestId('instance-time')).toHaveText(`数据保留至 ${cstTime(stopped.retain_until!)}`)

  await page.goto(`/instances/${runningId}`)
  const d1 = Number(await page.getByTestId('detail-remaining').getAttribute('data-remaining'))
  await expect.poll(async () => Number(await page.getByTestId('detail-remaining').getAttribute('data-remaining')), { timeout: 5000 }).toBeLessThan(d1)
  await page.goto(`/instances/${stoppedId}`)
  await expect(page.getByTestId('detail-retain')).toHaveText(cstTime(stopped.retain_until!))
})

// ---- 计划 6：预订在列表里的状态（预订都在本用例自己的机组里，见 helpers/booking.ts privateGroup）----

test('C-INS-05 预订到点变为开通中，Agent 完成后运行中', async ({ page }, info) => {
  const g = await privateGroup(1)
  const u = await seedUser({ balance: 1_000_000 })
  const api = await userApi(u.phone)
  const s = await startAfter(3)
  const { ids } = await bookViaApi(api, { startAt: s.iso, groupId: g.id })
  await loginConsole(page, u.phone)
  await nav(page, info, 'instances')
  const row = instanceRow(page, info, ids[0])
  await expect(row.getByTestId('instance-state')).toHaveText('已预订')
  await expect(row.getByTestId('instance-time')).toHaveText(`${cstTime(s.iso)} 开始 · 共 1 天`)
  await expect(row.getByTestId('instance-cancellable')).toBeVisible()
  await advanceToProvision(s.date)
  await hooks.sweep()
  await page.reload()
  await expect(instanceRow(page, info, ids[0]).getByTestId('instance-state')).toHaveText('开通中')
  await expect(instanceRow(page, info, ids[0]).getByTestId('instance-cancellable')).toHaveCount(0)
  await hooks.agent()
  await page.reload()
  await expect(instanceRow(page, info, ids[0]).getByTestId('instance-state')).toHaveText('运行中')
})

test('C-INS-06 预订机器进入维护后到点改派，列表显示已更换机器', async ({ page }, info) => {
  const g = await privateGroup(2)
  const [m1, m2] = g.machineIds
  const u = await seedUser({ balance: 1_000_000 })
  const api = await userApi(u.phone)
  const s = await startAfter(3)
  const { ids } = await bookViaApi(api, { startAt: s.iso, groupId: g.id, machineId: m1 })
  const admin = await createAdmin('ops')
  await adminPost(await adminApi(admin), admin, `/admin/api/v1/machines/${m1}/maintenance`, { on: true })
  await advanceToProvision(s.date)
  await hooks.sweep()
  await loginConsole(page, u.phone)
  await nav(page, info, 'instances')
  const row = instanceRow(page, info, ids[0])
  await expect(row.getByTestId('instance-state')).toHaveText('开通中')
  await expect(row.getByTestId('instance-reassigned')).toHaveText('已更换机器')
  await page.goto(`/instances/${ids[0]}`)
  await expect(page.getByTestId('detail-machine')).toHaveText(`机器 #${m2}（已更换机器 1 次）`)
})

test('C-INS-07 批次视图一键取消全部未开通预订：确认框显示台数与退款合计', async ({ page }) => {
  const g = await privateGroup(2)
  const u = await seedUser({ balance: 1_000_000 })
  const api = await userApi(u.phone)
  const s = await startAfter(30)
  const { batchId, ids } = await bookViaApi(api, { startAt: s.iso, count: 2, groupId: g.id })
  await loginConsole(page, u.phone)
  await page.goto(`/instances?batch=${batchId}`)
  await expect(page.getByTestId('instances-batch-head')).toContainText(`批次 ${batchId} · 2 台`)
  await expect(page.getByTestId('instance-state')).toHaveText(['已预订', '已预订'])
  await page.getByTestId('instances-cancel-all').click()
  await expect(page.getByTestId('instances-cancel-all-text')).toHaveText('将取消 2 台未开通的预订，共退款 ¥138.00（按各台当前取消规则计算）。')
  await page.getByTestId('instances-cancel-all-cancel').click() // 再想想：不取消
  await expect(page.getByTestId('instances-cancel-all-dialog')).toBeHidden()
  for (const id of ids) expect(((await (await api.get(`/api/v1/instances/${id}`)).json()) as { state: string }).state).toBe('reserved')
  await page.getByTestId('instances-cancel-all').click()
  await expect(page.getByTestId('instances-cancel-all-text')).toHaveText('将取消 2 台未开通的预订，共退款 ¥138.00（按各台当前取消规则计算）。')
  await page.getByTestId('instances-cancel-all-ok').click()
  await toast(page, '已取消 2 台')
  await expect(page.getByTestId('instance-state')).toHaveText(['已取消', '已取消'])
  await expect(page.getByTestId('instances-cancel-all')).toHaveCount(0)
  expect((await (await api.get('/api/v1/wallet')).json()).balance).toBe(1_000_000)
})
