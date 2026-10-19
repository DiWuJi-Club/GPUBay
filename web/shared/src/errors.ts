export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly requestId: string
  /** 后端错误响应体的完整对象（例如 import_invalid 的 errors 明细）；网络错误为 {} */
  readonly details: Record<string, unknown>

  constructor(status: number, code: string, message: string, requestId = '', details: Record<string, unknown> = {}) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.requestId = requestId
    this.details = details
  }
}

export function isApiError(e: unknown): e is ApiError {
  return e instanceof ApiError
}

/** 把任意错误变成可直接展示的文案；5xx 追加请求号便于排查。 */
export function messageOf(err: unknown): string {
  if (isApiError(err)) {
    if (err.status >= 500 && err.requestId) return `${err.message}（请求号 ${err.requestId}）`
    return err.message
  }
  return '操作失败，请稍后重试'
}
