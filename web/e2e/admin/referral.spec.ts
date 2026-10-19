import { readFileSync } from 'node:fs'
import { expect, test, type Page } from '@playwright/test'
import { adminApi, createAdmin, enterTotpDialog, expectAdminUrl, getJSON, loginAdmin, logout } from '../helpers/admin'
import { placeOrder, toastLocator, userApi } from '../helpers/console'
import { hooks } from '../helpers/hooks'
import { yuan } from '../helpers/money'
import { DAY, exactId, inviteeOf, inviterWithCode, restoreRate, rewardsOf, seedReward } from '../helpers/referral'

// 计划 8 邀请返利（后台）。每个用例只用自己新造的邀请人 / 被邀请人与新登记的机器；A-REF-03 的冻结 / 解冻只作用于自己的邀请人，
// A-REF-04 改的费率在 finally 里恢复初始版本。推进时钟后重新登录（后台会话 2 小时空闲失效）。

/** 分 → 元两位小数（与后端 csvx.Yuan 同格式，独立实现）。 */
const yuan2 = (cents: number) => `${Math.floor(cents / 100)}.${String(cents % 100).padStart(2, '0')}`

/** 邀请人 Tab 里某个邀请人的行（inv-user 里只有 ID，精确匹配）。 */
const inviterRow = (page: Page, id: number) => page.getByTestId('inv-row').filter({ has: page.getByTestId('inv-user').filter({ hasText: exactId(id) }) })
/** 待审核 Tab 里某个被邀请人的行。 */
const heldRow = (page: Page, inviteeId: number) => page.getByTestId('held-row').filter({ has: page.getByTestId('held-invitee').filter({ hasText: exactId(inviteeId) }) })

async function searchInviter(page: Page, id: number) {
  await page.getByTestId('inv-q').fill(String(id))
  await page.getByTestId('inv-q').press('Enter')
}

test('A-REF-01 概览数字：待发放、搁置数随新数据增加', async ({ page }) => {
  const fin = await createAdmin('finance')
  const before = await getJSON<{ pending: number; held_count: number }>(await adminApi(fin), '/admin/api/v1/referral/summary')
  const { inv } = await seedReward()
  // 同设备搁置：先让邀请人用设备 D 登录一次（钩子对已存在的手机号只是再登录，不会重复建用户），再用同一设备带码注册
  const D = crypto.randomUUID()
  await hooks.users({ phone: inv.phone, device_id: D })
  const held = await inviteeOf(inv.code, { device_id: D })
  expect(held.invite?.bound).toBe(true)
  await loginAdmin(page, fin)
  await page.goto('/admin/referral')
  await expect(page.getByTestId('ref-sum-pending')).toHaveText(yuan(before.pending + 690))
  await expect(page.getByTestId('ref-sum-held')).toHaveText(String(before.held_count + 1))
})

test('A-REF-02 待审核：通过与撤销（撤销要动态码、提示作废金额）', async ({ page }) => {
  const inv = await inviterWithCode({ balance: 0 })
  const D = crypto.randomUUID()
  await hooks.users({ phone: inv.phone, device_id: D })
  const held1 = await inviteeOf(inv.code, { device_id: D, balance: 100_000 })
  const held2 = await inviteeOf(inv.code, { device_id: D, balance: 100_000 })
  await hooks.machines(1)
  await placeOrder(await userApi(held2.phone))
  await hooks.sweep()
  const fin = await createAdmin('finance')
  await loginAdmin(page, fin)
  await page.goto('/admin/referral?tab=held')
  const row1 = heldRow(page, held1.id)
  await expect(row1.getByTestId('held-flags')).toContainText('同设备')
  await row1.getByTestId('held-approve').click()
  await page.getByTestId('held-approve-note').fill('家人')
  await page.getByTestId('held-approve-ok').click()
  await expect(toastLocator(page, '已通过')).toBeVisible()
  await expect(row1).toHaveCount(0)
  const row2 = heldRow(page, held2.id)
  await expect(row2.getByTestId('held-pending')).toHaveText(yuan(690))
  await row2.getByTestId('held-revoke').click()
  await page.getByTestId('held-revoke-ok').click() // 原因必填
  await expect(page.getByTestId('held-revoke-error')).toHaveText('请填写撤销原因')
  await expect(page.getByTestId('held-revoke-dialog')).toBeVisible()
  await page.getByTestId('held-revoke-reason').fill('刷单')
  await page.getByTestId('held-revoke-ok').click()
  await enterTotpDialog(page, fin.totp_secret!)
  await expect(toastLocator(page, `已撤销，作废待发放 ${yuan(690)}`)).toBeVisible()
  await expect(row2).toHaveCount(0)
  expect((await rewardsOf(inv.api))[0].status).toBe('void')
})

test('A-REF-03 邀请人：搜索、详情抽屉、冻结后新注册不绑定且到期不发放、解冻后发放', async ({ page }) => {
  const { inv } = await seedReward()
  const fin = await createAdmin('finance')
  await loginAdmin(page, fin)
  await page.goto('/admin/referral?tab=inviters')
  await searchInviter(page, inv.id)
  const row = inviterRow(page, inv.id)
  await expect(row.getByTestId('inv-code')).toHaveText(inv.code)
  await expect(row.getByTestId('inv-pending')).toHaveText(yuan(690))
  await row.click()
  await expect(page.getByTestId('inv-drawer')).toBeVisible()
  await expect(page.getByTestId('inv-binding-row')).toHaveCount(1)
  await expect(page.getByTestId('inv-reward-row')).toHaveCount(1)
  await page.getByTestId('inv-freeze').click()
  await page.getByTestId('inv-freeze-reason').fill('可疑')
  await page.getByTestId('inv-freeze-ok').click()
  await expect(toastLocator(page, '已冻结')).toBeVisible()
  await expect(page.getByTestId('inv-d-status')).toHaveText('已冻结')
  const blocked = await inviteeOf(inv.code)
  expect(blocked.invite?.bound).toBe(false)
  expect(blocked.invite?.reason).toBe('inviter_frozen')
  await hooks.advance(7 * DAY)
  await hooks.sweep()
  expect((await rewardsOf(inv.api))[0].status).toBe('pending') // 冻结：到期也不发放
  // 推进 7 天后后台会话已失效：重新登录
  await logout(page)
  await loginAdmin(page, fin)
  await page.goto('/admin/referral?tab=inviters')
  await searchInviter(page, inv.id)
  await expect(row.getByTestId('inv-pending')).toHaveText(yuan(690)) // 未发放
  await expect(row.getByTestId('inv-net')).toHaveText(yuan(0))
  await row.click()
  await page.getByTestId('inv-unfreeze').click()
  await expect(toastLocator(page, '已解冻')).toBeVisible()
  await expect(page.getByTestId('inv-d-status')).toHaveText('正常')
  await hooks.sweep()
  await page.reload()
  await searchInviter(page, inv.id)
  await expect(row.getByTestId('inv-net')).toHaveText(yuan(690))
  await expect(row.getByTestId('inv-pending')).toHaveText(yuan(0))
})

test('A-REF-04 配置：新增版本（super + 动态码）、追溯生效被拒、finance 看不到配置 Tab', async ({ page }) => {
  const sup = await createAdmin('super')
  try {
    await loginAdmin(page, sup)
    await page.goto('/admin/referral?tab=config')
    await expect(page.getByTestId('cfg-current-rate')).toHaveText('10%')
    await page.getByTestId('cfg-rate').fill('15')
    await page.getByTestId('cfg-invitee').fill('500')
    await page.getByTestId('cfg-inviter').fill('5000')
    await page.getByTestId('cfg-hold').fill('7')
    await page.getByTestId('cfg-period').fill('365')
    await page.getByTestId('cfg-note').fill('e2e')
    await page.getByTestId('cfg-submit').click()
    await enterTotpDialog(page, sup.totp_secret!)
    await expect(toastLocator(page, '已新增版本')).toBeVisible()
    await expect(page.getByTestId('cfg-current-rate')).toHaveText('15%')
    // 追溯生效
    const yesterday = new Date((await hooks.clock()).getTime() - DAY * 1000)
    const p2 = (n: number) => String(n).padStart(2, '0')
    await page.getByTestId('cfg-effective').fill(`${yesterday.getFullYear()}-${p2(yesterday.getMonth() + 1)}-${p2(yesterday.getDate())} 00:00:00`)
    await page.getByTestId('cfg-effective').press('Enter')
    await page.getByTestId('cfg-submit').click()
    await expect(toastLocator(page, '生效时间不能早于当前时间')).toBeVisible()
    await expect(page.getByTestId('cfg-current-rate')).toHaveText('15%')
    await logout(page)
    await loginAdmin(page, await createAdmin('finance'))
    await page.goto('/admin/referral?tab=config')
    await expect(page.getByTestId('ref-sum-pending')).toBeVisible() // 回落到概览
    await expect(page.getByTestId('ref-tab-config')).toHaveCount(0)
  } finally {
    await restoreRate()
  }
})

test('A-REF-05 返利明细筛选与 CSV 下载（中文表头、两位小数）', async ({ page }) => {
  const { inv, b } = await seedReward({ settle: true })
  const fin = await createAdmin('finance')
  await loginAdmin(page, fin)
  await page.goto('/admin/referral?tab=rewards')
  await page.getByTestId('rw-inviter').fill(String(inv.id))
  // rw-status 同时是筛选下拉与表格状态列的 testid：只点下拉本体
  await page.locator('.el-select[data-testid="rw-status"]').click()
  await page.getByRole('option', { name: '已发放', exact: true }).click()
  await expect(page.getByTestId('rw-row')).toHaveCount(1)
  await expect(page.getByTestId('rw-invitee-id').first()).toHaveText(String(b.id))
  await expect(page.getByTestId('rw-reward').first()).toHaveText(yuan(690))
  const dl = page.waitForEvent('download')
  await page.getByTestId('rw-export').click()
  const file = await dl
  expect(file.suggestedFilename()).toMatch(/^referral-rewards-\d{8}-\d{6}\.csv$/)
  const text = readFileSync(await file.path(), 'utf8').replace(/^﻿/, '')
  const lines = text.trim().split(/\r?\n/)
  expect(lines[0]).toBe('订单号,邀请人ID,被邀请人ID,消费(元),退款(元),费率(%),应得(元),状态,发放时间,已扣回(元),费率版本')
  expect(lines).toHaveLength(2)
  expect(lines[1]).toContain(`,${inv.id},${b.id},${yuan2(6900)},0.00,10.00,${yuan2(690)},已发放,`)
})

test('A-REF-06 用户详情邀请区块：被邀请人显示邀请人，邀请人显示统计与链接', async ({ page }) => {
  const { inv, b } = await seedReward({ settle: true })
  const fin = await createAdmin('finance')
  await loginAdmin(page, fin)
  await page.goto(`/admin/users/${b.id}`)
  await expect(page.getByTestId('user-referral-inviter')).toContainText(String(inv.id))
  await page.goto(`/admin/users/${inv.id}`)
  await expect(page.getByTestId('user-referral-code')).toHaveText(inv.code)
  await expect(page.getByTestId('user-referral-net')).toHaveText(yuan(690))
  await page.getByTestId('user-referral-link').click()
  await expectAdminUrl(page, '/referral', { tab: 'inviters', q: String(inv.id) })
  await expect(inviterRow(page, inv.id)).toHaveCount(1)
})

test('A-REF-07 ops 看不到菜单，直接访问 403，接口 403', async ({ page }) => {
  const ops = await createAdmin('ops')
  await loginAdmin(page, ops)
  await expect(page.getByTestId('menu-machines')).toBeVisible()
  await expect(page.getByTestId('menu-referral')).toHaveCount(0)
  await page.goto('/admin/referral')
  await expectAdminUrl(page, '/403', { from: '/referral' })
  const r = await (await adminApi(ops)).get('/admin/api/v1/referral/summary')
  expect(r.status()).toBe(403)
})
