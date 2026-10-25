import { expect, test, type APIRequestContext, type Page } from '@playwright/test'
import {
  FAILING_APP_SCRIPT, FAKE_APP_SCRIPT, appAction, appMenuItem, appRow, captureWindowOpen, cleanupApps, deployViaApi, expectRowState, listDeployments, ownInstance,
  privateApp, seedApps, tickUntil,
} from '../helpers/apps'
import { loginConsole, seedUser, toast } from '../helpers/console'
import { hooks } from '../helpers/hooks'
import { instanceOf, runningInstance } from '../helpers/instances'

// 实例详情「应用」卡片与实例列表的应用标签。每个用例用自己的用户与运行中实例（runningInstance），
// 部署经 hooks.appsTick 逐轮推进；afterEach 卸载本用例的全部部署（fake 服务进程随 run.sh remove 停掉）。
test.afterEach(cleanupApps)

async function setup() {
  const apps = await seedApps()
  const u = await seedUser()
  const { api, id } = await runningInstance(u.phone)
  ownInstance(api, id)
  return { apps, u, api, id }
}

async function openDetail(page: Page, phone: string, id: string) {
  await loginConsole(page, phone)
  await page.goto(`/instances/${id}`)
  await expect(page.getByTestId('detail-apps-card')).toBeVisible()
}

const rows = (page: Page) => page.getByTestId('detail-app-row')

async function running(api: APIRequestContext, id: string, appId: number, env: Record<string, string> = {}) {
  const d = await deployViaApi(api, id, appId, env)
  return tickUntil(api, id, d.id, 'running')
}

test('C-APP-01 添加应用弹窗：不兼容当前镜像的 ComfyUI 灰显并说明需要的镜像，点了也选不中', async ({ page }) => {
  const { u, api, id } = await setup()
  await openDetail(page, u.phone, id)
  await expect(page.getByTestId('detail-apps-empty')).toHaveText('还没有部署应用')
  await page.getByTestId('detail-app-add').click()
  await expect(page.getByTestId('app-deploy-dialog')).toBeVisible()
  await expect(page.getByTestId('app-deploy-pick-comfyui')).toHaveAttribute('aria-disabled', 'true')
  await expect(page.getByTestId('app-deploy-pick-comfyui-reason')).toHaveText('需要镜像：ComfyUI')
  for (const slug of ['vllm', 'open-webui', 'jupyterlab']) {
    await expect(page.getByTestId(`app-deploy-pick-${slug}`)).not.toHaveAttribute('aria-disabled', 'true')
    await expect(page.getByTestId(`app-deploy-pick-${slug}-reason`)).toHaveCount(0)
  }
  await page.getByTestId('app-deploy-pick-comfyui').click({ force: true }) // 灰显卡片：点了不选中、不出变量表单
  await expect(page.getByTestId('app-deploy-pick-comfyui')).not.toHaveAttribute('aria-pressed', 'true')
  await expect(page.getByTestId('app-env-form')).toHaveCount(0)
  await expect(page.getByTestId('app-deploy-ok')).toBeDisabled()
  await page.getByTestId('app-deploy-cancel').click()
  await expect(page.getByTestId('app-deploy-dialog')).toBeHidden()
  expect(await listDeployments(api, id)).toHaveLength(0)
})

test('C-APP-02 添加 Open WebUI：请求体只带非空变量，推进后两行都运行中', async ({ page }) => {
  const { apps, u, api, id } = await setup()
  const vllm = await running(api, id, apps.vllm.id)
  await openDetail(page, u.phone, id)
  await expectRowState(page, vllm.id, 'running')
  await page.getByTestId('detail-app-add').click()
  await expect(page.getByTestId('app-deploy-pick-vllm-reason')).toHaveText('已部署')
  await page.getByTestId('app-deploy-pick-open-webui').click()
  await expect(page.getByTestId('app-env-OPENAI_API_BASE_URL')).toHaveValue('http://127.0.0.1:8000/v1')
  const req = page.waitForRequest((r) => r.method() === 'POST' && new URL(r.url()).pathname === `/api/v1/instances/${id}/apps`)
  await page.getByTestId('app-deploy-ok').click()
  // 默认值原样带上，留空的 secret 不发
  expect((await req).postDataJSON()).toEqual({ app_id: apps['open-webui'].id, env: { OPENAI_API_BASE_URL: 'http://127.0.0.1:8000/v1' } })
  await expect(page.getByTestId('app-deploy-dialog')).toBeHidden()
  await expect(rows(page)).toHaveCount(2)
  const webui = (await listDeployments(api, id)).find((d) => d.app_slug === 'open-webui')!
  await expect(appRow(page, webui.id).getByTestId('detail-app-name')).toHaveText('Open WebUI 对话界面')
  await tickUntil(api, id, webui.id, 'running')
  await expectRowState(page, webui.id, 'running')
  await expect(page.locator('[data-testid="detail-app-row"][data-state="running"]')).toHaveCount(2)
  await expect(appRow(page, webui.id).getByTestId('detail-app-state')).toHaveText('运行中')
})

test('C-APP-03 第三个应用后不能再添加：按钮换成上限提示，API 第四个返回 409 app_limit', async ({ page }) => {
  const { apps, u, api, id } = await setup()
  await deployViaApi(api, id, apps.vllm.id)
  await deployViaApi(api, id, apps['open-webui'].id)
  await openDetail(page, u.phone, id)
  await expect(rows(page)).toHaveCount(2)
  await expect(page.getByTestId('detail-app-limit')).toHaveCount(0)
  await page.getByTestId('detail-app-add').click()
  await page.getByTestId('app-deploy-pick-jupyterlab').click()
  await page.getByTestId('app-deploy-ok').click()
  await expect(page.getByTestId('app-env-JUPYTER_TOKEN-error')).toHaveText('请填写 访问令牌') // 必填 secret 没有默认值
  await page.getByTestId('app-env-JUPYTER_TOKEN-gen').click()
  await expect(page.getByTestId('app-env-JUPYTER_TOKEN')).toHaveValue(/^[A-Za-z0-9]{32}$/)
  await page.getByTestId('app-deploy-ok').click()
  await expect(page.getByTestId('app-deploy-dialog')).toBeHidden()
  await expect(rows(page)).toHaveCount(3)
  await expect(page.getByTestId('detail-app-add')).toHaveCount(0)
  await expect(page.getByTestId('detail-app-limit')).toHaveText('每个实例最多部署 3 个应用')
  // 数量先于「已部署」检查：再部署任何应用都是 app_limit
  const r = await api.post(`/api/v1/instances/${id}/apps`, { data: { app_id: apps.vllm.id, env: {} } })
  expect(r.status()).toBe(409)
  expect(((await r.json()) as { code: string }).code).toBe('app_limit')
})

test('C-APP-04 部署失败：显示「脚本执行失败」，日志抽屉里有脚本输出', async ({ page }) => {
  const { u, api, id } = await setup()
  const app = await privateApp(api) // 后台发布一个 install 失败的版本（私有应用，不影响共享的 fake 应用）
  await app.publish('1.0.0', FAILING_APP_SCRIPT)
  const d = await deployViaApi(api, id, app.id)
  const failed = await tickUntil(api, id, d.id, 'failed')
  expect(failed.error_code).toBe('script_failed')
  await openDetail(page, u.phone, id)
  await expectRowState(page, d.id, 'failed')
  await expect(appRow(page, d.id).getByTestId('detail-app-state')).toContainText('失败')
  await expect(appRow(page, d.id).getByTestId('detail-app-error')).toContainText('脚本执行失败')
  await appAction(page, d.id, 'log')
  await expect(page.getByTestId('app-log-text')).toContainText('boom')
  await expect(page.getByTestId('app-log-live')).toBeVisible()
})

test('C-APP-05 重新部署：改模型、密钥留空 → 请求不带 API_KEY，运行后密钥仍已设置', async ({ page }) => {
  const { apps, u, api, id } = await setup()
  const d = await running(api, id, apps.vllm.id, { API_KEY: 'e2e-secret-key-1' })
  await openDetail(page, u.phone, id)
  await expectRowState(page, d.id, 'running')
  await appAction(page, d.id, 'redeploy')
  await expect(page.getByTestId('app-deploy-dialog')).toBeVisible()
  await expect(page.getByTestId('app-deploy-version')).toContainText('v1.0.0（当前）')
  await expect(page.getByTestId('app-env-API_KEY')).toHaveValue('') // secret 从不回显
  await expect(page.getByTestId('app-env-API_KEY')).toHaveAttribute('placeholder', '已设置，留空沿用')
  await page.getByTestId('app-env-MODEL_ID').click()
  await page.getByRole('option', { name: 'Qwen/Qwen3-4B', exact: true }).click()
  const resp = page.waitForResponse((r) => r.request().method() === 'POST' && new URL(r.url()).pathname === `/api/v1/instances/${id}/apps/${d.id}/redeploy`)
  await page.getByTestId('app-deploy-ok').click()
  const body = (await resp).request().postDataJSON() as { app_version_id: number; env: Record<string, string> }
  expect(body.app_version_id).toBe(apps.vllm.version_id)
  expect(body.env.MODEL_ID).toBe('Qwen/Qwen3-4B')
  expect(body.env).not.toHaveProperty('API_KEY')
  await expect(page.getByTestId('app-deploy-dialog')).toBeHidden()
  const after = await tickUntil(api, id, d.id, 'running')
  expect(after.env.MODEL_ID).toBe('Qwen/Qwen3-4B')
  expect(after.secret_env).toEqual({ API_KEY: true })
  expect(after.env).not.toHaveProperty('API_KEY')
  await expectRowState(page, d.id, 'running')
})

test('C-APP-06 停止与启动', async ({ page }) => {
  const { apps, u, api, id } = await setup()
  const d = await running(api, id, apps.vllm.id)
  await openDetail(page, u.phone, id)
  await expectRowState(page, d.id, 'running')
  await appAction(page, d.id, 'stop')
  await toast(page, '正在停止 vLLM 推理服务')
  await tickUntil(api, id, d.id, 'stopped')
  await expectRowState(page, d.id, 'stopped')
  await expect(appRow(page, d.id).getByTestId('detail-app-state')).toHaveText('已停止')
  await expect(appRow(page, d.id).getByTestId('detail-app-open')).toHaveCount(0)
  await appAction(page, d.id, 'start')
  await toast(page, '正在启动 vLLM 推理服务')
  await tickUntil(api, id, d.id, 'running')
  await expectRowState(page, d.id, 'running')
  await expect(appRow(page, d.id).getByTestId('detail-app-open')).toBeEnabled()
})

test('C-APP-07 卸载：确认文案说明模型缓存保留；卸载后行与 Web 访问里的 8000 端口都消失', async ({ page }) => {
  const { apps, u, api, id } = await setup()
  const d = await running(api, id, apps.vllm.id)
  await openDetail(page, u.phone, id)
  const port8000 = page.getByTestId('detail-port-url').filter({ hasText: `${id}-8000.` })
  await expect(port8000).toHaveCount(1)
  await appAction(page, d.id, 'remove')
  await expect(page.getByTestId('app-remove-text')).toContainText('模型缓存保留')
  await expect(page.getByTestId('app-remove-stopped')).toHaveCount(0)
  await page.getByTestId('app-remove-ok').click()
  await toast(page, '正在卸载 vLLM 推理服务')
  await tickUntil(api, id, d.id, 'removed')
  await expect(rows(page)).toHaveCount(0, { timeout: 15_000 })
  await expect(page.getByTestId('detail-apps-empty')).toBeVisible()
  await expect(port8000).toHaveCount(0)
  expect(await listDeployments(api, id)).toHaveLength(0)
})

test('C-APP-08 实例到期停机：每个应用行提示实例已停机，启停与重新部署不可用', async ({ page }) => {
  const { apps, u, api, id } = await setup()
  const a = await running(api, id, apps.vllm.id)
  await running(api, id, apps['open-webui'].id)
  // 1 天的实例推进 25 小时到期停机（只有本用例推进时钟，见 playwright.config.ts）
  await hooks.advance(25 * 3600)
  await hooks.sweep()
  await hooks.agent()
  expect((await instanceOf(api, id)).state).toBe('stopped')
  await openDetail(page, u.phone, id)
  await expect(rows(page)).toHaveCount(2)
  await expect(page.getByTestId('detail-app-instance-hint')).toHaveText(['实例已停机，续费后自动恢复', '实例已停机，续费后自动恢复'])
  // 部署仍记为 running（规格 §6.3），状态标签显示灰色「已停机」而不是绿色「运行中」（验收观察 O-4）
  await expect(appRow(page, a.id).getByTestId('detail-app-state')).toHaveText('已停机')
  await expect(appRow(page, a.id).getByTestId('detail-app-open')).toBeDisabled()
  await appRow(page, a.id).getByTestId('detail-app-actions').click()
  await expect(appMenuItem(page, 'redeploy')).toHaveAttribute('aria-disabled', 'true')
  await expect(appMenuItem(page, 'redeploy')).toContainText('实例已停机，续费后可操作')
  await expect(appMenuItem(page, 'stop')).toHaveAttribute('aria-disabled', 'true')
  await expect(appMenuItem(page, 'log')).not.toHaveAttribute('aria-disabled', 'true')
})

test('C-APP-09 后台发布新版本后应用行显示「有新版本」，重新部署可选最新版本', async ({ page }) => {
  const { u, api, id } = await setup()
  const app = await privateApp(api)
  await app.publish('1.0.0', FAKE_APP_SCRIPT)
  const d = await running(api, id, app.id)
  await openDetail(page, u.phone, id)
  await expectRowState(page, d.id, 'running')
  await expect(appRow(page, d.id).getByTestId('detail-app-version')).toHaveText('v1.0.0')
  await expect(appRow(page, d.id).getByTestId('detail-app-update')).toHaveCount(0)
  await app.publish('1.0.1', FAKE_APP_SCRIPT)
  await page.reload()
  await expect(appRow(page, d.id).getByTestId('detail-app-update')).toHaveText('有新版本 v1.0.1')
  await appAction(page, d.id, 'redeploy')
  await page.getByTestId('app-deploy-version').click()
  await expect(page.getByRole('option')).toHaveText(['v1.0.0（当前）', 'v1.0.1（最新）'])
})

test('C-APP-10 实例列表的应用标签：运行中可点击，打开应用地址', async ({ page }) => {
  const { apps, u, api, id } = await setup()
  const d = await running(api, id, apps.vllm.id)
  expect(d.url).toContain(`${id}-8000.spark.test`)
  const opened = await captureWindowOpen(page)
  await loginConsole(page, u.phone)
  await page.goto('/instances')
  const tag = page.locator(`[data-testid="instance-app"][data-dep="${d.id}"]:visible`)
  await expect(tag).toHaveAttribute('data-state', 'running')
  await expect(tag).toHaveText('vLLM 推理服务 · 运行中')
  await tag.click()
  await expect.poll(opened).toEqual([d.url])
})

test('C-APP-11 vLLM 应用行显示参数与调用方式，「修改参数」改最大上下文后重新部署生效', async ({ page }) => {
  const { apps, u, api, id } = await setup()
  const d = await running(api, id, apps.vllm.id, { MODEL_ID: 'Qwen/Qwen3-4B' })
  await openDetail(page, u.phone, id)
  await expectRowState(page, d.id, 'running')
  const row = appRow(page, d.id)
  await expect(row.getByTestId('detail-app-param-MODEL_ID')).toHaveText('模型：Qwen/Qwen3-4B')
  await expect(row.getByTestId('detail-app-param-MAX_MODEL_LEN')).toHaveText('最大上下文：32768')
  await expect(row.getByTestId('detail-app-param-API_KEY')).toHaveText('接口密钥：未设置')
  await expect(row.getByTestId('detail-app-call-model')).toHaveText('Qwen/Qwen3-4B')
  await expect(row.getByTestId('detail-app-call-local')).toHaveText('http://127.0.0.1:8000/v1')
  await expect(row.getByTestId('detail-app-call-public')).toHaveText(d.url!.replace(/\/docs$/, '/v1'))
  await expect(row.getByTestId('detail-app-call-curl')).toContainText('"model": "Qwen/Qwen3-4B"')

  await row.getByTestId('detail-app-edit-params').click()
  await expect(page.getByTestId('app-deploy-dialog')).toBeVisible()
  const len = page.getByTestId('app-env-MAX_MODEL_LEN').locator('input')
  await expect(len).toHaveValue('32768')
  await len.fill('65536')
  await len.blur()
  const req = page.waitForRequest((r) => r.url().endsWith(`/apps/${d.id}/redeploy`) && r.method() === 'POST')
  await page.getByTestId('app-deploy-ok').click()
  expect((await req).postDataJSON().env).toMatchObject({ MODEL_ID: 'Qwen/Qwen3-4B', MAX_MODEL_LEN: '65536' })
  await tickUntil(api, id, d.id, 'running')
  await page.reload()
  await expect(appRow(page, d.id).getByTestId('detail-app-param-MAX_MODEL_LEN')).toHaveText('最大上下文：65536')
})
