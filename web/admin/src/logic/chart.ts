/** 仪表盘近 30 天出租率柱状图的几何：viewBox 600×160，底线 y=160，柱顶最高到 y=20（留 20 给顶部余白），柱宽 14、间距 6。 */
export const CHART = { width: 600, height: 160, top: 20, barWidth: 14, step: 20, left: 4 } as const

export interface Bar {
  x: number
  y: number
  h: number
  date: string
  /** 出租率百分比文字（已限制在 0～100），一位小数 */
  pct: string
}

/** 出租率限制在 [0, 1]：后端四舍五入可能略超 1，异常值不能画到坐标系外。 */
export const clampRate = (rate: number) => (Number.isFinite(rate) ? Math.min(1, Math.max(0, rate)) : 0)

/** 第 i 根柱：高度按限制后的出租率取整，柱底贴着底线（y = 底线 − h）。 */
export function barOf(rate: number, i: number): Omit<Bar, 'date' | 'pct'> {
  const h = Math.round(clampRate(rate) * (CHART.height - CHART.top))
  return { x: i * CHART.step + CHART.left, y: CHART.height - h, h }
}

export function barsOf(utilization: readonly { date: string; rate: number }[]): Bar[] {
  return utilization.map((u, i) => ({ ...barOf(u.rate, i), date: u.date, pct: (clampRate(u.rate) * 100).toFixed(1) }))
}
