import { ApiError } from './errors'
import { uuid } from './uuid'

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
  /** 后台：弹出动态码输入框；取消返回 null。message 为上一次 invalid_totp / totp_used 的文案。 */
  promptTotp?: (message?: string) => Promise<string | null>
  /** 后台：403 totp_setup_required → 跳登录页的绑定步骤 */
  onTotpSetupRequired?: () => void
  /** 测试注入 */
  fetch?: typeof fetch
  /** 设备标识（X-Device-Id）；测试注入，缺省 defaultDeviceId（localStorage） */
  deviceId?: () => string
}

export interface Http {
  get<T>(path: string, query?: Record<string, unknown>, opt?: HttpOptions): Promise<T>
  post<T>(path: string, body?: unknown, opt?: HttpOptions): Promise<T>
  put<T>(path: string, body?: unknown, opt?: HttpOptions): Promise<T>
  /** DELETE：同样带 JSON 体（缺省 {}），错误处理与 post 相同 */
  del<T>(path: string, body?: unknown, opt?: HttpOptions): Promise<T>
  /** multipart，字段名 file；不手动设 Content-Type，由浏览器生成边界 */
  upload<T>(path: string, file: File): Promise<T>
  /** CSV 导出：JSON 响应按错误处理，否则从 Content-Disposition 取文件名 */
  download(path: string, query?: Record<string, unknown>): Promise<{ blob: Blob; filename: string }>
}

export const REQUEST_TIMEOUT_MS = 30_000
export const TRANSFER_TIMEOUT_MS = 120_000
/** 连续输错（invalid_totp）的上限；totp_used（码正确但已被消耗）不计入，后端也不计失败 */
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

export const DEVICE_KEY = 'sc_device'
const UUID_V4 = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i
/** localStorage 不可用时本页会话内固定的设备标识（刷新后换新，可接受） */
let sessionDeviceId: string | null = null

/**
 * 设备标识（随机 v4 UUID）：存 localStorage.sc_device，首次生成后一直复用；后端用于同设备自刷判定，非 UUID 一律丢弃。
 * 隐私模式等读写失败时退回本页会话内固定的 UUID。
 */
export function defaultDeviceId(): string {
  try {
    const v = localStorage.getItem(DEVICE_KEY)
    if (v && UUID_V4.test(v)) return v
    const id = uuid()
    localStorage.setItem(DEVICE_KEY, id)
    return id
  } catch {
    return (sessionDeviceId ??= uuid())
  }
}

const networkError = (status = 0) => new ApiError(status, 'network', '网络异常，请稍后重试', '')
const cancelledError = () => new ApiError(0, 'cancelled', '已取消', '')

function withTimeout(ms: number, extra?: AbortSignal): AbortSignal {
  const t = AbortSignal.timeout(ms)
  return extra ? AbortSignal.any([t, extra]) : t
}

/** 把非 2xx 响应解析为 ApiError（JSON 错误体 → code / message；网关 HTML、空响应 → network）。sse.ts 的 streamPost 复用。 */
export async function errorOf(resp: Response): Promise<ApiError> {
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
  const deviceId = cfg.deviceId ?? defaultDeviceId
  // 正在进行的二次验证（含首个请求的重发）；并发收到 totp_required 的其他请求等它完成后直接重发
  let stepUp: Promise<unknown> | null = null

  const isTotpRequired = (e: unknown): e is ApiError => e instanceof ApiError && e.status === 403 && e.code === 'totp_required'
  /** 收到 totp_required 后能否走二次验证：动态码随请求体发送，GET / HEAD 不能带请求体，只能等别的请求进行中的二次验证 */
  const canStepUp = (method: string) => !!cfg.promptTotp && (stepUp !== null || (method !== 'GET' && method !== 'HEAD'))

  async function raw<T>(method: string, path: string, body: unknown, opt: HttpOptions): Promise<T> {
    const headers: Record<string, string> = { Accept: 'application/json', 'X-Device-Id': deviceId() }
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
      try {
        return await rawAuthed<T>(method, path, body, opt) // 首个请求成功时后端 5 分钟窗口已打开，不再需要 totp
      } catch (e) {
        // 窗口只在操作成功后打开：首个请求的操作失败（它的码也已被消耗）时窗口没开，重新弹框输入新码
        if (isTotpRequired(e) && canStepUp(method)) return retryWithTotp<T>(method, path, body, opt)
        throw e
      }
    }
    const run = (async () => {
      let message: string | undefined
      let wrong = 0
      for (;;) {
        const code = await cfg.promptTotp!(message)
        if (code === null) throw cancelledError()
        const base = body && typeof body === 'object' ? (body as Record<string, unknown>) : {}
        try {
          return await rawAuthed<T>(method, path, { ...base, totp: code }, opt)
        } catch (e) {
          // totp_used：码正确但已被上一次（失败的）操作消耗，提示等下一个码再输
          if (e instanceof ApiError && (e.code === 'totp_used' || (e.code === 'invalid_totp' && ++wrong < TOTP_MAX_ATTEMPTS))) {
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
      // GET / HEAD 不能由自己发起二次验证：原样抛出 totp_required（而不是伪装成网络错误）；
      // 别的请求的二次验证进行中时可以等它完成后原样（不带体）重发
      if (isTotpRequired(e) && canStepUp(method)) return retryWithTotp<T>(method, path, body, opt)
      throw e
    }
  }

  return {
    get: (path, query, opt) => request(`GET`, path + buildQuery(query), undefined, opt),
    post: (path, body, opt) => request('POST', path, body ?? {}, opt),
    put: (path, body, opt) => request('PUT', path, body ?? {}, opt),
    del: (path, body, opt) => request('DELETE', path, body ?? {}, opt),
    async upload<T>(path: string, file: File): Promise<T> {
      const fd = new FormData()
      fd.append('file', file, file.name)
      let resp: Response
      try {
        resp = await doFetch(cfg.base + path, { method: 'POST', body: fd, headers: { 'X-Device-Id': deviceId() }, credentials: 'same-origin', signal: AbortSignal.timeout(TRANSFER_TIMEOUT_MS) })
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
        resp = await doFetch(cfg.base + path + buildQuery(query), { headers: { 'X-Device-Id': deviceId() }, credentials: 'same-origin', signal: AbortSignal.timeout(TRANSFER_TIMEOUT_MS) })
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
