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
