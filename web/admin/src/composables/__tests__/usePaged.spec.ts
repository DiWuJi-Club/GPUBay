import { nextTick } from 'vue'
import { describe, expect, it, vi, beforeEach } from 'vitest'

const { error } = vi.hoisted(() => ({ error: vi.fn() }))
vi.mock('element-plus', () => ({ ElMessage: { error, success: vi.fn(), warning: vi.fn() } }))

import { ApiError, type Paged } from '@spark/shared'
import { usePaged } from '../usePaged'

beforeEach(() => error.mockReset())

/** 手动控制返回时机的 fetcher：calls[i].resolve / reject 决定第 i 次请求的结果。 */
function deferredFetcher<T>() {
  const calls: { page: number; size: number; resolve: (v: Paged<T>) => void; reject: (e: unknown) => void }[] = []
  const fetcher = vi.fn(
    (page: number, size: number) =>
      new Promise<Paged<T>>((resolve, reject) => {
        calls.push({ page, size, resolve, reject })
      }),
  )
  return { fetcher, calls }
}

describe('usePaged', () => {
  it('翻页自动加载，reset 回到第一页', async () => {
    const fetcher = vi.fn(async (page: number, size: number) => ({ items: [`p${page}s${size}`], total: 45 }))
    const list = usePaged(fetcher, 20)
    await list.load()
    expect(list.items.value).toEqual(['p1s20'])
    expect(list.total.value).toBe(45)
    list.page.value = 3
    await nextTick()
    await vi.waitFor(() => expect(fetcher).toHaveBeenLastCalledWith(3, 20))
    list.reset()
    await vi.waitFor(() => expect(list.page.value).toBe(1))
    expect(fetcher).toHaveBeenLastCalledWith(1, 20)
  })

  it('reset 只发一次请求（第一页时直接加载，其他页时靠翻页触发）', async () => {
    const fetcher = vi.fn(async () => ({ items: [], total: 0 }))
    const list = usePaged(fetcher)
    list.reset()
    await vi.waitFor(() => expect(fetcher).toHaveBeenCalledTimes(1))
    list.page.value = 2
    await vi.waitFor(() => expect(fetcher).toHaveBeenCalledTimes(2))
    list.reset()
    await vi.waitFor(() => expect(fetcher).toHaveBeenCalledTimes(3))
    await nextTick()
    await nextTick()
    expect(fetcher).toHaveBeenCalledTimes(3)
    expect(fetcher).toHaveBeenLastCalledWith(1, 20)
  })

  it('请求失败提示原文', async () => {
    const list = usePaged(async () => {
      throw new ApiError(403, 'forbidden', '无权执行该操作')
    })
    await list.load()
    expect(error).toHaveBeenCalledWith('无权执行该操作')
    expect(list.items.value).toEqual([])
  })

  it('forbidden 无论后端原文如何都提示「无权执行该操作」', async () => {
    const list = usePaged(async () => {
      throw new ApiError(403, 'forbidden', 'Forbidden')
    })
    await list.load()
    expect(error).toHaveBeenCalledTimes(1)
    expect(error).toHaveBeenCalledWith('无权执行该操作')
  })

  it('401 / cancelled 静默（会话处理已提示一次），其他错误各提示一次', async () => {
    let err: unknown = new ApiError(401, 'unauthorized', '请先登录')
    const list = usePaged(async () => {
      throw err
    })
    await list.load()
    err = new ApiError(0, 'cancelled', '已取消')
    await list.load()
    expect(error).not.toHaveBeenCalled()
    err = new ApiError(500, 'internal', '服务器内部错误', 'req-1')
    await list.load()
    expect(error).toHaveBeenCalledTimes(1)
    expect(error).toHaveBeenCalledWith('服务器内部错误（请求号 req-1）')
  })

  it('任何一次失败都清空表格并给出 error（含改筛选 / 翻页之后）；total 保留、页码停在请求的那一页；成功后清除 error', async () => {
    let fail = true
    const fetcher = vi.fn(async (page: number) => {
      if (fail) throw new ApiError(500, 'internal', '服务器内部错误', 'req-2')
      return { items: [`p${page}`], total: 30 }
    })
    const list = usePaged(fetcher)
    await list.load()
    expect(list.error.value).toBe('服务器内部错误（请求号 req-2）')
    expect(list.loading.value).toBe(false)
    fail = false
    await list.load()
    expect(list.error.value).toBe('')
    expect(list.items.value).toEqual(['p1'])
    fail = true
    list.page.value = 2
    await vi.waitFor(() => expect(fetcher).toHaveBeenCalledTimes(3))
    await vi.waitFor(() => expect(list.loading.value).toBe(false))
    expect(list.items.value).toEqual([]) // 不保留第 1 页的行
    expect(list.error.value).toBe('服务器内部错误（请求号 req-2）')
    expect(list.total.value).toBe(30)
    expect(list.page.value).toBe(2)
    expect(error).toHaveBeenCalledTimes(2)
    fail = false
    await list.load() // 重试：仍请求第 2 页
    expect(fetcher).toHaveBeenLastCalledWith(2, 20)
    expect(list.items.value).toEqual(['p2'])
    expect(list.error.value).toBe('')
  })

  it('page_size 变化同样自动加载', async () => {
    const fetcher = vi.fn(async (page: number, size: number) => ({ items: [`p${page}s${size}`], total: 100 }))
    const list = usePaged(fetcher)
    await list.load()
    list.size.value = 50
    await vi.waitFor(() => expect(fetcher).toHaveBeenCalledTimes(2))
    expect(fetcher).toHaveBeenLastCalledWith(1, 50)
    await vi.waitFor(() => expect(list.items.value).toEqual(['p1s50']))
  })

  it('先发后到的旧响应不覆盖新结果（快速翻页 / 改筛选）', async () => {
    const { fetcher, calls } = deferredFetcher<string>()
    const list = usePaged(fetcher)
    void list.load()
    list.page.value = 2
    await vi.waitFor(() => expect(calls).toHaveLength(2))
    calls[1].resolve({ items: ['page2'], total: 40 })
    await vi.waitFor(() => expect(list.items.value).toEqual(['page2']))
    expect(list.loading.value).toBe(false)
    calls[0].resolve({ items: ['page1'], total: 40 })
    await nextTick()
    await nextTick()
    expect(list.items.value).toEqual(['page2'])
    // 旧请求失败也不提示
    list.page.value = 3
    await vi.waitFor(() => expect(calls).toHaveLength(3))
    list.page.value = 4
    await vi.waitFor(() => expect(calls).toHaveLength(4))
    calls[3].resolve({ items: ['page4'], total: 80 })
    calls[2].reject(new ApiError(500, 'internal', '旧请求失败'))
    await vi.waitFor(() => expect(list.items.value).toEqual(['page4']))
    await nextTick()
    expect(error).not.toHaveBeenCalled()
  })

  it('loading 在请求期间为 true', async () => {
    const { fetcher, calls } = deferredFetcher<string>()
    const list = usePaged(fetcher)
    const p = list.load()
    expect(list.loading.value).toBe(true)
    calls[0].resolve({ items: [], total: 0 })
    await p
    expect(list.loading.value).toBe(false)
  })
})
