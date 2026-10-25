import { ApiError } from './errors'
import { defaultDeviceId, errorOf } from './http'

/** 一个 SSE 帧：事件名（缺省 message）与 data 原文（多行 data 用 \n 连接）。 */
export interface SSEEvent {
  event: string
  data: string
}

/**
 * 把累积的流文本切成完整帧，返回已完成的事件与剩余的半帧。
 * 帧以空行分隔（\n\n 或 \r\n\r\n，兼容单独的 \r）；以 : 开头的注释行（保活 ping）忽略；
 * 没有 data 行的帧不派发（与浏览器 EventSource 一致）。
 */
export function parseSSE(buffer: string): { events: SSEEvent[]; rest: string } {
  // 统一换行；末尾单独的 \r 可能是被分块切开的 \r\n，留到下一次再处理
  const text = buffer.replace(/\r\n/g, '\n').replace(/\r(?!$)/g, '\n')
  const events: SSEEvent[] = []
  let start = 0
  for (;;) {
    const end = text.indexOf('\n\n', start)
    if (end < 0) break
    const ev = parseFrame(text.slice(start, end))
    if (ev) events.push(ev)
    start = end + 2
  }
  return { events, rest: text.slice(start) }
}

function parseFrame(frame: string): SSEEvent | null {
  let event = 'message'
  const data: string[] = []
  for (const line of frame.split('\n')) {
    if (line === '' || line.startsWith(':')) continue
    const i = line.indexOf(':')
    const field = i < 0 ? line : line.slice(0, i)
    let value = i < 0 ? '' : line.slice(i + 1)
    if (value.startsWith(' ')) value = value.slice(1)
    if (field === 'event') event = value || 'message'
    else if (field === 'data') data.push(value)
    // id / retry 等字段不需要
  }
  return data.length ? { event, data: data.join('\n') } : null
}

const networkError = (status = 0) => new ApiError(status, 'network', '网络异常，请稍后重试', '')
const cancelledError = () => new ApiError(0, 'cancelled', '已取消', '')

/** 空闲看门狗：服务端每 15 秒发一次 : ping，超过这么久一个字节都没收到就当连接已死。 */
export const SSE_IDLE_TIMEOUT_MS = 45_000

/**
 * POST JSON 并按 SSE 读取响应，每个完整帧回调一次 onEvent。
 * - 非 2xx 或非 text/event-stream 响应 → 按 errorOf 抛 ApiError（服务端在建立流之前以 JSON 返回限额 / busy / 校验错误）；
 *   不处理 401（不调用 onUnauthorized），调用方自行处理；
 * - 网络中断，或 idleTimeoutMs（缺省 45 秒）内没收到任何数据 → ApiError(0, 'network')；
 * - signal 取消 → ApiError(0, 'cancelled')；onEvent 自身抛出的异常原样上抛；
 * - 流正常关闭即 resolve，即使没收到 done / error 事件——是否完整结束由调用方按收到的事件判断。
 * 不设整体超时：一轮最长约 120 秒，由服务端控制。
 */
export async function streamPost(
  base: string,
  path: string,
  body: unknown,
  onEvent: (e: SSEEvent) => void,
  opt: { signal?: AbortSignal; fetch?: typeof fetch; idleTimeoutMs?: number } = {},
): Promise<void> {
  const doFetch: typeof fetch = opt.fetch ?? ((input, init) => fetch(input, init))
  const signal = opt.signal
  const failed = () => (signal?.aborted ? cancelledError() : networkError())
  let resp: Response
  try {
    resp = await doFetch(base + path, {
      method: 'POST',
      headers: { Accept: 'text/event-stream', 'Content-Type': 'application/json', 'X-Device-Id': defaultDeviceId() },
      body: JSON.stringify(body ?? {}),
      credentials: 'same-origin',
      signal,
    })
  } catch {
    throw failed()
  }
  const ct = resp.headers.get('Content-Type') ?? ''
  if (!resp.ok || !ct.includes('text/event-stream')) throw await errorOf(resp)
  if (!resp.body) throw networkError(resp.status)

  const reader = resp.body.getReader()
  const decoder = new TextDecoder()
  const idleMs = opt.idleTimeoutMs ?? SSE_IDLE_TIMEOUT_MS
  let idle = false
  let timer: ReturnType<typeof setTimeout> | undefined
  const cancel = () => {
    reader.cancel().catch(() => {})
  }
  const arm = () => {
    clearTimeout(timer)
    timer = setTimeout(() => {
      idle = true
      cancel()
    }, idleMs)
  }
  // 取消时主动 cancel 读取：fetch 的 signal 不一定能中断已建立的流（测试注入的 fetch、部分实现）
  const onAbort = () => cancel()
  signal?.addEventListener('abort', onAbort)
  let buf = ''
  try {
    if (signal?.aborted) throw cancelledError()
    arm()
    for (;;) {
      let chunk: ReadableStreamReadResult<Uint8Array>
      try {
        chunk = await reader.read()
      } catch {
        throw idle ? networkError() : failed()
      }
      // reader.cancel() 让挂起的 read 以 done 结束：区分看门狗 / 取消与服务端正常关闭
      if (signal?.aborted) throw cancelledError()
      if (idle) throw networkError()
      if (chunk.done) break
      arm()
      buf += decoder.decode(chunk.value, { stream: true })
      const { events, rest } = parseSSE(buf)
      buf = rest
      for (const e of events) onEvent(e)
    }
    // 流结束时未以空行收尾的残帧按 SSE 规范丢弃（服务端每帧都以 \n\n 结束）
  } finally {
    clearTimeout(timer)
    signal?.removeEventListener('abort', onAbort)
    cancel()
  }
}
