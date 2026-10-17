import { expect, request, test, type Page, type Route } from '@playwright/test'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import {
  adminApi, adminPost, clickTwiceSync, createAdmin, createPDU, drainIdleMachines, drainTasks, expectAdminUrl, loginAdmin, nextSerial, recordRequests, registerMachine, trackToasts,
} from '../helpers/admin'
import { placeOrder, seedUser, toast, userApi } from '../helpers/console'
import { CONSOLE_URL, hooks } from '../helpers/hooks'
import { runStamp } from '../helpers/phone'
import { cstTime } from '../helpers/time'

type MachineRow = { id: number; serial: string; state: string; agent_online: boolean; last_seen_at: string | null; pdu_id: number | null }
type Paged = { items: MachineRow[]; total: number }

const fixtureText = (name: string) => readFileSync(fileURLToPath(new URL(`../fixtures/${name}`, import.meta.url)), 'utf8').replaceAll('{{N}}', runStamp)
const rowBySerial = (page: Page, serial: string) => page.getByRole('row').filter({ has: page.getByTestId('machine-serial').filter({ hasText: serial }) })
const machineRows = (page: Page) => page.getByRole('row').filter({ has: page.getByTestId('machine-id') })
const fail500 = (id: string) => (r: Route) =>
  r.fulfill({ status: 500, contentType: 'application/json', body: JSON.stringify({ code: 'internal', message: '服务器内部错误', request_id: id }) })
const isMachines = (u: URL) => u.pathname === '/admin/api/v1/machines'

async function idleCount(): Promise<number> {
  const ctx = await request.newContext({ baseURL: CONSOLE_URL })
  return ((await (await ctx.get('/api/v1/products')).json()) as { items: { idle_count: number }[] }).items[0].idle_count
}

async function pick(page: Page, testid: string, option: string) {
  await page.getByTestId(testid).click()
  await page.getByRole('option', { name: option, exact: true }).click()
}

test('A-MCH-01 机器列表：状态 / 在线 / 机柜筛选作为查询参数、重置、分页与每页条数、首次加载失败可重试', async ({ page }) => {
  // 数据库整轮共享：本用例的机器都登记在唯一机柜下，断言只针对这些机柜
  const admin = await createAdmin('ops')
  const ctx = await adminApi(admin)
  const rack = `R${runStamp}`
  const bigRack = `P${runStamp}`
  const pdu = await createPDU(ctx, admin)
  const offline = await registerMachine(ctx, admin, { rack, pdu_id: pdu.id, pdu_outlet: 3 }) // 登记后维护中、从未心跳 → 离线
  const online = await registerMachine(ctx, admin, { rack })
  await adminPost(ctx, admin, `/admin/api/v1/machines/${online.id}/maintenance`, { on: false })
  await hooks.agent({ machine_id: online.id }) // 只对这台心跳 → 在线、空闲
  const onlineRow = ((await (await ctx.get(`/admin/api/v1/machines?rack=${rack}`)).json()) as Paged).items.find((m) => m.id === online.id)!
  expect(onlineRow.last_seen_at).not.toBeNull()
  const product = ((await (await ctx.get('/admin/api/v1/products')).json()) as { items: { name: string }[] }).items[0]
  for (let i = 0; i < 21; i++) await registerMachine(ctx, admin, { rack: bigRack })

  const listRequests = recordRequests(page, 'GET', '/machines')
  const searches = () => listRequests.map((r) => r.search)

  await test.step('首次加载失败：清空表格、显示页内错误与重试，不自动重试；重试后正常显示', async () => {
    await page.route(isMachines, fail500('req-mch-1'))
    await loginAdmin(page, admin)
    await page.getByTestId('menu-machines').click()
    await expectAdminUrl(page, '/machines')
    await expect(page.getByTestId('machines-error')).toContainText('服务器内部错误（请求号 req-mch-1）')
    await expect(machineRows(page)).toHaveCount(0)
    await drainTasks(page)
    expect(searches()).toEqual(['?page=1&page_size=20'])
    await page.unroute(isMachines)
    await page.getByTestId('machines-retry').click()
    await expect(page.getByTestId('machines-error')).toHaveCount(0)
    await expect(machineRows(page).first()).toBeVisible()
    expect(searches()).toEqual(['?page=1&page_size=20', '?page=1&page_size=20'])
  })

  await test.step('机柜筛选：rack 作为查询参数；列显示序列号、机柜 / 槽位、IP、商品、状态、在线（含最后心跳）、PDU 名称 + 插座', async () => {
    await page.getByTestId('machines-filter-rack').fill(rack)
    await page.getByTestId('machines-search').click()
    await expect.poll(() => searches().at(-1)).toBe(`?rack=${rack}&page=1&page_size=20`)
    await expect(page.getByTestId('machines-total')).toHaveText('共 2 台')
    await expect(machineRows(page)).toHaveCount(2)
    const off = rowBySerial(page, offline.serial)
    await expect(off.getByTestId('machine-id')).toHaveText(String(offline.id))
    await expect(off.getByTestId('machine-rack')).toHaveText(new RegExp(`^${rack} / \\d+$`))
    await expect(off.getByTestId('machine-ip')).toHaveText(/^10\.96\.\d+\.\d+$/)
    await expect(off.getByTestId('machine-state')).toHaveText('维护中')
    await expect(off.getByTestId('machine-online')).toHaveText('离线 从未心跳')
    await expect(off.getByTestId('machine-online')).toHaveAttribute('data-online', 'false')
    await expect(off.getByTestId('machine-pdu')).toHaveText(`${pdu.name} #3`)
    const on = rowBySerial(page, online.serial)
    await expect(on.getByTestId('machine-state')).toHaveText('空闲')
    await expect(on.getByTestId('machine-online')).toHaveText(`在线 ${cstTime(onlineRow.last_seen_at!)}`)
    await expect(on.getByTestId('machine-online')).toHaveAttribute('data-online', 'true')
    await expect(on.getByTestId('machine-product')).toHaveText(product.name)
    await expect(on.getByTestId('machine-pdu')).toHaveText('—')
  })

  await test.step('状态筛选：state=idle 只剩在线那台', async () => {
    await pick(page, 'machines-filter-state', '空闲')
    await page.getByTestId('machines-search').click()
    await expect.poll(() => searches().at(-1)).toBe(`?state=idle&rack=${rack}&page=1&page_size=20`)
    await expect(page.getByTestId('machines-total')).toHaveText('共 1 台')
    await expect(rowBySerial(page, online.serial)).toHaveCount(1)
    await expect(rowBySerial(page, offline.serial)).toHaveCount(0)
  })

  await test.step('在线筛选：online=false 只剩离线那台；维护中 + 在线 → 空', async () => {
    await page.getByTestId('machines-filter-state').hover()
    await page.getByTestId('machines-filter-state').locator('.el-select__clear').click() // 清空状态（Element Plus 的清除图标）
    await pick(page, 'machines-filter-online', '否')
    await page.getByTestId('machines-search').click()
    await expect.poll(() => searches().at(-1)).toBe(`?online=false&rack=${rack}&page=1&page_size=20`)
    await expect(page.getByTestId('machines-total')).toHaveText('共 1 台')
    await expect(rowBySerial(page, offline.serial)).toHaveCount(1)
    await expect(rowBySerial(page, online.serial)).toHaveCount(0)
    await pick(page, 'machines-filter-state', '维护中')
    await pick(page, 'machines-filter-online', '是')
    await page.getByTestId('machines-search').click()
    await expect.poll(() => searches().at(-1)).toBe(`?state=maintenance&online=true&rack=${rack}&page=1&page_size=20`)
    await expect(page.getByTestId('machines-total')).toHaveText('共 0 台')
    await expect(machineRows(page)).toHaveCount(0)
  })

  await test.step('重置：清空全部条件，只发一次不带筛选的请求，total 与该响应一致', async () => {
    const before = listRequests.length
    const resp = page.waitForResponse((r) => isMachines(new URL(r.url())) && r.request().method() === 'GET')
    await page.getByTestId('machines-reset').click()
    const body = (await (await resp).json()) as Paged
    expect(searches().at(-1)).toBe('?page=1&page_size=20')
    await expect(page.getByTestId('machines-total')).toHaveText(`共 ${body.total} 台`)
    await expect(page.getByTestId('machines-filter-rack')).toHaveValue('')
    await expect(page.getByTestId('machines-filter-state')).not.toContainText('维护中')
    await expect(page.getByTestId('machines-filter-online')).not.toContainText('是')
    await drainTasks(page)
    expect(listRequests).toHaveLength(before + 1)
  })

  await test.step('超过一页：21 台机器第 1 页 20 行，翻到第 2 页带 page=2 显示 1 行', async () => {
    await page.getByTestId('machines-filter-rack').fill(bigRack)
    await page.getByTestId('machines-search').click()
    await expect.poll(() => searches().at(-1)).toBe(`?rack=${bigRack}&page=1&page_size=20`)
    await expect(page.getByTestId('machines-total')).toHaveText('共 21 台')
    await expect(machineRows(page)).toHaveCount(20)
    await page.getByTestId('machines-pager').getByText('2', { exact: true }).click()
    await expect.poll(() => searches().at(-1)).toBe(`?rack=${bigRack}&page=2&page_size=20`)
    await expect(machineRows(page)).toHaveCount(1)
  })

  await test.step('每页条数改为 50：重新加载（page_size=50，回到第 1 页），一次请求显示全部 21 行', async () => {
    const before = listRequests.length
    await page.getByTestId('machines-pager').locator('.el-pagination__sizes').click() // Element Plus 的每页条数下拉
    await page.getByRole('option', { name: /^50/ }).click()
    await expect.poll(() => searches().at(-1)).toBe(`?rack=${bigRack}&page=1&page_size=50`)
    await expect(machineRows(page)).toHaveCount(21)
    await drainTasks(page)
    expect(listRequests).toHaveLength(before + 1)
    await page.getByTestId('machines-pager').locator('.el-pagination__sizes').click()
    await page.getByRole('option', { name: /^100/ }).click()
    await expect.poll(() => searches().at(-1)).toBe(`?rack=${bigRack}&page=1&page_size=100`)
    await expect(machineRows(page)).toHaveCount(21)
  })

  await test.step('改条件后加载失败：清空表格并显示错误，重试按当前条件请求', async () => {
    await page.route(isMachines, fail500('req-mch-2'))
    await page.getByTestId('machines-filter-rack').fill(rack)
    await page.getByTestId('machines-search').click()
    await expect(page.getByTestId('machines-error')).toContainText('服务器内部错误（请求号 req-mch-2）')
    await expect(machineRows(page)).toHaveCount(0)
    await page.unroute(isMachines)
    await page.getByTestId('machines-retry').click()
    await expect.poll(() => searches().at(-1)).toBe(`?rack=${rack}&page=1&page_size=100`)
    await expect(machineRows(page)).toHaveCount(2)
    await expect(page.getByTestId('machines-error')).toHaveCount(0)
  })

  await test.step('行点击进机器详情', async () => {
    await rowBySerial(page, online.serial).getByTestId('machine-serial').click()
    await expectAdminUrl(page, `/machines/${online.id}`)
    await expect(page.getByTestId('mdetail-serial')).toHaveText(online.serial)
  })

  await test.step('财务没有机器菜单，直接访问机器页与详情页 → 403', async () => {
    const fin = await createAdmin('finance')
    await page.context().clearCookies()
    await loginAdmin(page, fin)
    await expect(page.getByTestId('menu-machines')).toHaveCount(0)
    await page.goto('/admin/machines')
    await expectAdminUrl(page, '/403', { from: '/machines' })
    await page.goto(`/admin/machines/${online.id}`)
    await expectAdminUrl(page, '/403', { from: `/machines/${online.id}` })
  })
})

test('A-MCH-02 单台登记：成功出现在列表；校验错误、重复序列号、插座被占用原文提示；PDU 与插座只填一个提示；连点只发一次', async ({ page }) => {
  const admin = await createAdmin('ops')
  const ctx = await adminApi(admin)
  const pdu = await createPDU(ctx, admin)
  const rack = `G${runStamp}`
  const taken = await registerMachine(ctx, admin, { rack: `T${runStamp}`, pdu_id: pdu.id, pdu_outlet: 5 })
  const products = ((await (await ctx.get('/admin/api/v1/products')).json()) as { items: { id: number; name: string }[] }).items
  const toasts = await trackToasts(page)
  const posts = recordRequests(page, 'POST', '/machines')
  const listRequests = recordRequests(page, 'GET', '/machines')
  const serial = nextSerial()
  const dlg = page.getByTestId('register-dialog')
  const submitAndExpect = async (message: string) => {
    const n = posts.length
    await page.getByTestId('register-submit').click()
    await toast(page, message)
    await expect.poll(() => posts.length).toBe(n + 1)
    await expect(dlg).toBeVisible() // 失败时弹窗保留，便于修改
  }

  await loginAdmin(page, admin)
  await page.getByTestId('menu-machines').click()
  await page.getByTestId('machines-filter-rack').fill(rack)
  await page.getByTestId('machines-search').click()
  await expect.poll(() => listRequests.at(-1)?.search).toBe(`?rack=${rack}&page=1&page_size=20`)
  await expect(machineRows(page)).toHaveCount(0)

  await test.step('校验：序列号为空、IP 格式不正确 → 后端原文提示', async () => {
    await page.getByTestId('machines-register-btn').click()
    await expect(page.getByTestId('register-product')).toContainText(products[0].name) // 商品缺省第一个
    await page.getByTestId('register-rack').fill(rack)
    await page.getByTestId('register-slot').fill('07')
    await page.getByTestId('register-ip').fill('10.95.0.7')
    await submitAndExpect('请填写序列号')
    await page.getByTestId('register-serial').fill(serial)
    await page.getByTestId('register-ip').fill('10.95.0')
    await submitAndExpect('内网 IP 格式不正确')
    await page.getByTestId('register-ip').fill('10.95.0.7')
  })

  await test.step('PDU 与插座号只填一个 → 提示；请求体只带 pdu_id', async () => {
    await pick(page, 'register-pdu', pdu.name)
    await submitAndExpect('PDU 与插座号需同时填写')
    expect(posts.at(-1)!.body).toEqual({ serial, rack, slot: '07', lan_ip: '10.95.0.7', product_id: products[0].id, pdu_id: pdu.id })
  })

  await test.step('插座已被其他机器使用 → 提示「该 PDU 插座已被其他机器使用」', async () => {
    await page.getByTestId('register-outlet').locator('input').fill('5')
    await submitAndExpect('该 PDU 插座已被其他机器使用')
    expect(posts.at(-1)!.body).toMatchObject({ pdu_id: pdu.id, pdu_outlet: 5 })
  })

  await test.step('成功（连点两次只发一个请求）：提示、关闭弹窗、列表按当前筛选刷新出现新机器', async () => {
    await page.getByTestId('register-outlet').locator('input').fill('6')
    const n = posts.length
    const lists = listRequests.length
    await clickTwiceSync(page.getByTestId('register-submit'))
    await toast(page, '机器已登记')
    await expect(dlg).toBeHidden()
    await expect(rowBySerial(page, serial)).toHaveCount(1)
    await expect(rowBySerial(page, serial).getByTestId('machine-pdu')).toHaveText(`${pdu.name} #6`)
    await expect(rowBySerial(page, serial).getByTestId('machine-state')).toHaveText('维护中')
    await drainTasks(page)
    expect(posts).toHaveLength(n + 1)
    expect(posts.at(-1)!.body).toEqual({ serial, rack, slot: '07', lan_ip: '10.95.0.7', product_id: products[0].id, pdu_id: pdu.id, pdu_outlet: 6 })
    expect(listRequests.length).toBe(lists + 1)
    expect(listRequests.at(-1)!.search).toBe(`?rack=${rack}&page=1&page_size=20`)
    const api = (await (await ctx.get(`/admin/api/v1/machines?rack=${rack}`)).json()) as Paged
    expect(api.items.map((m) => m.serial)).toEqual([serial])
  })

  await test.step('同一序列号再登记 → 「序列号已存在」，不带 PDU 时请求体没有 pdu 字段', async () => {
    await page.getByTestId('machines-register-btn').click()
    await expect(page.getByTestId('register-serial')).toHaveValue('') // 重新打开是空表单
    await page.getByTestId('register-serial').fill(serial)
    await page.getByTestId('register-rack').fill(rack)
    await page.getByTestId('register-slot').fill('08')
    await page.getByTestId('register-ip').fill('10.95.0.8')
    await submitAndExpect('序列号已存在')
    expect(posts.at(-1)!.body).toEqual({ serial, rack, slot: '08', lan_ip: '10.95.0.8', product_id: products[0].id })
    expect(taken.serial).not.toBe(serial)
  })

  await test.step('每次失败只提示一次（notifyError，原文）', async () => {
    const errors = (await toasts()).filter((t) => t !== '机器已登记')
    expect(errors).toEqual(['请填写序列号', '内网 IP 格式不正确', 'PDU 与插座号需同时填写', '该 PDU 插座已被其他机器使用', '序列号已存在'])
  })

  await test.step('商品 / PDU 名称加载失败：提示一次，列表照常显示（PDU 列退回 ID）', async () => {
    const isPdus = (u: URL) => u.pathname === '/admin/api/v1/pdus'
    await page.route(isPdus, fail500('req-opt-1'))
    await page.reload()
    await page.getByTestId('machines-filter-rack').fill(rack)
    await page.getByTestId('machines-search').click()
    await expect(rowBySerial(page, serial).getByTestId('machine-pdu')).toHaveText(`${pdu.id} #6`)
    await expect(page.getByTestId('machines-error')).toHaveCount(0)
    await drainTasks(page)
    expect(await toasts()).toEqual(['服务器内部错误（请求号 req-opt-1）'])
    await page.unroute(isPdus)
  })
})

test('A-MCH-03 CSV 导入：超过 1MB 本地拒绝；成功 3 台并刷新列表；逐行错误整批不导入；模板带 BOM 与固定表头', async ({ page }) => {
  const admin = await createAdmin('ops')
  const ctx = await adminApi(admin)
  const countRack = async (rack: string) => ((await (await ctx.get(`/admin/api/v1/machines?rack=${rack}&page_size=1`)).json()) as Paged).total
  const imports = recordRequests(page, 'POST', '/machines/import')
  const listRequests = recordRequests(page, 'GET', '/machines')
  const dlg = page.getByTestId('import-dialog')
  await loginAdmin(page, admin)
  await page.getByTestId('menu-machines').click()
  await page.getByTestId('machines-filter-rack').fill('B01')
  await page.getByTestId('machines-search').click()
  await expect.poll(() => listRequests.at(-1)?.search).toBe('?rack=B01&page=1&page_size=20')

  await test.step('文件超过 1MB：页内提示「文件不能超过 1MB」，导入按钮禁用，不发请求', async () => {
    await page.getByTestId('machines-import-btn').click()
    await expect(page.getByTestId('import-submit')).toBeDisabled()
    await page.getByTestId('import-file').setInputFiles({ name: 'big.csv', mimeType: 'text/csv', buffer: Buffer.alloc((1 << 20) + 1, 0x61) })
    await expect(page.getByTestId('import-file-error')).toContainText('文件不能超过 1MB')
    await expect(page.getByTestId('import-file-name')).toHaveCount(0)
    await expect(page.getByTestId('import-submit')).toBeDisabled()
    await page.getByTestId('import-submit').click({ force: true })
    await drainTasks(page)
    expect(imports).toHaveLength(0)
  })

  await test.step('成功导入 3 台：提示台数、关闭弹窗、列表按当前筛选刷新出现 3 台；连点只发一次', async () => {
    const before = await countRack('B01')
    const text = fixtureText('machines-ok.csv')
    await page.getByTestId('import-file').setInputFiles({ name: 'machines-ok.csv', mimeType: 'text/csv', buffer: Buffer.from(text) })
    await expect(page.getByTestId('import-file-error')).toHaveCount(0)
    await expect(page.getByTestId('import-file-name')).toHaveText('已选择：machines-ok.csv')
    const lists = listRequests.length
    await clickTwiceSync(page.getByTestId('import-submit'))
    await toast(page, '成功导入 3 台机器')
    await expect(dlg).toBeHidden()
    for (let i = 1; i <= 3; i++) await expect(rowBySerial(page, `E2E-CSV-${runStamp}-${i}`)).toHaveCount(1)
    await drainTasks(page)
    expect(imports).toHaveLength(1)
    expect(imports[0].body).toEqual({ csv: text })
    expect(listRequests.length).toBe(lists + 1)
    expect(await countRack('B01')).toBe(before + 3)
  })

  await test.step('逐行错误：列出行号与原因，整批不导入；重新选择文件清空错误', async () => {
    const before = await countRack('B02')
    await page.getByTestId('machines-import-btn').click()
    await expect(page.getByTestId('import-file-name')).toHaveCount(0) // 重新打开是空的
    await page.getByTestId('import-file').setInputFiles({ name: 'machines-bad.csv', mimeType: 'text/csv', buffer: Buffer.from(fixtureText('machines-bad.csv')) })
    await page.getByTestId('import-submit').click()
    await toast(page, '导入数据有误，未导入任何机器')
    const rows = page.getByTestId('import-errors').getByRole('row').filter({ has: page.getByTestId('import-error-line') })
    await expect(rows.getByTestId('import-error-line')).toHaveText(['2', '3', '4'])
    await expect(rows.getByTestId('import-error-message')).toHaveText(['内网 IP 格式不正确', '商品 no-such-product 不存在', 'PDU ghost-pdu 不存在'])
    await expect(dlg).toBeVisible()
    expect(imports).toHaveLength(2)
    expect(await countRack('B02')).toBe(before)
    await page.getByTestId('import-file').setInputFiles({ name: 'machines-ok.csv', mimeType: 'text/csv', buffer: Buffer.from(fixtureText('machines-ok.csv')) })
    await expect(page.getByTestId('import-errors')).toHaveCount(0)
  })

  await test.step('模板下载：文件名、UTF-8 BOM 与固定表头', async () => {
    const download = page.waitForEvent('download')
    await page.getByTestId('import-template').click()
    const file = await download
    expect(file.suggestedFilename()).toBe('machines-template.csv')
    const bytes = readFileSync(await file.path())
    expect([...bytes.subarray(0, 3)]).toEqual([0xef, 0xbb, 0xbf])
    expect(bytes.subarray(3).toString('utf8')).toBe('serial,rack,slot,lan_ip,product_code,pdu_name,pdu_outlet\n')
  })
})

test('A-MCH-04 维护：进入维护 → 维护中、市场空闲数减 1；结束维护恢复；有实例时结束维护提示错误', async ({ page }) => {
  const admin = await createAdmin('ops')
  const [mid] = await hooks.machines(1)
  const before = await idleCount()
  const posts = recordRequests(page, 'POST', `/machines/${mid}/maintenance`)
  const btn = page.getByTestId('mdetail-maintenance-btn')
  await loginAdmin(page, admin)
  await page.goto(`/admin/machines/${mid}`)

  await test.step('进入维护（连点确定只发一次）：状态「维护中」、按钮变「结束维护」、市场空闲数减 1', async () => {
    await expect(page.getByTestId('mdetail-state')).toHaveText('空闲')
    await expect(btn).toHaveText('进入维护')
    await expect(page.getByTestId('mdetail-none')).toHaveText('无')
    await btn.click()
    await clickTwiceSync(page.getByTestId('mdetail-maintenance-ok'))
    await toast(page, '已进入维护')
    await expect(page.getByTestId('mdetail-state')).toHaveText('维护中')
    await expect(btn).toHaveText('结束维护')
    await expect(page.getByTestId('mdetail-maintenance-dialog')).toBeHidden()
    await drainTasks(page)
    expect(posts.map((p) => p.body)).toEqual([{ on: true }])
    expect(await idleCount()).toBe(before - 1)
  })

  await test.step('结束维护：状态回到「空闲」、按钮变「进入维护」、空闲数恢复', async () => {
    await btn.click()
    await page.getByTestId('mdetail-maintenance-ok').click()
    await toast(page, '已结束维护')
    await expect(page.getByTestId('mdetail-state')).toHaveText('空闲')
    await expect(btn).toHaveText('进入维护')
    expect(posts.map((p) => p.body)).toEqual([{ on: true }, { on: false }])
    expect(await idleCount()).toBe(before)
  })

  await test.step('洗机失败 → 故障（按钮为「结束维护」），实例停在洗机中 → 结束维护被拒绝，状态不变', async () => {
    await drainIdleMachines(await adminApi(admin), admin)
    const [faultyId] = await hooks.machines(1)
    const u = await seedUser()
    const api = await userApi(u.phone)
    const instanceId = await placeOrder(api)
    await hooks.agent({ machine_id: faultyId })
    await api.post(`/api/v1/instances/${instanceId}/release`, { data: {} })
    await hooks.agent({ machine_id: faultyId, fail: true })
    await page.goto(`/admin/machines/${faultyId}`)
    await expect(page.getByTestId('mdetail-state')).toHaveText('故障')
    await expect(btn).toHaveText('结束维护')
    await btn.click()
    await page.getByTestId('mdetail-maintenance-ok').click()
    await toast(page, '该机器上还有未结束的实例，不能结束维护')
    await expect(page.getByTestId('mdetail-state')).toHaveText('故障')
    await expect(btn).toHaveText('结束维护')
  })
})

test('A-MCH-05 断电重启：确认文案、取消不发请求、无 PDU 报错、mock PDU 成功（连点只发一次）并写审计', async ({ page }) => {
  const admin = await createAdmin('ops')
  const ctx = await adminApi(admin)
  const noPdu = await registerMachine(ctx, admin)
  const pdu = await createPDU(ctx, admin)
  const withPdu = await registerMachine(ctx, admin, { pdu_id: pdu.id, pdu_outlet: 3 })
  const posts: string[] = []
  page.on('request', (r) => {
    if (r.method() === 'POST' && new URL(r.url()).pathname.endsWith('/power-cycle')) posts.push(new URL(r.url()).pathname)
  })
  const dlg = page.getByTestId('mdetail-power-dialog')
  await loginAdmin(page, admin)
  await page.goto(`/admin/machines/${noPdu.id}`)

  await test.step('确认框文案固定；取消关闭且不发请求', async () => {
    await page.getByTestId('mdetail-power-btn').click()
    await expect(page.getByTestId('mdetail-power-text')).toHaveText('将通过 PDU 对该机器断电重启，机器上的实例会中断，确定执行？')
    await page.getByTestId('mdetail-power-cancel').click()
    await expect(dlg).toBeHidden()
    await drainTasks(page)
    expect(posts).toEqual([])
  })

  await test.step('无 PDU 的机器：确定后提示「该机器未配置 PDU」', async () => {
    await page.getByTestId('mdetail-power-btn').click()
    await page.getByTestId('mdetail-power-ok').click()
    await toast(page, '该机器未配置 PDU')
    await expect(dlg).toBeHidden()
    expect(posts).toEqual([`/admin/api/v1/machines/${noPdu.id}/power-cycle`])
  })

  await test.step('mock PDU 的机器：连点确定只发一个请求，成功提示，审计出现 machine.power_cycle', async () => {
    await page.goto(`/admin/machines/${withPdu.id}`)
    await page.getByTestId('mdetail-power-btn').click()
    await clickTwiceSync(page.getByTestId('mdetail-power-ok'))
    await toast(page, '已通过 PDU 断电重启')
    await expect(dlg).toBeHidden()
    await drainTasks(page)
    expect(posts).toEqual([`/admin/api/v1/machines/${noPdu.id}/power-cycle`, `/admin/api/v1/machines/${withPdu.id}/power-cycle`])
    const audit = (await (await ctx.get(`/admin/api/v1/audit?action=machine.power_cycle&target_id=${withPdu.id}`)).json()) as { total: number; items: { diff: Record<string, unknown> }[] }
    expect(audit.total).toBe(1)
    expect(audit.items[0].diff).toMatchObject({ pdu_id: pdu.id, outlet: 3, ok: true })
  })
})

test('A-MCH-06 机器详情：当前实例与历史实例（可点击），首次加载失败可重试', async ({ page }) => {
  const admin = await createAdmin('ops')
  await drainIdleMachines(await adminApi(admin), admin)
  const [mid] = await hooks.machines(1)
  const u = await seedUser({ balance: 100000 })
  const api = await userApi(u.phone)
  const released = await placeOrder(api)
  await hooks.agent({ machine_id: mid })
  await api.post(`/api/v1/instances/${released}/release`, { data: {} })
  await hooks.agent({ machine_id: mid })
  const running = await placeOrder(api)
  await hooks.agent({ machine_id: mid })
  const isDetail = (url: URL) => url.pathname === `/admin/api/v1/machines/${mid}`
  const gets = recordRequests(page, 'GET', `/machines/${mid}`)

  await test.step('首次加载失败：页内错误与重试，不自动重试；重试成功后显示详情', async () => {
    await page.route(isDetail, fail500('req-mdet-1'))
    await loginAdmin(page, admin)
    await page.goto(`/admin/machines/${mid}`)
    await expect(page.getByTestId('mdetail-error')).toContainText('服务器内部错误（请求号 req-mdet-1）')
    await expect(page.getByTestId('mdetail-serial')).toHaveCount(0)
    await drainTasks(page)
    expect(gets).toHaveLength(1)
    await page.unroute(isDetail)
    await page.getByTestId('mdetail-retry').click()
    await expect(page.getByTestId('mdetail-error')).toHaveCount(0)
    expect(gets).toHaveLength(2)
  })

  await test.step('当前实例与历史实例显示并链接到实例详情', async () => {
    await expect(page.getByTestId('mdetail-state')).toHaveText('已分配')
    await expect(page.getByTestId('mdetail-online')).toHaveText('在线')
    await expect(page.getByTestId('mdetail-current')).toHaveText(running)
    await expect(page.getByTestId('mdetail-none')).toHaveCount(0)
    await expect(page.getByTestId('mdetail-history-id')).toHaveText([released])
    await expect(page.getByTestId('mdetail-history-id').locator('a')).toHaveAttribute('href', `/admin/instances/${released}`)
    await page.getByTestId('mdetail-current').locator('a').click()
    await expectAdminUrl(page, `/instances/${running}`)
  })
})
