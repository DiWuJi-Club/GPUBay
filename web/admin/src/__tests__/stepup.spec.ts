import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from '../api'
import { cancelTotp, submitTotp, totpState } from '../store/totp'

// 后台 api 与全局动态码弹框的接线：totp_required → 弹一次框 → 带 totp 重发原请求；并发请求共用这一次输入。
type Call = { url: string; body: Record<string, unknown> }
let calls: Call[] = []
let stepUpOpen = false

const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })

beforeEach(() => {
  calls = []
  stepUpOpen = false
  // 假后端：没带正确 totp 且 5 分钟窗口未打开时一律 403 totp_required；带 123456 打开窗口，带其他码 400 invalid_totp
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init: RequestInit) => {
      const body = JSON.parse(String(init.body ?? '{}')) as Record<string, unknown>
      calls.push({ url, body })
      if (body.totp === '123456') stepUpOpen = true
      else if (body.totp !== undefined) return json(400, { code: 'invalid_totp', message: '动态码错误或已使用过', request_id: 'r' })
      if (!stepUpOpen) return json(403, { code: 'totp_required', message: '该操作需要输入动态码', request_id: 'r' })
      return json(200, { ok: true, echo: body.name })
    }),
  )
})
afterEach(() => {
  cancelTotp()
  vi.unstubAllGlobals()
})

const flush = () => new Promise((r) => setTimeout(r, 0))

describe('动态码二次验证', () => {
  it('并发两个请求都收到 totp_required：只弹一次框，输入后各重发一次，只有第一个带 totp', async () => {
    const a = api.post<{ echo: string }>('/admins', { name: 'a' })
    const b = api.post<{ echo: string }>('/admins/2', { name: 'b' })
    await flush()
    expect(totpState.visible).toBe(true)
    expect(calls).toHaveLength(2)
    totpState.code = '123 456'
    submitTotp()
    expect(await a).toEqual({ ok: true, echo: 'a' })
    expect(await b).toEqual({ ok: true, echo: 'b' })
    expect(calls.map((c) => [c.url, c.body.totp])).toEqual([
      ['/admin/api/v1/admins', undefined],
      ['/admin/api/v1/admins/2', undefined],
      ['/admin/api/v1/admins', '123456'],
      ['/admin/api/v1/admins/2', undefined],
    ])
    expect(calls[2].body.name).toBe('a') // 原请求体保留
    expect(totpState.visible).toBe(false)
  })

  it('取消：两个请求都以 cancelled 失败，不再重发、不再弹框', async () => {
    const a = api.post('/admins', { name: 'a' })
    const b = api.post('/admins/2', { name: 'b' })
    await flush()
    expect(totpState.visible).toBe(true)
    cancelTotp()
    await expect(a).rejects.toMatchObject({ code: 'cancelled' })
    await expect(b).rejects.toMatchObject({ code: 'cancelled' })
    await flush()
    expect(calls).toHaveLength(2)
    expect(totpState.visible).toBe(false)
  })

  it('动态码错误：弹框带错误文案再次出现，改对后成功', async () => {
    const a = api.post<{ echo: string }>('/admins', { name: 'a' })
    await flush()
    totpState.code = '000000'
    submitTotp()
    await vi.waitFor(() => expect(totpState.message).toBe('动态码错误或已使用过'))
    expect(totpState.visible).toBe(true)
    expect(totpState.code).toBe('')
    totpState.code = '123456'
    submitTotp()
    expect(await a).toEqual({ ok: true, echo: 'a' })
    expect(calls.map((c) => c.body.totp)).toEqual([undefined, '000000', '123456'])
  })
})
