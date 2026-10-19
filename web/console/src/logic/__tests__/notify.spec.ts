import { ApiError } from '@spark/shared'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const { error } = vi.hoisted(() => ({ error: vi.fn() }))
vi.mock('element-plus', () => ({ ElMessage: { error, warning: vi.fn(), success: vi.fn() } }))

import { notifyError } from '../notify'

describe('notifyError', () => {
  beforeEach(() => error.mockClear())

  it('unauthorized 与 cancelled 静默', () => {
    notifyError(new ApiError(401, 'unauthorized', '请先登录'))
    notifyError(new ApiError(0, 'cancelled', '已取消'))
    expect(error).not.toHaveBeenCalled()
  })

  it('其余错误按 messageOf 提示（5xx 带请求号，非 ApiError 用通用文案）', () => {
    notifyError(new ApiError(409, 'invalid_state', '当前状态不允许该操作'))
    notifyError(new ApiError(500, 'internal', '服务器开小差了', 'req-1'))
    notifyError(new Error('boom'))
    expect(error.mock.calls).toEqual([['当前状态不允许该操作'], ['服务器开小差了（请求号 req-1）'], ['操作失败，请稍后重试']])
  })
})
