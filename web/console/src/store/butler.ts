import { isApiError } from '@spark/shared'
import { computed, reactive } from 'vue'
import { api } from '../api'

/**
 * AI 管家抽屉状态（全站共享）：切换路由不关抽屉、不丢当前对话与草稿；关闭抽屉也不清状态。
 * pendingContext：从实例详情页「问问管家」带来的上下文，随下一条消息发送后由 consumeContext 取走。
 */
export const butler = reactive({
  open: false,
  conversationId: null as string | null,
  pendingContext: null as { instance_id: string } | null,
  draft: '',
})

/** 打开抽屉；instanceId → 下一条消息带上实例上下文（不传则清掉旧的上下文）；draft → 预填输入框（不传则保留用户已输入的草稿）。 */
export function openButler(opts: { instanceId?: string; draft?: string } = {}) {
  butler.pendingContext = opts.instanceId ? { instance_id: opts.instanceId } : null
  if (opts.draft !== undefined) butler.draft = opts.draft
  butler.open = true
}

export function closeButler() {
  butler.open = false
}

/** 取走待发送的实例上下文（只随一条消息发送一次）。 */
export function consumeContext(): { instance_id: string } | null {
  const ctx = butler.pendingContext
  butler.pendingContext = null
  return ctx
}

/** 退出登录 / 换账号：清空全部状态（对话属于上一个用户）。 */
export function resetButler() {
  butler.open = false
  butler.conversationId = null
  butler.pendingContext = null
  butler.draft = ''
}

/**
 * 平台是否开启了 AI 管家（SPARK_BUTLER=off → 控制台不出现任何入口，规格 §13）。
 * 探测 GET /butler/status 一次：200 → 可用（管理员在后台关闭时 enabled=false，入口照常显示，聊天区显示公告横幅）；
 * 401 → 可用（路由已注册、只是未登录：requireUser 组里未注册的路径直接 404，不会先走鉴权，所以匿名访客也能区分）；
 * 404 → 平台未启用。其它错误（网络 / 5xx）保持 unknown，下次导航再探测。unknown 期间入口隐藏，避免关闭时闪一下。
 */
export type ButlerAvailability = 'unknown' | 'available' | 'unavailable'
export const butlerAvail = reactive<{ state: ButlerAvailability }>({ state: 'unknown' })
export const butlerAvailable = computed(() => butlerAvail.state === 'available')

let availProbe: Promise<void> | null = null

export function probeButler(): Promise<void> {
  if (butlerAvail.state !== 'unknown') return Promise.resolve()
  availProbe ??= api
    .get<unknown>('/butler/status', undefined, { quiet: true })
    .then(
      () => {
        butlerAvail.state = 'available'
      },
      (e: unknown) => {
        if (isApiError(e) && e.status === 401) butlerAvail.state = 'available'
        else if (isApiError(e) && e.status === 404) butlerAvail.state = 'unavailable'
      },
    )
    .finally(() => {
      availProbe = null
    })
  return availProbe
}
