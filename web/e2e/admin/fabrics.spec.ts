import { expect, test, type Page } from '@playwright/test'
import { adminApi, createAdmin, expectStillOpen, getJSON, loginAdmin, logout } from '../helpers/admin'
import { seedClusterMachines, seedFabric } from '../helpers/clusters'
import { toast } from '../helpers/console'
import { hooks } from '../helpers/hooks'
import { runStamp } from '../helpers/phone'

// 集群网络（计划 13 规格 §3.1、§10.2、§11）。fabric 是全局列表（不分页）：用例只按自己建的名称 / ID 定位行。

type AuditList = { total: number; items: { action: string; target_id: string; diff: Record<string, unknown> }[] }
type FabricJSON = { id: number; name: string; kind: string; isolation: string; rdma: boolean; mtu: number; vlan_min: number | null; vlan_max: number | null; machine_count: number }

let nameSeq = 0
const uniqueName = (p: string) => `${p}${runStamp}-${++nameSeq}`

const rowById = (page: Page, id: number) => page.locator(`[data-testid="fabric-row"][data-id="${id}"]`)
const rowByName = (page: Page, name: string) => page.getByTestId('fabric-row').filter({ has: page.getByTestId('fabric-name').getByText(name, { exact: true }) })
const radio = (page: Page, group: string, text: string) => page.getByTestId(group).locator('label').filter({ hasText: text })

test('A-FAB-01 集群网络新建 / 编辑（ops）：类型决定隔离方式与 RDMA、VLAN 范围必填、列表各列；财务只读（无新建 / 编辑，接口 403）', async ({ page }) => {
  const ops = await createAdmin('ops')
  const ctx = await adminApi(ops)
  await loginAdmin(page, ops)
  await page.goto('/admin/fabrics')
  const lanName = uniqueName('L')
  const cx7Name = uniqueName('X')

  await test.step('普通以太网：只能三层路由、不能开 RDMA；名称必填', async () => {
    await page.getByTestId('fabric-create').click()
    await expect(page.getByTestId('fabric-drawer')).toBeVisible()
    await expect(page.getByTestId('fabric-drawer-isolation').locator('label')).toHaveText(['三层路由'])
    await expect(page.getByTestId('fabric-drawer-rdma')).toHaveClass(/is-disabled/)
    await expect(page.getByTestId('fabric-drawer-vlan-min')).toHaveCount(0)
    await page.getByTestId('fabric-drawer-save').click()
    await expect(page.getByTestId('fabric-drawer-error')).toHaveText('名称需为 1～32 个字符')
    await page.getByTestId('fabric-drawer-name').fill(lanName)
    await page.getByTestId('fabric-drawer-save').click()
    await toast(page, '集群网络已创建')
    const row = rowByName(page, lanName)
    await expect(row).toHaveAttribute('data-kind', 'lan')
    await expect(row.getByTestId('fabric-kind')).toHaveText('普通以太网')
    await expect(row.getByTestId('fabric-isolation')).toHaveText('三层路由')
    await expect(row.getByTestId('fabric-rdma')).toHaveText('否')
    await expect(row.getByTestId('fabric-mtu')).toHaveText('继承网卡')
    await expect(row.getByTestId('fabric-vlan')).toHaveText('—')
    await expect(row.getByTestId('fabric-machines')).toHaveText('0')
    await expect(row.getByTestId('fabric-clusters')).toHaveText('0')
  })

  await test.step('ConnectX-7：直连 / VLAN 隔离，VLAN 范围必填，开 RDMA', async () => {
    await page.getByTestId('fabric-create').click()
    await page.getByTestId('fabric-drawer-name').fill(cx7Name)
    await radio(page, 'fabric-drawer-kind', 'ConnectX-7').click()
    await expect(page.getByTestId('fabric-drawer-isolation').locator('label')).toHaveText(['直连', 'VLAN 隔离'])
    await expect(radio(page, 'fabric-drawer-isolation', '直连').locator('input')).toBeChecked()
    await expect(page.getByTestId('fabric-drawer-rdma')).not.toHaveClass(/is-disabled/)
    await radio(page, 'fabric-drawer-isolation', 'VLAN 隔离').click()
    await page.getByTestId('fabric-drawer-save').click()
    await expect(page.getByTestId('fabric-drawer-error')).toHaveText('VLAN 隔离需填写 VLAN 范围')
    await page.getByTestId('fabric-drawer-vlan-min').locator('input').fill('300')
    await page.getByTestId('fabric-drawer-vlan-max').locator('input').fill('310')
    await page.getByTestId('fabric-drawer-rdma').click()
    await page.getByTestId('fabric-drawer-save').click()
    await toast(page, '集群网络已创建')
    const row = rowByName(page, cx7Name)
    await expect(row).toHaveAttribute('data-kind', 'cx7')
    await expect(row.getByTestId('fabric-kind')).toHaveText('ConnectX-7')
    await expect(row.getByTestId('fabric-isolation')).toHaveText('VLAN 隔离')
    await expect(row.getByTestId('fabric-rdma')).toHaveText('是')
    await expect(row.getByTestId('fabric-vlan')).toHaveText('300～310')
    const f = (await getJSON<{ items: FabricJSON[] }>(ctx, '/admin/api/v1/fabrics')).items.find((x) => x.name === cx7Name)!
    expect(f).toMatchObject({ kind: 'cx7', isolation: 'vlan', rdma: true, mtu: 0, vlan_min: 300, vlan_max: 310 })
    expect((await getJSON<AuditList>(ctx, `/admin/api/v1/audit?action=fabric.create&target_type=fabric&target_id=${f.id}`)).total).toBe(1)
  })

  await test.step('编辑：抽屉回填，改 MTU 与说明后列表更新、审计 fabric.update', async () => {
    const row = rowByName(page, cx7Name)
    await row.getByTestId('fabric-edit').click()
    await expect(page.getByTestId('fabric-drawer-name')).toHaveValue(cx7Name)
    await expect(page.getByTestId('fabric-drawer-vlan-min').locator('input')).toHaveValue('300')
    const mtu = page.getByTestId('fabric-drawer-mtu').locator('input')
    await mtu.fill('100')
    await mtu.blur()
    await page.getByTestId('fabric-drawer-save').click()
    await expect(page.getByTestId('fabric-drawer-error')).toHaveText('MTU 需为 0（继承网卡）或 1280～9216')
    await mtu.fill('9000')
    await mtu.blur()
    await page.getByTestId('fabric-drawer-description').fill('机柜 A 交换机 trunk')
    await page.getByTestId('fabric-drawer-save').click()
    await toast(page, '已保存')
    await expect(row.getByTestId('fabric-mtu')).toHaveText('9000')
    await expect(row).toContainText('机柜 A 交换机 trunk')
    const id = Number(await row.getAttribute('data-id'))
    const audit = await getJSON<AuditList>(ctx, `/admin/api/v1/audit?action=fabric.update&target_type=fabric&target_id=${id}`)
    expect(audit.total).toBe(1)
  })

  await test.step('财务：能看列表，没有新建 / 编辑 / 删除，接口 403', async () => {
    const fin = await createAdmin('finance')
    await logout(page)
    await loginAdmin(page, fin)
    await page.goto('/admin/fabrics')
    await expect(rowByName(page, cx7Name).getByTestId('fabric-vlan')).toHaveText('300～310')
    await expect(page.getByTestId('fabric-create')).toHaveCount(0)
    await expect(page.getByTestId('fabric-edit')).toHaveCount(0)
    await expect(page.getByTestId('fabric-delete')).toHaveCount(0)
    const fctx = await adminApi(fin)
    const r = await fctx.post('/admin/api/v1/fabrics', { data: { name: uniqueName('F'), kind: 'lan', isolation: 'routed', rdma: false, mtu: 0, description: '' } })
    expect(r.status()).toBe(403)
  })
})

test('A-FAB-02 删除：还有机器时提示并被拒（fabric_in_use）、空网络可删；直连网络第 3 台机器被拒（fabric_full）', async ({ page }) => {
  const ops = await createAdmin('ops')
  const ctx = await adminApi(ops)
  const busy = await seedFabric('lan', { name: uniqueName('B') })
  await seedClusterMachines(1, busy.id)
  const direct = await seedFabric('cx7', { name: uniqueName('D') }) // 直连
  await seedClusterMachines(2, direct.id, 'cx7')
  const [third] = await hooks.machines(1, undefined, undefined, undefined, { net: 'cx7' })
  const empty = await seedFabric('lan', { name: uniqueName('E') })
  await loginAdmin(page, ops)
  await page.goto('/admin/fabrics')

  await test.step('有机器：提示删除会被拒绝，确认后原文报错、行仍在', async () => {
    const row = rowById(page, busy.id)
    await expect(row.getByTestId('fabric-machines')).toHaveText('1')
    await row.getByTestId('fabric-delete').click()
    await expect(page.getByTestId('fabric-delete-blocker')).toHaveText('还有 1 台机器，请先在机器详情里移出，删除会被拒绝。')
    await page.getByTestId('fabric-delete-ok').click()
    await toast(page, '该集群网络下还有机器或运行中的集群')
    await page.getByTestId('fabric-delete-cancel').click()
    await expect(rowById(page, busy.id)).toHaveCount(1)
    // 机器数链接到按 fabric 筛选的机器列表
    await rowById(page, busy.id).getByTestId('fabric-machines').click()
    await expect(page).toHaveURL(new RegExp(`/admin/machines\\?fabric_id=${busy.id}$`))
    await page.goBack()
  })

  await test.step('空网络：没有提示，删除成功并写审计', async () => {
    const row = rowById(page, empty.id)
    await row.getByTestId('fabric-delete').click()
    await expect(page.getByTestId('fabric-delete-blocker')).toHaveCount(0)
    await page.getByTestId('fabric-delete-ok').click()
    await toast(page, '已删除')
    await expect(rowById(page, empty.id)).toHaveCount(0)
    expect((await getJSON<{ items: FabricJSON[] }>(ctx, '/admin/api/v1/fabrics')).items.some((f) => f.id === empty.id)).toBe(false)
    expect((await getJSON<AuditList>(ctx, `/admin/api/v1/audit?action=fabric.delete&target_type=fabric&target_id=${empty.id}`)).total).toBe(1)
  })

  await test.step('直连网络已有 2 台：第 3 台在机器详情加入被拒，弹窗保留；接口 409 fabric_full', async () => {
    await expect(rowById(page, direct.id).getByTestId('fabric-isolation')).toHaveText('直连')
    await expect(rowById(page, direct.id).getByTestId('fabric-machines')).toHaveText('2')
    await page.goto(`/admin/machines/${third}`)
    await page.getByTestId('machine-net-edit').click()
    await page.getByTestId('machine-fabric-select').click()
    await page.getByRole('option', { name: `${direct.name}（ConnectX-7 · 直连）`, exact: true }).click()
    await page.getByTestId('machine-net-save').click()
    await toast(page, '直连网络最多 2 台机器')
    await expectStillOpen(page, 'machine-net-dialog', '直连网络最多 2 台机器')
    await expect(page.getByTestId('machine-fabric')).toHaveText('未加入')
    const r = await ctx.put(`/admin/api/v1/machines/${third}`, { data: { fabric_id: direct.id } })
    expect(r.status()).toBe(409)
    expect(((await r.json()) as { code: string }).code).toBe('fabric_full')
  })
})
