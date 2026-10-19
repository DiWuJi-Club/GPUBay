/** 与 @spark/shared 的 fmtYuan 同一格式（独立实现，避免测试进程加载 vue）。 */
export function yuan(cents: number): string {
  const abs = Math.abs(Math.trunc(cents))
  const whole = String(Math.floor(abs / 100)).replace(/\B(?=(\d{3})+(?!\d))/g, ',')
  return `${cents < 0 ? '-' : ''}¥${whole}.${String(abs % 100).padStart(2, '0')}`
}
