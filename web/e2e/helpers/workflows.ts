import { expect, type APIRequestContext, type Locator, type Page } from '@playwright/test'
import { deployViaApi, ownInstance, seedApps, tickUntil, type AppSlug, type DeploymentJSON, type SeededApp } from './apps'
import { placeOrder, seedUser, userApi } from './console'
import { hooks, type SeededWorkflow } from './hooks'

// 工作流市场 / 安装 / 快速出图（计划 11）用例的公共部分。
// - 工作流用 /hooks/workflows/seed 的 A（e2e-wf-a：txt2img、有封面、无节点、2.5 GB）与 B（e2e-wf-b：upscale、FakeNode、1.2 GB）；
//   要「8 个上限」或「发布新版本」的用例用 /hooks/workflows/seed-many 的额外工作流 e2e-wf-c…（0.5 GB）。
//   A、B 永远只有 1.0.0（没有用例给它们发新版本），新版本只发在额外工作流上，断言一律用钩子返回的版本号（控制台两个 project 各跑一轮）。
// - ComfyUI 部署用 fake comfyui 1.1.0（supports_workflows，MODEL_ID 默认 none），实例要用 ComfyUI 镜像；
//   部署由 hooks.appsTick 逐轮推进，fake app.sh 按部署包里的 workflows/manifest.json 「安装」（不下载）。
// - 快速出图经假容器的 8188 到进程内 fake ComfyUI，hooks.comfyHistory 把某次运行推到 running / done / error。
// - 造了部署的用例都要 ownInstance（comfySetup 已登记），afterEach 用 cleanupApps 卸载。

export type Seeded = { a: SeededWorkflow; b: SeededWorkflow }

/** /hooks/workflows/seed（幂等）：返回 A、B。 */
export async function seedWorkflows(): Promise<Seeded> {
  const { workflows } = await hooks.workflowsSeed()
  const a = workflows.find((w) => w.slug === 'e2e-wf-a')
  const b = workflows.find((w) => w.slug === 'e2e-wf-b')
  if (!a || !b) throw new Error(`seedWorkflows: 缺少 A / B：${JSON.stringify(workflows)}`)
  return { a, b }
}

/** /hooks/workflows/seed-many（幂等）：额外工作流 e2e-wf-c… 共 n 个（1～7）。 */
export async function seedExtraWorkflows(n: number): Promise<SeededWorkflow[]> {
  return (await hooks.workflowsSeedMany(n)).workflows
}

/** fake ComfyUI 对快速出图运行 runId 的 /history 状态。 */
export function setComfyHistory(runId: string, state: 'queued' | 'running' | 'done' | 'error', opts: { files?: number; message?: string } = {}) {
  return hooks.comfyHistory({ run_id: runId, state, ...opts })
}

/** 控制台部署视图里的已装工作流（GET /instances/{id}/apps 每项的 workflows）。 */
export type InstalledJSON = {
  workflow_id: number
  slug: string
  name: string
  version_id: number
  version: string
  state: 'pending' | 'installed' | 'failed'
  latest_version_id: number | null
  latest_version: string
  update_available: boolean
  quick_run: boolean
}
export type InstallResult = { deployment: DeploymentJSON & { workflows: InstalledJSON[] }; workflows: InstalledJSON[] }

async function ok<T>(r: Awaited<ReturnType<APIRequestContext['get']>>, what: string, status = 200): Promise<T> {
  if (r.status() !== status) throw new Error(`${what}: ${r.status()} ${await r.text()}`)
  return (await r.json()) as T
}

const wfBase = (instanceID: string, depID: string) => `/api/v1/instances/${instanceID}/apps/${depID}/workflows`

/** 经控制台 API 把工作流装到 ComfyUI 部署（POST …/workflows，201；部署回到 queued）。 */
export async function installWorkflowViaApi(api: APIRequestContext, instanceID: string, depID: string, workflowID: number): Promise<InstallResult> {
  return ok<InstallResult>(await api.post(wfBase(instanceID, depID), { data: { workflow_id: workflowID } }), `install workflow ${workflowID} on ${depID}`, 201)
}

/** GET …/workflows：部署上的已装工作流。 */
export async function installedWorkflows(api: APIRequestContext, instanceID: string, depID: string): Promise<InstalledJSON[]> {
  return (await ok<{ items: InstalledJSON[] }>(await api.get(wfBase(instanceID, depID)), `GET workflows of ${depID}`)).items
}

export type RunJSON = { id: string; prompt_id: string; seed: number | null; state: string; files: { index: number; filename: string; kind: string; url: string }[]; message: string }

/** 经控制台 API 发起一次快速出图（POST …/runs，201）。 */
export async function startRunViaApi(api: APIRequestContext, instanceID: string, depID: string, workflowID: number, inputs: Record<string, unknown> = {}): Promise<RunJSON> {
  return ok<RunJSON>(await api.post(`${wfBase(instanceID, depID)}/${workflowID}/runs`, { data: { inputs } }), `start run of ${workflowID}`, 201)
}

/** API 下单（ComfyUI 镜像）并让 Agent 开通，返回运行中的实例。 */
export async function comfyInstance(phone: string): Promise<{ api: APIRequestContext; id: string }> {
  const api = await userApi(phone)
  await hooks.machines(1)
  const images = (await ok<{ items: { name: string }[] }>(await api.get('/api/v1/images'), 'GET images')).items
  const imageIndex = images.findIndex((i) => i.name === 'ComfyUI')
  if (imageIndex < 0) throw new Error('comfyInstance: 没有 ComfyUI 镜像（先 seedApps）')
  const id = await placeOrder(api, { imageIndex })
  await hooks.agent()
  return { api, id }
}

export type ComfySetup = {
  apps: Record<AppSlug, SeededApp>
  wf: Seeded
  u: { id: number; phone: string }
  api: APIRequestContext
  id: string
  dep: DeploymentJSON
}

/** 种子数据 + 新用户 + ComfyUI 镜像的运行中实例 + 推进到 running 的 comfyui 部署（无工作流）；实例已 ownInstance。 */
export async function comfySetup(): Promise<ComfySetup> {
  const apps = await seedApps()
  const wf = await seedWorkflows() // 在 seedApps 之后也无妨：两者都会把 8188 转到 fake ComfyUI
  const u = await seedUser()
  const { api, id } = await comfyInstance(u.phone)
  ownInstance(api, id)
  const d = await deployViaApi(api, id, apps.comfyui.id)
  const dep = await tickUntil(api, id, d.id, 'running')
  return { apps, wf, u, api, id, dep }
}

/** 装上工作流并推进到部署 running（工作流 installed）。 */
export async function installedRunning(s: Pick<ComfySetup, 'api' | 'id' | 'dep'>, ...workflowIDs: number[]): Promise<InstalledJSON[]> {
  for (const w of workflowIDs) await installWorkflowViaApi(s.api, s.id, s.dep.id, w)
  await tickUntil(s.api, s.id, s.dep.id, 'running')
  const list = await installedWorkflows(s.api, s.id, s.dep.id)
  for (const w of list) if (w.state !== 'installed') throw new Error(`installedRunning: ${w.slug} 是 ${w.state}`)
  return list
}

/** 实例详情工作流子区块里某个工作流的行。 */
export const wfRow = (page: Page, slug: string): Locator => page.getByTestId(`wf-row-${slug}`)

/** 打开工作流行的「更多」菜单并点菜单项（菜单传送到 body，按可见的菜单定位）。 */
export async function wfMenu(page: Page, slug: string, action: 'update' | 'remove') {
  await page.getByTestId(`wf-more-${slug}`).click()
  await page.locator(`.el-dropdown-menu:visible [data-testid="wf-${action}-${slug}"]`).click()
}

/** 图片真的加载出来了（naturalWidth > 0）。 */
export async function expectImageLoaded(img: Locator) {
  await expect(img).toBeVisible()
  await expect.poll(() => img.evaluate((el) => (el as HTMLImageElement).complete && (el as HTMLImageElement).naturalWidth)).toBeGreaterThan(0)
}
