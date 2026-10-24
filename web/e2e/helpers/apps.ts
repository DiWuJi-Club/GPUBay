import { expect, type APIRequestContext, type Locator, type Page } from '@playwright/test'
import { adminApi, adminPost, adminPut, createAdmin, type SeededAdmin } from './admin'
import { hooks } from './hooks'
import { instanceOf } from './instances'
import { runStamp } from './phone'

// 应用一键部署（计划 7）用例的公共部分。
// - Runner 不在 e2e-server 里自己跑：用例用 hooks.appsTick 逐轮驱动（每轮把假时钟推进 5 秒），不靠真实等待。
// - 假容器 sshd 是 shell 模式：部署真的在本机临时目录里执行 run.sh，fake 服务是 setsid 出去的 `sleep 100000`，
//   sshtest.Close 杀不到。所以每个造了部署的用例都要 ownInstance 登记实例，并在 afterEach 里 cleanupApps：
//   卸载全部部署并推进到 removed（run.sh remove 会停服务、删目录），不把进程和「活着的部署」留给后面的用例。

export type AppSlug = 'vllm' | 'open-webui' | 'comfyui' | 'jupyterlab'
export type SeededApp = { id: number; version_id: number; port: number }

/** 调 /hooks/apps/seed（幂等）：返回四个 fake 应用的 id / 最新已发布版本 / 主端口。
 *  每次都重新把应用端口转到 fake 应用服务（ingressUpstream 用例会覆盖 8888 等端口），所以每个用例开始时都调一次。 */
export async function seedApps(): Promise<Record<AppSlug, SeededApp>> {
  const { apps } = await hooks.appsSeed()
  const out = {} as Record<AppSlug, SeededApp>
  for (const a of apps) out[a.slug as AppSlug] = { id: a.id, version_id: a.version_id, port: a.port }
  for (const slug of ['vllm', 'open-webui', 'comfyui', 'jupyterlab'] as const) if (!out[slug]) throw new Error(`seedApps: 缺少应用 ${slug}`)
  return out
}

/** 控制台部署视图里用例会读到的字段。 */
export type DeploymentJSON = {
  id: string
  instance_id: string
  app_id: number
  app_slug: string
  app_name: string
  version_id: number
  version: string
  latest_version: string
  update_available: boolean
  state: string
  desired: string
  phase: string
  progress: number
  message: string
  error_code: string
  env: Record<string, string>
  secret_env: Record<string, boolean>
  url: string | null
  cleanup_pending: boolean
}

async function json<T>(r: Awaited<ReturnType<APIRequestContext['get']>>, what: string): Promise<T> {
  if (!r.ok()) throw new Error(`${what} ${r.status()} ${await r.text()}`)
  return (await r.json()) as T
}

export async function getDeployment(api: APIRequestContext, instanceID: string, depID: string): Promise<DeploymentJSON> {
  return json<DeploymentJSON>(await api.get(`/api/v1/instances/${instanceID}/apps/${depID}`), `GET deployment ${depID}`)
}

/** 实例上未终结（非 removed / lost）的部署。 */
export async function listDeployments(api: APIRequestContext, instanceID: string): Promise<DeploymentJSON[]> {
  return (await json<{ items: DeploymentJSON[] }>(await api.get(`/api/v1/instances/${instanceID}/apps`), `GET apps of ${instanceID}`)).items
}

/** 经控制台 API 部署（POST /instances/{id}/apps），返回新部署（queued）。 */
export async function deployViaApi(api: APIRequestContext, instanceID: string, appId: number, env: Record<string, string> = {}): Promise<DeploymentJSON> {
  const r = await api.post(`/api/v1/instances/${instanceID}/apps`, { data: { app_id: appId, env } })
  if (r.status() !== 201) throw new Error(`deploy app ${appId} on ${instanceID}: ${r.status()} ${await r.text()}`)
  return (await r.json()) as DeploymentJSON
}

/** 逐轮 hooks.appsTick(1) 直到部署到达 state；意外失败（failed 而期望的不是 failed）立即报错，带错误码与消息。 */
export async function tickUntil(api: APIRequestContext, instanceID: string, depID: string, state: string, maxRounds = 20): Promise<DeploymentJSON> {
  let d = await getDeployment(api, instanceID, depID)
  for (let round = 0; d.state !== state; round++) {
    if (d.state === 'failed' && state !== 'failed') throw new Error(`tickUntil: ${d.app_slug} 部署失败（${d.error_code}：${d.message}），期望 ${state}`)
    if (round >= maxRounds) throw new Error(`tickUntil: ${d.app_slug} 推进 ${maxRounds} 轮后仍是 ${d.state}（${d.phase} ${d.message}），期望 ${state}`)
    await hooks.appsTick(1)
    d = await getDeployment(api, instanceID, depID)
  }
  return d
}

/** 实例详情「应用」卡片里某个部署的行。 */
export function appRow(page: Page, depId: string): Locator {
  return page.locator(`[data-testid="detail-app-row"][data-dep="${depId}"]`)
}

/** 打开某行的「操作」菜单（菜单项传送到 body，按可见的菜单定位）。 */
export function appMenuItem(page: Page, action: 'redeploy' | 'stop' | 'start' | 'log' | 'remove'): Locator {
  return page.locator(`.el-dropdown-menu:visible [data-testid="detail-app-${action}"]`)
}

export async function appAction(page: Page, depId: string, action: 'redeploy' | 'stop' | 'start' | 'log' | 'remove') {
  await appRow(page, depId).getByTestId('detail-app-actions').click()
  await appMenuItem(page, action).click()
}

/** 记录 window.open 打开的地址（「打开」按钮、列表标签都用 window.open 开新窗口）；在 page.goto 之前调用。 */
export async function captureWindowOpen(page: Page): Promise<() => Promise<string[]>> {
  await page.addInitScript(() => {
    const w = window as unknown as { __opened: string[]; open: (u?: string | URL) => null }
    w.__opened = []
    w.open = (u?: string | URL) => {
      w.__opened.push(String(u))
      return null
    }
  })
  return () => page.evaluate(() => (window as unknown as { __opened: string[] }).__opened.slice())
}

// ---- 清理 ----

const owned: { api: APIRequestContext; id: string }[] = []
const finalizers: (() => Promise<void>)[] = []

/** 登记本用例造了部署的实例：afterEach 里 cleanupApps 会卸载上面的全部部署。 */
export function ownInstance(api: APIRequestContext, id: string) {
  owned.push({ api, id })
}

/** 卸载实例上全部未终结的部署并推进到 removed。实例已停机时先续费 1 天并让 Agent 开机（停机时卸载只会标记
 *  cleanup_pending，fake 服务进程要等实例再次运行、Runner 执行 run.sh remove 才会停）。 */
export async function removeAllApps(api: APIRequestContext, instanceID: string) {
  if ((await instanceOf(api, instanceID)).state === 'stopped') {
    const r = await api.post(`/api/v1/instances/${instanceID}/renew`, { data: { duration: '1d' }, headers: { 'Idempotency-Key': crypto.randomUUID() } })
    if (!r.ok()) throw new Error(`renew ${instanceID} ${r.status()} ${await r.text()}`)
    await hooks.agent()
  }
  for (let round = 0; round < 40; round++) {
    const live = await listDeployments(api, instanceID)
    if (!live.length) return
    for (const d of live) {
      if (d.state === 'removing') continue
      // 写接口一律要求 Content-Type: application/json（兼作 CSRF 防护），DELETE 也要带
      const r = await api.delete(`/api/v1/instances/${instanceID}/apps/${d.id}`, { headers: { 'Content-Type': 'application/json' } })
      if (!r.ok()) throw new Error(`DELETE deployment ${d.id} ${r.status()} ${await r.text()}`)
    }
    await hooks.appsTick(1)
  }
  throw new Error(`removeAllApps: ${instanceID} 40 轮后仍有部署`)
}

/** afterEach：清理本用例登记的全部实例上的部署（失败的用例也清理），再执行登记的收尾（如私有应用退役）。 */
export async function cleanupApps() {
  const list = owned.splice(0)
  const fins = finalizers.splice(0)
  for (const o of list) await removeAllApps(o.api, o.id)
  for (const f of fins) await f()
}

// ---- 用例私有的应用（后台 API 发布）----

/** 与 internal/e2ehooks 的 fakeAppScript 相同：install 汇报一次下载进度，start 起一个挂住的 sleep 当服务。 */
export const FAKE_APP_SCRIPT = `#!/bin/sh
case "$1" in
  install)
    spark_status download 50 "downloading $MODEL_ID"
    sleep 1
    ;;
  start)
    sleep 100000 </dev/null >/dev/null 2>&1 &
    echo $! > "$SPARK_APP_DIR/service.pid"
    ;;
esac
`

/** install 失败（输出 boom、退出码 3）的脚本。 */
export const FAILING_APP_SCRIPT = `#!/bin/sh
case "$1" in
  install)
    echo boom
    exit 3
    ;;
esac
`

let privSeq = 0

export type PrivateApp = {
  id: number
  slug: string
  name: string
  admin: SeededAdmin
  ctx: APIRequestContext
  imageId: number
  /** 建版本并发布（动态码由 adminPost 补），返回版本 id。 */
  publish: (version: string, script: string) => Promise<number>
}

/**
 * 用例私有的已上架应用：要「后台发布新版本」的用例不能动共享的四个 fake 应用（之后的用例会部署到新版本）。
 * 兼容镜像是 api 看到的第一个镜像（runningInstance 下单用的那个），主端口 8000（假容器里转到 fake 应用服务）。
 */
export async function privateApp(api: APIRequestContext): Promise<PrivateApp> {
  privSeq += 1
  const images = (await json<{ items: { id: number }[] }>(await api.get('/api/v1/images'), 'GET images')).items
  const imageId = images[0].id
  const admin = await createAdmin('super')
  const ctx = await adminApi(admin)
  const slug = `e2e-${runStamp}-${privSeq}`
  const body = { slug, name: `E2E 应用 ${privSeq}`, summary: 'e2e 私有应用', description: '', category: 'tool', icon: 'tool', sort: 9000, status: 'published' }
  const app = await adminPost<{ id: number }>(ctx, admin, '/admin/api/v1/apps', body)
  const publish = async (version: string, script: string) => {
    const v = await adminPost<{ id: number }>(ctx, admin, `/admin/api/v1/apps/${app.id}/versions`, {
      version, image_ids: [imageId], script, changelog: `e2e ${version}`,
      spec: { ports: [{ port: 8000, label: '服务', health_path: '/health' }], timeout_minutes: 10 },
    })
    await adminPost(ctx, admin, `/admin/api/v1/apps/${app.id}/versions/${v.id}/publish`, {})
    return v.id
  }
  // 用例结束（cleanupApps 卸载完部署之后）：应用改为已退役，从控制台目录里消失
  // （重新登录：用例里推进过假时钟，后台会话可能已空闲失效）
  finalizers.push(async () => {
    await adminPut(await adminApi(admin), admin, `/admin/api/v1/apps/${app.id}`, { ...body, status: 'retired' })
  })
  return { id: app.id, slug, name: body.name, admin, ctx, imageId, publish }
}

/** 断言部署行最终处于 state（卡片每 5 秒轮询，Runner 只在 tick 时推进，所以状态在两次 tick 之间是稳定的）。 */
export async function expectRowState(page: Page, depId: string, state: string) {
  await expect(appRow(page, depId)).toHaveAttribute('data-state', state, { timeout: 15_000 })
}
