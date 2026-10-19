import type { Duration } from '@spark/shared'

export const MAX_COUNT = 8
export const MAX_QUANTITY: Record<Duration, number> = { '1d': 90, '1w': 12, '1m': 3 }
export const DAYS_OF: Record<Duration, number> = { '1d': 1, '1w': 7, '1m': 30 }
const HOUR = 3600_000
const CST_OFFSET = 8 * HOUR
const MIN_LEAD = 2 * HOUR
const MAX_AHEAD = 60 * 24 * HOUR

/** 最早可预订的整点：now + 2h 向上取整到整点（北京时间整点与 UTC 整点重合）。 */
export function earliestStart(now: Date): Date {
  return new Date(Math.ceil((now.getTime() + MIN_LEAD) / HOUR) * HOUR)
}
/** 最晚可预订的整点：now + 60d 向下取整到整点。 */
export function latestStart(now: Date): Date {
  return new Date(Math.floor((now.getTime() + MAX_AHEAD) / HOUR) * HOUR)
}
/** 北京时间日期 YYYY-MM-DD。 */
export function cstDay(t: Date): string {
  return new Date(t.getTime() + CST_OFFSET).toISOString().slice(0, 10)
}
/** 北京时间小时 0～23。 */
export function cstHour(t: Date): number {
  return new Date(t.getTime() + CST_OFFSET).getUTCHours()
}
/** 某一天（北京时间）里可选的整点：在 [earliestStart, latestStart] 内。 */
export function hourOptions(day: string, now: Date): number[] {
  const lo = earliestStart(now).getTime()
  const hi = latestStart(now).getTime()
  const out: number[] = []
  for (let h = 0; h < 24; h++) {
    const t = new Date(startAtISO(day, h)).getTime()
    if (t >= lo && t <= hi) out.push(h)
  }
  return out
}
/** RFC3339（+08:00），后端按整点校验。 */
export function startAtISO(day: string, hour: number): string {
  return `${day}T${String(hour).padStart(2, '0')}:00:00+08:00`
}
export function totalDays(duration: Duration, quantity: number): number {
  return DAYS_OF[duration] * quantity
}
export function describeDuration(duration: Duration, quantity: number): string {
  const label = { '1d': '1 天', '1w': '1 周', '1m': '1 月' }[duration]
  return quantity === 1 ? label : `${label} × ${quantity} = ${totalDays(duration, quantity)} 天`
}
export function totalPrice(unit: number, quantity: number, count: number): number {
  return unit * quantity * count
}

export interface RentForm {
  immediate: boolean
  count: number
  quantity: number
  groupId: number | null
  machineId: number | null
  autoRenew: boolean
}
/** 与计划 5 完全相同的请求（单台、一个单位、立即、不限机组、不开自动续费）走旧接口 POST /orders。 */
export function isLegacyImmediate(f: RentForm): boolean {
  return f.immediate && f.count === 1 && f.quantity === 1 && f.groupId === null && f.machineId === null && !f.autoRenew
}
