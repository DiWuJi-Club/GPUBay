import { useIdempotencyKey, type Duration, type IdempotencyKey } from '@spark/shared'
import { computed } from 'vue'
import { stripEmpty } from './apps'

/** RentDialog 的表单字段（请求体与幂等键都从这里派生）。 */
export interface RentForm {
  groupId: number | null
  count: number
  immediate: boolean
  day: string
  hour: number | null
  duration: Duration
  quantity: number
  imageId: number | null
  machineId: number | null
  autoRenew: boolean
  autoRenewDuration: Duration
  appId: number | null
}

/** 预装应用部分：appId 为 null（不预装 / 应用不在目录里）时什么都不带，请求体与计划 6 一致；变量去掉空值交给服务器补默认。 */
function appPart(appId: number | null, env: Record<string, string>) {
  return appId != null ? { app_id: appId, app_env: stripEmpty(env) } : {}
}

/** POST /orders（单台、立即开始的旧接口）。 */
export function orderBody(productId: number, f: RentForm, appId: number | null, env: Record<string, string>) {
  return { product_id: productId, duration: f.duration, image_id: f.imageId, ...appPart(appId, env) }
}

/** POST /bookings。startAt 为 null（立即开始）时不带 start_at。 */
export function bookingBody(productId: number, f: RentForm, startAt: string | null, appId: number | null, env: Record<string, string>) {
  return {
    product_id: productId, image_id: f.imageId, group_id: f.groupId, machine_id: f.machineId, count: f.count,
    duration: f.duration, quantity: f.quantity, start_at: startAt ?? undefined, auto_renew: f.autoRenew, auto_renew_duration: f.autoRenewDuration,
    ...appPart(appId, env),
  }
}

/** 幂等键的派生参数（序列化为字符串）：变量按提交时的样子（去掉空值）参与。 */
export function rentKeySignature(productId: number, f: RentForm, env: Record<string, string>): string {
  return JSON.stringify([productId, f.groupId, f.count, f.immediate, f.day, f.hour, f.duration, f.quantity, f.imageId, f.machineId, f.autoRenew, f.autoRenewDuration, f.appId, stripEmpty(env)])
}

/**
 * RentDialog 的幂等键：派生参数的值变了才换键。经 computed 比较字符串，变量表单每次输入都会替换对象，
 * 但值没变（例如改了又改回）时 computed 不触发，重试仍复用同一个键。
 */
export function useRentKey(productId: () => number, f: RentForm, env: () => Record<string, string>): IdempotencyKey {
  const sig = computed(() => rentKeySignature(productId(), f, env()))
  return useIdempotencyKey(() => sig.value)
}
