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

  it('弹框已打开时再次调用返回同一个 Promise，不清空已输入内容、不覆盖等待者', async () => {
    const p1 = promptTotp()
    totpState.code = '12'
    const p2 = promptTotp('动态码错误或已使用过')
    expect(p2).toBe(p1)
    expect(totpState.code).toBe('12')
    expect(totpState.message).toBe('')
    totpState.code = '654321'
    submitTotp()
    expect(await p1).toBe('654321')
    expect(await p2).toBe('654321')
    const p3 = promptTotp() // 结束后可以重新打开
    expect(p3).not.toBe(p1)
    cancelTotp()
    expect(await p3).toBeNull()
  })
})
