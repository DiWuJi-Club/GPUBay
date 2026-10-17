import { describe, expect, it } from 'vitest'
import { alipayLink, isMockQR, mmss, validateAmount } from '../recharge'

describe('recharge logic', () => {
  it('支付宝拉起链接按 encodeURIComponent 编码二维码内容', () => {
    expect(alipayLink('mock://pay/rch-1')).toBe('alipays://platformapi/startapp?saId=10000007&qrcode=mock%3A%2F%2Fpay%2Frch-1')
    expect(alipayLink('https://qr.alipay.com/bax?x=1&y=2')).toContain('qrcode=https%3A%2F%2Fqr.alipay.com%2Fbax%3Fx%3D1%26y%3D2')
  })
  it('mock 判断与倒计时格式', () => {
    expect(isMockQR('mock://pay/rch-1')).toBe(true)
    expect(isMockQR('https://qr.alipay.com/x')).toBe(false)
    expect(mmss(900)).toBe('15:00')
    expect(mmss(59)).toBe('00:59')
    expect(mmss(0)).toBe('00:00')
  })
  it('金额校验：格式与范围', () => {
    expect(validateAmount('88.5')).toEqual({ cents: 8850 })
    expect(validateAmount('1.234')).toEqual({ error: '金额格式不正确' })
    expect(validateAmount('abc')).toEqual({ error: '金额格式不正确' })
    expect(validateAmount('0.5')).toEqual({ error: '充值金额需在 1 元到 100 万元之间' })
    expect(validateAmount('1000001')).toEqual({ error: '充值金额需在 1 元到 100 万元之间' })
    expect(validateAmount('1000000')).toEqual({ cents: 100000000 })
    expect(validateAmount('1')).toEqual({ cents: 100 })
  })
})
