import { createHttp } from '@spark/shared'
import { promptTotp } from './store/totp'

// 401 与「需先绑定动态码」的处理由 store/session 注册，避免 api ↔ session 循环引用
const handlers = { unauthorized: () => {}, totpSetupRequired: () => {} }

/** 后台 API 客户端：totp_required 时弹全局动态码框（单例），带 totp 重发原请求。 */
export const api = createHttp({
  base: '/admin/api/v1',
  onUnauthorized: () => handlers.unauthorized(),
  promptTotp,
  onTotpSetupRequired: () => handlers.totpSetupRequired(),
})

export function onUnauthorized(fn: () => void) {
  handlers.unauthorized = fn
}
export function onTotpSetupRequired(fn: () => void) {
  handlers.totpSetupRequired = fn
}
