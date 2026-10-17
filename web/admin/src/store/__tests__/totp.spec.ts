import { describe, expect, it } from 'vitest'
import { cancelTotp, promptTotp, submitTotp, totpState } from '../totp'

describe('promptTotp 单例', () => {
  it('提交返回去空白的动态码；取消返回 null', async () => {
    const p = promptTotp()
    expect(totpState.visible).toBe(true)
    totpState.code = '123 456'
    submitTotp()
    expect(await p).toBe('123456')
    expect(totpState.visible).toBe(false)
    const q = promptTotp('动态码错误或已使用过')
    expect(totpState.message).toBe('动态码错误或已使用过')
    cancelTotp()
    expect(await q).toBeNull()
  })
})
