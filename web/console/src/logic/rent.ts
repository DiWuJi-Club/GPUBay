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
  /** 计划 11：预装工作流（只在预装 ComfyUI 时非空，RentDialog 换应用时清空） */
  appWorkflowIds: number[]
  /** 计划 13：租用方式。0 / 缺省 = 单机；2 / 4 = 集群台数（只即时开通、不指定机器、不预装应用） */
  clusterSize?: number
}

/**
 * 预装应用部分：appId 为 null（不预装 / 应用不在目录里）时什么都不带，请求体与计划 6 一致；变量去掉空值交给服务器补默认。
 * 预装工作流（计划 11）只随应用发送，空列表不带 app_workflow_ids（后端：有 app_workflow_ids 无 app_id → 400）。
 */
function appPart(appId: number | null, env: Record<string, string>, workflowIds: number[]) {
  if (appId == null) return {}
  return { app_id: appId, app_env: stripEmpty(env), ...(workflowIds.length ? { app_workflow_ids: [...workflowIds] } : {}) }
}

/** POST /orders（单台、立即开始的旧接口）。 */
export function orderBody(productId: number, f: RentForm, appId: number | null, env: Record<string, string>) {
  return { product_id: productId, duration: f.duration, image_id: f.imageId, ...appPart(appId, env, f.appWorkflowIds) }
}

/** POST /bookings。startAt 为 null（立即开始）时不带 start_at。集群：count = 台数、cluster: true，不带开始时间 / 指定机器 / 预装应用。 */
export function bookingBody(productId: number, f: RentForm, startAt: string | null, appId: number | null, env: Record<string, string>) {
  if (f.clusterSize) {
    return {
      product_id: productId, image_id: f.imageId, group_id: f.groupId, machine_id: null, count: f.clusterSize,
      duration: f.duration, quantity: f.quantity, auto_renew: f.autoRenew, auto_renew_duration: f.autoRenewDuration, cluster: true,
    }
  }
  return {
    product_id: productId, image_id: f.imageId, group_id: f.groupId, machine_id: f.machineId, count: f.count,
    duration: f.duration, quantity: f.quantity, start_at: startAt ?? undefined, auto_renew: f.autoRenew, auto_renew_duration: f.autoRenewDuration,
    ...appPart(appId, env, f.appWorkflowIds),
  }
}

/** 幂等键的派生参数（序列化为字符串）：变量按提交时的样子（去掉空值）参与；预装工作流按选择顺序参与（与请求体一致）。 */
export function rentKeySignature(productId: number, f: RentForm, env: Record<string, string>): string {
  return JSON.stringify([productId, f.groupId, f.count, f.immediate, f.day, f.hour, f.duration, f.quantity, f.imageId, f.machineId, f.autoRenew, f.autoRenewDuration, f.appId, stripEmpty(env), f.appWorkflowIds, f.clusterSize ?? 0])
}

/**
 * RentDialog 的幂等键：派生参数的值变了才换键。经 computed 比较字符串，变量表单每次输入都会替换对象，
 * 但值没变（例如改了又改回）时 computed 不触发，重试仍复用同一个键。
 */
export function useRentKey(productId: () => number, f: RentForm, env: () => Record<string, string>): IdempotencyKey {
  const sig = computed(() => rentKeySignature(productId(), f, env()))
  return useIdempotencyKey(() => sig.value)
}
