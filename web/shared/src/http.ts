import { ApiError } from './errors'

export interface HttpOptions {
  idempotencyKey?: string
  signal?: AbortSignal
  /** 401 时不触发 onUnauthorized，仅用于启动时的会话探测 GET /me。 */
  quiet?: boolean
}

export interface HttpConfig {
  /** '/api/v1' 或 '/admin/api/v1' */
  base: string
  /** 401：清会话并跳登录 */
  onUnauthorized: () => void
  /** 后台：弹出动态码输入框；取消返回 null。message 为上一次 invalid_totp 的文案。 */
  promptTotp?: (message?: string) => Promise<string | null>
  /** 后台：403 totp_setup_required → 跳登录页的绑定步骤 */
  onTotpSetupRequired?: () => void
  /** 测试注入 */
  fetch?: typeof fetch
}

export interface Http {
  get<T>(path: string, query?: Record<string, unknown>, opt?: HttpOptions): Promise<T>
  post<T>(path: string, body?: unknown, opt?: HttpOptions): Promise<T>
  put<T>(path: string, body?: unknown, opt?: HttpOptions): Promise<T>
  /** multipart，字段名 file；不手动设 Content-Type，由浏览器生成边界 */
  upload<T>(path: string, file: File): Promise<T>
  /** CSV 导出：JSON 响应按错误处理，否则从 Content-Disposition 取文件名 */
  download(path: string, query?: Record<string, unknown>): Promise<{ blob: Blob; filename: string }>
}

export const REQUEST_TIMEOUT_MS = 30_000
export const TRANSFER_TIMEOUT_MS = 120_000
const TOTP_MAX_ATTEMPTS = 3

/** 查询参数值为 undefined / null / '' 时不拼入（后台列表筛选留空即「不筛选」）。 */
export function buildQuery(query?: Record<string, unknown>): string {
  if (!query) return ''
  const p = new URLSearchParams()
  for (const [k, v] of Object.entries(query)) {
    if (v === undefined || v === null || v === '') continue
    p.set(k, String(v))
  }
  const s = p.toString()
  return s ? `?${s}` : ''
}

export function filenameOf(disposition: string | null, fallback = 'export.csv'): string {
  if (!disposition) return fallback
  const star = /filename\*=(?:UTF-8'')?([^;]+)/i.exec(disposition)
  if (star) {
    try {
      return decodeURIComponent(star[1].trim().replace(/^"|"$/g, ''))
    } catch {
      /* 非法编码，退回普通写法 */
    }
  }
  const plain = /filename="?([^";]+)"?/i.exec(disposition)
  return plain ? plain[1].trim() : fallback
}

const networkError = (status = 0) => new ApiError(status, 'network', '网络异常，请稍后重试', '')
const cancelledError = () => new ApiError(0, 'cancelled', '已取消', '')

function withTimeout(ms: number, extra?: AbortSignal): AbortSignal {
  const t = AbortSignal.timeout(ms)
  return extra ? AbortSignal.any([t, extra]) : t
}

async function errorOf(resp: Response): Promise<ApiError> {
  try {
    const body = (await resp.json()) as { code?: unknown; message?: unknown; request_id?: unknown }
    if (typeof body?.code === 'string' && typeof body.message === 'string') {
      // 整个响应体放进 details：import_invalid 的逐行 errors 等明细由页面从 err.details 读取，不必重放请求
      return new ApiError(resp.status, body.code, body.message, typeof body.request_id === 'string' ? body.request_id : '', body as Record<string, unknown>)
    }
  } catch {
    /* 网关 HTML、空响应 */
  }
  return networkError(resp.status)
}

export function createHttp(cfg: HttpConfig): Http {
  const doFetch: typeof fetch = cfg.fetch ?? ((input, init) => fetch(input, init))
  // 正在进行的二次验证（含首个请求的重发）；并发收到 totp_required 的其他请求等它完成后直接重发
  let stepUp: Promise<unknown> | null = null

  async function raw<T>(method: string, path: string, body: unknown, opt: HttpOptions): Promise<T> {
    const headers: Record<string, string> = { Accept: 'application/json' }
    if (body !== undefined) headers['Content-Type'] = 'application/json'
    if (opt.idempotencyKey) headers['Idempotency-Key'] = opt.idempotencyKey
    let resp: Response
    try {
      resp = await doFetch(cfg.base + path, {
        method,
        headers,
        body: body === undefined ? undefined : JSON.stringify(body),
        credentials: 'same-origin',
        signal: withTimeout(REQUEST_TIMEOUT_MS, opt.signal),
      })
    } catch {
      throw networkError()
    }
    if (resp.ok) {
      try {
        return (await resp.json()) as T
      } catch {
        throw networkError(resp.status)
      }
    }
    throw await errorOf(resp)
  }

  function handleAuth(err: ApiError, opt: HttpOptions) {
    if (err.status === 401 && err.code === 'unauthorized' && !opt.quiet) cfg.onUnauthorized()
    if (err.status === 403 && err.code === 'totp_setup_required') cfg.onTotpSetupRequired?.()
  }

  /** 二次验证路径里的请求（重发 / 等待者重发）：错误同样经 handleAuth，弹框期间会话失效也会被带去登录页。 */
  async function rawAuthed<T>(method: string, path: string, body: unknown, opt: HttpOptions): Promise<T> {
    try {
      return await raw<T>(method, path, body, opt)
    } catch (e) {
      if (e instanceof ApiError) handleAuth(e, opt)
      throw e
    }
  }

  async function retryWithTotp<T>(method: string, path: string, body: unknown, opt: HttpOptions): Promise<T> {
    if (stepUp) {
      try {
        await stepUp
      } catch (e) {
        if (e instanceof ApiError && e.code === 'cancelled') throw e // 首个被取消则一起取消
      }
      return rawAuthed<T>(method, path, body, opt) // 后端 5 分钟窗口已打开，不再需要 totp
    }
    const run = (async () => {
      let message: string | undefined
      for (let attempt = 1; ; attempt++) {
        const code = await cfg.promptTotp!(message)
        if (code === null) throw cancelledError()
        const base = body && typeof body === 'object' ? (body as Record<string, unknown>) : {}
        try {
          return await rawAuthed<T>(method, path, { ...base, totp: code }, opt)
        } catch (e) {
          if (e instanceof ApiError && e.code === 'invalid_totp' && attempt < TOTP_MAX_ATTEMPTS) {
            message = e.message
            continue
          }
          throw e
        }
      }
    })()
    stepUp = run.finally(() => {
      stepUp = null
    })
    stepUp.catch(() => {}) // 等待者各自处理错误，这里只防止未处理的 rejection
    return run
  }

  async function request<T>(method: string, path: string, body: unknown, opt: HttpOptions = {}): Promise<T> {
    try {
      return await raw<T>(method, path, body, opt)
    } catch (e) {
      if (!(e instanceof ApiError)) throw e
      handleAuth(e, opt)
      if (e.status === 403 && e.code === 'totp_required' && cfg.promptTotp) {
        // 动态码随请求体发送，GET / HEAD 不能带请求体：不能由它发起二次验证，原样抛出 totp_required（而不是伪装成网络错误）；
        // 别的请求的二次验证进行中时可以等它完成后原样（不带体）重发
        if ((method === 'GET' || method === 'HEAD') && !stepUp) throw e
        return retryWithTotp<T>(method, path, body, opt)
      }
      throw e
    }
  }

  return {
    get: (path, query, opt) => request(`GET`, path + buildQuery(query), undefined, opt),
    post: (path, body, opt) => request('POST', path, body ?? {}, opt),
    put: (path, body, opt) => request('PUT', path, body ?? {}, opt),
    async upload<T>(path: string, file: File): Promise<T> {
      const fd = new FormData()
      fd.append('file', file, file.name)
      let resp: Response
      try {
        resp = await doFetch(cfg.base + path, { method: 'POST', body: fd, credentials: 'same-origin', signal: AbortSignal.timeout(TRANSFER_TIMEOUT_MS) })
      } catch {
        throw networkError()
      }
      if (resp.ok) return (await resp.json()) as T
      const err = await errorOf(resp)
      handleAuth(err, {})
      throw err
    },
    async download(path, query) {
      let resp: Response
      try {
        resp = await doFetch(cfg.base + path + buildQuery(query), { credentials: 'same-origin', signal: AbortSignal.timeout(TRANSFER_TIMEOUT_MS) })
      } catch {
        throw networkError()
      }
      const ct = resp.headers.get('Content-Type') ?? ''
      if (!resp.ok || ct.includes('application/json')) {
        const err = await errorOf(resp)
        handleAuth(err, {})
        throw err
      }
      return { blob: await resp.blob(), filename: filenameOf(resp.headers.get('Content-Disposition')) }
    },
  }
}
