import { reactive } from 'vue'

export const totpState = reactive({ visible: false, message: '', code: '' })
let resolver: ((v: string | null) => void) | null = null
let pending: Promise<string | null> | null = null

/**
 * 全局动态码弹框（单例）：请求层收到 totp_required 时调用；并发请求共用同一次输入。
 * 弹框已打开时再次调用返回同一个 Promise（不重置已输入的内容、不覆盖等待者）。
 */
export function promptTotp(message?: string): Promise<string | null> {
  if (pending) return pending
  totpState.message = message ?? ''
  totpState.code = ''
  totpState.visible = true
  pending = new Promise((resolve) => {
    resolver = resolve
  })
  return pending
}

export function submitTotp() {
  const r = resolver
  resolver = null
  pending = null
  totpState.visible = false
  r?.(totpState.code.replace(/\s+/g, ''))
}

export function cancelTotp() {
  const r = resolver
  resolver = null
  pending = null
  totpState.visible = false
  r?.(null)
}
