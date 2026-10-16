import { nextTick, ref } from 'vue'
import { describe, expect, it } from 'vitest'
import { createIdempotencyKey, useIdempotencyKey } from '../idempotency'

describe('idempotency key (U-08)', () => {
  it('重试复用同一个键，成功后 rotate 生成新键', () => {
    const k = createIdempotencyKey()
    const first = k.current()
    expect(first).toMatch(/^[0-9a-f-]{36}$/)
    expect(k.current()).toBe(first)
    k.rotate()
    expect(k.current()).not.toBe(first)
  })

  it('参数变化时自动换键', async () => {
    const duration = ref('1d')
    const imageId = ref(1)
    const k = useIdempotencyKey(() => [duration.value, imageId.value])
    const first = k.current()
    duration.value = '1w'
    await nextTick()
    const second = k.current()
    expect(second).not.toBe(first)
    imageId.value = 2
    await nextTick()
    expect(k.current()).not.toBe(second)
    await nextTick()
    expect(k.current()).toBe(k.current())
  })
})
