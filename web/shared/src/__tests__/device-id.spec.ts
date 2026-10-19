import { afterEach, describe, expect, it, vi } from 'vitest'
import { createHttp, DEVICE_KEY, defaultDeviceId } from '../http'

const V4 = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/

describe('defaultDeviceId (计划 8)', () => {
  afterEach(() => {
    localStorage.clear()
    vi.restoreAllMocks()
  })

  it('首次生成 v4 UUID 写入 localStorage.sc_device，之后一直复用', () => {
    expect(DEVICE_KEY).toBe('sc_device')
    const a = defaultDeviceId()
    expect(a).toMatch(V4)
    expect(localStorage.getItem('sc_device')).toBe(a)
    expect(defaultDeviceId()).toBe(a)
  })

  it('存储里的值不是 UUID（被改坏）时重新生成并覆盖', () => {
    localStorage.setItem('sc_device', 'garbage')
    const a = defaultDeviceId()
    expect(a).toMatch(V4)
    expect(localStorage.getItem('sc_device')).toBe(a)
  })

  it('localStorage 不可用（隐私模式）时退回本页会话内固定的 UUID', () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('SecurityError')
    })
    const a = defaultDeviceId()
    expect(a).toMatch(V4)
    expect(defaultDeviceId()).toBe(a)
  })

  it('写入失败（配额为 0）时同样用会话内固定的 UUID', () => {
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('QuotaExceededError')
    })
    const a = defaultDeviceId()
    expect(a).toMatch(V4)
    expect(defaultDeviceId()).toBe(a)
  })

  it('createHttp 缺省用 localStorage 里的设备标识', async () => {
    const calls: RequestInit[] = []
    const fetch = vi.fn(async (_: RequestInfo | URL, init: RequestInit = {}) => {
      calls.push(init)
      return new Response('{}', { status: 200, headers: { 'Content-Type': 'application/json' } })
    }) as unknown as typeof globalThis.fetch
    const http = createHttp({ base: '/api/v1', onUnauthorized: vi.fn(), fetch })
    await http.get('/me')
    await http.get('/me')
    const id = localStorage.getItem('sc_device')
    expect(id).toMatch(V4)
    for (const c of calls) expect((c.headers as Record<string, string>)['X-Device-Id']).toBe(id)
  })
})
