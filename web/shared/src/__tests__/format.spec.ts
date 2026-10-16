import { describe, expect, it, vi } from 'vitest'
import { fmtDate, fmtDuration, fmtTime, fmtTimeSec, fmtYuan, useCountdown, yuanToCents } from '../format'

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
