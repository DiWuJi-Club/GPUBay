import { expect, test } from '@playwright/test'
import { adminApi, adminPost, createAdmin } from '../helpers/admin'
import { loginConsole, nav, placeOrder, seedUser, toast, userApi } from '../helpers/console'
import { hooks } from '../helpers/hooks'
import { yuan } from '../helpers/money'
import { uniquePhone } from '../helpers/phone'
import { DAY, inviteeOf, inviterWithCode, masked, restoreRate, rewardsOf } from '../helpers/referral'

// 计划 8 邀请返利（控制台）。每个用例只用自己新造的邀请人 / 被邀请人与新登记的机器；改费率的用例在 finally 里恢复初始版本。

test('C-REF-01 带 ?ic= 落地：地址栏参数被去掉、登录页显示邀请人、新号注册提示已绑定、邀请人页出现被邀请人', async ({ page }) => {
  const inv = await inviterWithCode()
  await page.goto(`/?ic=${inv.code.toLowerCase()}`)
  await expect(page).toHaveURL(/\/market$/) // 参数已移除
  const phone = uniquePhone()
  await hooks.advance(61)
  await page.goto('/login')
  await expect(page.getByTestId('login-invite')).toContainText(masked(inv.phone))
  await page.getByTestId('login-phone').fill(phone)
  await page.getByTestId('login-send').click()
  await page.getByTestId('login-code').fill('000000')
  await page.getByTestId('login-submit').click()
  await toast(page, '已绑定邀请关系')
  await expect(page).toHaveURL(/\/market/)
  // 再回登录页：存储已清，不再显示邀请人
  await page.context().clearCookies()
  await page.goto('/login')
  await expect(page.getByTestId('login-card')).toBeVisible()
  await expect(page.getByTestId('login-invite')).toHaveCount(0)
  // 邀请人页
  await loginConsole(page, inv.phone)
  await page.goto('/invite')
  await expect(page.getByTestId('invite-stat-invitees')).toHaveText('1')
  await expect(page.getByTestId('invitee-phone').first()).toHaveText(masked(phone))
  await expect(page.getByTestId('invitee-kyc').first()).toHaveText('未实名')
  await expect(page.getByTestId('invitee-status').first()).toHaveText('有效')
})

test('C-REF-02 无效码静默：落地不存、登录页无提示、注册成功无 toast', async ({ page }) => {
  const codeLookups: string[] = []
  page.on('response', (r) => {
    if (new URL(r.url()).pathname.startsWith('/api/v1/referral/codes/')) codeLookups.push(r.url())
  })
  const loginBodies: Record<string, unknown>[] = []
  page.on('request', (r) => {
    if (new URL(r.url()).pathname === '/api/v1/auth/login') loginBodies.push(r.postDataJSON() as Record<string, unknown>)
  })
  await page.goto('/?ic=ZZZZZZZZ')
  await expect(page).toHaveURL(/\/market$/)
  await page.goto('/login')
  await expect.poll(() => codeLookups.length).toBeGreaterThan(0) // 查码已返回 valid=false
  await expect.poll(() => page.evaluate(() => localStorage.getItem('sc_invite'))).toBeNull() // 明确无效：静默丢弃
  await expect(page.getByTestId('login-invite')).toHaveCount(0)
  const phone = uniquePhone()
  await hooks.advance(61)
  await page.getByTestId('login-phone').fill(phone)
  await page.getByTestId('login-send').click()
  await page.getByTestId('login-code').fill('000000')
  await page.getByTestId('login-submit').click()
  await expect(page).toHaveURL(/\/market/)
  expect(loginBodies).toHaveLength(1)
  expect(loginBodies[0]).not.toHaveProperty('invite_code')
  await expect(page.locator('.el-message')).toHaveCount(0)
})

test('C-REF-03 老用户带码登录不绑定、不提示；清除按钮去掉邀请', async ({ page }) => {
  const inv = await inviterWithCode()
  const old = await seedUser()
  await page.goto(`/?ic=${inv.code}`)
  await hooks.advance(61)
  await page.goto('/login')
  await expect(page.getByTestId('login-invite')).toBeVisible()
  const login = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/v1/auth/login')
  await page.getByTestId('login-phone').fill(old.phone)
  await page.getByTestId('login-send').click()
  await page.getByTestId('login-code').fill('000000')
  await page.getByTestId('login-submit').click()
  expect(((await (await login).json()) as { invite?: { bound: boolean; reason: string } }).invite).toEqual({ bound: false, reason: 'existing_user' })
  await expect(page).toHaveURL(/\/market/)
  await expect(page.locator('.el-message')).toHaveCount(0)
  const invitees = (await (await inv.api.get('/api/v1/referral/invitees')).json()) as { total: number }
  expect(invitees.total).toBe(0)
  // 清除按钮
  await page.context().clearCookies()
  await page.goto(`/?ic=${inv.code}`)
  await expect(page).toHaveURL(/\/market$/)
  await page.goto('/login')
  await expect(page.getByTestId('login-invite')).toBeVisible()
  await page.getByTestId('login-invite-clear').click()
  await expect(page.getByTestId('login-invite')).toHaveCount(0)
  await page.reload()
  await expect(page.getByTestId('login-card')).toBeVisible()
  await expect(page.getByTestId('login-invite')).toHaveCount(0)
})

test('C-REF-04 邀请页：码、链接、二维码、规则、复制', async ({ page }, info) => {
  const inv = await inviterWithCode()
  await loginConsole(page, inv.phone)
  await nav(page, info, 'account')
  await page.getByTestId('account-invite').click()
  await expect(page).toHaveURL(/\/invite$/)
  await expect(page.getByTestId('invite-code')).toHaveText(inv.code)
  await expect(page.getByTestId('invite-link')).toHaveText(`http://spark.test:18080/?ic=${inv.code}`)
  await expect(page.getByTestId('invite-qr')).toBeVisible()
  await expect(page.getByTestId('invite-rules')).toContainText('10%')
  await expect(page.getByTestId('invite-rules')).toContainText('不可提现')
  await page.getByTestId('invite-copy-link').click()
  await toast(page, '已复制')
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(`http://spark.test:18080/?ic=${inv.code}`)
  await expect(page.getByTestId('invite-kyc-alert')).toHaveCount(0)
})

test('C-REF-05 未实名邀请人：提示条与去实名链接；码仍可见', async ({ page }) => {
  const inv = await inviterWithCode({ verified: false })
  await loginConsole(page, inv.phone)
  await page.goto('/invite')
  await expect(page.getByTestId('invite-kyc-alert')).toContainText('实名认证后才能收到返利')
  await expect(page.getByTestId('invite-code')).toHaveText(inv.code)
  await page.getByTestId('invite-kyc-alert').getByRole('link').click()
  await expect(page).toHaveURL(/\/kyc$/)
})

test('C-REF-06 被邀请人列表：脱敏、状态、实名、贡献、截止日；分页', async ({ page }) => {
  const inv = await inviterWithCode()
  for (let i = 0; i < 21; i++) await inviteeOf(inv.code, { verified: i % 2 === 0 })
  await loginConsole(page, inv.phone)
  await page.goto('/invite')
  await expect(page.getByTestId('invite-stat-invitees')).toHaveText('21')
  await expect(page.getByTestId('invite-stat-verified')).toHaveText('11')
  await expect(page.getByTestId('invitee-row')).toHaveCount(20)
  await expect(page.getByTestId('invitee-phone').first()).toHaveText(/^139\*\*\*\*\d{4}$/)
  await expect(page.getByTestId('invitee-contributed').first()).toHaveText(yuan(0))
  await expect(page.getByTestId('invitee-until').first()).toHaveText(/^\d{4}-\d{2}-\d{2}$/)
  await page.getByTestId('invite-invitees-pager').getByText('2', { exact: true }).click()
  await expect(page.getByTestId('invitee-row')).toHaveCount(1)
})

test('C-REF-07 返利明细：待发放 → 推进 7 天 + 清扫 → 已发放，钱包流水「邀请返利」', async ({ page }, info) => {
  const inv = await inviterWithCode({ balance: 0 })
  const b = await inviteeOf(inv.code, { balance: 100_000 })
  const bApi = await userApi(b.phone)
  await hooks.machines(1)
  const instance = await placeOrder(bApi)
  await hooks.agent() // 实例进入运行中（停在开通中会超时整单退款、返利作废）
  await hooks.sweep()
  await loginConsole(page, inv.phone)
  await page.goto('/invite?tab=rewards')
  await page.getByTestId('invite-tab-rewards').click()
  await expect(page.getByTestId('reward-row')).toHaveCount(1)
  await expect(page.getByTestId('reward-amount')).toHaveText(yuan(690))
  await expect(page.getByTestId('reward-status')).toHaveText('待发放')
  await expect(page.getByTestId('invite-stat-pending')).toHaveText(yuan(690))
  await hooks.advance(7 * DAY)
  await hooks.sweep()
  await page.reload()
  await page.getByTestId('invite-tab-rewards').click()
  await expect(page.getByTestId('reward-status')).toHaveText('已发放')
  await expect(page.getByTestId('invite-stat-settled')).toHaveText(yuan(690))
  await nav(page, info, 'wallet')
  await expect(page.getByTestId('wallet-balance')).toHaveText(yuan(690))
  await expect(page.getByTestId('ledger-kind').first()).toHaveText('邀请返利')
  await expect(page.getByTestId('ledger-remark').first()).toContainText(`邀请返利 ${masked(b.phone)} 订单 ord-`)
  expect((await rewardsOf(inv.api))[0]).toMatchObject({ status: 'settled', reward: 690 })
  void instance
})

test('C-REF-08 后台退款后：明细显示已扣回、钱包流水「返利扣回」', async ({ page }, info) => {
  // 退款要在实例仍运行时做（refund-release 只接受 creating / running / stopped），所以这里把发放延迟临时改为 0 天，
  // 让计提与发放在同一轮清扫完成；用例结束时恢复初始费率（费率配置是全局的，不恢复会影响后面的用例）。
  await hooks.referralConfig({ hold_days: 0 })
  try {
    const inv = await inviterWithCode({ balance: 0 })
    const b = await inviteeOf(inv.code, { balance: 100_000 })
    const bApi = await userApi(b.phone)
    await hooks.machines(1)
    const instance = await placeOrder(bApi)
    await hooks.sweep() // 计提 + 发放
    await hooks.agent() // 实例进入运行中
    const fin = await createAdmin('finance')
    const fapi = await adminApi(fin)
    await adminPost(fapi, fin, `/admin/api/v1/instances/${instance}/refund-release`, { amount: 3000, reason: 'e2e' })
    await hooks.sweep()
    const rewards = await rewardsOf(inv.api)
    expect(rewards[0]).toMatchObject({ status: 'settled', reward: 390, refunded_amount: 3000, clawed_amount: 300 })
    await loginConsole(page, inv.phone)
    await page.goto('/invite?tab=rewards')
    await page.getByTestId('invite-tab-rewards').click()
    await expect(page.getByTestId('reward-amount')).toHaveText(yuan(390))
    await expect(page.getByTestId('reward-clawed')).toContainText(yuan(300))
    await nav(page, info, 'wallet')
    await expect(page.getByTestId('wallet-balance')).toHaveText(yuan(390))
    await expect(page.getByTestId('ledger-kind').first()).toHaveText('返利扣回')
    await expect(page.getByTestId('ledger-amount').first()).toHaveText(`-${yuan(300)}`)
  } finally {
    await restoreRate()
  }
})
