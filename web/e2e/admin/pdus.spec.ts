import { expect, test, type Page, type Route } from '@playwright/test'
import { adminApi, clickTwiceSync, createAdmin, createPDU, drainTasks, expectAdminUrl, loginAdmin, recordRequests, registerMachine, trackToasts } from '../helpers/admin'
import { toast } from '../helpers/console'
import { runStamp } from '../helpers/phone'

type PduRow = { id: number; name: string; driver: string; address: string; has_community: boolean; oid_prefix: string }

const isPdus = (u: URL) => u.pathname === '/admin/api/v1/pdus'
const rowByName = (page: Page, name: string) => page.getByRole('row').filter({ has: page.getByTestId('pdu-name').filter({ hasText: name }) })
const fail500 = (id: string) => (r: Route) =>
  r.fulfill({ status: 500, contentType: 'application/json', body: JSON.stringify({ code: 'internal', message: '服务器内部错误', request_id: id }) })

/** 记录页面收到的每个后台 GET 响应体与每个请求地址，用于断言 community 从不下发、从不进 URL。 */
function recordTraffic(page: Page) {
  const bodies: { url: string; text: string }[] = []
  const urls: string[] = []
  page.on('request', (r) => urls.push(r.url()))
  page.on('response', async (r) => {
    if (r.request().method() !== 'GET' || !new URL(r.url()).pathname.startsWith('/admin/api/v1/')) return
    try {
      bodies.push({ url: r.url(), text: await r.text() })
    } catch {
      /* 页面跳走后响应体可能已释放 */
    }
  })
  return { bodies, urls }
}

async function storageText(page: Page): Promise<string> {
  return page.evaluate(() => JSON.stringify({ local: { ...localStorage }, session: { ...sessionStorage }, cookie: document.cookie }))
}

test('A-PDU-01 新增 PDU：mock 隐藏 SNMP 字段并成功；snmp 缺 community 报错，填写后成功显示「已设置」；community 从不下发', async ({ page }) => {
  const admin = await createAdmin('ops')
  const secret = `sec-${runStamp}-create`
  const posts = recordRequests(page, 'POST', '/pdus')
  const gets = recordRequests(page, 'GET', '/pdus')
  const traffic = recordTraffic(page)
  const toasts = await trackToasts(page)
  const dlg = page.getByTestId('pdu-dialog')
  const mockName = `ui-mock-${runStamp}`
  const snmpName = `ui-snmp-${runStamp}`

  await test.step('首次加载失败：页内错误与重试，不自动重试；重试成功', async () => {
    await page.route(isPdus, fail500('req-pdu-1'))
    await loginAdmin(page, admin)
    await page.getByTestId('menu-pdus').click()
    await expectAdminUrl(page, '/pdus')
    await expect(page.getByTestId('pdus-error')).toContainText('服务器内部错误（请求号 req-pdu-1）')
    await expect(page.getByTestId('pdu-name')).toHaveCount(0)
    await drainTasks(page)
    expect(gets).toHaveLength(1)
    expect(await toasts()).toEqual(['服务器内部错误（请求号 req-pdu-1）'])
    await page.unroute(isPdus)
    await page.getByTestId('pdus-retry').click()
    await expect(page.getByTestId('pdus-error')).toHaveCount(0)
    expect(gets).toHaveLength(2)
  })

  await test.step('mock：SNMP 字段（community / OID / 取值）隐藏；请求体不带 community；成功后出现在列表', async () => {
    await page.getByTestId('pdus-create-btn').click()
    await expect(page.getByTestId('pdu-form-community')).toBeVisible() // 缺省 snmp：字段可见
    await expect(page.getByTestId('pdu-form-community')).toHaveAttribute('type', 'password')
    await expect(page.getByTestId('pdu-form-community')).toHaveAttribute('placeholder', '') // 新增时没有「留空不修改」提示
    await page.getByTestId('pdu-form-name').fill(mockName)
    await page.getByTestId('pdu-form-driver-mock').click()
    for (const id of ['pdu-form-community', 'pdu-form-oid', 'pdu-form-cycle', 'pdu-form-on']) await expect(page.getByTestId(id)).toHaveCount(0)
    await page.getByTestId('pdu-form-address').fill('10.94.0.1')
    await page.getByTestId('pdu-form-submit').click()
    await toast(page, 'PDU 已添加')
    await expect(dlg).toBeHidden()
    expect(posts.at(-1)!.body).toEqual({ name: mockName, driver: 'mock', address: '10.94.0.1', oid_prefix: '', cycle_value: 3, on_value: 1 })
    const row = rowByName(page, mockName)
    await expect(row.getByTestId('pdu-driver')).toHaveText('mock')
    await expect(row.getByTestId('pdu-address')).toHaveText('10.94.0.1')
    await expect(row.getByTestId('pdu-has-community')).toHaveCount(0)
  })

  await test.step('snmp 不填 community → 「请填写 SNMP community」，弹窗保留', async () => {
    await page.getByTestId('pdus-create-btn').click()
    await expect(page.getByTestId('pdu-form-name')).toHaveValue('') // 重新打开是空表单，驱动回到 snmp
    await expect(page.getByTestId('pdu-form-community')).toBeVisible()
    await page.getByTestId('pdu-form-name').fill(snmpName)
    await page.getByTestId('pdu-form-address').fill('10.94.0.2')
    await page.getByTestId('pdu-form-oid').fill('1.3.6.1.4.1.318.1.1.4.4.2.1.3')
    await page.getByTestId('pdu-form-submit').click()
    await toast(page, '请填写 SNMP community')
    await expect(dlg).toBeVisible()
    expect(posts.at(-1)!.body).not.toHaveProperty('community')
  })

  await test.step('填写 community 后成功（连点只发一次）：列表显示「已设置」，不显示 community 本身', async () => {
    await page.getByTestId('pdu-form-community').fill(secret)
    const n = posts.length
    await clickTwiceSync(page.getByTestId('pdu-form-submit'))
    await toast(page, 'PDU 已添加')
    await expect(dlg).toBeHidden()
    await drainTasks(page)
    expect(posts).toHaveLength(n + 1)
    expect(posts.at(-1)!.body).toEqual({ name: snmpName, driver: 'snmp', address: '10.94.0.2', community: secret, oid_prefix: '1.3.6.1.4.1.318.1.1.4.4.2.1.3', cycle_value: 3, on_value: 1 })
    const row = rowByName(page, snmpName)
    await expect(row.getByTestId('pdu-has-community')).toHaveText('已设置')
    await expect(row.getByTestId('pdu-driver')).toHaveText('snmp')
  })

  await test.step('安全：community 不出现在任何 GET 响应、请求地址、页面内容与浏览器存储中', async () => {
    await page.reload()
    await expect(rowByName(page, snmpName).getByTestId('pdu-has-community')).toHaveText('已设置')
    await rowByName(page, snmpName).getByTestId('pdu-edit').click()
    await expect(page.getByTestId('pdu-form-community')).toHaveValue('') // 编辑时不回填
    await drainTasks(page)
    const pduBodies = traffic.bodies.filter((b) => isPdus(new URL(b.url)))
    expect(pduBodies.length).toBeGreaterThanOrEqual(2)
    expect(pduBodies.some((b) => b.text.includes(snmpName))).toBe(true)
    for (const b of traffic.bodies) expect(b.text).not.toContain(secret)
    for (const u of traffic.urls) expect(u).not.toContain(secret)
    expect(await page.content()).not.toContain(secret)
    expect(await storageText(page)).not.toContain(secret)
  })

  await test.step('财务没有 PDU 菜单，直接访问 → 403', async () => {
    const fin = await createAdmin('finance')
    await page.context().clearCookies()
    await loginAdmin(page, fin)
    await expect(page.getByTestId('menu-pdus')).toHaveCount(0)
    await page.goto('/admin/pdus')
    await expectAdminUrl(page, '/403', { from: '/pdus' })
  })
})

test('A-PDU-02 编辑 PDU：community 密码框、提示与「已设置」；留空保存不发送 community 且仍已设置；改地址不填 community 报错', async ({ page }) => {
  const admin = await createAdmin('ops')
  const ctx = await adminApi(admin)
  const secret = `sec-${runStamp}-edit`
  const pdu = await createPDU(ctx, admin, { driver: 'snmp', community: secret })
  await registerMachine(ctx, admin, { pdu_id: pdu.id, pdu_outlet: 9 }) // 机器详情 / 列表的 GET 也不能带出 community
  const puts = recordRequests(page, 'PUT', `/pdus/${pdu.id}`)
  const traffic = recordTraffic(page)
  const dlg = page.getByTestId('pdu-dialog')
  const row = rowByName(page, pdu.name)
  const apiPdu = async () => ((await (await ctx.get('/admin/api/v1/pdus')).json()) as { items: PduRow[] }).items.find((p) => p.id === pdu.id)!
  const original = await apiPdu()
  await loginAdmin(page, admin)
  await page.getByTestId('menu-pdus').click()

  await test.step('编辑弹窗：community 为密码框、空值、占位提示；显示「已设置」标记', async () => {
    await expect(row.getByTestId('pdu-has-community')).toHaveText('已设置')
    await row.getByTestId('pdu-edit').click()
    const community = page.getByTestId('pdu-form-community')
    await expect(community).toHaveAttribute('type', 'password')
    await expect(community).toHaveValue('')
    await expect(community).toHaveAttribute('placeholder', '留空表示不修改；修改了地址必须重新填写')
    await expect(dlg.getByTestId('pdu-has-community')).toHaveText('已设置')
    await expect(page.getByTestId('pdu-form-name')).toHaveValue(pdu.name)
    await expect(page.getByTestId('pdu-form-address')).toHaveValue(original.address)
    await expect(page.getByTestId('pdu-form-oid')).toHaveValue(original.oid_prefix)
  })

  await test.step('留空保存（连点只发一次）：请求体没有 community 字段；成功后仍「已设置」', async () => {
    await page.getByTestId('pdu-form-cycle').locator('input').fill('4')
    await clickTwiceSync(page.getByTestId('pdu-form-submit'))
    await toast(page, 'PDU 已更新')
    await expect(dlg).toBeHidden()
    await drainTasks(page)
    expect(puts).toHaveLength(1)
    expect(puts[0].body).toEqual({ name: pdu.name, driver: 'snmp', address: original.address, oid_prefix: original.oid_prefix, cycle_value: 4, on_value: 1 })
    await expect(row.getByTestId('pdu-has-community')).toHaveText('已设置')
    const after = await apiPdu()
    expect(after.has_community).toBe(true)
    expect((after as unknown as { cycle_value: number }).cycle_value).toBe(4)
  })

  await test.step('改地址不填 community → 「修改 PDU 地址时请重新填写 SNMP community」，地址不变', async () => {
    await row.getByTestId('pdu-edit').click()
    await page.getByTestId('pdu-form-address').fill('10.94.9.9')
    await page.getByTestId('pdu-form-submit').click()
    await toast(page, '修改 PDU 地址时请重新填写 SNMP community')
    await expect(dlg).toBeVisible()
    expect(puts.at(-1)!.body).not.toHaveProperty('community')
    expect((await apiPdu()).address).toBe(original.address)
  })

  await test.step('改地址并重新填写 community：成功，地址更新', async () => {
    const fresh = `${secret}-2`
    await page.getByTestId('pdu-form-community').fill(fresh)
    await page.getByTestId('pdu-form-submit').click()
    await toast(page, 'PDU 已更新')
    await expect(dlg).toBeHidden()
    expect(puts.at(-1)!.body).toMatchObject({ address: '10.94.9.9', community: fresh })
    await expect(row.getByTestId('pdu-address')).toHaveText('10.94.9.9')
    await expect(row.getByTestId('pdu-has-community')).toHaveText('已设置')
  })

  await test.step('安全：PDU 列表、机器列表的 GET 响应与地址、页面、存储都不含 community', async () => {
    await page.getByTestId('menu-machines').click()
    await expect(page.getByTestId('machine-id').first()).toBeVisible()
    await page.getByTestId('menu-pdus').click()
    await expect(row).toHaveCount(1)
    await drainTasks(page)
    expect(traffic.bodies.some((b) => isPdus(new URL(b.url)) && b.text.includes(pdu.name))).toBe(true)
    expect(traffic.bodies.some((b) => new URL(b.url).pathname === '/admin/api/v1/machines')).toBe(true)
    for (const b of traffic.bodies) {
      expect(b.text).not.toContain(secret)
      expect(b.text).not.toContain('"community"') // 只有 has_community，没有 community 字段
    }
    for (const u of traffic.urls) expect(u).not.toContain(secret)
    expect(await page.content()).not.toContain(secret)
    expect(await storageText(page)).not.toContain(secret)
  })
})
