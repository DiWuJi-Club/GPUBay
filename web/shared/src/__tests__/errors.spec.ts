import { describe, expect, it } from 'vitest'
import { ApiError, isApiError, messageOf } from '../errors'

describe('messageOf (U-10)', () => {
  it('业务错误显示 message；5xx 追加请求号', () => {
    expect(messageOf(new ApiError(402, 'insufficient_balance', '余额不足，请先充值', 'req-1'))).toBe('余额不足，请先充值')
    expect(messageOf(new ApiError(500, 'internal', '服务器内部错误，请稍后重试', 'abc123'))).toBe('服务器内部错误，请稍后重试（请求号 abc123）')
    expect(messageOf(new ApiError(503, 'kyc_unavailable', '实名核验服务暂时不可用，请稍后再试', ''))).toBe('实名核验服务暂时不可用，请稍后再试')
  })
  it('网络错误与未知错误', () => {
    expect(messageOf(new ApiError(0, 'network', '网络异常，请稍后重试', ''))).toBe('网络异常，请稍后重试')
    expect(messageOf(new TypeError('boom'))).toBe('操作失败，请稍后重试')
    expect(messageOf(undefined)).toBe('操作失败，请稍后重试')
  })
  it('isApiError', () => {
    expect(isApiError(new ApiError(404, 'not_found', '资源不存在'))).toBe(true)
    expect(isApiError(new Error('x'))).toBe(false)
  })
})
