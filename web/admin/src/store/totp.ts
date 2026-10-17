import { reactive } from 'vue'

export const totpState = reactive({ visible: false, message: '', code: '' })
let resolver: ((v: string | null) => void) | null = null

/** 全局动态码弹框（单例）：请求层收到 totp_required 时调用；并发请求共用同一次输入。 */
export function promptTotp(message?: string): Promise<string | null> {
  totpState.message = message ?? ''
  totpState.code = ''
  totpState.visible = true
  return new Promise((resolve) => {
    resolver = resolve
  })
}

export function submitTotp() {
  const r = resolver
  resolver = null
  totpState.visible = false
  r?.(totpState.code.replace(/\s+/g, ''))
}

export function cancelTotp() {
  const r = resolver
  resolver = null
  totpState.visible = false
  r?.(null)
}
