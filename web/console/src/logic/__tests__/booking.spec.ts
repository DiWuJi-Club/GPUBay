import { describe, expect, it } from 'vitest'
import { cstDay, cstHour, describeDuration, earliestStart, hourOptions, isLegacyImmediate, latestStart, startAtISO, totalDays, totalPrice } from '../booking'

describe('booking time helpers (U-20)', () => {
  // 2026-01-01 00:00 UTC = 北京时间 08:00
  const now = new Date('2026-01-01T00:00:00Z')
  it('最早整点 = now + 2h 向上取整；正好整点不再加', () => {
    expect(earliestStart(now).toISOString()).toBe('2026-01-01T02:00:00.000Z')
    expect(earliestStart(new Date('2026-01-01T00:00:01Z')).toISOString()).toBe('2026-01-01T03:00:00.000Z')
    expect(latestStart(now).toISOString()).toBe('2026-03-02T00:00:00.000Z')
  })
  it('北京时间日期与小时', () => {
    expect(cstDay(now)).toBe('2026-01-01')
    expect(cstHour(now)).toBe(8)
    expect(cstDay(new Date('2026-01-01T17:00:00Z'))).toBe('2026-01-02')
  })
  it('当天可选小时：今天只从最早整点起；60 天那天只到最晚整点', () => {
    expect(hourOptions('2026-01-01', now)).toEqual([10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23])
    expect(hourOptions('2026-01-02', now)).toHaveLength(24)
    expect(hourOptions('2026-03-02', now)).toEqual([0, 1, 2, 3, 4, 5, 6, 7, 8])
    expect(hourOptions('2026-03-03', now)).toEqual([])
    expect(hourOptions('2025-12-31', now)).toEqual([])
  })
  it('RFC3339 带 +08:00', () => {
    expect(startAtISO('2026-01-02', 9)).toBe('2026-01-02T09:00:00+08:00')
  })
  it('租期 × 数量', () => {
    expect(totalDays('1m', 2)).toBe(60)
    expect(totalDays('1w', 3)).toBe(21)
    expect(describeDuration('1m', 2)).toBe('1 月 × 2 = 60 天')
    expect(describeDuration('1d', 1)).toBe('1 天')
    expect(totalPrice(6900, 3, 2)).toBe(41400)
  })
  it('单台即时且未选机组 / 机器走旧接口', () => {
    expect(isLegacyImmediate({ immediate: true, count: 1, quantity: 1, groupId: null, machineId: null, autoRenew: false })).toBe(true)
    expect(isLegacyImmediate({ immediate: true, count: 1, quantity: 1, groupId: null, machineId: null, autoRenew: true })).toBe(false)
    expect(isLegacyImmediate({ immediate: true, count: 2, quantity: 1, groupId: null, machineId: null, autoRenew: false })).toBe(false)
    expect(isLegacyImmediate({ immediate: false, count: 1, quantity: 1, groupId: null, machineId: null, autoRenew: false })).toBe(false)
  })
})
