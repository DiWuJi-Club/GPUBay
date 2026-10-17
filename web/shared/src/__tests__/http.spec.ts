// @vitest-environment node
import { describe, expect, it, vi } from 'vitest'
import { ApiError } from '../errors'
import { buildQuery, createHttp, filenameOf } from '../http'

type Call = { url: string; init: RequestInit; body: unknown }

function json(status: number, body: unknown, headers: Record<string, string> = {}) {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json', ...headers } })
}

/** 记录每次调用，按顺序返回预置响应（函数形式可根据请求决定响应）。 */
function fakeFetch(...responses: Array<Response | ((c: Call) => Response)>) {
  const calls: Call[] = []
  const fn = vi.fn(async (input: RequestInfo | URL, init: RequestInit = {}) => {
    const body = typeof init.body === 'string' ? JSON.parse(init.body) : init.body
    const call = { url: String(input), init, body }
    calls.push(call)
    const next = responses.shift()
    if (!next) throw new Error(`unexpected fetch #${calls.length} ${call.url}`)
    return typeof next === 'function' ? next(call) : next
  })
  return { fn: fn as unknown as typeof fetch, calls }
}

describe('buildQuery', () => {
  it('空值剔除、其余编码', () => {
    expect(buildQuery({ state: '', user_id: undefined, machine_id: null, page: 2, q: '张 三' })).toBe('?page=2&q=%E5%BC%A0+%E4%B8%89')
    expect(buildQuery({})).toBe('')
    expect(buildQuery()).toBe('')
  })
})

describe('http 基本行为 (U-06)', () => {
  it('GET 带查询参数、同源 Cookie、JSON 响应', async () => {
    const f = fakeFetch(json(200, { items: [1] }))
    const http = createHttp({ base: '/api/v1', onUnauthorized: vi.fn(), fetch: f.fn })
    const out = await http.get<{ items: number[] }>('/orders', { status: '', page: 1 })
    expect(out.items).toEqual([1])
    expect(f.calls[0].url).toBe('/api/v1/orders?page=1')
    expect(f.calls[0].init.credentials).toBe('same-origin')
    expect(f.calls[0].init.method).toBe('GET')
  })

  it('POST 带 JSON 头与 Idempotency-Key', async () => {
    const f = fakeFetch(json(201, { ok: true }))
    const http = createHttp({ base: '/api/v1', onUnauthorized: vi.fn(), fetch: f.fn })
    await http.post('/orders', { product_id: 1 }, { idempotencyKey: 'k-1' })
    const h = f.calls[0].init.headers as Record<string, string>
    expect(h['Content-Type']).toBe('application/json')
    expect(h['Idempotency-Key']).toBe('k-1')
    expect(f.calls[0].body).toEqual({ product_id: 1 })
  })

  it('错误体解析为 ApiError，并保留完整响应体 details', async () => {
    const f = fakeFetch(
      json(402, { code: 'insufficient_balance', message: '余额不足，请先充值', request_id: 'r1' }),
      json(400, { code: 'import_invalid', message: '导入数据有误，未导入任何机器', request_id: 'r2', errors: [{ line: 2, message: '内网 IP 格式不正确' }] }),
    )
    const http = createHttp({ base: '/api/v1', onUnauthorized: vi.fn(), fetch: f.fn })
    const err = (await http.post('/orders', {}).catch((e) => e)) as ApiError
    expect(err).toBeInstanceOf(ApiError)
    expect(err).toMatchObject({ status: 402, code: 'insufficient_balance', message: '余额不足，请先充值', requestId: 'r1' })
    expect(err.details).toEqual({ code: 'insufficient_balance', message: '余额不足，请先充值', request_id: 'r1' })
    const imp = (await http.post('/machines/import', { csv: 'x' }).catch((e) => e)) as ApiError
    expect(imp).toMatchObject({ status: 400, code: 'import_invalid' })
    expect(imp.details.errors).toEqual([{ line: 2, message: '内网 IP 格式不正确' }])
  })

  it('非 JSON 响应与网络异常映射为 network', async () => {
    const html = new Response('<html>502 Bad Gateway</html>', { status: 502, headers: { 'Content-Type': 'text/html' } })
    const f = fakeFetch(html, () => {
      throw new TypeError('Failed to fetch')
    })
    const http = createHttp({ base: '/api/v1', onUnauthorized: vi.fn(), fetch: f.fn })
    const e1 = await http.get('/wallet').catch((e) => e)
    expect(e1).toMatchObject({ code: 'network', status: 502, message: '网络异常，请稍后重试' })
    const e2 = await http.get('/wallet').catch((e) => e)
    expect(e2).toMatchObject({ code: 'network', status: 0 })
  })

  it('401 调 onUnauthorized 并抛出；quiet 不调', async () => {
    const onUnauthorized = vi.fn()
    const f = fakeFetch(json(401, { code: 'unauthorized', message: '请先登录' }), json(401, { code: 'unauthorized', message: '请先登录' }))
    const http = createHttp({ base: '/api/v1', onUnauthorized, fetch: f.fn })
    await expect(http.get('/me')).rejects.toMatchObject({ code: 'unauthorized' })
    expect(onUnauthorized).toHaveBeenCalledTimes(1)
    await expect(http.get('/me', undefined, { quiet: true })).rejects.toMatchObject({ code: 'unauthorized' })
    expect(onUnauthorized).toHaveBeenCalledTimes(1)
  })

  it('403 totp_setup_required 调 onTotpSetupRequired', async () => {
    const onTotpSetupRequired = vi.fn()
    const f = fakeFetch(json(403, { code: 'totp_setup_required', message: '请先绑定动态码' }))
    const http = createHttp({ base: '/admin/api/v1', onUnauthorized: vi.fn(), onTotpSetupRequired, fetch: f.fn })
    await expect(http.get('/me')).rejects.toMatchObject({ code: 'totp_setup_required' })
    expect(onTotpSetupRequired).toHaveBeenCalledTimes(1)
  })

  it('upload 用 multipart 字段 file，不手动设 Content-Type', async () => {
    const f = fakeFetch(json(201, { key: 'k' }))
    const http = createHttp({ base: '/api/v1', onUnauthorized: vi.fn(), fetch: f.fn })
    const file = new File([new Uint8Array([1, 2, 3])], 'license.png', { type: 'image/png' })
    await http.upload('/uploads', file)
    const init = f.calls[0].init
    expect(init.body).toBeInstanceOf(FormData)
    expect((init.body as FormData).get('file')).toBeInstanceOf(File)
    expect((init.headers as Record<string, string> | undefined)?.['Content-Type']).toBeUndefined()
  })
})

const totpRequired = () => json(403, { code: 'totp_required', message: '该操作需要输入动态码' })
const invalidTotp = () => json(400, { code: 'invalid_totp', message: '动态码错误或已使用过' })

describe('http 动态码重发 (U-07)', () => {
  it('totp_required → promptTotp → 重发时请求体含 totp', async () => {
    const promptTotp = vi.fn(async () => '123456')
    const f = fakeFetch(totpRequired(), json(200, { balance: 1 }))
    const http = createHttp({ base: '/admin/api/v1', onUnauthorized: vi.fn(), promptTotp, fetch: f.fn })
    const out = await http.post('/users/1/adjust', { amount: 100, remark: 'x' })
    expect(out).toEqual({ balance: 1 })
    expect(promptTotp).toHaveBeenCalledTimes(1)
    expect(f.calls[1].body).toEqual({ amount: 100, remark: 'x', totp: '123456' })
  })

  it('并发两个请求只弹一次；后到的请求在首个验证完成后直接重发', async () => {
    let stepped = false
    const serve = (c: Call) => {
      const body = c.body as { totp?: string } | undefined
      if (body?.totp === '123456') stepped = true
      if (!stepped && !body?.totp) return totpRequired()
      return json(200, { ok: c.url })
    }
    const f = fakeFetch(serve, serve, serve, serve)
    const promptTotp = vi.fn(() => new Promise<string | null>((resolve) => setTimeout(() => resolve('123456'), 10)))
    const http = createHttp({ base: '/admin/api/v1', onUnauthorized: vi.fn(), promptTotp, fetch: f.fn })
    const [a, b] = await Promise.all([http.post('/users/1/adjust', { amount: 1 }), http.post('/users/2/adjust', { amount: 2 })])
    expect(a).toEqual({ ok: '/admin/api/v1/users/1/adjust' })
    expect(b).toEqual({ ok: '/admin/api/v1/users/2/adjust' })
    expect(promptTotp).toHaveBeenCalledTimes(1)
    expect(f.calls).toHaveLength(4)
  })

  it('取消抛 cancelled，不再发请求', async () => {
    const f = fakeFetch(totpRequired())
    const http = createHttp({ base: '/admin/api/v1', onUnauthorized: vi.fn(), promptTotp: async () => null, fetch: f.fn })
    await expect(http.post('/admins', { username: 'x' })).rejects.toMatchObject({ code: 'cancelled' })
    expect(f.calls).toHaveLength(1)
  })

  it('invalid_totp 最多重试 3 次，之后抛出 invalid_totp', async () => {
    const promptTotp = vi.fn(async (message?: string) => (message ? 'again' : 'first'))
    const f = fakeFetch(totpRequired(), invalidTotp(), invalidTotp(), invalidTotp())
    const http = createHttp({ base: '/admin/api/v1', onUnauthorized: vi.fn(), promptTotp, fetch: f.fn })
    await expect(http.put('/products/1/prices/1d', { amount: 1 })).rejects.toMatchObject({ code: 'invalid_totp' })
    expect(promptTotp).toHaveBeenCalledTimes(3)
    expect(promptTotp.mock.calls[1][0]).toBe('动态码错误或已使用过')
    expect(f.calls).toHaveLength(4)
  })

  it('重发得到 401：调 onUnauthorized（弹框期间会话失效也要带去登录页）', async () => {
    const onUnauthorized = vi.fn()
    const f = fakeFetch(totpRequired(), json(401, { code: 'unauthorized', message: '请先登录' }))
    const http = createHttp({ base: '/admin/api/v1', onUnauthorized, promptTotp: async () => '123456', fetch: f.fn })
    await expect(http.post('/users/1/adjust', { amount: 1 })).rejects.toMatchObject({ code: 'unauthorized' })
    expect(onUnauthorized).toHaveBeenCalledTimes(1)
  })

  it('等待者重发得到 401：同样调 onUnauthorized；quiet 不调', async () => {
    const onUnauthorized = vi.fn()
    const seen = new Map<string, number>()
    const serve = (c: Call) => {
      const n = (seen.get(c.url) ?? 0) + 1
      seen.set(c.url, n)
      if ((c.body as { totp?: string } | undefined)?.totp) return json(200, { ok: true }) // 首个请求带码重发成功
      if (n === 1) return totpRequired()
      return json(401, { code: 'unauthorized', message: '请先登录' }) // 等待者重发时会话已失效
    }
    const promptTotp = vi.fn(() => new Promise<string | null>((resolve) => setTimeout(() => resolve('123456'), 10)))
    const http = createHttp({ base: '/admin/api/v1', onUnauthorized, promptTotp, fetch: fakeFetch(...Array.from({ length: 8 }, () => serve)).fn })
    const [a, b] = await Promise.allSettled([http.post('/users/1/adjust', { amount: 1 }), http.post('/users/2/adjust', { amount: 2 })])
    expect(a).toMatchObject({ status: 'fulfilled' })
    expect(b).toMatchObject({ status: 'rejected', reason: { code: 'unauthorized' } })
    expect(promptTotp).toHaveBeenCalledTimes(1)
    expect(onUnauthorized).toHaveBeenCalledTimes(1)

    seen.clear()
    const [c, d] = await Promise.allSettled([http.post('/users/1/adjust', { amount: 1 }), http.post('/users/2/adjust', { amount: 2 }, { quiet: true })])
    expect(c).toMatchObject({ status: 'fulfilled' })
    expect(d).toMatchObject({ status: 'rejected', reason: { code: 'unauthorized' } })
    expect(onUnauthorized).toHaveBeenCalledTimes(1)
  })

  it('GET 收到 totp_required 原样抛出，不弹框、不带请求体重发', async () => {
    const promptTotp = vi.fn(async () => '123456')
    const f = fakeFetch(totpRequired())
    const http = createHttp({ base: '/admin/api/v1', onUnauthorized: vi.fn(), promptTotp, fetch: f.fn })
    await expect(http.get('/secret')).rejects.toMatchObject({ code: 'totp_required', status: 403 })
    expect(promptTotp).not.toHaveBeenCalled()
    expect(f.calls).toHaveLength(1)
  })

  it('GET 在别的请求二次验证进行中收到 totp_required：等其完成后不带请求体原样重发', async () => {
    let stepped = false
    const serve = (c: Call) => {
      if ((c.body as { totp?: string } | undefined)?.totp === '123456') stepped = true
      if (!stepped && !(c.body as { totp?: string } | undefined)?.totp) return totpRequired()
      return json(200, { ok: c.url })
    }
    let release!: (v: string) => void
    const promptTotp = vi.fn(() => new Promise<string | null>((resolve) => (release = resolve)))
    const f = fakeFetch(serve, serve, serve, serve)
    const http = createHttp({ base: '/admin/api/v1', onUnauthorized: vi.fn(), promptTotp, fetch: f.fn })
    const post = http.post('/me/password', { password: 'x' })
    await vi.waitFor(() => expect(promptTotp).toHaveBeenCalledTimes(1))
    const get = http.get('/alerts')
    await vi.waitFor(() => expect(f.calls).toHaveLength(2))
    release('123456')
    await expect(post).resolves.toEqual({ ok: '/admin/api/v1/me/password' })
    await expect(get).resolves.toEqual({ ok: '/admin/api/v1/alerts' })
    expect(promptTotp).toHaveBeenCalledTimes(1)
    const resend = f.calls.filter((c) => c.url.endsWith('/alerts'))
    expect(resend).toHaveLength(2)
    expect(resend.map((c) => c.init.body)).toEqual([undefined, undefined])
  })

  it('没有配置 promptTotp（控制台）时 totp_required 原样抛出', async () => {
    const f = fakeFetch(totpRequired())
    const http = createHttp({ base: '/api/v1', onUnauthorized: vi.fn(), fetch: f.fn })
    await expect(http.post('/x', {})).rejects.toMatchObject({ code: 'totp_required' })
  })
})

describe('download (U-14)', () => {
  it('CSV 响应取文件名并返回 Blob', async () => {
    const csv = new Response('﻿订单号,用户ID\n', { status: 200, headers: { 'Content-Type': 'text/csv; charset=utf-8', 'Content-Disposition': 'attachment; filename="orders-20261017-140501.csv"' } })
    const f = fakeFetch(csv)
    const http = createHttp({ base: '/admin/api/v1', onUnauthorized: vi.fn(), fetch: f.fn })
    const out = await http.download('/orders/export', { kind: 'new', status: '' })
    expect(f.calls[0].url).toBe('/admin/api/v1/orders/export?kind=new')
    expect(out.filename).toBe('orders-20261017-140501.csv')
    expect(await out.blob.text()).toContain('订单号')
  })

  it('JSON 响应按错误抛出（例如 export_too_large）', async () => {
    const f = fakeFetch(json(400, { code: 'export_too_large', message: '导出数据超过 10 万行，请缩小筛选范围' }))
    const http = createHttp({ base: '/admin/api/v1', onUnauthorized: vi.fn(), fetch: f.fn })
    await expect(http.download('/ledger/export')).rejects.toMatchObject({ code: 'export_too_large' })
  })

  it('filenameOf 支持两种写法并有兜底', () => {
    expect(filenameOf('attachment; filename="a.csv"')).toBe('a.csv')
    expect(filenameOf('attachment; filename=b.csv')).toBe('b.csv')
    expect(filenameOf("attachment; filename*=UTF-8''%E6%B5%81%E6%B0%B4.csv")).toBe('流水.csv')
    expect(filenameOf(null)).toBe('export.csv')
    expect(filenameOf('inline')).toBe('export.csv')
  })
})
