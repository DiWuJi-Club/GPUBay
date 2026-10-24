import { describe, expect, it, vi } from 'vitest'
import {
  centsToYuanInput, digitsOnly, fmtDate, formatMicroFen, formatPricePerM, formatTokens, fmtDuration, fmtTime, fmtTimeSec, fmtYuan, RECHARGE_MAX_CENTS, RECHARGE_MIN_CENTS, toHalfWidthDigits, useCountdown, validateYuanRange, yuanToCents,
} from '../format'

describe('fmtYuan (U-01)', () => {
  it.each([
    [0, '¥0.00'],
    [6900, '¥69.00'],
    [159900, '¥1,599.00'],
    [-1000, '-¥10.00'],
    [1, '¥0.01'],
    [123456789, '¥1,234,567.89'],
  ])('%d -> %s', (cents, want) => {
    expect(fmtYuan(cents)).toBe(want)
  })
})

describe('centsToYuanInput', () => {
  it.each([
    [0, '0.00'],
    [1, '0.01'],
    [10, '0.10'],
    [29, '0.29'],
    [105, '1.05'],
    [6900, '69.00'],
    [123456789, '1234567.89'],
    [-435, '-4.35'],
  ])('%i 分 → %s（整数运算，无千分位，可直接放进输入框）', (cents, out) => {
    expect(centsToYuanInput(cents)).toBe(out)
  })
  it('与 yuanToCents 往返一致', () => {
    for (const c of [0, 1, 29, 435, 8850, 100_000_000, Number.MAX_SAFE_INTEGER]) expect(yuanToCents(centsToYuanInput(c))).toBe(c)
  })
})

describe('yuanToCents (U-02)', () => {
  it.each([
    ['88.5', 8850],
    ['1.234', null],
    ['-1', null],
    ['abc', null],
    ['1000000', 100000000],
    ['0', 0],
    ['69', 6900],
    ['0.01', 1],
  ])('%s -> %s', (input, want) => {
    expect(yuanToCents(input)).toBe(want)
  })

  it('边缘写法：首尾空白可接受，其余一律拒绝（Review Focus 3）', () => {
    expect(yuanToCents(' 88.50 ')).toBe(8850)
    expect(yuanToCents('100.')).toBeNull()
    expect(yuanToCents('.5')).toBeNull()
    expect(yuanToCents('1e3')).toBeNull()
    expect(yuanToCents('１００')).toBeNull()
    expect(yuanToCents('')).toBeNull()
    expect(yuanToCents('1,000')).toBeNull()
  })
})

describe('fmtTime / fmtDate 北京时间 (U-03)', () => {
  it('跨日边界：UTC 17:30 是北京时间次日 01:30', () => {
    expect(fmtTime('2026-10-17T17:30:00Z')).toBe('2026-10-18 01:30')
    expect(fmtTimeSec('2026-10-17T17:30:05Z')).toBe('2026-10-18 01:30:05')
    expect(fmtDate('2026-10-17T17:30:00Z')).toBe('2026-10-18')
  })
  it('午夜显示 00 而不是 24', () => {
    expect(fmtTime('2026-10-17T16:00:00Z')).toBe('2026-10-18 00:00')
  })
  it('空值与非法值返回空串', () => {
    expect(fmtTime(null)).toBe('')
    expect(fmtTime(undefined)).toBe('')
    expect(fmtTime('not-a-date')).toBe('')
  })
})

describe('fmtDuration (U-04)', () => {
  it.each([
    [0, '0 分钟'],
    [59, '不足 1 分钟'],
    [3600, '1 小时'],
    [86400 + 3661, '1 天 1 小时 1 分'],
    [6 * 86400 + 23 * 3600 + 59 * 60 + 30, '6 天 23 小时 59 分'],
  ])('%d -> %s', (seconds, want) => {
    expect(fmtDuration(seconds)).toBe(want)
  })
})

describe('useCountdown (U-05)', () => {
  it('以注入时间源递减并在归零触发一次回调', () => {
    vi.useFakeTimers()
    let t = 0
    const onZero = vi.fn()
    const cd = useCountdown({ now: () => t, onZero })
    cd.start(3)
    expect(cd.remaining.value).toBe(3)
    t = 1000
    vi.advanceTimersByTime(1000)
    expect(cd.remaining.value).toBe(2)
    t = 3000
    vi.advanceTimersByTime(1000)
    expect(cd.remaining.value).toBe(0)
    expect(onZero).toHaveBeenCalledTimes(1)
    t = 10000
    vi.advanceTimersByTime(5000)
    expect(onZero).toHaveBeenCalledTimes(1)
    vi.useRealTimers()
  })

  it('按时间源而不是 tick 数递减：标签页被节流后一次跳到正确值（Review Focus 4）', () => {
    vi.useFakeTimers()
    let t = 0
    const cd = useCountdown({ now: () => t })
    cd.start(600)
    t = 120_000 // 两分钟内浏览器只给了一次 tick
    vi.advanceTimersByTime(1000)
    expect(cd.remaining.value).toBe(480)
    cd.stop()
    vi.useRealTimers()
  })

  it('重新 start 会重置基准', () => {
    vi.useFakeTimers()
    let t = 0
    const cd = useCountdown({ now: () => t })
    cd.start(10)
    t = 5000
    vi.advanceTimersByTime(1000)
    expect(cd.remaining.value).toBe(5)
    cd.start(100)
    expect(cd.remaining.value).toBe(100)
    t = 6000
    vi.advanceTimersByTime(1000)
    expect(cd.remaining.value).toBe(99)
    cd.stop()
    vi.useRealTimers()
  })
})

describe('validateYuanRange', () => {
  const msgs = { range: '需在 1 元到 100 万元之间' }
  it('范围内返回分（按字符串换算，无浮点误差）', () => {
    expect(validateYuanRange('0.29', 1, 100, msgs)).toEqual({ cents: 29 })
    expect(validateYuanRange('1', RECHARGE_MIN_CENTS, RECHARGE_MAX_CENTS, msgs)).toEqual({ cents: 100 })
    expect(validateYuanRange('1000000', RECHARGE_MIN_CENTS, RECHARGE_MAX_CENTS, msgs)).toEqual({ cents: 100_000_000 })
  })
  it('格式不正确', () => {
    for (const s of ['', '-1', '1.234', '1e3', '１００', 'abc']) expect(validateYuanRange(s, 1, 100, msgs)).toEqual({ error: '金额格式不正确' })
  })
  it('上下限：range 兜底，min / max 优先', () => {
    expect(validateYuanRange('0.99', RECHARGE_MIN_CENTS, RECHARGE_MAX_CENTS, msgs)).toEqual({ error: msgs.range })
    expect(validateYuanRange('1000000.01', RECHARGE_MIN_CENTS, RECHARGE_MAX_CENTS, msgs)).toEqual({ error: msgs.range })
    expect(validateYuanRange('0', 1, 100, { min: '太小', max: '太大' })).toEqual({ error: '太小' })
    expect(validateYuanRange('1.01', 1, 100, { min: '太小', max: '太大' })).toEqual({ error: '太大' })
  })
  it('充值范围常量与后端一致：1 元～100 万元', () => {
    expect([RECHARGE_MIN_CENTS, RECHARGE_MAX_CENTS]).toEqual([100, 100_000_000])
  })
})

describe('toHalfWidthDigits / digitsOnly', () => {
  it('全角数字转半角，其余字符不动', () => {
    expect(toHalfWidthDigits('１３８ａ0')).toBe('138ａ0')
  })
  it('digitsOnly：全角转半角后只留数字', () => {
    expect(digitsOnly('１３８ ００００ 1234')).toBe('13800001234')
    expect(digitsOnly('12ab34')).toBe('1234')
    expect(digitsOnly('abc')).toBe('')
    expect(digitsOnly('０')).toBe('0')
  })
})

describe('Token 商城格式化（计划 9）', () => {
  it.each([
    [0, '¥0.00'],
    [50, '¥0.50'],
    [200, '¥2.00'],
    [123456, '¥1,234.56'],
  ])('formatPricePerM(%i 分 / 百万 token) → %s', (fen, want) => {
    expect(formatPricePerM(fen)).toBe(want)
  })

  it.each([
    [0, '¥0.00'],
    [1, '< ¥0.01'],
    [12500, '< ¥0.01'],
    [999_999, '< ¥0.01'],
    [1_000_000, '¥0.01'],
    [1_250_000, '¥0.0125'],
    [123_456_789, '¥1.234568'],
    [100_000_000_000, '¥1,000.00'],
  ])('formatMicroFen(%i 微分) → %s', (micro, want) => {
    expect(formatMicroFen(micro)).toBe(want)
  })

  it.each([
    [0, '0'],
    [999, '999'],
    [1000, '1K'],
    [1500, '1.5K'],
    [12_345, '12.3K'],
    [999_960, '1M'],
    [2_500_000, '2.5M'],
    [3_000_000_000, '3B'],
  ])('formatTokens(%i) → %s', (n, want) => {
    expect(formatTokens(n)).toBe(want)
  })
})
