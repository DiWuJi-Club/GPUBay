import { readFileSync } from 'node:fs'
import { expect, test, type Page } from '@playwright/test'
import { adminApi, adminPost, createAdmin, enterTotpDialog, expectAdminUrl, expectStillOpen, getJSON, loginAdmin, logout, pick } from '../helpers/admin'
import { seedUser, toast, userApi } from '../helpers/console'
import { hooks } from '../helpers/hooks'
import {
  callChat, cleanupLLM, createKey, createPoolMachineAndNode, nodesOf, ownPoolMachine, poolGroupId, poolMachine, seedLLMModel, settleNodes, sweep, uniqueSlug, waitGatewayModel,
} from '../helpers/llm'
import { yuan } from '../helpers/money'
import { runStamp } from '../helpers/phone'

// Token 商城后台（计划 9 规格 §10.2、§11、§16.3）。模型 / 节点是全局状态：每个用例自己建模型与池机器，
// afterEach 的 cleanupLLM 把模型下线、节点推进到 removed、机器移出池（helpers/llm.ts）。
// 「贵」模型一次调用 ¥1.00（FakeVLLM usage 100 / 50），达到结算阈值，清扫一轮即结算。
const PRICY = { input_price: 500_000, output_price: 1_000_000 }

test.afterEach(cleanupLLM)

type AuditList = { total: number; items: { action: string; target_id: string; diff: Record<string, unknown> }[] }

const modelRow = (page: Page, slug: string) => page.getByRole('row').filter({ has: page.locator(`[data-testid="llm-model-row"][data-slug="${slug}"]`) })
const nodeRow = (page: Page, id: string) => page.locator(`[data-testid="llm-node-row"][data-id="${id}"]`)
const poolRow = (page: Page, id: number) => page.locator(`[data-testid="llm-pool-row"][data-id="${id}"]`)
const accRow = (page: Page, userId: number) => page.getByRole('row').filter({ has: page.locator(`[data-testid="llm-acc-row"][data-user="${userId}"]`) })

/** 用量 / 结算的默认日期范围（最近 7 天）按浏览器时间算：把页面时钟对齐到服务器假时钟（之前的用例已推进了若干天）。 */
async function alignPageClock(page: Page) {
  await page.clock.install({ time: await hooks.clock() })
}

let rackSeq = 0
const uniqueRack = () => `L${runStamp}-${++rackSeq}`

test('A-LLM-01 新建模型（super，抽屉表单）与编辑价格：列表价格（元 / 百万 token）更新，审计记下前后价格', async ({ page }) => {
  await hooks.appsSeed() // 内部应用 vllm-node 已发布：抽屉才能保存
  const admin = await createAdmin('super')
  await loginAdmin(page, admin)
  await page.goto('/admin/llm?tab=models')
  const slug = uniqueSlug()
  await page.getByTestId('llm-model-create').click()
  await expect(page.getByTestId('llm-model-slug')).toBeVisible()
  await page.getByTestId('llm-model-slug').fill(slug)
  await page.getByTestId('llm-model-name').fill('E2E 后台建的模型')
  await page.getByTestId('llm-model-source-id').fill('Qwen/Qwen3-8B')
  await page.getByTestId('llm-model-input-price').fill('1.50')
  await page.getByTestId('llm-model-output-price').fill('6.00')
  await page.getByTestId('llm-model-save').click()
  await toast(page, '模型已创建')
  const row = modelRow(page, slug)
  await expect(row.getByTestId('llm-model-row-input-price')).toHaveText('¥1.50')
  await expect(row.getByTestId('llm-model-row-output-price')).toHaveText('¥6.00')
  await expect(row.getByTestId('llm-model-row-status')).toHaveText('草稿')

  const ctx = await adminApi(admin)
  const created = (await getJSON<{ items: { id: number; slug: string; input_price: number; output_price: number }[] }>(ctx, '/admin/api/v1/llm/models')).items.find((m) => m.slug === slug)!
  expect(created).toMatchObject({ input_price: 150, output_price: 600 })
  expect((await getJSON<AuditList>(ctx, `/admin/api/v1/audit?action=llm.model.create&target_type=llm_model&target_id=${created.id}`)).total).toBe(1)

  await row.getByTestId('llm-model-edit').click()
  await expect(page.getByTestId('llm-model-input-price')).toHaveValue('1.50')
  await page.getByTestId('llm-model-input-price').fill('2.00')
  await page.getByTestId('llm-model-save').click()
  await toast(page, '模型已保存')
  await expect(row.getByTestId('llm-model-row-input-price')).toHaveText('¥2.00')
  await expect(row.getByTestId('llm-model-row-output-price')).toHaveText('¥6.00')
  const audit = await getJSON<AuditList>(ctx, `/admin/api/v1/audit?action=llm.model.update&target_type=llm_model&target_id=${created.id}`)
  expect(audit.total).toBe(1)
  expect(audit.items[0].diff).toMatchObject({ before: { input_price: 150 }, after: { input_price: 200, output_price: 600 } })
  // 删除草稿（清理）
  const r = await ctx.delete(`/admin/api/v1/llm/models/${created.id}`, { data: {} })
  expect(r.ok()).toBe(true)
})

test('A-LLM-02 上线：0 副本被拦下、确认后要动态码；删除只对没有节点的草稿开放（已上线 / 有节点的接口 409）', async ({ page }) => {
  const zero = await seedLLMModel({ online: false, replicas: 0 })
  const one = await seedLLMModel({ online: false, replicas: 1 })
  const busy = await seedLLMModel({ online: false, replicas: 1 }) // 草稿，但机器上有节点（钩子直接造）
  await hooks.llmNode(busy.id, await poolMachine(busy.admin))
  const admin = zero.admin
  await loginAdmin(page, admin)
  await page.goto('/admin/llm?tab=models')

  await modelRow(page, zero.slug).getByTestId('llm-model-online').click()
  await expect(page.getByTestId('llm-model-status-dialog')).toBeVisible()
  await expect(page.getByTestId('llm-model-status-block')).toContainText('上线前请设置至少 1 个副本')
  await expect(page.getByTestId('llm-model-status-ok')).toBeDisabled()
  await page.getByTestId('llm-model-status-cancel').click()
  await expect(page.getByTestId('llm-model-status-dialog')).toBeHidden()

  await modelRow(page, one.slug).getByTestId('llm-model-online').click()
  await expect(page.getByTestId('llm-model-status-text')).toContainText(one.slug)
  await page.getByTestId('llm-model-status-ok').click()
  await enterTotpDialog(page, admin.totp_secret!)
  await toast(page, '模型已上线')
  await expect(modelRow(page, one.slug).getByTestId('llm-model-row-status')).toHaveText('已上线')
  await expect(modelRow(page, one.slug).getByTestId('llm-model-row-replicas')).toHaveText('1 / 0')
  await expect(modelRow(page, one.slug).getByTestId('llm-model-offline')).toBeVisible()
  await expect(modelRow(page, one.slug).getByTestId('llm-model-delete')).toHaveCount(0) // 已上线不能删

  // 接口层：不带动态码上下线 → totp_required
  const ctx = await adminApi(admin)
  const r = await ctx.post(`/admin/api/v1/llm/models/${zero.id}/status`, { data: { status: 'online' } })
  expect(r.status()).toBe(403)
  expect(((await r.json()) as { code: string }).code).toBe('totp_required')
  // 删除条件（接口层）：已上线、或有节点（含已结束的）的模型一律 llm_model_in_use；界面上这两行也没有「删除」
  for (const id of [one.id, busy.id]) {
    const d = await ctx.delete(`/admin/api/v1/llm/models/${id}`, { data: {} })
    expect(d.status()).toBe(409)
    expect(((await d.json()) as { code: string }).code).toBe('llm_model_in_use')
  }
  await expect(modelRow(page, busy.slug).getByTestId('llm-model-row-status')).toHaveText('草稿')
  await expect(modelRow(page, busy.slug).getByTestId('llm-model-delete')).toHaveCount(0)

  await modelRow(page, zero.slug).getByTestId('llm-model-delete').click()
  await expect(page.getByTestId('llm-model-delete-text')).toContainText(zero.slug)
  await page.getByTestId('llm-model-delete-ok').click()
  await toast(page, '模型已删除')
  await expect(modelRow(page, zero.slug)).toHaveCount(0)
})

test('A-LLM-03 机器池：从未分组机器加入，机器上有节点时不能移出（按钮禁用、接口 409），节点移除后可移出', async ({ page }) => {
  const m = await seedLLMModel()
  const ops = await createAdmin('ops')
  const rack = uniqueRack()
  const [mid] = await hooks.machines(1, undefined, rack)
  ownPoolMachine(mid)
  await loginAdmin(page, ops)
  await page.goto('/admin/llm?tab=nodes')
  await page.getByTestId('llm-pool-add').click()
  await page.getByTestId('llm-pool-add-rack').fill(rack)
  await page.getByTestId('llm-pool-add-search').click()
  const cand = page.locator(`[data-testid="llm-pool-add-row"][data-id="${mid}"]`)
  await expect(page.getByTestId('llm-pool-add-row')).toHaveCount(1)
  await cand.getByTestId('llm-pool-add-check').click()
  await expect(page.getByTestId('llm-pool-add-count')).toHaveText('1')
  await page.getByTestId('llm-pool-add-submit').click()
  await toast(page, '已加入 1 台机器')
  await expect(poolRow(page, mid).getByTestId('llm-pool-state')).toHaveText('空闲')
  await expect(poolRow(page, mid).getByTestId('llm-pool-online')).toHaveText('在线')
  await expect(poolRow(page, mid).getByTestId('llm-pool-node')).toHaveText('—')

  await hooks.llmUpstream()
  const { node_id } = await hooks.llmNode(m.id, mid)
  await page.getByTestId('llm-nodes-refresh').click()
  await expect(poolRow(page, mid).getByTestId('llm-pool-node')).toContainText(m.slug)
  await expect(poolRow(page, mid).getByTestId('llm-pool-node')).toContainText('就绪')
  await expect(poolRow(page, mid).getByTestId('llm-pool-remove')).toBeDisabled()
  const ctx = await adminApi(ops)
  const pool = await poolGroupId(ctx)
  const busy = await ctx.delete(`/admin/api/v1/groups/${pool}/machines`, { data: { machine_ids: [mid] } })
  expect(busy.status()).toBe(409)
  expect(((await busy.json()) as { code: string }).code).toBe('llm_pool_machine_busy')

  // 排空节点（后台接口）并推进到已移除；模型先下线：在线模型缺节点时 Controller 会去池里补建
  await adminPost(await adminApi(m.admin), m.admin, `/admin/api/v1/llm/models/${m.id}/status`, { status: 'offline' })
  await settleNodes(ctx, m.id)
  expect((await getJSON<{ node: { state: string } }>(ctx, `/admin/api/v1/llm/nodes/${node_id}`)).node.state).toBe('removed')
  expect(await nodesOf(ctx, m.id)).toEqual([]) // 列表缺省不含已移除的节点
  await page.getByTestId('llm-nodes-refresh').click()
  await expect(poolRow(page, mid).getByTestId('llm-pool-node')).toHaveText('—') // 池表只显示未结束的节点
  await expect(poolRow(page, mid).getByTestId('llm-pool-remove')).toBeEnabled()
  await poolRow(page, mid).getByTestId('llm-pool-remove').click()
  await page.getByTestId('llm-pool-remove-ok').click()
  await toast(page, '已移出 Token 服务池')
  await expect(poolRow(page, mid)).toHaveCount(0)
})

test('A-LLM-04 节点列表：状态与详情抽屉；排空、重建（钩子节点没有部署 → 原文报错、弹窗保留）、移除需原因与动态码', async ({ page }) => {
  const m = await seedLLMModel({ replicas: 2 })
  const a = await createPoolMachineAndNode(m)
  const bMachine = await poolMachine(m.admin)
  const b = await hooks.llmNode(m.id, bMachine)
  const ops = await createAdmin('ops')
  await loginAdmin(page, ops)
  await page.goto('/admin/llm?tab=nodes')
  await expect(nodeRow(page, a.nodeId)).toHaveAttribute('data-state', 'ready')
  await expect(nodeRow(page, a.nodeId).getByTestId('llm-node-row-state')).toHaveText('就绪')
  await expect(nodeRow(page, a.nodeId).getByTestId('llm-node-row-inflight')).toHaveText('0')
  await expect(nodeRow(page, b.node_id).getByTestId('llm-node-row-state')).toHaveText('就绪')

  await nodeRow(page, a.nodeId).getByTestId('llm-node-drain').click()
  await expect(page.getByTestId('llm-node-op-text')).toContainText('在途请求完成')
  await page.getByTestId('llm-node-op-ok').click()
  await toast(page, '节点开始排空')
  await expect(nodeRow(page, a.nodeId)).toHaveAttribute('data-state', 'draining')
  await expect(nodeRow(page, a.nodeId).getByTestId('llm-node-row-state')).toHaveText('排空中')
  await expect(nodeRow(page, a.nodeId).getByTestId('llm-node-drain')).toHaveCount(0)
  await expect(nodeRow(page, a.nodeId).getByTestId('llm-node-rebuild')).toHaveCount(0)
  await expect(nodeRow(page, a.nodeId).getByTestId('llm-node-remove')).toBeVisible()

  await nodeRow(page, b.node_id).getByTestId('llm-node-rebuild').click()
  await page.getByTestId('llm-node-op-ok').click()
  await toast(page, '节点当前状态不允许该操作')
  await expectStillOpen(page, 'llm-node-op-dialog', '节点当前状态不允许该操作')
  await page.getByTestId('llm-node-op-cancel').click()

  await nodeRow(page, b.node_id).getByTestId('llm-node-remove').click()
  await page.getByTestId('llm-node-op-ok').click()
  await expect(page.getByTestId('llm-node-op-error')).toBeVisible() // 原因必填，不发请求
  await page.getByTestId('llm-node-remove-reason').fill('机器硬件故障')
  await page.getByTestId('llm-node-op-ok').click()
  await enterTotpDialog(page, ops.totp_secret!)
  await toast(page, '节点正在移除')
  await expect(nodeRow(page, b.node_id)).toHaveAttribute('data-state', 'removing')
  await expect(nodeRow(page, b.node_id).getByTestId('llm-node-row-state')).toHaveText('移除中')

  const ctx = await adminApi(ops)
  expect((await getJSON<AuditList>(ctx, `/admin/api/v1/audit?action=llm.node.drain&target_type=llm_node&target_id=${a.nodeId}`)).total).toBe(1)
  const removed = await getJSON<AuditList>(ctx, `/admin/api/v1/audit?action=llm.node.remove&target_type=llm_node&target_id=${b.node_id}`)
  expect(removed.total).toBe(1)
  expect(JSON.stringify(removed.items[0].diff)).toContain('机器硬件故障')

  await nodeRow(page, a.nodeId).getByTestId('llm-node-open').click()
  await expect(page.getByTestId('llm-node-drawer')).toBeVisible()
  await expect(page.getByTestId('llm-node-id')).toHaveText(a.nodeId)
  await expect(page.getByTestId('llm-node-state')).toHaveAttribute('data-state', 'draining')
  await expect(page.getByTestId('llm-node-instance')).toContainText(a.instanceId)
  await expect(page.locator('[data-testid="llm-node-event"][data-to="draining"]')).toHaveCount(1)
  await expect(page.getByTestId('llm-node-no-deployment')).toBeVisible()
})

test('A-LLM-05 账户：按用户定位，编辑限额（校验、留空 = 默认），停用需原因（网关 403）与解除；展开行停用 Key', async ({ page }) => {
  const u = await seedUser()
  const api = await userApi(u.phone)
  const k = await createKey(api, '账户 Key')
  const ops = await createAdmin('ops')
  await loginAdmin(page, ops)
  await page.goto(`/admin/llm?tab=accounts&user_id=${u.id}`)
  const row = accRow(page, u.id)
  await expect(row.getByTestId('llm-acc-keys-count')).toHaveText('1')
  await expect(row.getByTestId('llm-acc-limit-rpm')).toHaveText('RPM 60（默认）')
  await expect(row.getByTestId('llm-acc-blocked-tag')).toHaveText('正常')

  await row.getByTestId('llm-acc-edit-limits').click()
  await page.getByTestId('llm-limit-rpm').fill('0')
  await page.getByTestId('llm-limits-submit').click()
  await expect(page.getByTestId('llm-limit-rpm-error')).toBeVisible()
  await page.getByTestId('llm-limit-rpm').fill('2')
  await page.getByTestId('llm-limit-concurrency').fill('3')
  await page.getByTestId('llm-limits-submit').click()
  await toast(page, '限额已更新')
  await expect(row.getByTestId('llm-acc-limit-rpm')).toHaveText('RPM 2')
  await expect(row.getByTestId('llm-acc-limit-concurrency')).toHaveText('并发 3')
  await expect(row.getByTestId('llm-acc-limit-tpm')).toHaveText('TPM 200,000（默认）')
  // 留空 = 恢复默认
  await row.getByTestId('llm-acc-edit-limits').click()
  await page.getByTestId('llm-limit-concurrency').fill('')
  await page.getByTestId('llm-limits-submit').click()
  await toast(page, '限额已更新')
  await expect(row.getByTestId('llm-acc-limit-concurrency')).toHaveText('并发 8（默认）')

  await row.getByTestId('llm-acc-block').click()
  await page.getByTestId('llm-block-submit').click()
  await expect(page.getByTestId('llm-block-error')).toBeVisible()
  await page.getByTestId('llm-block-reason').fill('疑似转售')
  await page.getByTestId('llm-block-submit').click()
  await toast(page, '已停用该账户的 Token 服务')
  await expect(row.getByTestId('llm-acc-blocked-tag')).toHaveText(['已停用']) // 数组：等切换动画里离场的旧标签消失
  await hooks.advance(31) // 其它进程的鉴权缓存最多 30 秒（本进程已立即失效）
  const blocked = await callChat(k.key, 'no-such-model')
  expect(blocked.status).toBe(403)
  expect((blocked.json?.error as { code: string }).code).toBe('account_blocked')
  await row.getByTestId('llm-acc-unblock').click()
  await page.getByTestId('llm-unblock-ok').click()
  await toast(page, '已恢复 Token 服务')
  await expect(row.getByTestId('llm-acc-blocked-tag')).toHaveText(['正常'])
  await hooks.advance(31)
  expect((await callChat(k.key, 'no-such-model')).status).toBe(404)

  // ?user_id= 命中的行自动展开 Key 列表（不用点展开）
  const keys = page.locator(`[data-testid="llm-acc-keys"][data-user="${u.id}"]`)
  await expect(keys.getByTestId('llm-key-name')).toHaveText(['账户 Key'])
  await expect(keys.getByTestId('llm-key-status')).toHaveText(['启用'])
  await keys.getByTestId('llm-key-disable').click()
  await page.getByTestId('llm-key-disable-reason').fill('泄露')
  await page.getByTestId('llm-key-disable-submit').click()
  await expect(keys.getByTestId('llm-key-status')).toHaveText(['已停用'])
  expect((await callChat(k.key, 'no-such-model')).status).toBe(401)
  const ctx = await adminApi(ops)
  expect((await getJSON<AuditList>(ctx, `/admin/api/v1/audit?action=llm.key.admin_disable&target_type=llm_api_key&target_id=${k.id}`)).total).toBe(1)
})

test('A-LLM-06 用量明细：按用户筛选、CSV 下载与页面一致；汇总按模型', async ({ page }) => {
  const m = await seedLLMModel()
  await createPoolMachineAndNode(m)
  const u = await seedUser()
  const other = await seedUser()
  const k = await createKey(await userApi(u.phone))
  await waitGatewayModel(k.key, m.slug)
  const k2 = await createKey(await userApi(other.phone))
  const rids: string[] = []
  for (let i = 0; i < 2; i++) {
    const r = await callChat(k.key, m.slug)
    expect(r.status).toBe(200)
    rids.push(r.requestId)
  }
  expect((await callChat(k2.key, m.slug)).status).toBe(200)

  await alignPageClock(page)
  await loginAdmin(page, await createAdmin('finance'))
  await page.goto('/admin/llm?tab=usage')
  await page.getByTestId('llm-usage-user').fill(String(u.id))
  await page.getByTestId('llm-usage-search').click()
  await expect(page.getByTestId('llm-usage-total')).toHaveText('共 2 条')
  await expect(page.getByTestId('llm-usage-rid')).toHaveText([...rids].reverse())
  await expect(page.getByTestId('llm-usage-row-user').first()).toHaveText(u.phone)
  await expect(page.getByTestId('llm-usage-row-model')).toHaveText([m.slug, m.slug])
  await expect(page.getByTestId('llm-usage-tokens').first()).toHaveText('100 / 50')
  await expect(page.getByTestId('llm-usage-cost').first()).toHaveText('< ¥0.01') // 12,500 微分
  await expect(page.getByTestId('llm-usage-row-status').first()).toHaveText('成功')

  const pending = page.waitForEvent('download')
  await page.getByTestId('llm-usage-export').click()
  const file = await pending
  expect(file.suggestedFilename()).toMatch(/\.csv$/)
  const lines = readFileSync(await file.path(), 'utf8').replace(/^﻿/, '').trim().split(/\r?\n/)
  expect(lines).toHaveLength(3) // 表头 + 2 行
  for (const id of rids) expect(lines.some((l) => l.includes(id))).toBe(true)

  await page.getByTestId('llm-usage-view-daily').click()
  await page.getByTestId('llm-usage-group-model').click()
  const sum = page.getByRole('row').filter({ has: page.getByTestId('llm-daily-key').getByText(m.slug, { exact: true }) })
  await expect(sum.getByTestId('llm-daily-requests')).toHaveText('3')
})

test('A-LLM-07 结算记录：按用户筛选、用量与实扣；后台流水按「Token 消费」类型筛选', async ({ page }) => {
  const m = await seedLLMModel(PRICY)
  await createPoolMachineAndNode(m)
  const u = await seedUser()
  const api = await userApi(u.phone)
  const k = await createKey(api)
  await waitGatewayModel(k.key, m.slug)
  expect((await callChat(k.key, m.slug)).status).toBe(200)
  await sweep()
  const s = ((await (await api.get('/api/v1/llm/settlements')).json()) as { items: { id: number; wallet_ledger_id: number }[] }).items
  expect(s).toHaveLength(1)

  await alignPageClock(page)
  await loginAdmin(page, await createAdmin('finance'))
  await page.goto('/admin/llm?tab=usage')
  await page.getByTestId('llm-usage-view-settlements').click()
  await page.getByTestId('llm-usage-user').fill(String(u.id))
  await page.getByTestId('llm-usage-search').click()
  await expect(page.getByTestId('llm-settle-total')).toHaveText('共 1 条')
  await expect(page.getByTestId('llm-settle-id')).toHaveText([String(s[0].id)])
  await expect(page.getByTestId('llm-settle-usage')).toHaveText([yuan(100)])
  await expect(page.getByTestId('llm-settle-charged')).toHaveText([yuan(100)])

  await page.goto('/admin/ledger')
  await page.getByTestId('fin-filter-user').fill(String(u.id))
  await pick(page, 'fin-filter-kind', 'Token 消费')
  await page.getByTestId('fin-search').click()
  await expect(page.getByTestId('led-kind')).toHaveText(['Token 消费'])
  await expect(page.getByTestId('led-id')).toHaveText([String(s[0].wallet_ledger_id)])
  await expect(page.getByTestId('led-amount')).toHaveText([`-${yuan(100)}`])
  await expect(page.getByTestId('led-ref')).toHaveText([`llm_settlement ${s[0].id}`])
})

test('A-LLM-08 仪表盘「Token 商城」卡片与 Token 商城总览：节点、今日请求、按模型的就绪 / 期望副本', async ({ page }) => {
  const m = await seedLLMModel(PRICY)
  await createPoolMachineAndNode(m)
  const u = await seedUser()
  const k = await createKey(await userApi(u.phone))
  await waitGatewayModel(k.key, m.slug)
  expect((await callChat(k.key, m.slug)).status).toBe(200)
  await sweep()
  const admin = await createAdmin('finance')
  const ctx = await adminApi(admin)
  const dash = (await getJSON<{ llm: { nodes_ready: number; nodes_total: number; today_requests: number; today_settled_amount: number } }>(ctx, '/admin/api/v1/dashboard')).llm
  expect(dash.nodes_ready).toBeGreaterThanOrEqual(1)
  expect(dash.today_requests).toBeGreaterThanOrEqual(1)
  expect(dash.today_settled_amount).toBeGreaterThanOrEqual(100)

  await loginAdmin(page, admin)
  await page.goto('/admin/')
  await expect(page.getByTestId('dash-llm')).toBeVisible()
  await expect(page.getByTestId('dash-llm-nodes-ready')).toHaveText(String(dash.nodes_ready))
  await expect(page.getByTestId('dash-llm-nodes-total')).toHaveText(String(dash.nodes_total))
  await expect(page.getByTestId('dash-llm-requests')).toHaveText(dash.today_requests.toLocaleString('en-US'))
  await expect(page.getByTestId('dash-llm-income')).toHaveText(yuan(dash.today_settled_amount))
  await page.getByTestId('dash-llm-link').click()
  await expectAdminUrl(page, '/llm')

  const sum = await getJSON<{ nodes: { ready: number; total: number }; today: { requests: number } }>(ctx, '/admin/api/v1/llm/summary')
  await expect(page.getByTestId('llm-overview')).toBeVisible()
  await expect(page.getByTestId('llm-sum-nodes-ready')).toHaveText(String(sum.nodes.ready))
  await expect(page.getByTestId('llm-sum-nodes-total')).toHaveText(String(sum.nodes.total))
  await expect(page.getByTestId('llm-sum-requests')).toHaveText(sum.today.requests.toLocaleString('en-US'))
  const card = page.locator(`[data-testid="llm-overview-model"][data-slug="${m.slug}"]`)
  await expect(card.getByTestId('llm-overview-model-nodes')).toHaveText('1 / 1')
  await expect(card.getByTestId('llm-overview-model-wanted')).toHaveText('1')
})

test('A-LLM-09 按角色：财务看不到「节点与机器池」与节点操作，运维看不到新建 / 编辑价格；接口同样 403', async ({ page }) => {
  const m = await seedLLMModel()
  const fin = await createAdmin('finance')
  await loginAdmin(page, fin)
  await page.goto('/admin/llm?tab=nodes') // 无权的 Tab 回到总览
  await expect(page.getByTestId('llm-overview')).toBeVisible()
  for (const t of ['overview', 'models', 'accounts', 'usage']) await expect(page.getByTestId(`llm-tab-${t}`)).toBeVisible()
  await expect(page.getByTestId('llm-tab-nodes')).toHaveCount(0)
  await page.getByTestId('llm-tab-models').click()
  await expect(modelRow(page, m.slug).getByTestId('llm-model-view')).toBeVisible()
  await expect(page.getByTestId('llm-model-create')).toHaveCount(0)
  await expect(page.getByTestId('llm-model-edit')).toHaveCount(0)
  const finCtx = await adminApi(fin)
  expect((await finCtx.get('/admin/api/v1/llm/nodes')).status()).toBe(403)

  await logout(page)
  const ops = await createAdmin('ops')
  await loginAdmin(page, ops)
  await page.goto('/admin/llm?tab=models')
  await expect(page.getByTestId('llm-model-create')).toHaveCount(0)
  await expect(page.getByTestId('llm-model-edit')).toHaveCount(0)
  await expect(page.getByTestId('llm-model-online')).toHaveCount(0)
  await expect(page.getByTestId('llm-model-offline')).toHaveCount(0)
  await modelRow(page, m.slug).getByTestId('llm-model-view').click()
  await expect(page.getByTestId('llm-model-input-price')).toBeVisible()
  await expect(page.getByTestId('llm-model-input-price')).toBeDisabled()
  await expect(page.getByTestId('llm-model-save')).toHaveCount(0)
  await expect(page.getByTestId('llm-tab-nodes')).toBeVisible()
  await expect(page.getByTestId('llm-tab-usage')).toHaveCount(0)
  const opsCtx = await adminApi(ops)
  const put = await opsCtx.put(`/admin/api/v1/llm/models/${m.id}`, { data: { name: 'x' } })
  expect(put.status()).toBe(403)
  expect((await opsCtx.get('/admin/api/v1/llm/usage')).status()).toBe(403)
})
