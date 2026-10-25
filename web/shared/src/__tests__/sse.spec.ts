// @vitest-environment node
import { describe, expect, it, vi } from 'vitest'
import { ApiError } from '../errors'
import { parseSSE, streamPost, type SSEEvent } from '../sse'

describe('parseSSE', () => {
  it('按空行分帧，event / data 行，缺省事件名 message', () => {
    const { events, rest } = parseSSE('event: start\ndata: {"a":1}\n\ndata: hi\n\n')
    expect(events).toEqual([
      { event: 'start', data: '{"a":1}' },
      { event: 'message', data: 'hi' },
    ])
    expect(rest).toBe('')
  })

  it('半帧留在 rest，拼上后续分块后续接', () => {
    const a = parseSSE('event: delta\ndata: {"te')
    expect(a.events).toEqual([])
    const b = parseSSE(a.rest + 'xt":"你好"}\n\nevent: done\n')
    expect(b.events).toEqual([{ event: 'delta', data: '{"text":"你好"}' }])
    const c = parseSSE(b.rest + 'data: {"reason":"end_turn"}\n\n')
    expect(c.events).toEqual([{ event: 'done', data: '{"reason":"end_turn"}' }])
    expect(c.rest).toBe('')
  })

  it('CRLF 分隔，含被分块切开的 \\r\\n', () => {
    expect(parseSSE('event: a\r\ndata: 1\r\n\r\nevent: b\r\ndata: 2\r\n\r\n').events).toEqual([
      { event: 'a', data: '1' },
      { event: 'b', data: '2' },
    ])
    const a = parseSSE('event: a\r\ndata: 1\r\n\r')
    expect(a.events).toEqual([])
    expect(parseSSE(a.rest + '\n').events).toEqual([{ event: 'a', data: '1' }])
  })

  it('忽略注释行与只有注释的保活帧', () => {
    expect(parseSSE(': ping\n\n: hello\nevent: delta\ndata: x\n\n: ping\n\n').events).toEqual([{ event: 'delta', data: 'x' }])
  })

  it('多行 data 用 \\n 连接；冒号后只去一个空格；未知字段忽略', () => {
    expect(parseSSE('id: 3\nretry: 10\ndata: a\ndata:  b\ndata\n\n').events).toEqual([{ event: 'message', data: 'a\n b\n' }])
  })
})

type Chunk = string | Error

/** 按分块吐出的 SSE 响应；Error 项让流在此处出错（模拟断网）。 */
function sseResponse(chunks: Chunk[], headers: Record<string, string> = { 'Content-Type': 'text/event-stream; charset=utf-8' }) {
  const enc = new TextEncoder()
  let i = 0
  const body = new ReadableStream<Uint8Array>({
    pull(c) {
      if (i >= chunks.length) return c.close()
      const next = chunks[i++]
      if (next instanceof Error) c.error(next)
      else c.enqueue(enc.encode(next))
    },
  })
  return new Response(body, { status: 200, headers })
}

function fakeFetch(resp: Response | (() => Promise<Response>)) {
  const calls: Array<{ url: string; init: RequestInit }> = []
  const fn = vi.fn(async (input: RequestInfo | URL, init: RequestInit = {}) => {
    calls.push({ url: String(input), init })
    return typeof resp === 'function' ? resp() : resp
  })
  return { fn: fn as unknown as typeof fetch, calls }
}

describe('streamPost', () => {
  it('POST JSON，按顺序回调每个事件（跨分块、跳过 ping）', async () => {
    const { fn, calls } = fakeFetch(
      sseResponse([
        'event: start\ndata: {"conversation_id":"cv-1","message_id":1}\n\n: ping\n\nevent: del',
        'ta\ndata: {"text":"你"}\n\nevent: delta\ndata: {"text":"好"}\n',
        '\nevent: done\ndata: {"reason":"end_turn"}\n\n',
      ]),
    )
    const got: SSEEvent[] = []
    await streamPost('/api/v1', '/butler/conversations/new/messages', { text: 'hi' }, (e) => got.push(e), { fetch: fn })
    expect(got.map((e) => e.event)).toEqual(['start', 'delta', 'delta', 'done'])
    expect(got[1].data).toBe('{"text":"你"}')
    expect(calls[0].url).toBe('/api/v1/butler/conversations/new/messages')
    expect(calls[0].init.method).toBe('POST')
    expect(calls[0].init.credentials).toBe('same-origin')
    expect(JSON.parse(String(calls[0].init.body))).toEqual({ text: 'hi' })
    expect((calls[0].init.headers as Record<string, string>)['Content-Type']).toBe('application/json')
  })

  it('UTF-8 多字节字符被切在两个分块之间也能正确解码', async () => {
    const bytes = new TextEncoder().encode('event: delta\ndata: {"text":"管家"}\n\n')
    const cut = new TextEncoder().encode('event: delta\ndata: {"text":"').length + 1 // 切在「管」的 3 个字节中间
    const body = new ReadableStream<Uint8Array>({
      start(c) {
        c.enqueue(bytes.slice(0, cut))
        c.enqueue(bytes.slice(cut))
        c.close()
      },
    })
    const { fn } = fakeFetch(new Response(body, { headers: { 'Content-Type': 'text/event-stream' } }))
    const got: SSEEvent[] = []
    await streamPost('', '/x', {}, (e) => got.push(e), { fetch: fn })
    expect(got).toEqual([{ event: 'delta', data: '{"text":"管家"}' }])
  })

  it('非 2xx 的 JSON 错误体 → ApiError 带 code / status', async () => {
    const { fn } = fakeFetch(
      new Response(JSON.stringify({ code: 'butler_quota_exceeded', message: '今日 AI 管家额度已用完，明天再来', request_id: 'r1' }), {
        status: 429,
        headers: { 'Content-Type': 'application/json' },
      }),
    )
    const err = await streamPost('', '/x', {}, () => {}, { fetch: fn }).catch((e) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect(err).toMatchObject({ status: 429, code: 'butler_quota_exceeded', requestId: 'r1' })
  })

  it('2xx 但不是 text/event-stream → ApiError（不当作事件流读）', async () => {
    const onEvent = vi.fn()
    const { fn } = fakeFetch(new Response('<html>gateway</html>', { status: 200, headers: { 'Content-Type': 'text/html' } }))
    const err = await streamPost('', '/x', {}, onEvent, { fetch: fn }).catch((e) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect(err.code).toBe('network')
    expect(onEvent).not.toHaveBeenCalled()
  })

  it('fetch 失败 → network', async () => {
    const fn = vi.fn(async () => {
      throw new TypeError('Failed to fetch')
    }) as unknown as typeof fetch
    await expect(streamPost('', '/x', {}, () => {}, { fetch: fn })).rejects.toMatchObject({ status: 0, code: 'network' })
  })

  it('读到一半断开 → 已收事件已回调，然后抛 network', async () => {
    const { fn } = fakeFetch(sseResponse(['event: delta\ndata: {"text":"a"}\n\nevent: delta\ndata: {"te', new TypeError('terminated')]))
    const got: SSEEvent[] = []
    const err = await streamPost('', '/x', {}, (e) => got.push(e), { fetch: fn }).catch((e) => e)
    expect(got).toEqual([{ event: 'delta', data: '{"text":"a"}' }])
    expect(err).toBeInstanceOf(ApiError)
    expect(err).toMatchObject({ status: 0, code: 'network' })
  })

  it('signal 取消 → cancelled', async () => {
    const ac = new AbortController()
    const body = new ReadableStream<Uint8Array>({
      start(c) {
        c.enqueue(new TextEncoder().encode('event: delta\ndata: {"text":"a"}\n\n'))
        ac.signal.addEventListener('abort', () => c.error(new DOMException('aborted', 'AbortError')))
      },
    })
    const { fn } = fakeFetch(new Response(body, { headers: { 'Content-Type': 'text/event-stream' } }))
    const p = streamPost('', '/x', {}, () => ac.abort(), { fetch: fn, signal: ac.signal })
    await expect(p).rejects.toMatchObject({ status: 0, code: 'cancelled' })
  })

  it('signal 取消而流本身不响应 abort：主动 cancel 读取 → cancelled', async () => {
    const ac = new AbortController()
    const body = new ReadableStream<Uint8Array>({
      start(c) {
        c.enqueue(new TextEncoder().encode('event: delta\ndata: {"text":"a"}\n\n'))
      }, // 之后永远不再吐数据、也不关闭
    })
    const { fn } = fakeFetch(new Response(body, { headers: { 'Content-Type': 'text/event-stream' } }))
    const got: SSEEvent[] = []
    const p = streamPost('', '/x', {}, (e) => got.push(e), { fetch: fn, signal: ac.signal })
    setTimeout(() => ac.abort(), 20)
    await expect(p).rejects.toMatchObject({ status: 0, code: 'cancelled' })
    expect(got).toHaveLength(1)
  })

  it('空闲看门狗：超过 idleTimeoutMs 没有任何数据 → network；每个分块（含 ping）都会重置计时', async () => {
    // 假时钟：ping 间隔与看门狗都按虚拟时间走，负载下的计时器延迟不会让看门狗在 ping 期间误触发
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'Date'] })
    try {
      const enc = new TextEncoder()
      let pings = 0
      const body = new ReadableStream<Uint8Array>({
        start(c) {
          c.enqueue(enc.encode('event: delta\ndata: {"text":"a"}\n\n'))
          const t = setInterval(() => {
            if (pings++ < 4) c.enqueue(enc.encode(': ping\n\n')) // 每 20ms 一次 ping（20/40/60/80ms），之后沉默
            else clearInterval(t)
          }, 20)
        },
      })
      const { fn } = fakeFetch(new Response(body, { headers: { 'Content-Type': 'text/event-stream' } }))
      const got: SSEEvent[] = []
      let settled = false
      const p = streamPost('', '/x', {}, (e) => got.push(e), { fetch: fn, idleTimeoutMs: 50 })
        .catch((e) => e)
        .finally(() => (settled = true))
      await vi.advanceTimersByTimeAsync(80 + 50 - 1)
      expect(settled).toBe(false) // ping 期间没有误触发：最后一个 ping（80ms）之后再等满 50ms 才超时
      await vi.advanceTimersByTimeAsync(1)
      const err = await p
      expect(settled).toBe(true)
      expect(err).toBeInstanceOf(ApiError)
      expect(err).toMatchObject({ status: 0, code: 'network' })
      expect(got).toEqual([{ event: 'delta', data: '{"text":"a"}' }])
    } finally {
      vi.useRealTimers()
    }
  })

  it('流正常关闭但没收到 done → 正常 resolve（由调用方判断是否完整）', async () => {
    const { fn } = fakeFetch(sseResponse(['event: delta\ndata: {"text":"a"}\n\n']))
    const got: SSEEvent[] = []
    await expect(streamPost('', '/x', {}, (e) => got.push(e), { fetch: fn })).resolves.toBeUndefined()
    expect(got.map((e) => e.event)).toEqual(['delta'])
  })

  it('请求前已取消（fetch 抛 AbortError）→ cancelled', async () => {
    const ac = new AbortController()
    ac.abort()
    const fn = vi.fn(async () => {
      throw new DOMException('aborted', 'AbortError')
    }) as unknown as typeof fetch
    await expect(streamPost('', '/x', {}, () => {}, { fetch: fn, signal: ac.signal })).rejects.toMatchObject({ code: 'cancelled' })
  })

  it('onEvent 抛出的异常原样上抛', async () => {
    const { fn } = fakeFetch(sseResponse(['event: delta\ndata: x\n\n']))
    const boom = new Error('boom')
    await expect(
      streamPost('', '/x', {}, () => {
        throw boom
      }, { fetch: fn }),
    ).rejects.toBe(boom)
  })
})
