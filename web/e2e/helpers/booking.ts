import { expect, request, type APIRequestContext, type Page } from '@playwright/test'
import { CONSOLE_URL, hooks } from './hooks'
import { runStamp } from './phone'
import { cstDate } from './time'

type Duration = '1d' | '1w' | '1m'

let groupSeq = 0
export function uniqueGroupName(): string {
  groupSeq += 1
  return `G${runStamp}-${groupSeq}`
}

/**
 * 建一个只属于本用例的可见机组并放入 count 台新机器。整轮共享一个机器池：不限机组的预订可能落到更早用例留下、
 * 到点还没洗完的机器上而被改派，所以预订用例都在自己的机组里下单（preflight D21）。
 */
export async function privateGroup(count: number): Promise<{ id: number; name: string; machineIds: number[] }> {
  const name = uniqueGroupName()
  const { id } = await hooks.groups(name)
  const machineIds = await hooks.machines(count, undefined, undefined, id)
  return { id, name, machineIds }
}

const HOUR = 3600_000
/** 服务器时间 + hours 向上取整到整点；返回 RFC3339、北京时间日期与小时。 */
export async function startAfter(hours: number) {
  const now = await hooks.clock()
  const t = Math.ceil((now.getTime() + hours * HOUR) / HOUR) * HOUR
  const date = new Date(t)
  const cst = new Date(t + 8 * HOUR)
  return { iso: date.toISOString(), day: cstDate(date), hour: cst.getUTCHours(), date }
}

/** 把服务器时钟推进到 start 前 1 小时（预订的开通时刻）。 */
export async function advanceToProvision(start: Date) {
  await hooks.advance(Math.ceil((start.getTime() - (await hooks.clock()).getTime()) / 1000) - 3600)
}

/** POST /api/v1/bookings（商品取第一个、镜像取第一个）；startAt 缺省为即时。 */
export async function bookViaApi(
  api: APIRequestContext,
  opts: { startAt?: string; count?: number; groupId?: number; machineId?: number; duration?: Duration; quantity?: number; autoRenew?: boolean; autoRenewDuration?: Duration },
) {
  const products = (await (await api.get('/api/v1/products')).json()).items as { id: number }[]
  const images = (await (await api.get('/api/v1/images')).json()).items as { id: number }[]
  const r = await api.post('/api/v1/bookings', {
    data: {
      product_id: products[0].id, image_id: images[0].id, group_id: opts.groupId, machine_id: opts.machineId, count: opts.count ?? 1,
      duration: opts.duration ?? '1d', quantity: opts.quantity ?? 1, start_at: opts.startAt, auto_renew: opts.autoRenew ?? false, auto_renew_duration: opts.autoRenewDuration,
    },
    headers: { 'Idempotency-Key': crypto.randomUUID() },
  })
  if (r.status() !== 201) throw new Error(`booking ${r.status()} ${await r.text()}`)
  const out = (await r.json()) as { batch: { id: string } | null; instances: { id: string; machine_label: string }[] }
  return { batchId: out.batch?.id ?? null, ids: out.instances.map((i) => i.id), labels: out.instances.map((i) => i.machine_label) }
}

/** 控制台匿名接口：机组内机器 ID（用于指定机器）。 */
export async function groupMachineIds(groupId: number): Promise<number[]> {
  const ctx = await request.newContext({ baseURL: CONSOLE_URL })
  try {
    const r = await ctx.get(`/api/v1/groups/${groupId}/machines`)
    if (!r.ok()) throw new Error(`group machines ${r.status()} ${await r.text()}`)
    return ((await r.json()).items as { id: number }[]).map((m) => m.id)
  } finally {
    await ctx.dispose()
  }
}

/**
 * 用例结束时取消自己留下的未开通预订并释放请求上下文。整轮共享一个假时钟，后面的用例会把时间推过这些预订的开始时刻，
 * 不取消的话它们会在之后的清扫里开通，留下与本用例无关的运行中实例和清扫噪声。
 */
export async function cancelBookingsAndDispose(api: APIRequestContext, ids: string[]) {
  for (const id of ids) {
    const r = await api.post(`/api/v1/instances/${id}/cancel`, { data: {} })
    if (!r.ok()) throw new Error(`cancel ${id} ${r.status()} ${await r.text()}`)
    expect(((await r.json()) as { state: string }).state).toBe('cancelled')
  }
  await api.dispose()
}

/** 整串精确匹配的正则（机组名都以 G<stamp>- 开头，子串 / 前缀匹配会命中多个选项或行）。 */
export function exact(text: string): RegExp {
  return new RegExp(`^${text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}$`)
}

/** Element Plus 下拉：点开后选择文本匹配的选项。 */
export async function pickOption(page: Page, testid: string, text: string | RegExp) {
  await page.getByTestId(testid).click()
  await page.getByRole('option', { name: text }).click()
  await expect(page.getByRole('option', { name: text })).toBeHidden()
}
