const fmt = new Intl.DateTimeFormat('en-US', { timeZone: 'Asia/Shanghai', hourCycle: 'h23', year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' })

/** 与 @spark/shared 的 fmtTime 同格式：北京时间 yyyy-MM-dd HH:mm。 */
export function cstTime(iso: string): string {
  const p: Record<string, string> = {}
  for (const x of fmt.formatToParts(new Date(iso))) p[x.type] = x.value
  return `${p.year}-${p.month}-${p.day} ${p.hour}:${p.minute}`
}

const dateFmt = new Intl.DateTimeFormat('en-US', { timeZone: 'Asia/Shanghai', year: 'numeric', month: '2-digit', day: '2-digit' })

/** 北京时间 yyyy-MM-dd（用于后台日期筛选）。 */
export function cstDate(iso: string | Date): string {
  const p: Record<string, string> = {}
  for (const x of dateFmt.formatToParts(new Date(iso))) p[x.type] = x.value
  return `${p.year}-${p.month}-${p.day}`
}

/** 与 @spark/shared 的 fmtDuration 同格式（独立实现）：2 天 3 小时 5 分、不足 1 分钟、0 分钟。 */
export function duration(seconds: number): string {
  const s = Math.max(0, Math.floor(seconds))
  if (s === 0) return '0 分钟'
  if (s < 60) return '不足 1 分钟'
  const parts: string[] = []
  const d = Math.floor(s / 86400)
  const h = Math.floor((s % 86400) / 3600)
  const m = Math.floor((s % 3600) / 60)
  if (d) parts.push(`${d} 天`)
  if (h) parts.push(`${h} 小时`)
  if (m) parts.push(`${m} 分`)
  return parts.join(' ')
}
