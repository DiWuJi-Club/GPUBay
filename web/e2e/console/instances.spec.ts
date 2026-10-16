import { expect, test } from '@playwright/test'
import { loginConsole, nav, placeOrder, seedUser, userApi } from '../helpers/console'
import { hooks } from '../helpers/hooks'
import { instanceRow, stateLabels } from '../helpers/instances'

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

  await loginConsole(page, u.phone)
  await nav(page, info, 'instances')
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
  await page.clock.runFor(10_000)
  await expect(row.getByTestId('instance-state')).toHaveText('运行中')
  await expect(row.getByTestId('instance-time')).toHaveText(/^剩余 /)
})
