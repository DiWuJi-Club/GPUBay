import { afterEach, describe, expect, it, vi } from 'vitest'
import { createIdempotencyKey } from '../idempotency'
import { uuid } from '../uuid'

const V4 = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/

describe('uuid', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('有 randomUUID 时直接用它', () => {
    const randomUUID = vi.fn(() => '11111111-2222-4333-8444-555555555555')
    expect(uuid({ randomUUID, getRandomValues: crypto.getRandomValues.bind(crypto) })).toBe('11111111-2222-4333-8444-555555555555')
    expect(randomUUID).toHaveBeenCalledOnce()
  })

  it('没有 randomUUID（非安全上下文）时用 getRandomValues 生成合法 v4，且不重复', () => {
    const insecure = { getRandomValues: crypto.getRandomValues.bind(crypto) }
    const seen = new Set<string>()
    for (let i = 0; i < 1000; i++) {
      const v = uuid(insecure)
      expect(v).toMatch(V4)
      seen.add(v)
    }
    expect(seen.size).toBe(1000)
  })

  it('版本与变体位不受随机字节影响（全 0xff / 全 0x00）', () => {
    const fill = (n: number) => ({ getRandomValues: <T extends ArrayBufferView>(a: T) => ((a as unknown as Uint8Array).fill(n), a) })
    expect(uuid(fill(0xff))).toBe('ffffffff-ffff-4fff-bfff-ffffffffffff')
    expect(uuid(fill(0x00))).toBe('00000000-0000-4000-8000-000000000000')
  })

  it('全局 crypto 没有 randomUUID 时幂等键仍能生成并轮换', () => {
    vi.stubGlobal('crypto', { getRandomValues: globalThis.crypto.getRandomValues.bind(globalThis.crypto) })
    const k = createIdempotencyKey()
    const first = k.current()
    expect(first).toMatch(V4)
    k.rotate()
    expect(k.current()).toMatch(V4)
    expect(k.current()).not.toBe(first)
  })
})
