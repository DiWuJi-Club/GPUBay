import { describe, expect, it, vi, beforeEach } from 'vitest'

const { get, error } = vi.hoisted(() => ({ get: vi.fn(), error: vi.fn() }))
vi.mock('../../api', () => ({ api: { get } }))
vi.mock('element-plus', () => ({ ElMessage: { error, success: vi.fn(), warning: vi.fn() } }))

import { ApiError } from '@spark/shared'
import { refreshUnresolved, unresolvedAlerts } from '../alerts'

beforeEach(() => {
  get.mockReset()
  error.mockReset()
  unresolvedAlerts.value = 0
})

describe('store/alerts', () => {
  it('取未处理告警的 total（resolved=false&page_size=1）', async () => {
    get.mockResolvedValueOnce({ items: [], total: 7 })
    await refreshUnresolved()
    expect(get).toHaveBeenCalledWith('/alerts', { resolved: 'false', page_size: 1 })
    expect(unresolvedAlerts.value).toBe(7)
  })

  it('失败静默并保留上次的数', async () => {
    get.mockResolvedValueOnce({ items: [], total: 3 })
    await refreshUnresolved()
    get.mockRejectedValueOnce(new ApiError(500, 'internal', '服务器内部错误', 'req-x'))
    await expect(refreshUnresolved()).resolves.toBeUndefined()
    expect(unresolvedAlerts.value).toBe(3)
    expect(error).not.toHaveBeenCalled()
  })

  it('先发后到的旧响应不覆盖新值（定时刷新与「标记已处理」后的刷新同时进行）', async () => {
    let resolveOld!: (v: { items: never[]; total: number }) => void
    get.mockReturnValueOnce(new Promise((r) => (resolveOld = r)))
    get.mockResolvedValueOnce({ items: [], total: 4 })
    const old = refreshUnresolved()
    await refreshUnresolved()
    expect(unresolvedAlerts.value).toBe(4)
    resolveOld({ items: [], total: 5 })
    await old
    expect(unresolvedAlerts.value).toBe(4)
  })
})
