import { expect, test, type Page } from '@playwright/test'
import { adminApi, adminPost, createAdmin } from '../helpers/admin'
import { cleanupApps, deployViaApi, ownInstance, seedApps, tickUntil } from '../helpers/apps'
import { loginConsoleAt, seedUser } from '../helpers/console'
import { CONSOLE_SITE_URL, hooks } from '../helpers/hooks'
import { instanceOf, runningInstance } from '../helpers/instances'

// 实例 Web 入口：浏览器真的访问 http://ins-xxxxxxxx-<port>.spark.test:18080（console project 的 --host-resolver-rules 解析到本机），
// 经平台入口 → SSH 上游连接池 → 假容器 sshd 的 direct-tcpip → 回显服务。用例都「先经 API 加端口、再调 ingressUpstream」：
// 该钩子在登记回显服务后立即刷新路由表，不必等 2 秒的定时刷新。
const portUrl = (id: string, port: number) => `http://${id}-${port}.spark.test:18080`

// C-ING-07 部署了应用：afterEach 卸载（其余用例没有部署，清理是空操作）
test.afterEach(cleanupApps)

test('C-ING-01 私有端口：未登录 → 登录页 → 回到目标页，内容来自容器端口；再次访问不再跳转', async ({ page }) => {
  const u = await seedUser()
  const { api, id } = await runningInstance(u.phone)
  await api.post(`/api/v1/instances/${id}/ports`, { data: { port: 8888 } })
  await hooks.ingressUpstream(8888, 'hello-lab')
  await hooks.advance(61) // runningInstance 刚为该手机号发过验证码
  await page.goto(`${portUrl(id, 8888)}/lab/tree?x=1`)
  await expect(page).toHaveURL(/^http:\/\/spark\.test:18080\/login\?redirect=/)
  await page.getByTestId('login-phone').fill(u.phone)
  await page.getByTestId('login-send').click()
  await page.getByTestId('login-code').fill('000000')
  await page.getByTestId('login-submit').click()
  await expect(page).toHaveURL(`${portUrl(id, 8888)}/lab/tree?x=1`)
  await expect(page.locator('body')).toHaveText(`hello-lab|host=${id}-8888.spark.test:18080|path=/lab/tree?x=1|cookie=`)
  // 应用自己的 Cookie 原样到达容器；平台的 sc_ingress 浏览器带着，但转发前被剥掉
  await page.context().addCookies([{ name: 'app', value: '1', domain: `${id}-8888.spark.test`, path: '/' }])
  const resp = await page.goto(`${portUrl(id, 8888)}/again`)
  expect(resp!.url()).toBe(`${portUrl(id, 8888)}/again`)
  expect(resp!.request().redirectedFrom()).toBeNull()
  expect(await resp!.request().headerValue('cookie')).toMatch(/(^|; )sc_ingress=/)
  await expect(page.locator('body')).toHaveText(`hello-lab|host=${id}-8888.spark.test:18080|path=/again|cookie=app=1`)
})

test('C-ING-02 非主人访问私有端口 → 无权访问，看不到内容', async ({ page }) => {
  const owner = await seedUser()
  const { api, id } = await runningInstance(owner.phone)
  await api.post(`/api/v1/instances/${id}/ports`, { data: { port: 8888 } })
  await hooks.ingressUpstream(8888, 'secret')
  const other = await seedUser()
  await loginConsoleAt(page, CONSOLE_SITE_URL, other.phone)
  const resp = await page.goto(`${portUrl(id, 8888)}/`)
  expect(resp!.status()).toBe(403)
  await expect(page.locator('body')).toContainText('无权访问该实例')
  await expect(page.locator('body')).not.toContainText('secret')
})

test('C-ING-03 公开端口免登录直接访问', async ({ page }) => {
  const u = await seedUser()
  const { api, id } = await runningInstance(u.phone)
  await api.post(`/api/v1/instances/${id}/ports`, { data: { port: 7860, visibility: 'public' } })
  await hooks.ingressUpstream(7860, 'gradio')
  const resp = await page.goto(`${portUrl(id, 7860)}/`)
  expect(resp!.status()).toBe(200)
  await expect(page.locator('body')).toHaveText(`gradio|host=${id}-7860.spark.test:18080|path=/|cookie=`)
})

test('C-ING-04 实例停机后显示 503 页', async ({ page }) => {
  const u = await seedUser()
  const { api, id } = await runningInstance(u.phone)
  await api.post(`/api/v1/instances/${id}/ports`, { data: { port: 7860, visibility: 'public' } })
  await hooks.ingressUpstream(7860, 'x')
  await expect(page.goto(`${portUrl(id, 7860)}/`).then((r) => r!.status())).resolves.toBe(200)
  await hooks.advance(86400)
  await hooks.sweep()
  await hooks.agent()
  expect((await instanceOf(api, id)).state).toBe('stopped')
  // 路由表每 2 秒刷新一次
  await expect.poll(async () => (await page.goto(`${portUrl(id, 7860)}/`))!.status(), { timeout: 10_000 }).toBe(503)
  await expect(page.locator('body')).toContainText('实例已到期停机')
})

test('C-ING-05 未开放的端口 404', async ({ page }) => {
  const u = await seedUser()
  const { id } = await runningInstance(u.phone)
  const resp = await page.goto(`${portUrl(id, 9999)}/`)
  expect(resp!.status()).toBe(404)
  await expect(page.locator('body')).toContainText('该端口未开放')
})

test('C-ING-06 被后台强制私有后：详情页显示锁定、无切换按钮；用户改公开被拒', async ({ page }) => {
  const u = await seedUser()
  const { api, id } = await runningInstance(u.phone)
  await api.post(`/api/v1/instances/${id}/ports`, { data: { port: 8888, visibility: 'public' } })
  const admin = await createAdmin('ops')
  const ctx = await adminApi(admin)
  await adminPost(ctx, admin, `/admin/api/v1/instances/${id}/ports/8888/force-private`, { reason: 'e2e' })
  // 在控制台对外地址（http，非安全上下文）上看详情页：也覆盖幂等键在没有 crypto.randomUUID 时的回退
  await loginConsoleAt(page, CONSOLE_SITE_URL, u.phone)
  await page.goto(`${CONSOLE_SITE_URL}/instances/${id}`)
  await expect(page.getByTestId('detail-port-row')).toHaveCount(1)
  await expect(page.getByTestId('detail-port-visibility')).toHaveText('私有')
  await expect(page.getByTestId('detail-port-forced')).toBeVisible()
  await expect(page.getByTestId('detail-port-forced-text')).toContainText('不能改为公开')
  await expect(page.getByTestId('detail-port-toggle')).toHaveCount(0)
  await expect(page.getByTestId('detail-port-public-warn')).toHaveCount(0)
  await expect(page.getByTestId('detail-port-delete')).toBeVisible() // 删除仍可以
  const r = await api.put(`/api/v1/instances/${id}/ports/8888`, { data: { visibility: 'public' } })
  expect(r.status()).toBe(409)
  expect(((await r.json()) as { code: string }).code).toBe('port_forced_private')
})

test('C-ING-07 应用端口默认私有：未登录 302 到登录，登录后回到应用页面', async ({ page }) => {
  const apps = await seedApps() // 8000 端口在假容器里转到 fake 应用服务（回 spark-app-ok|path=…）
  const u = await seedUser()
  const { api, id } = await runningInstance(u.phone)
  ownInstance(api, id)
  const d = await deployViaApi(api, id, apps.vllm.id)
  const done = await tickUntil(api, id, d.id, 'running')
  expect(done.url).toBe(`${portUrl(id, 8000)}/docs`) // 「打开」= 主端口 URL + ports[0].path（vLLM 是 /docs，A-4）
  const ports = ((await (await api.get(`/api/v1/instances/${id}/ports`)).json()) as { items: { port: number; visibility: string }[] }).items
  expect(ports.map((p) => [p.port, p.visibility])).toEqual([[8000, 'private']])
  await hooks.advance(61) // runningInstance 刚为该手机号发过验证码
  // 路由表每 2 秒刷新：端口登记后最多 2 秒才从「未开放」变成走 authorize 跳转
  let resp = null as Awaited<ReturnType<Page['goto']>>
  await expect.poll(async () => {
    resp = await page.goto(`${portUrl(id, 8000)}/docs`)
    return new URL(page.url()).host
  }, { timeout: 10_000 }).toBe('spark.test:18080')
  await expect(page).toHaveURL(/^http:\/\/spark\.test:18080\/login\?redirect=/)
  let first = resp!.request()
  while (first.redirectedFrom()) first = first.redirectedFrom()!
  expect(first.url()).toBe(`${portUrl(id, 8000)}/docs`)
  expect((await first.response())!.status()).toBe(302) // 实例域名上的第一跳就是 302
  await page.getByTestId('login-phone').fill(u.phone)
  await page.getByTestId('login-send').click()
  await page.getByTestId('login-code').fill('000000')
  await page.getByTestId('login-submit').click()
  await expect(page).toHaveURL(`${portUrl(id, 8000)}/docs`)
  await expect(page.locator('body')).toHaveText('spark-app-ok|path=/docs')
})
