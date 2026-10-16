import { createHttp } from '@spark/shared'

const handlers = { unauthorized: () => {} }

/** 控制台 API 客户端。401 的处理由 store/session 注册，避免循环引用。 */
export const api = createHttp({ base: '/api/v1', onUnauthorized: () => handlers.unauthorized() })

export function onUnauthorized(fn: () => void) {
  handlers.unauthorized = fn
}
