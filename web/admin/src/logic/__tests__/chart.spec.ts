import { describe, expect, it } from 'vitest'
import { barOf, barsOf, CHART, clampRate } from '../chart'

const baseline = CHART.height

describe('出租率柱状图几何', () => {
  it.each([
    [0, 0],
    [0.5, 70],
    [1, 140],
    [1.2, 140],
    [-0.3, 0],
    [Number.NaN, 0],
  ])('rate %s → h %s，柱底贴底线（y = 底线 − h）', (rate, h) => {
    const b = barOf(rate, 3)
    expect(b.h).toBe(h)
    expect(b.y).toBe(baseline - h)
    expect(b.y).toBeGreaterThanOrEqual(CHART.top)
    expect(b.y + b.h).toBe(baseline)
    expect(b.x).toBe(64)
  })

  it('30 根柱都在 viewBox 内，pct 用限制后的值', () => {
    const bars = barsOf(Array.from({ length: 30 }, (_, i) => ({ date: `d${i}`, rate: i === 0 ? 1.004 : i === 1 ? -0.2 : 0.123 })))
    expect(bars).toHaveLength(30)
    expect(bars[29].x + CHART.barWidth).toBeLessThanOrEqual(CHART.width)
    expect(bars[0]).toEqual({ x: 4, y: 20, h: 140, date: 'd0', pct: '100.0' })
    expect(bars[1]).toEqual({ x: 24, y: 160, h: 0, date: 'd1', pct: '0.0' })
    expect(bars[2]).toEqual({ x: 44, y: 143, h: 17, date: 'd2', pct: '12.3' })
  })

  it('空数据没有柱', () => {
    expect(barsOf([])).toEqual([])
  })

  it('clampRate', () => {
    expect([clampRate(2), clampRate(-1), clampRate(0.42), clampRate(Infinity)]).toEqual([1, 0, 0.42, 0])
  })
})
