import { expect, request, type APIRequestContext, type Locator, type Page, type TestInfo } from '@playwright/test'
import { CONSOLE_URL, hooks } from './hooks'
import { uniquePhone } from './phone'

export const isMobile = (info: TestInfo) => info.project.name === 'console-mobile'

export async function seedUser(opts: { verified?: boolean; balance?: number; ledger_rows?: number } = {}) {
  const phone = uniquePhone()
  const u = await hooks.users({ phone, verified: opts.verified ?? true, balance: opts.balance ?? 100000, ledger_rows: opts.ledger_rows })
  return { id: u.id, phone }
}

/**
 * 界面登录。同一手机号 60 秒只能发一次验证码，而假时钟不会自己流逝：
 * 先推进 61 秒，保证本测试里之前（例如 userApi）发过的码不会让这次 rate_limited。
 */
export async function loginConsole(page: Page, phone: string, expectPath = '/market') {
  await hooks.advance(61)
  await page.goto('/login')
  await page.getByTestId('login-phone').fill(phone)
  await page.getByTestId('login-send').click()
  await page.getByTestId('login-code').fill('000000')
  await page.getByTestId('login-submit').click()
  await expect(page).toHaveURL(new RegExp(`${expectPath.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}(\\?|$)`))
}

export type ConsoleTab = 'market' | 'instances' | 'wallet' | 'account'

/** 电脑点侧边菜单，手机点底部标签。 */
export async function nav(page: Page, info: TestInfo, tab: ConsoleTab) {
  await page.getByTestId(isMobile(info) ? `tabbar-${tab}` : `menu-${tab}`).click()
}

/** API 登录（发码 + 登录），用于造数据；之后若要界面登录同一手机号，loginConsole 会先推进 61 秒。 */
export async function userApi(phone: string): Promise<APIRequestContext> {
  const ctx = await request.newContext({ baseURL: CONSOLE_URL })
  await hooks.advance(61)
  const sms = await ctx.post('/api/v1/auth/sms-code', { data: { phone } })
  if (!sms.ok()) throw new Error(`sms-code ${sms.status()} ${await sms.text()}`)
  const login = await ctx.post('/api/v1/auth/login', { data: { phone, code: '000000' } })
  if (!login.ok()) throw new Error(`login ${login.status()} ${await login.text()}`)
  return ctx
}

async function getJSON<T>(api: APIRequestContext, path: string): Promise<T> {
  const r = await api.get(path)
  if (!r.ok()) throw new Error(`GET ${path} ${r.status()} ${await r.text()}`)
  return (await r.json()) as T
}

/** 通过 API 下单（商品固定 spark-1），返回实例 ID；默认 1 天、第一个镜像。 */
export async function placeOrder(api: APIRequestContext, opts: { duration?: '1d' | '1w' | '1m'; imageIndex?: number } = {}): Promise<string> {
  const products = (await getJSON<{ items: { id: number }[] }>(api, '/api/v1/products')).items
  const images = (await getJSON<{ items: { id: number }[] }>(api, '/api/v1/images')).items
  if (!products.length) throw new Error('placeOrder: /api/v1/products 为空')
  const image = images[opts.imageIndex ?? 0]
  if (!image) throw new Error(`placeOrder: 镜像下标 ${opts.imageIndex ?? 0} 超出范围（共 ${images.length} 个）`)
  const r = await api.post('/api/v1/orders', {
    data: { product_id: products[0].id, duration: opts.duration ?? '1d', image_id: image.id },
    headers: { 'Idempotency-Key': crypto.randomUUID() },
  })
  if (r.status() !== 201) throw new Error(`order ${r.status()} ${await r.text()}`)
  return ((await r.json()) as { instance: { id: string } }).instance.id
}

/** ElMessage 渲染为 role="alert"。 */
export async function toast(page: Page, text: string | RegExp) {
  await expect(page.getByRole('alert').filter({ hasText: text }).first()).toBeVisible()
}

export function dialog(page: Page): Locator {
  return page.getByRole('dialog')
}
