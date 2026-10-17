import { expect, request, test, type APIRequestContext, type Page } from '@playwright/test'
import {
  adminApi, clickTwiceSync, createAdmin, drainTasks, expectAdminUrl, fail500, isPath, loginAdmin, recordRequests, trackToasts,
} from '../helpers/admin'
import { seedUser, submitEnterpriseKyc, toast, toastLocator, userApi } from '../helpers/console'
import { ADMIN_URL, CONSOLE_URL } from '../helpers/hooks'
import { runStamp } from '../helpers/phone'
import { cstTime } from '../helpers/time'

const LICENSE_NO = '91110000MA01ABCD2X'
type Pending = { id: number; user_id: number; real_name: string; license_no: string; license_file: string; created_at: string }

/** 全部待审核记录（按后端顺序：最早提交的在前）。数据库整轮共享，控制台用例也会留下待审核记录，所以按接口定位本用例的记录。 */
async function allPending(ctx: APIRequestContext): Promise<Pending[]> {
  const out: Pending[] = []
  for (let p = 1; ; p++) {
    const r = await ctx.get(`/admin/api/v1/kyc/pending?page=${p}&page_size=100`)
    if (!r.ok()) throw new Error(`GET kyc/pending ${r.status()} ${await r.text()}`)
    const { items, total } = (await r.json()) as { items: Pending[]; total: number }
    out.push(...items)
    if (out.length >= total || items.length === 0) return out
  }
}
const recordOf = (list: Pending[], company: string) => {
  const i = list.findIndex((r) => r.real_name === company)
  if (i < 0) throw new Error(`待审核列表里没有 ${company}`)
  return { record: list[i], page: Math.floor(i / 20) + 1 }
}

const rows = (page: Page) => page.getByRole('row').filter({ has: page.getByTestId('kyc-company') })
const rowOf = (page: Page, company: string) => rows(page).filter({ has: page.getByTestId('kyc-company').getByText(company, { exact: true }) })

/** 打开待审核页并翻到第 n 页（每页 20 条） */
async function openKycPage(page: Page, n: number) {
  await page.goto('/admin/kyc')
  await expect(rows(page).first()).toBeVisible()
  if (n > 1) {
    await page.getByTestId('kyc-pager').getByText(String(n), { exact: true }).click()
    await expect(page.getByTestId('kyc-pager').locator('[aria-current="true"]')).toHaveText(String(n))
  }
}

test('A-KYC-01 待审核列表（超过一页翻页）、PNG 预览、PDF 与预览失败时新窗口打开；营业执照只有运维 / 超管能读取；首次加载失败可重试', async ({ page, context }) => {
  // 至少 22 条待审核：保证有第 2 页；png / pdf 两条最后提交
  for (let i = 0; i < 20; i++) {
    const f = await seedUser({ verified: false })
    await submitEnterpriseKyc(await userApi(f.phone), { company: `填充企业-${runStamp}-${i}`, licenseNo: LICENSE_NO, file: 'license.png' })
  }
  const png = await seedUser({ verified: false })
  const pngApi = await userApi(png.phone)
  const pngKey = await submitEnterpriseKyc(pngApi, { company: `图片企业-${runStamp}`, licenseNo: LICENSE_NO, file: 'license.png' })
  const pdf = await seedUser({ verified: false })
  const pdfKey = await submitEnterpriseKyc(await userApi(pdf.phone), { company: `文档企业-${runStamp}`, licenseNo: LICENSE_NO, file: 'license.pdf' })
  expect(pngKey).toMatch(/\.png$/)
  expect(pdfKey).toMatch(/\.pdf$/)
  const admin = await createAdmin('ops')
  const ctx = await adminApi(admin)
  const all = await allPending(ctx)
  expect(all.length).toBeGreaterThanOrEqual(22)
  const pngAt = recordOf(all, `图片企业-${runStamp}`)
  const pdfAt = recordOf(all, `文档企业-${runStamp}`)

  const listRequests = recordRequests(page, 'GET', '/kyc/pending')
  const searches = () => listRequests.map((r) => r.search)
  const toasts = await trackToasts(page)
  const isList = isPath('/kyc/pending')

  await test.step('首次加载失败：页内错误与重试，只提示一次，不自动重试', async () => {
    await page.route(isList, fail500('req-kyc-1'))
    await loginAdmin(page, admin)
    await page.getByTestId('menu-kyc').click()
    await expectAdminUrl(page, '/kyc')
    await expect(page.getByTestId('kyc-error')).toContainText('服务器内部错误（请求号 req-kyc-1）')
    await expect(rows(page)).toHaveCount(0)
    await drainTasks(page)
    expect(searches()).toEqual(['?page=1&page_size=20'])
    expect(await toasts()).toEqual(['服务器内部错误（请求号 req-kyc-1）'])
    await page.unroute(isList)
    await page.getByTestId('kyc-retry').click()
    await expect(page.getByTestId('kyc-error')).toHaveCount(0)
    await expect(rows(page)).toHaveCount(20)
    expect(searches()).toEqual(['?page=1&page_size=20', '?page=1&page_size=20'])
  })

  await test.step('第 1、2 页与接口顺序一致；总数', async () => {
    await expect(page.getByTestId('kyc-total')).toHaveText(`共 ${all.length} 条待审核`)
    await expect(page.getByTestId('kyc-company')).toHaveText(all.slice(0, 20).map((r) => r.real_name))
    await page.getByTestId('kyc-pager').getByText('2', { exact: true }).click()
    await expect.poll(() => searches().at(-1)).toBe('?page=2&page_size=20')
    await expect(page.getByTestId('kyc-company')).toHaveText(all.slice(20, 40).map((r) => r.real_name))
  })

  await test.step('PNG 记录：列表各列；审核弹窗里 <img> 从后台文件接口加载成功', async () => {
    await openKycPage(page, pngAt.page)
    const row = rowOf(page, `图片企业-${runStamp}`)
    await expect(row.getByTestId('kyc-license-no')).toHaveText(LICENSE_NO)
    await expect(row.getByTestId('kyc-user')).toHaveText(String(png.id))
    await expect(row.getByTestId('kyc-user').locator('a')).toHaveAttribute('href', `/admin/users/${png.id}`)
    await expect(row.getByTestId('kyc-submitted')).toHaveText(cstTime(pngAt.record.created_at))
    await row.getByTestId('kyc-review-btn').click()
    await expect(page.getByTestId('kyc-review-dialog')).toBeVisible()
    await expect(page.getByTestId('kyc-review-company')).toHaveText(`图片企业-${runStamp}`)
    await expect(page.getByTestId('kyc-review-license-no')).toHaveText(LICENSE_NO)
    await expect(page.getByTestId('kyc-review-user')).toHaveText(String(png.id))
    const img = page.getByTestId('kyc-preview-img')
    await expect(img).toBeVisible()
    await expect(img).toHaveAttribute('src', `/admin/api/v1/files/${pngKey}`)
    await expect.poll(() => img.evaluate((el) => (el as HTMLImageElement).complete && (el as HTMLImageElement).naturalWidth)).toBe(1)
    await expect(page.getByTestId('kyc-open-pdf')).toHaveCount(0)
    await expect(page.getByTestId('kyc-open-file')).toHaveCount(0)
    await page.keyboard.press('Escape')
    await expect(page.getByTestId('kyc-review-dialog')).toBeHidden()
  })

  await test.step('PDF 记录：不内嵌预览，点按钮在新窗口打开 /admin/api/v1/files/<key>', async () => {
    await openKycPage(page, pdfAt.page)
    await rowOf(page, `文档企业-${runStamp}`).getByTestId('kyc-review-btn').click()
    await expect(page.getByTestId('kyc-review-dialog')).toBeVisible()
    await expect(page.getByTestId('kyc-preview-img')).toHaveCount(0)
    const popup = context.waitForEvent('page')
    await page.getByTestId('kyc-open-pdf').click()
    const opened = await popup
    expect(opened.url()).toBe(`${ADMIN_URL}/admin/api/v1/files/${pdfKey}`)
    expect(await opened.evaluate(() => window.opener)).toBeNull() // noopener：新窗口拿不到后台页面
    await opened.close()
    // 新窗口带的是本页的后台会话：同一个地址能读到 PDF
    const r = await page.request.get(`/admin/api/v1/files/${pdfKey}`)
    expect(r.status()).toBe(200)
    expect(r.headers()['content-type']).toBe('application/pdf')
    await page.keyboard.press('Escape')
  })

  await test.step('图片加载失败：改为「在新窗口打开」按钮', async () => {
    const isFile = isPath(`/files/${pngKey}`)
    await page.route(isFile, (r) => r.fulfill({ status: 404, contentType: 'application/json', body: '{"code":"not_found","message":"不存在"}' }))
    await openKycPage(page, pngAt.page)
    await rowOf(page, `图片企业-${runStamp}`).getByTestId('kyc-review-btn').click()
    await expect(page.getByTestId('kyc-open-file')).toBeVisible()
    await expect(page.getByTestId('kyc-preview-img')).toHaveCount(0)
    await page.unroute(isFile)
    const popup = context.waitForEvent('page')
    await page.getByTestId('kyc-open-file').click()
    const opened = await popup
    expect(opened.url()).toBe(`${ADMIN_URL}/admin/api/v1/files/${pngKey}`)
    expect(await opened.evaluate(() => window.opener)).toBeNull() // noopener：新窗口拿不到后台页面
    await opened.close()
  })

  await test.step('营业执照只有运维 / 超管能读取：控制台用户会话、未登录都是 401，财务 403；控制台没有读取接口', async () => {
    // 控制台与后台同主机不同端口，Cookie 按主机共享（不区分端口）：用户会话 Cookie（Path=/、仅主机）会被带到后台地址上
    const session = (await pngApi.storageState()).cookies.find((c) => c.name === 'sc_session')
    expect(session).toMatchObject({ domain: '127.0.0.1', path: '/' })
    expect(new URL(ADMIN_URL).hostname).toBe(session!.domain)
    expect((await pngApi.get(`${CONSOLE_URL}/api/v1/wallet`)).status()).toBe(200) // 会话本身有效
    const asUser = await pngApi.get(`${ADMIN_URL}/admin/api/v1/files/${pngKey}`)
    expect(asUser.status()).toBe(401)
    // 显式带上这个 Cookie 头再请求一次，排除「Cookie 根本没发出去」的可能
    const anonWithCookie = await request.newContext({ baseURL: ADMIN_URL })
    const explicit = await anonWithCookie.get(`/admin/api/v1/files/${pngKey}`, { headers: { cookie: `sc_session=${session!.value}` } })
    expect(explicit.status()).toBe(401)
    expect((await pngApi.get(`${CONSOLE_URL}/api/v1/files/${pngKey}`)).status()).toBe(404)
    // 控制台端口上的 /admin/... 只是前端路由回退（index.html），不是文件
    const viaConsole = await pngApi.get(`${CONSOLE_URL}/admin/api/v1/files/${pngKey}`)
    expect(viaConsole.headers()['content-type']).toContain('text/html')
    const anon = await request.newContext({ baseURL: ADMIN_URL })
    expect((await anon.get(`/admin/api/v1/files/${pngKey}`)).status()).toBe(401)
    const finance = await adminApi(await createAdmin('finance'))
    expect((await finance.get(`/admin/api/v1/files/${pngKey}`)).status()).toBe(403)
    expect((await ctx.get(`/admin/api/v1/files/${pngKey}`)).status()).toBe(200)
  })
})

test('A-KYC-02 通过与驳回：通过 → verified；驳回原因必填（不发请求）；填原因 → rejected；失败保留输入；连点只发一次；列表随之更新', async ({ page }) => {
  const ok = await seedUser({ verified: false })
  await submitEnterpriseKyc(await userApi(ok.phone), { company: `通过企业-${runStamp}`, licenseNo: LICENSE_NO, file: 'license.png' })
  const bad = await seedUser({ verified: false })
  await submitEnterpriseKyc(await userApi(bad.phone), { company: `驳回企业-${runStamp}`, licenseNo: LICENSE_NO, file: 'license.png' })
  const admin = await createAdmin('ops')
  const ctx = await adminApi(admin)
  const okAt = recordOf(await allPending(ctx), `通过企业-${runStamp}`)
  const approves = recordRequests(page, 'POST', `/kyc/${okAt.record.id}/review`)
  const lists = recordRequests(page, 'GET', '/kyc/pending')
  const toasts = await trackToasts(page)
  const userOf = async (id: number) =>
    (await (await ctx.get(`/admin/api/v1/users/${id}`)).json()) as { user: { kyc_status: string; kind: string }; kyc: { kind: string; result: string; reason: string }[] }
  await loginAdmin(page, admin)

  await test.step('通过：先确认，取消不发请求；连点确认只发一次；提示已通过，该行从列表消失（列表重新加载）；用户已认证', async () => {
    await openKycPage(page, okAt.page)
    await rowOf(page, `通过企业-${runStamp}`).getByTestId('kyc-review-btn').click()
    await page.getByTestId('kyc-approve').click()
    await page.getByTestId('kyc-approve-cancel').click()
    await expect(page.getByTestId('kyc-approve-ok')).toHaveCount(0)
    expect(approves).toHaveLength(0)
    await page.getByTestId('kyc-approve').click()
    const before = lists.length
    await clickTwiceSync(page.getByTestId('kyc-approve-ok'))
    await toast(page, '已通过')
    await expect(page.getByTestId('kyc-review-dialog')).toBeHidden()
    await expect(rowOf(page, `通过企业-${runStamp}`)).toHaveCount(0)
    await drainTasks(page)
    expect(lists.length).toBe(before + 1)
    expect(approves.map((a) => a.body)).toEqual([{ approve: true, reason: '' }])
    expect(await toasts()).toEqual(['已通过'])
    const u = await userOf(ok.id)
    expect(u.user.kyc_status).toBe('verified')
    expect(u.user.kind).toBe('enterprise')
    expect(u.kyc[0]).toMatchObject({ kind: 'enterprise', result: 'verified' })
  })

  const badAt = recordOf(await allPending(ctx), `驳回企业-${runStamp}`)
  const rejects = recordRequests(page, 'POST', `/kyc/${badAt.record.id}/review`)

  await test.step('驳回原因必填：空与空白都在页内提示，不发请求', async () => {
    await openKycPage(page, badAt.page)
    await rowOf(page, `驳回企业-${runStamp}`).getByTestId('kyc-review-btn').click()
    await page.getByTestId('kyc-reject').click()
    await page.getByTestId('kyc-reject-submit').click()
    await expect(page.getByTestId('kyc-reject-error')).toHaveText('请填写驳回原因')
    await page.getByTestId('kyc-reject-reason').fill('   ')
    await page.getByTestId('kyc-reject-submit').click()
    await expect(page.getByTestId('kyc-reject-error')).toHaveText('请填写驳回原因')
    await drainTasks(page)
    expect(rejects).toHaveLength(0)
  })

  await test.step('接口失败：只提示一次，弹窗不关、原因保留', async () => {
    await page.getByTestId('kyc-reject-reason').fill('执照不清晰')
    await page.route(isPath(`/kyc/${badAt.record.id}/review`), fail500('req-kyc-r1'), { times: 1 })
    await page.getByTestId('kyc-reject-submit').click()
    await toast(page, '服务器内部错误（请求号 req-kyc-r1）')
    await expect(toastLocator(page, '服务器内部错误（请求号 req-kyc-r1）')).toHaveCount(0)
    await expect(page.getByTestId('kyc-review-dialog')).toBeVisible()
    await expect(page.getByTestId('kyc-reject-reason')).toHaveValue('执照不清晰')
    await expect(page.getByTestId('kyc-reject-error')).toHaveCount(0)
    await expect(rowOf(page, `驳回企业-${runStamp}`)).toHaveCount(1)
  })

  await test.step('连点确认驳回只发一次；提示已驳回，该行消失；用户未通过且记录带原因', async () => {
    await clickTwiceSync(page.getByTestId('kyc-reject-submit'))
    await toast(page, '已驳回')
    await expect(page.getByTestId('kyc-review-dialog')).toBeHidden()
    await expect(rowOf(page, `驳回企业-${runStamp}`)).toHaveCount(0)
    await drainTasks(page)
    expect(rejects.map((r) => r.body)).toEqual([{ approve: false, reason: '执照不清晰' }, { approve: false, reason: '执照不清晰' }])
    const u = await userOf(bad.id)
    expect(u.user.kyc_status).toBe('rejected')
    expect(u.kyc[0]).toMatchObject({ kind: 'enterprise', result: 'rejected', reason: '执照不清晰' })
    expect(await toasts()).toEqual(['服务器内部错误（请求号 req-kyc-r1）', '已驳回']) // 翻页用的 page.goto 让记录从零开始
  })
})
