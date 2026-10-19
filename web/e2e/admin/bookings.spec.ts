import { expect, test, type Page } from '@playwright/test'
import { adminApi, createAdmin, enterTotpDialog, getJSON, loginAdmin, recordRequests } from '../helpers/admin'
import { bookViaApi, privateGroup, startAfter } from '../helpers/booking'
import { seedUser, toast, userApi } from '../helpers/console'
import { hooks } from '../helpers/hooks'
import { cstDate, cstTime } from '../helpers/time'

const HOUR = 3600e3

/** 自己的机组（groupMachines 台机器）里，一个用户在 30 小时后预订 count 台（第 k 台钉在第 k 台机器上）。 */
async function seedBooking(groupMachines = 2, count = 1) {
  const g = await privateGroup(groupMachines)
  const u = await seedUser({ balance: 1_000_000 })
  const api = await userApi(u.phone)
  const s = await startAfter(30)
  const ids: string[] = []
  for (let k = 0; k < count; k++) ids.push(...(await bookViaApi(api, { startAt: s.iso, groupId: g.id, machineId: g.machineIds[k] })).ids)
  return { g, mids: g.machineIds, u, api, s, id: ids[0], ids }
}

const bookingRow = (page: Page, id: string) => page.getByRole('row').filter({ has: page.getByTestId('booking-row').filter({ hasText: id }) })

async function searchUser(page: Page, userId: number) {
  await page.getByTestId('bookings-filter-user').fill(String(userId))
  await page.getByTestId('bookings-search').click()
}

type AuditList = { total: number }

test('A-BKG-01 预订列表：按用户筛选，显示开始 / 机组 / 机器 / 时长；财务无操作按钮', async ({ page }) => {
  const { u, s, id, g, mids } = await seedBooking()
  const admin = await createAdmin('finance')
  await loginAdmin(page, admin)
  await page.getByTestId('menu-bookings').click()
  await searchUser(page, u.id)
  await expect(page.getByTestId('booking-row')).toHaveCount(1)
  await expect(page.getByTestId('bookings-total')).toHaveText('共 1 个')
  const tr = bookingRow(page, id)
  await expect(tr.getByTestId('booking-start')).toHaveText(cstTime(s.iso))
  await expect(tr.getByTestId('booking-group')).toHaveText(g.name)
  await expect(tr.getByTestId('booking-machine')).toHaveText(String(mids[0]))
  await expect(tr.getByTestId('booking-duration')).toHaveText('1 天')
  await expect(tr.getByTestId('booking-reassigned')).toHaveText('0')
  await expect(tr.getByTestId('booking-reassign')).toHaveCount(0) // finance 无操作
  await expect(tr.getByTestId('booking-cancel')).toHaveCount(0)
})

test('A-BKG-02 改派：原因必填；改派到指定机器并写审计；再改到同一台被拒', async ({ page }) => {
  const { u, mids, id } = await seedBooking()
  const admin = await createAdmin('ops')
  const posts = recordRequests(page, 'POST', `/instances/${id}/reassign`)
  await loginAdmin(page, admin)
  await page.goto('/admin/bookings')
  await searchUser(page, u.id)
  const tr = bookingRow(page, id)
  await expect(tr.getByTestId('booking-machine')).toHaveText(String(mids[0]))
  await tr.getByTestId('booking-reassign').click()
  await page.getByTestId('reassign-machine').fill(String(mids[1]))
  await page.getByTestId('reassign-submit').click()
  await toast(page, '请填写改派原因')
  await expect(page.getByTestId('reassign-dialog')).toBeVisible()
  expect(posts).toHaveLength(0)
  await page.getByTestId('reassign-reason').fill('换机柜')
  await page.getByTestId('reassign-submit').click()
  await toast(page, `已改派到机器 ${mids[1]}`)
  expect(posts.map((p) => p.body)).toEqual([{ machine_id: mids[1], reason: '换机柜' }])
  await expect(tr.getByTestId('booking-machine')).toHaveText(String(mids[1]))
  await expect(tr.getByTestId('booking-reassigned')).toHaveText('1')
  await tr.getByTestId('booking-reassign').click()
  await page.getByTestId('reassign-machine').fill(String(mids[1]))
  await page.getByTestId('reassign-reason').fill('再来')
  await page.getByTestId('reassign-submit').click()
  await toast(page, '该机器在所选时段不可用')
  await expect(page.getByTestId('reassign-dialog')).toBeVisible()
  const ctx = await adminApi(admin)
  expect((await getJSON<AuditList>(ctx, `/admin/api/v1/audit?action=booking.reassign&target_id=${id}`)).total).toBe(1)
  const detail = await getJSON<{ instance: { machine_id: number; reassign_count: number } }>(ctx, `/admin/api/v1/instances/${id}`)
  expect(detail.instance).toMatchObject({ machine_id: mids[1], reassign_count: 1 })
})

test('A-BKG-03 取消预订：原因必填、需要动态码、全额退款；动态码通过但操作失败不开免码窗口', async ({ page }) => {
  const { u, api, ids } = await seedBooking(2, 2)
  const [a, b] = ids
  const admin = await createAdmin('ops')
  const postsA = recordRequests(page, 'POST', `/instances/${a}/cancel-booking`)
  const postsB = recordRequests(page, 'POST', `/instances/${b}/cancel-booking`)
  await loginAdmin(page, admin)
  await page.goto('/admin/bookings')
  await searchUser(page, u.id)
  await expect(page.getByTestId('booking-row')).toHaveCount(2)

  await test.step('原因为空：前端拦截，不发请求', async () => {
    await bookingRow(page, a).getByTestId('booking-cancel').click()
    await page.getByTestId('cancel-booking-submit').click()
    await toast(page, '请填写取消原因')
    await expect(page.getByTestId('cancel-booking-dialog')).toBeVisible()
    await expect(page.getByTestId('totp-dialog')).toBeHidden()
    expect(postsA).toHaveLength(0)
  })

  await test.step('列表打开后用户已自行取消：动态码校验通过、取消失败，弹窗保留', async () => {
    const r = await api.post(`/api/v1/instances/${a}/cancel`, { data: {} })
    expect(r.ok()).toBe(true)
    await page.getByTestId('cancel-booking-reason').fill('机房断电')
    await page.getByTestId('cancel-booking-submit').click()
    await enterTotpDialog(page, admin.totp_secret!)
    await toast(page, '当前状态不允许该操作')
    await expect(page.getByTestId('cancel-booking-dialog')).toBeVisible()
    expect(postsA.map((p) => p.body)).toEqual([{ reason: '机房断电' }, { reason: '机房断电', totp: expect.stringMatching(/^\d{6}$/) }])
    await page.keyboard.press('Escape')
    await expect(page.getByTestId('cancel-booking-dialog')).toBeHidden()
  })

  await test.step('另一个预订：上一次操作失败没有打开免码窗口，仍要动态码；成功后全额退款、从未开通列表消失', async () => {
    await bookingRow(page, b).getByTestId('booking-cancel').click()
    await page.getByTestId('cancel-booking-reason').fill('机房断电')
    await page.getByTestId('cancel-booking-submit').click()
    await expect(page.getByTestId('totp-dialog')).toBeVisible()
    await enterTotpDialog(page, admin.totp_secret!)
    await toast(page, '已取消预订，退款 ¥69.00')
    await expect(page.getByTestId('cancel-booking-dialog')).toBeHidden()
    expect(postsB.map((p) => p.body)).toEqual([{ reason: '机房断电' }, { reason: '机房断电', totp: expect.stringMatching(/^\d{6}$/) }])
    await expect(page.getByTestId('booking-row')).toHaveCount(0) // a 用户已取消、b 后台取消：都不在「未开通」里
    expect((await (await api.get(`/api/v1/instances/${b}`)).json()).state).toBe('cancelled')
    expect((await (await api.get('/api/v1/wallet')).json()).balance).toBe(1_000_000)
    const ctx = await adminApi(admin)
    expect((await getJSON<AuditList>(ctx, `/admin/api/v1/audit?action=booking.admin_cancel&target_id=${b}`)).total).toBe(1)
    expect((await getJSON<AuditList>(ctx, `/admin/api/v1/audit?action=booking.admin_cancel&target_id=${a}`)).total).toBe(0)
  })
})

test('A-BKG-04 机器详情显示档期', async ({ page }) => {
  const { mids, id, s, u } = await seedBooking(1)
  const admin = await createAdmin('ops')
  await loginAdmin(page, admin)
  await page.goto(`/admin/machines/${mids[0]}`)
  await expect(page.getByTestId('mdetail-lease-row')).toHaveCount(1)
  // 时段 = [开始前 1 小时开通, 结束 + 7 小时保留与洗机]
  await expect(page.getByTestId('mdetail-lease-row').locator('td')).toHaveText([
    cstTime(new Date(s.date.getTime() - HOUR).toISOString()), cstTime(new Date(s.date.getTime() + 31 * HOUR).toISOString()), id, String(u.id),
  ])
})

test('A-BKG-05 进入维护时提示未开通预订数量', async ({ page }) => {
  const { mids } = await seedBooking(1)
  const admin = await createAdmin('ops')
  await loginAdmin(page, admin)
  await page.goto(`/admin/machines/${mids[0]}`)
  await page.getByTestId('mdetail-maintenance-btn').click()
  await expect(page.getByTestId('mdetail-maintenance-upcoming')).toHaveText('该机器有 1 个未开通的预订，将在开通时自动改派。')
  await page.getByTestId('mdetail-maintenance-ok').click()
  await toast(page, '已进入维护')
  await expect(page.getByTestId('mdetail-maintenance-btn')).toHaveText('结束维护')
})

test('A-BKG-06 仪表盘显示未来 7 天预订开始台数', async ({ page }) => {
  const admin = await createAdmin('finance')
  const ctx = await adminApi(admin)
  const dayBefore = cstDate(await hooks.clock())
  const before = (await getJSON<{ bookings_upcoming_7d: number }>(ctx, '/admin/api/v1/dashboard')).bookings_upcoming_7d
  await seedBooking(1)
  await loginAdmin(page, admin)
  const dash = page.getByTestId('dash-bookings-7d')
  await expect(dash).toHaveText(/^\d+$/)
  const shown = Number(await dash.textContent())
  // 统计窗口按北京时间自然日：期间假时钟只推进了几分钟，没跨过午夜时恰好多出本用例的 1 台
  if (cstDate(await hooks.clock()) === dayBefore) expect(shown).toBe(before + 1)
  expect((await getJSON<{ bookings_upcoming_7d: number }>(ctx, '/admin/api/v1/dashboard')).bookings_upcoming_7d).toBe(shown)
})
