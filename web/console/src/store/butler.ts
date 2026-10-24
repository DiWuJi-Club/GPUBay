import { reactive } from 'vue'

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
