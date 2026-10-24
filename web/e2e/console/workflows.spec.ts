import { expect, test, type Page } from '@playwright/test'
import { cleanupApps, expectRowState, listDeployments, ownInstance, seedApps, tickUntil } from '../helpers/apps'
import { isMobile, loginConsole, seedUser, settle, toast } from '../helpers/console'
import { hooks } from '../helpers/hooks'
import { runStamp } from '../helpers/phone'
import {
  comfySetup, expectImageLoaded, installedRunning, installedWorkflows, installWorkflowViaApi, seedExtraWorkflows, seedWorkflows, setComfyHistory, wfMenu, wfRow,
  type ComfySetup,
} from '../helpers/workflows'

// 工作流市场（计划 11）：市场页 / 详情 / 一键安装 / 实例详情工作流子区块 / 快速出图。约定见 helpers/workflows.ts。
// 造了部署的用例 afterEach 卸载；拒绝 /prompt 的用例（C-WF-11）结束时恢复 fake ComfyUI。
test.afterEach(async () => {
  await hooks.comfyReject('')
  await cleanupApps()
})

const card = (page: Page, slug: string) => page.getByTestId(`wf-card-${slug}`)

async function openMarket(page: Page, mobile: boolean) {
  await page.goto('/market')
  await page.getByTestId(mobile ? 'tabbar-workflows' : 'menu-workflows').click()
  await expect(page).toHaveURL(/\/workflows$/)
  await expect(page.getByTestId('wf-market')).toBeVisible()
}

/** 登录后打开实例详情，等工作流子区块出现。 */
async function openDetail(page: Page, s: Pick<ComfySetup, 'u' | 'id'>) {
  await loginConsole(page, s.u.phone)
  await page.goto(`/instances/${s.id}`)
  await expect(page.getByTestId('wf-panel')).toBeVisible()
}

test('C-WF-01 市场页卡片：未登录可见；封面 / 分类占位、下载量、自定义节点数、快速出图标记，按排序', async ({ page }, info) => {
  await seedWorkflows()
  await openMarket(page, isMobile(info))
  const a = card(page, 'e2e-wf-a')
  const b = card(page, 'e2e-wf-b')
  await expect(a.getByTestId('wf-card-name')).toHaveText('E2E 文生图 A')
  await expect(a.getByTestId('wf-card-size')).toHaveText('需下载约 2.5 GB')
  await expect(a.getByTestId('wf-card-nodes')).toHaveCount(0)
  await expect(a.getByTestId('wf-card-quickrun')).toHaveText('支持快速出图')
  await expect(a.getByTestId('wf-cover-placeholder')).toHaveCount(0)
  await expectImageLoaded(a.locator('img'))
  await expect(b.getByTestId('wf-card-name')).toHaveText('E2E 节点工作流 B')
  await expect(b.getByTestId('wf-card-size')).toHaveText('需下载约 1.2 GB')
  await expect(b.getByTestId('wf-card-nodes')).toHaveText('含 1 个自定义节点')
  await expect(b.getByTestId('wf-cover-placeholder')).toHaveText('放大修复') // 无封面：按分类占位
  await expect(b.locator('img')).toHaveCount(0)
  await expect(a.getByTestId('wf-install-btn')).toHaveText('一键安装')
  // sort 10 的 A 排在 sort 20 的 B 前面
  const order = await page.locator('[data-testid^="wf-card-e2e-wf-"]').evaluateAll((els) => els.map((e) => e.getAttribute('data-testid')))
  expect(order.indexOf('wf-card-e2e-wf-a')).toBeGreaterThanOrEqual(0)
  expect(order.indexOf('wf-card-e2e-wf-a')).toBeLessThan(order.indexOf('wf-card-e2e-wf-b'))
})

test('C-WF-02 分类筛选与搜索：只在前端过滤，无结果时给出空状态', async ({ page }) => {
  await seedWorkflows()
  await page.goto('/workflows')
  await expect(card(page, 'e2e-wf-a')).toBeVisible()
  await expect(page.getByTestId('wf-category-all')).toHaveAttribute('aria-pressed', 'true')
  await page.getByTestId('wf-category-txt2img').click()
  await expect(page.getByTestId('wf-category-txt2img')).toHaveAttribute('aria-pressed', 'true')
  await expect(card(page, 'e2e-wf-a')).toBeVisible()
  await expect(card(page, 'e2e-wf-b')).toHaveCount(0)
  await page.getByTestId('wf-category-upscale').click()
  await expect(card(page, 'e2e-wf-b')).toBeVisible()
  await expect(card(page, 'e2e-wf-a')).toHaveCount(0)
  await page.getByTestId('wf-category-all').click()
  await page.getByTestId('wf-search').fill('节点工作流')
  await expect(card(page, 'e2e-wf-b')).toBeVisible()
  await expect(card(page, 'e2e-wf-a')).toHaveCount(0)
  await page.getByTestId('wf-search').fill('没有这个工作流')
  await expect(page.getByTestId('wf-market-empty')).toContainText('没有符合条件的工作流')
  await page.getByTestId('wf-search').fill('')
  await expect(card(page, 'e2e-wf-a')).toBeVisible()
  await expect(card(page, 'e2e-wf-b')).toBeVisible()
})

test('C-WF-03 详情页：版本、下载量、模型 / 节点 / 快速出图参数表、更新说明；不存在的工作流给出提示', async ({ page }) => {
  await seedWorkflows()
  await page.goto('/workflows')
  await card(page, 'e2e-wf-b').getByTestId('wf-card-name').click()
  await expect(page).toHaveURL(/\/workflows\/e2e-wf-b$/)
  await expect(page.getByTestId('wf-detail-name')).toHaveText('E2E 节点工作流 B')
  await expect(page.getByTestId('wf-detail-version')).toHaveText('v1.0.0')
  await expect(page.getByTestId('wf-detail-size')).toHaveText('1.2 GB')
  await expect(page.getByTestId('wf-detail-description')).toContainText('e2e 用的 fake 工作流')
  const models = page.getByTestId('wf-detail-models')
  await expect(models).toContainText('e2e-b.safetensors')
  await expect(models).toContainText('models/checkpoints')
  await expect(models).toContainText('ModelScope spark-e2e/fake-models')
  const nodes = page.getByTestId('wf-detail-nodes')
  await expect(nodes).toContainText('FakeNode')
  await expect(nodes).toContainText('https://github.com/spark-e2e/FakeNode.git')
  await expect(nodes).toContainText('aaaaaaaa')
  const inputs = page.getByTestId('wf-detail-inputs')
  for (const t of ['提示词', '步数', '种子', '1～50', '留空随机']) await expect(inputs).toContainText(t)
  await expect(page.getByTestId('wf-detail-changelog')).toContainText('首个版本')
  await expect(page.getByTestId('wf-detail-versions')).toContainText('v1.0.0')

  await page.goto('/workflows/e2e-wf-a')
  await expect(page.getByTestId('wf-detail-name')).toHaveText('E2E 文生图 A')
  await expect(page.getByTestId('wf-detail-nodes')).toContainText('只用 ComfyUI 内置节点')

  await page.goto('/workflows/no-such-workflow')
  await expect(page.getByTestId('wf-detail-not-found')).toContainText('工作流不存在或未上架')
})

test('C-WF-04 未登录点「一键安装」跳登录，登录后回到市场并自动打开安装弹窗', async ({ page }) => {
  await seedWorkflows()
  const u = await seedUser()
  await page.goto('/workflows')
  await card(page, 'e2e-wf-a').getByTestId('wf-install-btn').click()
  await expect(page).toHaveURL(/\/login\?redirect=/)
  expect(new URL(page.url()).searchParams.get('redirect')).toBe('/workflows?install=e2e-wf-a')
  await hooks.advance(61)
  await page.getByTestId('login-phone').fill(u.phone)
  await page.getByTestId('login-send').click()
  await page.getByTestId('login-code').fill('000000')
  await page.getByTestId('login-submit').click()
  await expect(page).toHaveURL(/\/workflows$/) // install 参数用过即去掉
  await expect(page.getByTestId('wf-install-dialog')).toBeVisible()
  await expect(page.getByTestId('wf-install-dialog')).toContainText('安装「E2E 文生图 A」')
  await expect(page.getByTestId('wf-install-empty')).toBeVisible() // 新用户没有 ComfyUI
})

test('C-WF-05 安装弹窗选 ComfyUI 部署：确认文案、请求体、跳实例详情并高亮工作流；已装的目标灰显', async ({ page }) => {
  const s = await comfySetup()
  await loginConsole(page, s.u.phone)
  await page.goto('/workflows')
  await card(page, 'e2e-wf-a').getByTestId('wf-install-btn').click()
  const target = page.getByTestId(`wf-target-${s.dep.id}`)
  await expect(target).toHaveAttribute('aria-checked', 'true') // 默认选第一个能装的
  await expect(target).toContainText(s.id)
  await expect(target).toContainText('ComfyUI 运行中')
  await expect(target).toContainText('已添加 0/8')
  await expect(page.getByTestId('wf-install-text')).toContainText('安装「E2E 文生图 A」会重启实例上的 ComfyUI，正在生成的任务会中断；需下载约 2.5 GB 模型')
  const req = page.waitForRequest((r) => r.method() === 'POST' && new URL(r.url()).pathname === `/api/v1/instances/${s.id}/apps/${s.dep.id}/workflows`)
  await page.getByTestId('wf-install-confirm').click()
  expect((await req).postDataJSON()).toEqual({ workflow_id: s.wf.a.id })
  await toast(page, '已开始安装 E2E 文生图 A')
  await expect(page).toHaveURL((u) => u.pathname === `/instances/${s.id}` && u.searchParams.get('highlight') === 'workflows' &&
    u.searchParams.get('dep') === s.dep.id && u.searchParams.get('workflow') === 'e2e-wf-a')
  await expect(wfRow(page, 'e2e-wf-a')).toHaveAttribute('data-state', 'pending')
  await expect(page.getByTestId('wf-state-e2e-wf-a')).toHaveText('待安装')
  await expectRowState(page, s.dep.id, 'queued') // 改集合 → 部署重新排队

  // 再装同一个：目标灰显「已安装」，按钮不可用
  await page.goto('/workflows')
  await card(page, 'e2e-wf-a').getByTestId('wf-install-btn').click()
  await expect(target).toHaveAttribute('aria-disabled', 'true')
  await expect(target).toContainText('已安装')
  await expect(target).toContainText('已添加 1/8')
  await expect(page.getByTestId('wf-install-confirm')).toBeDisabled()
})

test('C-WF-06 没有 ComfyUI：「新开一台实例」打开租用对话框，预选 ComfyUI、MODEL_ID=none、预装该工作流，下单后跳实例详情', async ({ page }) => {
  const apps = await seedApps()
  const { a } = await seedWorkflows()
  await hooks.machines(1)
  const u = await seedUser()
  await loginConsole(page, u.phone)
  await page.goto('/workflows')
  await card(page, 'e2e-wf-a').getByTestId('wf-install-btn').click()
  await expect(page.getByTestId('wf-install-empty')).toBeVisible()
  await page.getByTestId('wf-rent-new').click()
  await expect(page.getByTestId('wf-install-dialog')).toBeHidden()
  await expect(page.getByTestId('rent-dialog')).toBeVisible()
  await expect(page.getByTestId('rent-app')).toContainText('ComfyUI 图像生成')
  await expect(page.getByTestId('rent-image')).toContainText('ComfyUI')
  await expect(page.getByTestId('rent-image-locked')).toBeVisible()
  await expect(page.getByTestId('app-env-MODEL_ID')).toContainText('none')
  await expect(page.getByTestId('rent-workflows-e2e-wf-a')).toHaveAttribute('aria-pressed', 'true')
  await expect(page.getByTestId('rent-workflows-e2e-wf-b')).toHaveAttribute('aria-pressed', 'false')
  await expect(page.getByTestId('rent-workflows-total')).toHaveText('已选 1/8 · 合计需下载约 2.5 GB')
  await page.getByTestId('rent-next').click()
  await expect(page.getByTestId('rent-confirm-workflows')).toHaveText('E2E 文生图 A（合计需下载约 2.5 GB）')
  const req = page.waitForRequest((r) => r.method() === 'POST' && new URL(r.url()).pathname === '/api/v1/orders')
  await page.getByTestId('market-confirm-ok').click()
  const body = (await req).postDataJSON() as { app_id: number; app_env: Record<string, string>; app_workflow_ids: number[] }
  expect(body.app_id).toBe(apps.comfyui.id)
  expect(body.app_env.MODEL_ID).toBe('none') // D15：工作流自带模型，不下预置模型
  expect(body.app_workflow_ids).toEqual([a.id])
  await toast(page, '下单成功，正在开通，ComfyUI 与工作流会自动安装')
  await expect(page).toHaveURL((url) => /^\/instances\/ins-[a-z0-9]+$/.test(url.pathname) && url.searchParams.get('highlight') === 'workflows')
  const id = new URL(page.url()).pathname.split('/').pop()!
  ownInstance(page.request, id)
  const deps = await listDeployments(page.request, id)
  expect(deps.map((d) => d.app_slug)).toEqual(['comfyui'])
  await expect(wfRow(page, 'e2e-wf-a')).toHaveAttribute('data-state', 'pending')
})

test('C-WF-07 安装后应用卡片：部署中显示进度与「正在安装工作流」，运行后工作流「已安装」并可打开 / 快速出图', async ({ page }) => {
  const s = await comfySetup()
  await installWorkflowViaApi(s.api, s.id, s.dep.id, s.wf.b.id)
  await openDetail(page, s)
  await expectRowState(page, s.dep.id, 'queued')
  await expect(page.getByTestId('wf-state-e2e-wf-b')).toHaveText('待安装')
  await tickUntil(s.api, s.id, s.dep.id, 'installing')
  await expectRowState(page, s.dep.id, 'installing')
  await expect(page.getByTestId('wf-installing')).toHaveText('正在安装工作流…（随应用进度）')
  await expect(page.getByTestId('wf-quickrun-e2e-wf-b')).toHaveCount(0) // 部署中不给操作
  await tickUntil(s.api, s.id, s.dep.id, 'running')
  await expectRowState(page, s.dep.id, 'running')
  await expect(wfRow(page, 'e2e-wf-b')).toHaveAttribute('data-state', 'installed', { timeout: 15_000 })
  await expect(page.getByTestId('wf-state-e2e-wf-b')).toHaveText('已安装')
  await expect(page.getByTestId('wf-installing')).toHaveCount(0)
  await expect(wfRow(page, 'e2e-wf-b')).toContainText('v1.0.0')
  await expect(page.getByTestId('wf-open-e2e-wf-b')).toBeEnabled()
  await expect(page.getByTestId('wf-quickrun-e2e-wf-b')).toBeEnabled()
  await expect(page.getByTestId('wf-add')).toBeVisible()
})

test('C-WF-08 每个 ComfyUI 最多 8 个工作流：满了不显示「添加」，安装弹窗灰显目标，API 第 9 个返回 409 workflow_limit', async ({ page }) => {
  const extras = await seedExtraWorkflows(7)
  const s = await comfySetup()
  const eight = [s.wf.a, s.wf.b, ...extras.slice(0, 6)]
  for (const w of eight) await installWorkflowViaApi(s.api, s.id, s.dep.id, w.id) // 部署排队中也能改集合
  const ninth = extras[6]
  const r = await s.api.post(`/api/v1/instances/${s.id}/apps/${s.dep.id}/workflows`, { data: { workflow_id: ninth.id } })
  expect(r.status()).toBe(409)
  expect(((await r.json()) as { code: string }).code).toBe('workflow_limit')
  expect(await installedWorkflows(s.api, s.id, s.dep.id)).toHaveLength(8)

  await openDetail(page, s)
  await expect(page.locator('[data-testid^="wf-row-"]')).toHaveCount(8)
  await expect(page.getByTestId('wf-limit')).toHaveText('每个 ComfyUI 最多安装 8 个工作流')
  await expect(page.getByTestId('wf-add')).toHaveCount(0)

  await page.goto(`/workflows/${ninth.slug}`)
  await page.getByTestId('wf-install-btn').click()
  const target = page.getByTestId(`wf-target-${s.dep.id}`)
  await expect(target).toHaveAttribute('aria-disabled', 'true')
  await expect(target).toContainText('已装满 8 个工作流')
  await expect(target).toContainText('已添加 8/8')
  await expect(page.getByTestId('wf-install-confirm')).toBeDisabled()
})

test('C-WF-09 后台发布新版本后工作流行显示「有新版本」，更新确认含更新说明，更新后版本变为新版本', async ({ page }) => {
  const [c] = await seedExtraWorkflows(1)
  const s = await comfySetup()
  const [before] = await installedRunning(s, c.id)
  const changelog = `e2e 更新说明 ${runStamp}`
  const nv = await hooks.workflowsVersion({ slug: c.slug, changelog })
  await openDetail(page, s)
  await expect(page.getByTestId(`wf-update-tag-${c.slug}`)).toHaveText(`有新版本 v${nv.version}`)
  await wfMenu(page, c.slug, 'update')
  await expect(page.getByTestId('wf-confirm-dialog')).toBeVisible()
  await expect(page.getByTestId('wf-confirm-text')).toHaveText(
    `将「E2E 工作流 C」从 v${before.version} 更新到 v${nv.version}，会重启实例上的 ComfyUI，正在生成的任务会中断`)
  await expect(page.getByTestId('wf-confirm-changelog')).toContainText(changelog)
  const req = page.waitForRequest((r) => r.method() === 'PUT' && new URL(r.url()).pathname === `/api/v1/instances/${s.id}/apps/${s.dep.id}/workflows/${c.id}`)
  await page.getByTestId('wf-confirm-ok').click()
  expect((await req).postDataJSON()).toEqual({ version_id: nv.version_id })
  await toast(page, '正在更新 E2E 工作流 C')
  await expectRowState(page, s.dep.id, 'queued')
  await tickUntil(s.api, s.id, s.dep.id, 'running')
  await expectRowState(page, s.dep.id, 'running')
  await expect(wfRow(page, c.slug)).toHaveAttribute('data-state', 'installed', { timeout: 15_000 })
  await expect(wfRow(page, c.slug)).toContainText(`v${nv.version}`)
  await expect(page.getByTestId(`wf-update-tag-${c.slug}`)).toHaveCount(0)
})

test('C-WF-10 移除：确认文案说明模型与节点保留、会重启 ComfyUI；取消不变，确认后行消失', async ({ page }) => {
  const s = await comfySetup()
  await installedRunning(s, s.wf.a.id)
  await openDetail(page, s)
  await expect(wfRow(page, 'e2e-wf-a')).toHaveAttribute('data-state', 'installed')
  let deletes = 0
  page.on('request', (r) => {
    if (r.method() === 'DELETE' && new URL(r.url()).pathname.endsWith(`/workflows/${s.wf.a.id}`)) deletes += 1
  })
  await wfMenu(page, 'e2e-wf-a', 'remove')
  await expect(page.getByTestId('wf-confirm-text')).toHaveText(
    '移除「E2E 文生图 A」后模型与节点保留在 /root/data/comfyui（可手动删除释放空间）；会重启实例上的 ComfyUI，正在生成的任务会中断')
  await page.getByTestId('wf-confirm-cancel').click()
  await expect(page.getByTestId('wf-confirm-dialog')).toBeHidden()
  await settle(page)
  expect(deletes).toBe(0)
  await expect(wfRow(page, 'e2e-wf-a')).toBeVisible()

  await wfMenu(page, 'e2e-wf-a', 'remove')
  await page.getByTestId('wf-confirm-ok').click()
  await toast(page, '正在移除 E2E 文生图 A')
  expect(deletes).toBe(1)
  await expect(wfRow(page, 'e2e-wf-a')).toHaveCount(0)
  await expect(page.getByTestId('wf-empty')).toBeVisible()
  expect(await installedWorkflows(s.api, s.id, s.dep.id)).toHaveLength(0)
  await tickUntil(s.api, s.id, s.dep.id, 'running') // 重新部署（文件删掉、模型保留）
})

test('C-WF-11 快速出图表单：默认值；种子 / 长度在本地校验不发请求；ComfyUI 拒绝的错误显示在抽屉里', async ({ page }) => {
  const s = await comfySetup()
  await installedRunning(s, s.wf.a.id)
  await openDetail(page, s)
  await page.getByTestId('wf-quickrun-e2e-wf-a').click()
  await expect(page.getByTestId('qr-drawer')).toBeVisible()
  await expect(page.getByTestId('qr-input-prompt')).toHaveValue('a cat')
  await expect(page.getByTestId('qr-input-steps').locator('input')).toHaveValue('20')
  await expect(page.getByTestId('qr-input-seed')).toHaveValue('')
  let runs = 0
  page.on('request', (r) => {
    if (r.method() === 'POST' && new URL(r.url()).pathname.endsWith('/runs')) runs += 1
  })

  await page.getByTestId('qr-input-seed').fill('abc')
  await page.getByTestId('qr-run').click()
  await expect(page.getByTestId('qr-input-seed-error')).toHaveText('种子应为 -1（随机）或 0～9007199254740991 的整数')
  await page.getByTestId('qr-input-seed-random').click() // 「随机」清空种子
  await expect(page.getByTestId('qr-input-seed')).toHaveValue('')
  await expect(page.getByTestId('qr-input-seed-error')).toHaveCount(0)

  await page.getByTestId('qr-input-prompt').fill('x'.repeat(501))
  await page.getByTestId('qr-run').click()
  await expect(page.getByTestId('qr-input-prompt-error')).toHaveText('不能超过 500 字')
  await settle(page)
  expect(runs).toBe(0)

  // 服务器端：ComfyUI 拒绝（node_errors）→ 422，文案显示在 qr-error
  await page.getByTestId('qr-input-prompt').fill('a dog')
  await hooks.comfyReject('e2e 节点参数错误')
  await page.getByTestId('qr-run').click()
  await expect(page.getByTestId('qr-error')).toContainText('e2e 节点参数错误')
  expect(runs).toBe(1)
  await expect(page.getByTestId('qr-status')).toHaveCount(0)
  await expect(page.getByTestId('qr-run')).toBeEnabled()
})

test('C-WF-12 快速出图：排队 → 生成中 → 完成，显示输出图片与本次种子；提交的 prompt 带表单参数', async ({ page }) => {
  const s = await comfySetup()
  await installedRunning(s, s.wf.a.id)
  await openDetail(page, s)
  await page.getByTestId('wf-quickrun-e2e-wf-a').click()
  const text = `e2e 一只猫 ${runStamp}`
  await page.getByTestId('qr-input-prompt').fill(text)
  const steps = page.getByTestId('qr-input-steps').locator('input')
  await steps.fill('8')
  await steps.press('Tab')
  const resp = page.waitForResponse((r) => r.request().method() === 'POST' && new URL(r.url()).pathname.endsWith(`/workflows/${s.wf.a.id}/runs`))
  await page.getByTestId('qr-run').click()
  const res = await resp
  expect(res.status()).toBe(201)
  expect(res.request().postDataJSON()).toEqual({ inputs: { prompt: text, steps: 8 } }) // 种子留空不发 = 随机
  const run = (await res.json()) as { id: string; seed: number }
  expect(Number.isInteger(run.seed)).toBe(true)
  await expect(page.getByTestId('qr-status')).toHaveAttribute('data-state', 'queued')
  await expect(page.getByTestId('qr-status')).toHaveText('排队中')
  await expect(page.getByTestId('qr-run')).toContainText('运行中')
  await expect(page.getByTestId('qr-seed')).toHaveText(`本次种子：${run.seed}`)

  const sent = (await hooks.comfyPrompts()).find((p) => p.client_id === run.id)
  expect(sent).toBeTruthy()
  expect(sent!.prompt['6'].inputs.text).toBe(text)
  expect(sent!.prompt['3'].inputs.steps).toBe(8)
  expect(sent!.prompt['3'].inputs.seed).toBe(run.seed)

  await setComfyHistory(run.id, 'running')
  await expect(page.getByTestId('qr-status')).toHaveAttribute('data-state', 'running') // 每 2 秒轮询
  await expect(page.getByTestId('qr-status')).toHaveText('生成中')
  await setComfyHistory(run.id, 'done', { files: 2 })
  await expect(page.getByTestId('qr-status')).toHaveAttribute('data-state', 'done')
  await expect(page.getByTestId('qr-status')).toHaveText(/^完成/)
  await expect(page.getByTestId('qr-file-0')).toHaveAttribute('data-kind', 'image')
  await expectImageLoaded(page.getByTestId('qr-file-0').locator('img'))
  await expect(page.getByTestId('qr-file-1')).toBeVisible()
  await expect(page.getByTestId('qr-file-0-download')).toHaveAttribute('download', /\.png$/)
  await expect(page.getByTestId('qr-run')).toHaveText('运行')
  await expect(page.getByTestId('qr-run')).toBeEnabled()
})
