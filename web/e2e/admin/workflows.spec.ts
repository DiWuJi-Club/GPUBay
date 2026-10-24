import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { expect, test, type APIRequestContext, type Page } from '@playwright/test'
import { adminApi, adminPost, createAdmin, drainTasks, enterTotpDialog, expectAdminUrl, getJSON, loginAdmin, pick, recordRequests, type SeededAdmin } from '../helpers/admin'
import { cleanupApps } from '../helpers/apps'
import { toast } from '../helpers/console'
import { runStamp } from '../helpers/phone'
import { comfySetup, expectImageLoaded, installedRunning, seedWorkflows, setComfyHistory, startRunViaApi } from '../helpers/workflows'

// 后台工作流目录（计划 11 规格 §12）：新建 / 编辑 / 封面、版本结构化表单与校验（含 D16）、发布（动态码）、已发布只读、安装列表、仪表盘。
// 目录用例只建自己的草稿工作流（控制台看不到草稿）；版本内容取自 fake 工作流 A 的 1.0.0（graph 引用 e2e-a.safetensors）。
// 安装 / 仪表盘用例用自己的用户与实例，afterEach 卸载其部署。
test.afterEach(cleanupApps)

let slugSeq = 0
const nextSlug = () => `w${runStamp}-${++slugSeq}`

type Version = { id: number; version: string; status: string; graph: unknown; api_prompt: unknown; manifest: Record<string, unknown>; changelog: string }

/** fake 工作流 A 1.0.0 的 graph / api_prompt / manifest（后台 API 读出）。 */
async function contentOfA(ctx: APIRequestContext) {
  const { a } = await seedWorkflows()
  const v = await getJSON<Version>(ctx, `/admin/api/v1/workflows/${a.id}/versions/${a.version_id}`)
  return { a, graph: v.graph, api_prompt: v.api_prompt, manifest: v.manifest }
}

/** 经后台 API 建一个草稿工作流（控制台不可见）。 */
async function draftWorkflow(ctx: APIRequestContext, admin: SeededAdmin, name = 'E2E 草稿工作流') {
  const slug = nextSlug()
  const w = await adminPost<{ id: number }>(ctx, admin, '/admin/api/v1/workflows', { slug, name, summary: 'e2e', description: '', category: 'txt2img', tags: ['e2e'], sort: 9000 })
  return { id: w.id, slug, name }
}

/** 草稿工作流 + 内容同 A 的草稿版本（publish 时再发布）。 */
async function draftVersion(ctx: APIRequestContext, admin: SeededAdmin, opts: { publish?: boolean } = {}) {
  const c = await contentOfA(ctx)
  const w = await draftWorkflow(ctx, admin)
  const v = await adminPost<{ id: number }>(ctx, admin, `/admin/api/v1/workflows/${w.id}/versions`, {
    version: '1.0.0', graph: c.graph, api_prompt: c.api_prompt, manifest: c.manifest, changelog: 'e2e 1.0.0',
  })
  if (opts.publish) await adminPost(ctx, admin, `/admin/api/v1/workflows/${w.id}/versions/${v.id}/publish`, {})
  return { w, vid: v.id, content: c }
}

const fixture = (name: string) => fileURLToPath(new URL(`../fixtures/${name}`, import.meta.url))
const coverInput = (page: Page) => page.getByTestId('adm-wf-cover-upload').locator('input[type=file]')

test('A-WF-01 新建 / 编辑工作流：必填校验、详情信息、slug 不可改、状态可改；没有版本的草稿可删除', async ({ page }) => {
  const admin = await createAdmin('super')
  await loginAdmin(page, admin)
  await page.getByTestId('menu-workflows').click()
  await expectAdminUrl(page, '/workflows')
  await page.getByTestId('adm-wf-new').click()
  await expect(page.getByTestId('adm-wf-dialog')).toBeVisible()
  await expect(page.getByTestId('adm-wf-status')).toHaveCount(0) // 状态只在编辑时可改
  const creates = recordRequests(page, 'POST', '/workflows')
  await page.getByTestId('adm-wf-submit').click()
  await expect(page.getByTestId('adm-wf-form-error')).toBeVisible()
  await drainTasks(page)
  expect(creates).toHaveLength(0) // 空表单在本地拦下，不发请求

  const slug = nextSlug()
  await page.getByTestId('adm-wf-slug').fill(slug)
  await page.getByTestId('adm-wf-name').fill('E2E 新工作流')
  await page.getByTestId('adm-wf-summary').fill('一句话简介')
  await page.getByTestId('adm-wf-description').fill('介绍第一段')
  await pick(page, 'adm-wf-category', '视频')
  await page.getByTestId('adm-wf-tags').fill('SDXL, 入门')
  await page.getByTestId('adm-wf-submit').click()
  await toast(page, '工作流已创建')
  await expect(page).toHaveURL(/\/admin\/workflows\/\d+$/)
  await expect(page.getByTestId('adm-wf-detail-name')).toHaveText('E2E 新工作流')
  await expect(page.getByTestId('adm-wf-detail-status')).toHaveText('草稿')
  await expect(page.getByTestId('adm-wf-detail-slug')).toHaveText(slug)
  await expect(page.getByTestId('adm-wf-detail-category')).toHaveText('视频')
  await expect(page.getByTestId('adm-wf-detail-tags').locator('.el-tag')).toHaveText(['SDXL', '入门'])
  await expect(page.getByTestId('adm-wf-detail-summary')).toHaveText('一句话简介')

  await page.getByTestId('adm-wf-detail-edit').click()
  await expect(page.getByTestId('adm-wf-slug')).toBeDisabled()
  await page.getByTestId('adm-wf-name').fill('E2E 改过的工作流')
  await pick(page, 'adm-wf-status', '已发布')
  await page.getByTestId('adm-wf-submit').click()
  await toast(page, '工作流已更新')
  await expect(page.getByTestId('adm-wf-detail-name')).toHaveText('E2E 改过的工作流')
  await expect(page.getByTestId('adm-wf-detail-status')).toHaveText('已发布')
  await expect(page.getByTestId('adm-wf-delete')).toHaveCount(0) // 不是草稿：不能删
  await page.getByTestId('adm-wf-detail-edit').click()
  await pick(page, 'adm-wf-status', '草稿')
  await page.getByTestId('adm-wf-submit').click()
  await toast(page, '工作流已更新')

  // 列表
  await page.goto('/admin/workflows')
  const row = page.getByTestId(`adm-wf-row-${slug}`)
  await expect(row.getByTestId('adm-wf-row-name')).toHaveText('E2E 改过的工作流')
  await expect(page.getByRole('row').filter({ has: row }).getByTestId('adm-wf-row-versions')).toHaveText('0')

  // 没有版本的草稿可以删除
  await row.click()
  await page.getByTestId('adm-wf-delete').click()
  await page.getByTestId('adm-wf-delete-ok').click()
  await toast(page, '工作流已删除')
  await expectAdminUrl(page, '/workflows')
  await expect(page.getByTestId(`adm-wf-row-${slug}`)).toHaveCount(0)
})

test('A-WF-02 封面上传：类型不对在本地拦下；PNG 预览后保存，详情与列表显示封面', async ({ page }) => {
  const admin = await createAdmin('super')
  const ctx = await adminApi(admin)
  const w = await draftWorkflow(ctx, admin)
  await loginAdmin(page, admin)
  await page.goto(`/admin/workflows/${w.id}`)
  await expect(page.getByTestId('adm-wf-detail-cover')).toHaveCount(0)
  await page.getByTestId('adm-wf-detail-edit').click()
  await coverInput(page).setInputFiles(fixture('bad.gif'))
  await expect(page.getByTestId('adm-wf-cover-error')).toHaveText('封面只能是 PNG、JPG 或 WebP 图片')
  await expect(page.getByTestId('adm-wf-cover-preview')).toHaveCount(0)
  await coverInput(page).setInputFiles(fixture('license.png'))
  await expect(page.getByTestId('adm-wf-cover-error')).toHaveCount(0)
  await expectImageLoaded(page.getByTestId('adm-wf-cover-preview'))
  const upload = page.waitForResponse((r) => r.request().method() === 'POST' && new URL(r.url()).pathname === `/admin/api/v1/workflows/${w.id}/cover`)
  await page.getByTestId('adm-wf-submit').click()
  expect((await upload).status()).toBe(200)
  await toast(page, '工作流已更新')
  await expect(page.getByTestId('adm-wf-dialog')).toBeHidden()
  await expectImageLoaded(page.getByTestId('adm-wf-detail-cover'))
  const view = await getJSON<{ cover_url: string | null }>(ctx, `/admin/api/v1/workflows/${w.id}`)
  expect(view.cover_url).toBeTruthy()

  await page.goto('/admin/workflows')
  const row = page.getByRole('row').filter({ has: page.getByTestId(`adm-wf-row-${w.slug}`) })
  await expectImageLoaded(row.getByTestId('adm-wf-row-cover'))
})

test('A-WF-03 版本结构化表单：graph 引用未声明的模型（D16）被指出，补模型行后通过；Git 主机不在白名单时报错并高亮该行', async ({ page }) => {
  const admin = await createAdmin('super')
  const ctx = await adminApi(admin)
  const c = await contentOfA(ctx)
  const w = await draftWorkflow(ctx, admin)
  await loginAdmin(page, admin)
  await page.goto(`/admin/workflows/${w.id}`)
  await page.getByTestId('adm-wf-version-new').click()
  await expectAdminUrl(page, `/workflows/${w.id}/versions/new`)
  await page.getByTestId('adm-wf-version-number').fill('1.0.0')
  await page.getByTestId('adm-wf-graph').fill(JSON.stringify(c.graph, null, 2))
  await page.getByTestId('adm-wf-api').fill(JSON.stringify(c.api_prompt, null, 2))
  await expect(page.getByTestId('adm-wf-graph-info')).toContainText('3 个节点')
  await expect(page.getByTestId('adm-wf-api-info')).toContainText('7 个节点')

  // D16：前端即时提示 + 后端校验都指出文件名
  await expect(page.getByTestId('adm-wf-missing-models')).toContainText('e2e-a.safetensors')
  await page.getByTestId('adm-wf-validate').click()
  await expect(page.getByTestId('adm-wf-error')).toContainText('e2e-a.safetensors 不在 manifest.models 里')

  await page.getByTestId('adm-wf-add-missing').click()
  await expect(page.getByTestId('adm-wf-missing')).toHaveCount(0)
  const model = page.getByTestId('adm-wf-model-row')
  await expect(model).toHaveCount(1)
  await expect(model.getByTestId('adm-wf-model-name')).toHaveValue('e2e-a.safetensors')
  await expect(model.getByTestId('adm-wf-model-dir')).toContainText('checkpoints')
  await model.getByTestId('adm-wf-model-size').fill('2.5')
  await model.getByTestId('adm-wf-model-ms-repo').fill('spark-e2e/fake-models')
  await model.getByTestId('adm-wf-model-hf-repo').fill('spark-e2e/fake-models')
  await expect(page.getByTestId('adm-wf-download')).toHaveText('下载约 2.5 GB')

  // 节点 Git 主机不在白名单
  await page.getByTestId('adm-wf-node-add').click()
  const node = page.getByTestId('adm-wf-node-row')
  await node.getByTestId('adm-wf-node-name').fill('EvilNode')
  await node.getByTestId('adm-wf-node-git').fill('https://evil.example/x/y.git')
  await node.getByTestId('adm-wf-node-commit').fill('b'.repeat(40))
  await node.getByTestId('adm-wf-node-types').fill('EvilType')
  await page.getByTestId('adm-wf-validate').click()
  await expect(page.getByTestId('adm-wf-error')).toContainText('manifest.nodes[0].git')
  await expect(page.getByTestId('adm-wf-error')).toContainText('Git 主机只能是 github.com / gitee.com / gitcode.com')
  await expect(node).toHaveClass(/err-row/)
  await node.getByTestId('adm-wf-node-remove').click()

  await page.getByTestId('adm-wf-validate').click()
  await expect(page.getByTestId('adm-wf-validate-ok')).toContainText('校验通过')
  await expect(page.getByTestId('adm-wf-error')).toHaveCount(0)
  await page.getByTestId('adm-wf-json-toggle').click()
  await expect(page.getByTestId('adm-wf-json')).toContainText('"name": "e2e-a.safetensors"')
  await page.getByTestId('adm-wf-save').click()
  await toast(page, '版本已创建（草稿）')
  await expect(page).toHaveURL(new RegExp(`/admin/workflows/${w.id}/versions/\\d+$`))
  await expect(page.getByTestId('adm-wf-version-status')).toHaveText('草稿')
  const vs = (await getJSON<{ items: { version: string; status: string; download_gb: number }[] }>(ctx, `/admin/api/v1/workflows/${w.id}/versions`)).items
  expect(vs.map((v) => [v.version, v.status, v.download_gb])).toEqual([['1.0.0', 'draft', 2.5]])
})

test('A-WF-04 发布：先校验并显示下载量，确认后要动态码，发布后版本冻结', async ({ page }) => {
  const admin = await createAdmin('super')
  const ctx = await adminApi(admin)
  const { w, vid } = await draftVersion(ctx, admin)
  await loginAdmin(page, admin)
  await page.goto(`/admin/workflows/${w.id}/versions/${vid}`)
  await expect(page.getByTestId('adm-wf-version-status')).toHaveText('草稿')
  await page.getByTestId('adm-wf-publish').click()
  await expect(page.getByTestId('adm-wf-totp')).toBeVisible()
  await expect(page.getByTestId('adm-wf-publish-check')).toHaveText('校验通过')
  await expect(page.getByTestId('adm-wf-publish-download')).toHaveText('2.5 GB')
  await page.getByTestId('adm-wf-publish-ok').click()
  await enterTotpDialog(page, admin.totp_secret!)
  await toast(page, '已发布，控制台工作流市场可安装该版本')
  await expect(page.getByTestId('adm-wf-version-status')).toHaveText('已发布')
  await expect(page.getByTestId('adm-wf-publish')).toHaveCount(0)
  await expect(page.getByTestId('adm-wf-readonly')).toBeVisible()
  await page.getByTestId('adm-wf-version-back').click()
  await expect(page.getByTestId('adm-wf-version-row-1.0.0').getByTestId('adm-wf-version-row-version')).toHaveText('1.0.0')
  await expect(page.getByRole('row').filter({ has: page.getByTestId('adm-wf-version-row-1.0.0') }).getByTestId('adm-wf-version-row-status')).toHaveText('已发布')
})

test('A-WF-05 已发布版本只读，只能改更新说明；退役后完全只读', async ({ page }) => {
  const admin = await createAdmin('super')
  const ctx = await adminApi(admin)
  const { w, vid } = await draftVersion(ctx, admin, { publish: true })
  await loginAdmin(page, admin)
  await page.goto(`/admin/workflows/${w.id}/versions/${vid}`)
  await expect(page.getByTestId('adm-wf-version-status')).toHaveText('已发布')
  await expect(page.getByTestId('adm-wf-readonly')).toContainText('已发布的版本不能修改')
  await expect(page.getByTestId('adm-wf-version-number')).toBeDisabled()
  await expect(page.getByTestId('adm-wf-graph')).toHaveAttribute('readonly', '')
  await expect(page.getByTestId('adm-wf-model-name')).toBeDisabled()
  await expect(page.getByTestId('adm-wf-model-add')).toHaveCount(0)
  await expect(page.getByTestId('adm-wf-graph-file')).toHaveCount(0)
  await expect(page.getByTestId('adm-wf-validate')).toHaveCount(0)
  await expect(page.getByTestId('adm-wf-publish')).toHaveCount(0)
  await page.getByTestId('adm-wf-changelog').fill('修正了更新说明')
  await page.getByTestId('adm-wf-save').click()
  await toast(page, '已保存')
  await page.reload()
  await expect(page.getByTestId('adm-wf-changelog')).toHaveValue('修正了更新说明')

  await page.getByTestId('adm-wf-retire').click()
  await page.getByTestId('adm-wf-retire-ok').click()
  await toast(page, '版本已退役')
  await expect(page.getByTestId('adm-wf-version-status')).toHaveText('已退役')
  await expect(page.getByTestId('adm-wf-readonly')).toContainText('已退役的版本不能修改')
  await expect(page.getByTestId('adm-wf-save')).toHaveCount(0)
  await expect(page.getByTestId('adm-wf-retire')).toHaveCount(0)
  await expect(page.getByTestId('adm-wf-changelog')).toBeDisabled()
})

test('A-WF-06 安装列表：部署、实例、用户、版本与安装状态；列表与详情的安装数一致', async ({ page }) => {
  const s = await comfySetup()
  await installedRunning(s, s.wf.a.id)
  const admin = await createAdmin('finance') // 所有角色可看
  const ctx = await adminApi(admin)
  const installs = await getJSON<{ total: number; items: { deployment_id: string }[] }>(ctx, `/admin/api/v1/workflows/${s.wf.a.id}/installs?page_size=100`)
  expect(installs.items.some((i) => i.deployment_id === s.dep.id)).toBe(true)
  const rowStats = (await getJSON<{ items: { id: number; installs_live: number }[] }>(ctx, '/admin/api/v1/workflows')).items.find((x) => x.id === s.wf.a.id)!

  await loginAdmin(page, admin)
  await page.goto('/admin/workflows')
  const listRow = page.getByRole('row').filter({ has: page.getByTestId('adm-wf-row-e2e-wf-a') })
  await expect(listRow.getByTestId('adm-wf-row-installs')).toHaveText(String(rowStats.installs_live))
  await expect(page.getByTestId('adm-wf-new')).toHaveCount(0) // 财务只读
  await expect(page.getByTestId('adm-wf-actions-e2e-wf-a')).toHaveCount(0)
  await listRow.click()
  await expect(page.getByTestId('adm-wf-detail-name')).toHaveText('E2E 文生图 A')
  await expect(page.getByTestId('adm-wf-detail-installs')).toHaveText(String(rowStats.installs_live))
  await expect(page.getByTestId('adm-wf-detail-edit')).toHaveCount(0)
  await page.getByTestId('adm-wf-installs').click()
  await expectAdminUrl(page, `/workflows/${s.wf.a.id}/installs`)
  await expect(page.getByTestId('adm-wf-installs-total')).toHaveText(`共 ${installs.total} 个`)
  const link = page.locator(`[data-testid="adm-wf-install-row"][data-dep="${s.dep.id}"]`)
  await expect(link).toHaveText(s.dep.id) // 按加入时间倒序：本用例的安装在第一页
  const row = page.getByRole('row').filter({ has: link })
  await expect(row.getByTestId('adm-wf-install-dep-state')).toHaveText('运行中')
  await expect(row.getByTestId('adm-wf-install-instance')).toContainText(s.id)
  await expect(row.getByTestId('adm-wf-install-user')).toContainText(String(s.u.id))
  await expect(row.getByTestId('adm-wf-install-version')).toHaveText('1.0.0')
  await expect(row.getByTestId('adm-wf-install-state')).toHaveAttribute('data-state', 'installed')
  await expect(row.getByTestId('adm-wf-install-state')).toHaveText('已安装')
  await link.click()
  await expectAdminUrl(page, '/deployments', { dep: s.dep.id })
  await expect(page.getByTestId('deployment-state')).toHaveAttribute('data-state', 'running')
})

test('A-WF-07 仪表盘：已安装工作流数与 24 小时快速出图数随安装 / 运行增加，链接到工作流目录', async ({ page }) => {
  const admin = await createAdmin('ops')
  const ctx = await adminApi(admin)
  type Dash = { workflow_installs: number; workflow_runs_24h: number }
  // 两个计数都是全库口径（admin project 串行，没有别的用例同时改）：各自在变化前一刻取基线。
  // 24 小时窗口按假时钟算，comfySetup / tick 会推进时钟，所以运行数的基线放到 startRunViaApi 前一刻
  const installsBefore = (await getJSON<Dash>(ctx, '/admin/api/v1/dashboard')).workflow_installs
  const s = await comfySetup()
  await installedRunning(s, s.wf.a.id, s.wf.b.id)
  const runsBefore = (await getJSON<Dash>(ctx, '/admin/api/v1/dashboard')).workflow_runs_24h
  const run = await startRunViaApi(s.api, s.id, s.dep.id, s.wf.a.id, { prompt: 'dashboard' })
  await setComfyHistory(run.id, 'done')
  const after = await getJSON<Dash>(ctx, '/admin/api/v1/dashboard')
  expect(after.workflow_installs).toBe(installsBefore + 2)
  expect(after.workflow_runs_24h).toBe(runsBefore + 1)

  await loginAdmin(page, admin)
  await expect(page.getByTestId('dash-workflow-installs')).toHaveText(String(after.workflow_installs))
  await expect(page.getByTestId('dash-workflow-runs')).toHaveText(String(after.workflow_runs_24h))
  await page.getByTestId('dash-workflows-link').click()
  await expectAdminUrl(page, '/workflows')
  await expect(page.getByTestId('adm-wf-table')).toBeVisible()
})
