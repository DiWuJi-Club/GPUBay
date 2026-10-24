import { expect, test, type APIRequestContext, type Page } from '@playwright/test'
import { adminApi, adminPost, createAdmin, enterTotpDialog, expectAdminUrl, getJSON, loginAdmin, pick, type SeededAdmin } from '../helpers/admin'
import { FAKE_APP_SCRIPT, cleanupApps, deployViaApi, ownInstance, seedApps, tickUntil } from '../helpers/apps'
import { seedUser, toast } from '../helpers/console'
import { hooks } from '../helpers/hooks'
import { runningInstance } from '../helpers/instances'
import { runStamp } from '../helpers/phone'

// 后台应用目录（应用 / 版本 / 发布 / 覆盖率）与应用部署排障。目录用例只建自己的草稿应用（控制台看不到草稿）；
// 部署用例用自己的用户与实例，afterEach 卸载其部署（fake 服务进程随 run.sh remove 停掉）。
test.afterEach(cleanupApps)

let slugSeq = 0
const nextSlug = () => `a${runStamp}-${++slugSeq}`

type AdminImage = { id: number; name: string; ref: string; enabled: boolean; sort: number }

/** 默认镜像：启用的镜像里排序最前的（与控制台下单用的第一个镜像相同）。 */
async function defaultImage(ctx: APIRequestContext): Promise<AdminImage> {
  const imgs = (await getJSON<{ items: AdminImage[] }>(ctx, '/admin/api/v1/images')).items.filter((i) => i.enabled)
  imgs.sort((a, b) => a.sort - b.sort || a.id - b.id)
  return imgs[0]
}

/** 经后台 API 建一个草稿应用（控制台不可见）。 */
async function draftApp(ctx: APIRequestContext, admin: SeededAdmin, name = 'E2E 草稿应用') {
  const slug = nextSlug()
  const app = await adminPost<{ id: number }>(ctx, admin, '/admin/api/v1/apps', { slug, name, summary: 'e2e', category: 'tool', icon: 'tool', sort: 9000 })
  return { id: app.id, slug, name }
}

const SPEC = { ports: [{ port: 8000, label: '服务', health_path: '/health' }], timeout_minutes: 10, env: [{ name: 'MODEL_ID', label: '模型', type: 'enum', required: true, options: ['m1', 'm2'], default: 'm1' }] }

async function draftVersion(ctx: APIRequestContext, admin: SeededAdmin, appId: number, imageId: number, version = '1.0.0', script = FAKE_APP_SCRIPT) {
  return adminPost<{ id: number }>(ctx, admin, `/admin/api/v1/apps/${appId}/versions`, { version, image_ids: [imageId], spec: SPEC, script, changelog: `e2e ${version}` })
}

/** 新建版本页：版本号 + 兼容镜像（下拉多选，选完点别处收起）+ 脚本。 */
async function fillNewVersion(page: Page, version: string, imageName: string, script: string) {
  await page.getByTestId('app-version-number').fill(version)
  await page.getByTestId('app-version-images').click()
  await page.getByRole('option', { name: imageName, exact: true }).click()
  await page.getByTestId('app-version-number').click()
  await page.getByTestId('app-version-script').fill(script)
}

const portInput = (page: Page) => page.getByTestId('app-spec-port-row').first().locator('input').first()

test('A-APP-01 应用新建 / 编辑；只有没有版本的草稿应用可以删除', async ({ page }) => {
  const admin = await createAdmin('super')
  const ctx = await adminApi(admin)
  await loginAdmin(page, admin)
  await page.getByTestId('menu-apps').click()
  await expectAdminUrl(page, '/apps')
  const slug = nextSlug()
  await page.getByTestId('apps-create').click()
  await page.getByTestId('apps-slug').fill(slug)
  await page.getByTestId('apps-name').fill('E2E 新应用')
  await page.getByTestId('apps-summary').fill('一句话简介')
  await pick(page, 'apps-category', '工具')
  await page.getByTestId('apps-submit').click()
  await toast(page, '应用已创建')
  await expect(page).toHaveURL(/\/admin\/apps\/\d+$/)
  const id = Number(page.url().split('/').pop())
  await expect(page.getByTestId('app-detail-name')).toHaveText('E2E 新应用')
  await expect(page.getByTestId('app-detail-slug')).toHaveText(slug)
  await expect(page.getByTestId('app-detail-status')).toHaveText('草稿')

  // 编辑：slug 不可改
  await page.getByTestId('app-detail-edit').click()
  await expect(page.getByTestId('apps-slug')).toBeDisabled()
  await page.getByTestId('apps-name').fill('E2E 改过的名字')
  await page.getByTestId('apps-submit').click()
  await toast(page, '应用已更新')
  await expect(page.getByTestId('app-detail-name')).toHaveText('E2E 改过的名字')

  // 有版本的应用不能删除（不显示删除按钮；后端同样拒绝）
  const image = await defaultImage(ctx)
  const other = await draftApp(ctx, admin, 'E2E 有版本的应用')
  await draftVersion(ctx, admin, other.id, image.id)
  await page.goto(`/admin/apps/${other.id}`)
  await expect(page.getByTestId('app-detail-name')).toHaveText('E2E 有版本的应用')
  await expect(page.getByTestId('app-detail-delete')).toHaveCount(0)
  const del = await ctx.delete(`/admin/api/v1/apps/${other.id}`, { headers: { 'Content-Type': 'application/json' } })
  expect(del.status()).toBe(409)
  expect(((await del.json()) as { code: string }).code).toBe('invalid_state')

  // 没有版本的草稿可以删除
  await page.goto(`/admin/apps/${id}`)
  await page.getByTestId('app-detail-delete').click()
  await page.getByTestId('app-delete-ok').click()
  await toast(page, '应用已删除')
  await expectAdminUrl(page, '/apps')
  await expect(page.getByTestId('apps-row-slug').filter({ hasText: other.slug })).toHaveCount(1)
  await expect(page.getByTestId('apps-row-slug').filter({ hasText: slug })).toHaveCount(0)
})

test('A-APP-02 新建版本用结构化表单：端口 22 校验失败并高亮该行，改正后保存为草稿', async ({ page }) => {
  const admin = await createAdmin('super')
  const ctx = await adminApi(admin)
  const app = await draftApp(ctx, admin)
  const image = await defaultImage(ctx)
  await loginAdmin(page, admin)
  await page.goto(`/admin/apps/${app.id}`)
  await page.getByTestId('app-version-create').click()
  await expectAdminUrl(page, `/apps/${app.id}/versions/new`)
  await fillNewVersion(page, '1.0.0', image.name, FAKE_APP_SCRIPT)
  await portInput(page).fill('22')
  await portInput(page).press('Tab')
  await page.getByTestId('app-version-validate').click()
  await expect(page.getByTestId('app-version-validate-result')).toContainText('ports[0].port')
  await expect(page.getByTestId('app-spec-port-row').first()).toHaveClass(/err-row/)
  await expect(page.getByTestId('app-version-field-error')).toHaveText('第 1 行 端口：端口必须在 1024～65535 之间且不为 22、2222')

  await portInput(page).fill('8000')
  await portInput(page).press('Tab')
  await page.getByTestId('app-version-validate').click()
  await expect(page.getByTestId('app-version-validate-result')).toContainText('校验通过')
  await expect(page.getByTestId('app-spec-port-row').first()).not.toHaveClass(/err-row/)
  await page.getByTestId('app-version-save').click()
  await toast(page, '版本已创建（草稿）')
  await expect(page).toHaveURL(new RegExp(`/admin/apps/${app.id}/versions/\\d+$`))
  await expect(page.getByTestId('app-version-status')).toHaveText('草稿')
  const versions = (await getJSON<{ items: { version: string; status: string; spec: { ports: { port: number }[] } }[] }>(ctx, `/admin/api/v1/apps/${app.id}/versions`)).items
  expect(versions.map((v) => [v.version, v.status, v.spec.ports[0].port])).toEqual([['1.0.0', 'draft', 8000]])
})

test('A-APP-03 脚本语法错误：校验结果给出行号，编辑器行号栏高亮该行', async ({ page }) => {
  const admin = await createAdmin('super')
  const ctx = await adminApi(admin)
  const app = await draftApp(ctx, admin)
  const image = await defaultImage(ctx)
  await loginAdmin(page, admin)
  await page.goto(`/admin/apps/${app.id}/versions/new`)
  await fillNewVersion(page, '1.0.0', image.name, '#!/bin/sh\necho start\nif true; then\n  echo hi\n')
  await page.getByTestId('app-version-validate').click()
  await expect(page.getByTestId('app-version-validate-result')).toContainText('script：第 3 行')
  await expect(page.getByTestId('app-version-field-error')).toContainText('第 3 行')
  await expect(page.locator('.gutter .err')).toHaveText('3') // 行号栏（UI 控件）高亮出错行
  await page.getByTestId('app-version-script').fill('#!/bin/sh\necho start\nif true; then\n  echo hi\nfi\n')
  await page.getByTestId('app-version-validate').click()
  await expect(page.getByTestId('app-version-validate-result')).toContainText('校验通过')
  await expect(page.locator('.gutter .err')).toHaveCount(0)
})

test('A-APP-04 发布：覆盖率不足时提示（在线机器缓存上报后变为全覆盖），确认后要动态码', async ({ page }) => {
  await hooks.machines(1) // 至少一台在线机器
  const admin = await createAdmin('super')
  const ctx = await adminApi(admin)
  // 用例专属的镜像：之前没有任何机器缓存过它
  const ref = `e2e.local/spark/cov-${runStamp}-${++slugSeq}:1`
  const img = await adminPost<{ id: number }>(ctx, admin, '/admin/api/v1/images', { name: `E2E 覆盖率 ${runStamp}-${slugSeq}`, ref, description: 'e2e', enabled: true, sort: 5000 })
  const app = await draftApp(ctx, admin)
  const v = await draftVersion(ctx, admin, app.id, img.id)
  await loginAdmin(page, admin)
  await page.goto(`/admin/apps/${app.id}/versions/${v.id}`)
  await page.getByTestId('app-version-publish').click()
  const dlg = page.getByTestId('app-version-publish-dialog')
  await expect(page.getByTestId('app-version-publish-check')).toHaveText('校验通过')
  await expect(page.getByTestId('app-version-publish-coverage')).toContainText(/已缓存 0 \/ 在线 [1-9]\d*（未知 \d+）/)
  await dlg.getByRole('button', { name: '取消' }).click()
  await expect(dlg).toBeHidden()

  // 全部在线机器上报已缓存该镜像 → 覆盖率变为全覆盖、未知 0
  await hooks.agent({ images: [ref] })
  await page.goto(`/admin/apps/${app.id}`)
  await expect(page.getByTestId('app-version-coverage')).toHaveText(/^已缓存 ([1-9]\d*) \/ 在线 \1（未知 0）$/)

  await page.goto(`/admin/apps/${app.id}/versions/${v.id}`)
  await page.getByTestId('app-version-publish').click()
  await expect(page.getByTestId('app-version-publish-check')).toHaveText('校验通过')
  await expect(page.getByTestId('app-version-publish-coverage')).toHaveCount(0)
  await page.getByTestId('app-version-publish-ok').click()
  await enterTotpDialog(page, admin.totp_secret!)
  await toast(page, /已发布，已向 \d+ 台在线机器下发预拉取/)
  await expect(page.getByTestId('app-version-status')).toHaveText('已发布')
  await expect(page.getByTestId('app-version-publish')).toHaveCount(0)
})

test('A-APP-05 已发布版本只读，只能改更新说明；退役后完全只读', async ({ page }) => {
  const admin = await createAdmin('super')
  const ctx = await adminApi(admin)
  const app = await draftApp(ctx, admin)
  const v = await draftVersion(ctx, admin, app.id, (await defaultImage(ctx)).id)
  await adminPost(ctx, admin, `/admin/api/v1/apps/${app.id}/versions/${v.id}/publish`, {})
  await loginAdmin(page, admin)
  await page.goto(`/admin/apps/${app.id}/versions/${v.id}`)
  await expect(page.getByTestId('app-version-status')).toHaveText('已发布')
  await expect(page.getByTestId('app-version-number')).toBeDisabled()
  await expect(page.getByTestId('app-version-script')).toHaveAttribute('readonly', '')
  await expect(page.getByTestId('app-version-script')).toHaveValue(FAKE_APP_SCRIPT)
  await expect(page.getByTestId('app-version-validate')).toHaveCount(0)
  await expect(page.getByTestId('app-version-publish')).toHaveCount(0)
  await page.getByTestId('app-version-changelog').fill('修正了说明文字')
  await page.getByTestId('app-version-save').click()
  await toast(page, '已保存')
  await page.reload()
  await expect(page.getByTestId('app-version-changelog')).toHaveValue('修正了说明文字')

  await page.getByTestId('app-version-retire').click()
  await page.getByTestId('app-version-retire-ok').click()
  await toast(page, '版本已退役')
  await expect(page.getByTestId('app-version-status')).toHaveText('已退役')
  await expect(page.getByTestId('app-version-save')).toHaveCount(0)
  await expect(page.getByTestId('app-version-retire')).toHaveCount(0)
  await expect(page.getByTestId('app-version-changelog')).toBeDisabled()
})

test('A-APP-06 从已有版本复制为新版本：脚本与变量带入，版本号重新填写', async ({ page }) => {
  const admin = await createAdmin('super')
  const ctx = await adminApi(admin)
  const app = await draftApp(ctx, admin)
  const script = `${FAKE_APP_SCRIPT}# copied marker\n`
  const v = await draftVersion(ctx, admin, app.id, (await defaultImage(ctx)).id, '1.0.0', script)
  await loginAdmin(page, admin)
  await page.goto(`/admin/apps/${app.id}`)
  await pick(page, 'app-version-copy-from', '1.0.0（草稿）')
  await page.getByTestId('app-version-create').click()
  await expectAdminUrl(page, `/apps/${app.id}/versions/new`, { copy_from: String(v.id) })
  await expect(page.getByTestId('app-version-script')).toHaveValue(script)
  await expect(page.getByTestId('app-spec-env-row')).toHaveCount(1)
  await expect(page.getByTestId('app-version-number')).toHaveValue('')
  await page.getByTestId('app-version-number').fill('1.0.1')
  await page.getByTestId('app-version-save').click()
  await toast(page, '版本已创建（草稿）')
  await expect(page).toHaveURL(new RegExp(`/admin/apps/${app.id}/versions/\\d+$`))
  const nv = Number(page.url().split('/').pop())
  expect(nv).not.toBe(v.id)
  expect((await getJSON<{ script: string }>(ctx, `/admin/api/v1/apps/${app.id}/versions/${nv}/script`)).script).toBe(script)
  await page.getByTestId('app-version-back').click()
  await expect(page.getByTestId('app-version-row-version')).toHaveText(['1.0.1', '1.0.0'])
})

/** 用户在自己的运行中实例上部署 vLLM（带密钥，推进到运行中），再加一个排队中的 JupyterLab。 */
async function userWithApps() {
  const apps = await seedApps()
  const u = await seedUser()
  const { api, id } = await runningInstance(u.phone)
  ownInstance(api, id)
  const vllm = await deployViaApi(api, id, apps.vllm.id, { API_KEY: SECRET })
  await tickUntil(api, id, vllm.id, 'running')
  const jupyter = await deployViaApi(api, id, apps.jupyterlab.id, { JUPYTER_TOKEN: 'e2eToken0123456789' })
  return { u, api, id, vllm, jupyter }
}
const SECRET = 'e2e-very-secret-key'

test('A-APP-07 部署列表按实例 / 状态 / 应用筛选；详情里 secret 只显示 ***', async ({ page }) => {
  const { id, vllm, jupyter } = await userWithApps()
  const admin = await createAdmin('finance')
  await loginAdmin(page, admin)
  await page.getByTestId('menu-deployments').click()
  await expectAdminUrl(page, '/deployments')
  const apps = page.getByTestId('deployments-app')
  await page.getByTestId('deployments-filter-instance').fill(id)
  await page.getByTestId('deployments-search').click()
  await expect(page.getByTestId('deployments-total')).toHaveText('共 2 个')
  await expect(apps).toHaveCount(2)
  await pick(page, 'deployments-filter-state', '运行中')
  await page.getByTestId('deployments-search').click()
  await expect(apps).toHaveCount(1)
  await expect(apps).toHaveAttribute('data-dep', vllm.id)
  await page.getByTestId('deployments-reset').click()
  await page.getByTestId('deployments-filter-instance').fill(id)
  await pick(page, 'deployments-filter-app', 'JupyterLab')
  await page.getByTestId('deployments-search').click()
  await expect(apps).toHaveCount(1)
  await expect(apps).toHaveAttribute('data-dep', jupyter.id)
  await expect(page.getByTestId('deployments-state')).toHaveText('等待部署')

  await page.getByTestId('deployments-reset').click()
  await page.getByTestId('deployments-filter-instance').fill(id)
  await pick(page, 'deployments-filter-state', '运行中')
  await page.getByTestId('deployments-search').click()
  await expect(apps).toHaveCount(1)
  await page.getByTestId('deployments-detail').click()
  const drawer = page.getByTestId('deployment-drawer')
  await expect(page.getByTestId('deployment-state')).toHaveAttribute('data-state', 'running')
  const env = (name: string) => page.locator(`[data-testid="deployment-env-row"][data-name="${name}"]`)
  await expect(env('API_KEY')).toHaveAttribute('data-kind', 'secret_set')
  await expect(env('API_KEY').getByTestId('deployment-env-value')).toHaveText('***')
  await expect(env('MODEL_ID').getByTestId('deployment-env-value')).toHaveText('Qwen/Qwen3-8B')
  await expect(page.getByTestId('deployment-log')).toContainText('downloading Qwen/Qwen3-8B')
  expect(await page.content()).not.toContain(SECRET)
  await expect(drawer.getByTestId('deployment-force-remove')).toHaveCount(0) // 财务只读
})

test('A-APP-08 强制卸载要填原因，部署事件记录「平台卸载：原因」，审计记录 app.deployment.force_remove', async ({ page }) => {
  const { api, id, vllm } = await userWithApps()
  const admin = await createAdmin('ops')
  await loginAdmin(page, admin)
  await page.goto(`/admin/deployments?instance_id=${id}&dep=${vllm.id}`)
  await expect(page.getByTestId('deployment-state')).toHaveAttribute('data-state', 'running')
  await page.getByTestId('deployment-force-remove').click()
  await expect(page.getByTestId('deployment-force-remove-dialog')).toBeVisible()
  await page.getByTestId('deployment-force-remove-submit').click()
  await expect(page.getByTestId('deployment-force-remove-error')).toHaveText('请填写原因')
  await page.getByTestId('deployment-force-remove-reason').fill('e2e 违规内容')
  await page.getByTestId('deployment-force-remove-submit').click()
  await expect(page.getByTestId('deployment-force-remove-dialog')).toBeHidden()
  await expect(page.getByTestId('deployment-state')).toHaveAttribute('data-state', 'removing')
  await tickUntil(api, id, vllm.id, 'removed')
  await page.getByTestId('deployment-refresh').click()
  await expect(page.getByTestId('deployment-state')).toHaveAttribute('data-state', 'removed')
  await expect(page.getByTestId('deployment-event-detail').filter({ hasText: '平台卸载：e2e 违规内容' })).toHaveCount(1)

  await page.goto('/admin/audit') // 部署抽屉的遮罩挡着侧边菜单
  await page.getByTestId('audit-actor').fill(String(admin.id))
  await pick(page, 'audit-action', 'app.deployment.force_remove')
  await page.getByTestId('audit-search').click()
  const rows = page.getByRole('row').filter({ has: page.getByTestId('audit-action-cell') })
  await expect(rows).toHaveCount(1)
  await expect(rows.getByTestId('audit-action-cell')).toHaveText('app.deployment.force_remove')
})
