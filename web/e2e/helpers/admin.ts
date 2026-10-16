import { expect, request, type APIRequestContext, type Page } from '@playwright/test'
import { ADMIN_URL, hooks, type Role } from './hooks'
import { runStamp } from './phone'
import { totp } from './totp'

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
