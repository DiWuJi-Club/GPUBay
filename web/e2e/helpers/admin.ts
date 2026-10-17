import { expect, request, type APIRequestContext, type Locator, type Page } from '@playwright/test'
import { ADMIN_URL, hooks, type Role } from './hooks'
import { runStamp } from './phone'
import { totp } from './totp'

export { pauseClock } from './clock'

let adminSeq = 0
let serialSeq = 0

export function nextUsername(): string {
  adminSeq += 1
  return `e2e_${runStamp}_${adminSeq}`
}

export function nextSerial(): string {
  serialSeq += 1
  return `E2E-${runStamp}-${serialSeq}`
}

export interface SeededAdmin {
  id: number
  username: string
  password: string
  role: Role
  totp_secret?: string
  recovery_codes?: string[]
}

export async function createAdmin(role: Role, bindTotp = true): Promise<SeededAdmin> {
  const username = nextUsername()
  const r = await hooks.admins({ username, role, bind_totp: bindTotp })
  return { ...r, username, role }
}

/** 取管理员的 TOTP 密钥；createAdmin(role, false) 造出的管理员没有密钥，给出明确错误而不是算出无效码。 */
function secretOf(admin: SeededAdmin): string {
  if (!admin.totp_secret) throw new Error(`管理员 ${admin.username} 未绑定动态码（createAdmin 时 bindTotp=false），无法计算动态码`)
  return admin.totp_secret
}

/** 先推进 30 秒（同一时间片的动态码只能用一次），再按服务器时间算码。 */
export async function totpNow(secret: string): Promise<string> {
  if (!secret) throw new Error('totpNow: 动态码密钥为空（管理员未绑定动态码？）')
  const now = await hooks.advance(30)
  return totp(secret, Math.floor(now.getTime() / 1000))
}

export async function loginAdmin(page: Page, admin: SeededAdmin) {
  await page.goto('/admin/login')
  await page.getByTestId('login-username').fill(admin.username)
  await page.getByTestId('login-password').fill(admin.password)
  await page.getByTestId('login-submit').click()
  await expect(page.getByTestId('login-totp-code')).toBeVisible()
  await page.getByTestId('login-totp-code').fill(await totpNow(secretOf(admin)))
  await page.getByTestId('login-totp-submit').click()
  await expect(page.getByTestId('layout-username')).toHaveText(admin.username)
}

/** 全局动态码弹框（请求层 totp_required 触发）。 */
export async function enterTotpDialog(page: Page, secret: string) {
  await expect(page.getByTestId('totp-dialog')).toBeVisible()
  await page.getByTestId('totp-input').fill(await totpNow(secret))
  await page.getByTestId('totp-ok').click()
}

export async function adminApi(admin: SeededAdmin): Promise<APIRequestContext> {
  const ctx = await request.newContext({ baseURL: ADMIN_URL })
  const login = await ctx.post('/admin/api/v1/auth/login', { data: { username: admin.username, password: admin.password } })
  if (!login.ok()) throw new Error(`admin login ${login.status()} ${await login.text()}`)
  const t = await ctx.post('/admin/api/v1/auth/totp', { data: { code: await totpNow(secretOf(admin)) } })
  if (!t.ok()) throw new Error(`admin totp ${t.status()} ${await t.text()}`)
  return ctx
}

/** 后台写接口；收到 totp_required 时自动补 totp 重发一次。 */
export async function adminSend<T = Record<string, unknown>>(ctx: APIRequestContext, admin: SeededAdmin, method: 'post' | 'put', path: string, data: Record<string, unknown> = {}): Promise<T> {
  let r = await ctx[method](path, { data })
  if (r.status() === 403 && ((await r.json()) as { code: string }).code === 'totp_required') {
    r = await ctx[method](path, { data: { ...data, totp: await totpNow(secretOf(admin)) } })
  }
  if (!r.ok()) throw new Error(`${method.toUpperCase()} ${path} ${r.status()} ${await r.text()}`)
  return (await r.json()) as T
}
export const adminPost = <T = Record<string, unknown>>(ctx: APIRequestContext, admin: SeededAdmin, path: string, data: Record<string, unknown> = {}) => adminSend<T>(ctx, admin, 'post', path, data)
export const adminPut = <T = Record<string, unknown>>(ctx: APIRequestContext, admin: SeededAdmin, path: string, data: Record<string, unknown> = {}) => adminSend<T>(ctx, admin, 'put', path, data)

/** 把当前全部空闲机器置为维护中：数据库整轮共享，「没有空闲机器」只能这样构造。 */
export async function drainIdleMachines(ctx: APIRequestContext, admin: SeededAdmin) {
  for (let round = 0; round < 10; round++) {
    const r = await ctx.get('/admin/api/v1/machines?state=idle&page_size=100')
    if (!r.ok()) throw new Error(`GET machines ${r.status()} ${await r.text()}`)
    const { items } = (await r.json()) as { items: { id: number }[] }
    if (items.length === 0) return
    for (const m of items) await adminPost(ctx, admin, `/admin/api/v1/machines/${m.id}/maintenance`, { on: true })
  }
  throw new Error('drainIdleMachines: 10 轮后仍有空闲机器')
}

/** 精确断言后台地址：路径与查询参数都必须完全一致（不用后缀正则，避免 ?redirect=/machines 这类误配）。 */
export async function expectAdminUrl(page: Page, path: string, query: Record<string, string> = {}) {
  await expect(page).toHaveURL((u) => {
    const actual = Object.fromEntries(u.searchParams)
    return u.origin === ADMIN_URL && u.pathname === `/admin${path}` && JSON.stringify(actual) === JSON.stringify(query)
  })
}

/** 记录某个后台接口的请求（方法 + 路径精确匹配，查询串不参与匹配）；body 为解析后的 JSON。 */
export function recordRequests(page: Page, method: string, path: string) {
  const seen: { body: Record<string, unknown> | null; search: string }[] = []
  page.on('request', (r) => {
    const u = new URL(r.url())
    if (r.method() !== method || u.pathname !== `/admin/api/v1${path}`) return
    const raw = r.postData()
    seen.push({ body: raw ? (JSON.parse(raw) as Record<string, unknown>) : null, search: u.search })
  })
  return seen
}

/** 让页面跑完若干轮宏任务（MessageChannel 不受 page.clock 接管）：时钟冻结时 settle 依赖的 requestAnimationFrame 不会触发，用它代替。 */
export function drainTasks(page: Page, n = 20) {
  return page.evaluate(async (count) => {
    for (let i = 0; i < count; i++)
      await new Promise<void>((resolve) => {
        const ch = new MessageChannel()
        ch.port1.onmessage = () => resolve()
        ch.port2.postMessage(0)
      })
  }, n)
}

/**
 * 同一个任务里连点两次（中间没有渲染机会，按钮来不及变成 loading 禁用）：
 * 模拟极快的双击 / 重复提交，专门验证「请求进行中不再提交」的同步守卫。
 */
export async function clickTwiceSync(locator: Locator) {
  await locator.evaluate((el) => {
    ;(el as HTMLElement).click()
    ;(el as HTMLElement).click()
  })
}

/**
 * 记录页面上出现过的每一条 ElMessage 提示（MutationObserver，提示自动关闭后仍计入），返回读取函数。
 * 用于「同一次故障只提示一次」这类断言：数当前可见的 .el-message 会因为提示 3 秒后自动关闭而漏数。
 * 在 page.goto 之前调用；每次整页加载（reload / goto）记录从零开始。
 */
export async function trackToasts(page: Page): Promise<() => Promise<string[]>> {
  await page.addInitScript(() => {
    const w = window as unknown as { __toasts: string[] }
    w.__toasts = []
    new MutationObserver((records) => {
      for (const r of records)
        for (const n of Array.from(r.addedNodes)) if (n instanceof HTMLElement && n.classList.contains('el-message')) w.__toasts.push(n.textContent?.trim() ?? '')
    }).observe(document, { childList: true, subtree: true })
  })
  return () => page.evaluate(() => (window as unknown as { __toasts: string[] }).__toasts.slice())
}

let pduSeq = 0
/** 通过 API 新增 PDU（缺省 mock）；snmp 时 community 缺省为 public，可传入专门的值用于「不泄露」断言。 */
export async function createPDU(ctx: APIRequestContext, admin: SeededAdmin, opts: { name?: string; driver?: 'mock' | 'snmp'; community?: string } = {}) {
  pduSeq += 1
  const name = opts.name ?? `pdu-${runStamp}-${pduSeq}`
  const driver = opts.driver ?? 'mock'
  const body = { name, driver, address: `10.97.0.${(pduSeq % 250) + 1}`, community: driver === 'snmp' ? (opts.community ?? 'public') : '', oid_prefix: '1.3.6.1.4.1.318.1.1.4.4.2.1.3', cycle_value: 3, on_value: 1 }
  const out = await adminPost<{ id: number; name: string }>(ctx, admin, '/admin/api/v1/pdus', body)
  return { id: out.id, name }
}

let ipSeq = 0
/** 通过 API 登记一台机器（商品取第一个）：登记后处于维护中，且从未心跳 → 离线。 */
export async function registerMachine(ctx: APIRequestContext, admin: SeededAdmin, opts: { rack?: string; pdu_id?: number; pdu_outlet?: number } = {}) {
  ipSeq += 1
  const r = await ctx.get('/admin/api/v1/products')
  if (!r.ok()) throw new Error(`GET products ${r.status()} ${await r.text()}`)
  const products = ((await r.json()) as { items: { id: number }[] }).items
  const serial = nextSerial()
  const body: Record<string, unknown> = { serial, rack: opts.rack ?? 'E2E', slot: String(ipSeq).padStart(3, '0'), lan_ip: `10.96.${Math.floor(ipSeq / 250)}.${(ipSeq % 250) + 1}`, product_id: products[0].id }
  if (opts.pdu_id != null) {
    body.pdu_id = opts.pdu_id
    body.pdu_outlet = opts.pdu_outlet
  }
  const out = await adminPost<{ id: number }>(ctx, admin, '/admin/api/v1/machines', body)
  return { id: out.id, serial }
}
