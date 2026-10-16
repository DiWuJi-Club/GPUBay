import { describe, expect, it } from 'vitest'
import { isValidPhone, maskPhone, normalizeCode, normalizePhone } from '../login'

describe('login logic (Review Focus 1)', () => {
  it('手机号去空白、连字符、全角', () => {
    expect(normalizePhone('138 0000 1234')).toBe('13800001234')
    expect(normalizePhone('138-0000-1234')).toBe('13800001234')
    expect(normalizePhone('１３８００００１２３４')).toBe('13800001234')
    expect(isValidPhone(' 139 0000 0000 ')).toBe(true)
    expect(isValidPhone('12800000000')).toBe(false)
    expect(isValidPhone('1380000123')).toBe(false)
  })
  it('验证码只留数字、最多 6 位', () => {
    expect(normalizeCode('000 000')).toBe('000000')
    expect(normalizeCode('０００００0')).toBe('000000')
    expect(normalizeCode('12-34-56-78')).toBe('123456')
    expect(normalizeCode('abc')).toBe('')
  })
  it('打码', () => {
    expect(maskPhone('13800001234')).toBe('138****1234')
    expect(maskPhone('')).toBe('')
  })
})
