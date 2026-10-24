import { getCurrentScope, onScopeDispose, ref, type Ref } from 'vue'

/** 分 → ¥x,xxx.xx；负数在 ¥ 前加负号。 */
export function fmtYuan(cents: number): string {
  const abs = Math.abs(Math.trunc(cents))
  const yuan = String(Math.floor(abs / 100)).replace(/\B(?=(\d{3})+(?!\d))/g, ',')
  const fen = String(abs % 100).padStart(2, '0')
  return `${cents < 0 ? '-' : ''}¥${yuan}.${fen}`
}

/** 分 → 输入框用的元字符串（`1234.50`，无 ¥、无千分位）；按整数的十进制数字串补零后插入小数点，不做任何除法。与 yuanToCents 互逆。 */
export function centsToYuanInput(cents: number): string {
  const digits = String(Math.abs(Math.trunc(cents))).padStart(3, '0')
  return `${cents < 0 ? '-' : ''}${digits.slice(0, -2)}.${digits.slice(-2)}`
}

const yuanRe = /^(\d+)(?:\.(\d{1,2}))?$/

/** 元（字符串）→ 分；非法（负数、超过两位小数、科学计数、全角、空）返回 null。 */
export function yuanToCents(input: string): number | null {
  const m = yuanRe.exec(input.trim())
  if (!m) return null
  const yuan = Number(m[1])
  if (!Number.isSafeInteger(yuan * 100)) return null
  return yuan * 100 + Number((m[2] ?? '').padEnd(2, '0'))
}

// ---- 计划 9：Token 商城（价格「分 / 百万 token」，费用 cost_micro「微分」，1 分 = 10^6 微分）----

const thousands = (digits: string) => digits.replace(/\B(?=(\d{3})+(?!\d))/g, ',')

/** 单价（分 / 百万 token）→ 「¥0.50」（元，两位小数，千分位）。 */
export function formatPricePerM(fen: number): string {
  return fmtYuan(fen)
}

/**
 * 费用（微分）→ 元：0 显示「¥0.00」；不足 1 分显示「< ¥0.01」；其余最多 6 位小数（四舍五入到 10^-6 元 = 100 微分），
 * 去掉末尾的 0 但至少保留 2 位：1_250_000 → 「¥0.0125」、1_000_000 → 「¥0.01」。全程整数运算。
 */
export function formatMicroFen(micro: number): string {
  const sign = micro < 0 ? '-' : ''
  const abs = Math.abs(Math.trunc(micro))
  if (abs === 0) return '¥0.00'
  if (abs < 1_000_000) return '< ¥0.01' // 不足 1 分不带符号（「-< ¥0.01」没有意义）
  const units = String(Math.round(abs / 100)).padStart(7, '0') // 10^-6 元
  const frac = units.slice(-6).replace(/0+$/, '').padEnd(2, '0')
  return `${sign}¥${thousands(units.slice(0, -6))}.${frac}`
}

/** token 数 → 「999」「12.3K」「2.5M」「3B」（一位小数，去掉「.0」；进位到 1000 时升一级）。 */
export function formatTokens(n: number): string {
  const v = Math.max(0, Math.trunc(n))
  if (v < 1000) return String(v)
  const units = ['K', 'M', 'B', 'T']
  let x = v / 1000
  let i = 0
  while (Math.round(x * 10) / 10 >= 1000 && i < units.length - 1) {
    x /= 1000
    i++
  }
  return `${(Math.round(x * 10) / 10).toFixed(1).replace(/\.0$/, '')}${units[i]}`
}

/** 充值 / 手动入账的金额范围（分）：1 元～100 万元，与后端 billing.MinAmount / MaxAmount 一致。只在这里定义。 */
export const RECHARGE_MIN_CENTS = 100
export const RECHARGE_MAX_CENTS = 100_000_000

/**
 * 元（字符串）→ 分，并检查范围 [minCents, maxCents]。
 * 格式不对一律「金额格式不正确」；低于下限 / 高于上限分别用 messages.min / messages.max（缺省用 messages.range）。
 */
export function validateYuanRange(
  input: string,
  minCents: number,
  maxCents: number,
  messages: { range?: string; min?: string; max?: string },
): { cents: number } | { error: string } {
  const cents = yuanToCents(input)
  if (cents == null) return { error: '金额格式不正确' }
  if (cents < minCents) return { error: messages.min ?? messages.range ?? '金额超出范围' }
  if (cents > maxCents) return { error: messages.max ?? messages.range ?? '金额超出范围' }
  return { cents }
}

/** 全角数字转半角（输入法打出的「１３８」）。 */
export function toHalfWidthDigits(s: string): string {
  return s.replace(/[０-９]/g, (c) => String.fromCharCode(c.charCodeAt(0) - 0xfee0))
}

/** 只留数字：先把全角数字转成半角，再去掉其余字符（粘贴来的「138 0000 1234」「１３８」）。 */
export function digitsOnly(s: string): string {
  return toHalfWidthDigits(s).replace(/\D/g, '')
}

const cst = new Intl.DateTimeFormat('en-US', {
  timeZone: 'Asia/Shanghai',
  hourCycle: 'h23',
  year: 'numeric',
  month: '2-digit',
  day: '2-digit',
  hour: '2-digit',
  minute: '2-digit',
  second: '2-digit',
})

function cstParts(iso: string | Date | null | undefined): Record<string, string> | null {
  if (iso == null || iso === '') return null
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return null
  const out: Record<string, string> = {}
  for (const p of cst.formatToParts(d)) out[p.type] = p.value
  return out
}

export function fmtDate(iso?: string | Date | null): string {
  const p = cstParts(iso)
  return p ? `${p.year}-${p.month}-${p.day}` : ''
}

export function fmtTime(iso?: string | Date | null): string {
  const p = cstParts(iso)
  return p ? `${p.year}-${p.month}-${p.day} ${p.hour}:${p.minute}` : ''
}

export function fmtTimeSec(iso?: string | Date | null): string {
  const p = cstParts(iso)
  return p ? `${p.year}-${p.month}-${p.day} ${p.hour}:${p.minute}:${p.second}` : ''
}

/** 秒 → 「6 天 23 小时 59 分」；不足 1 分钟 / 0 分钟单独处理。 */
export function fmtDuration(seconds: number): string {
  const s = Math.max(0, Math.floor(seconds))
  if (s === 0) return '0 分钟'
  if (s < 60) return '不足 1 分钟'
  const d = Math.floor(s / 86400)
  const h = Math.floor((s % 86400) / 3600)
  const m = Math.floor((s % 3600) / 60)
  const parts: string[] = []
  if (d) parts.push(`${d} 天`)
  if (h) parts.push(`${h} 小时`)
  if (m) parts.push(`${m} 分`)
  return parts.join(' ')
}

export interface CountdownOptions {
  now?: () => number
  onZero?: () => void
}

/**
 * 倒计时：以 start 时刻为基准，每秒按真实流逝时间重算剩余秒数（不是每 tick 减 1），
 * 浏览器节流后台标签页的定时器时也不会漂移；归零时触发一次 onZero 并停止。
 */
export function useCountdown(opts: CountdownOptions = {}): { remaining: Ref<number>; start: (seconds: number) => void; stop: () => void } {
  const now = opts.now ?? (() => Date.now())
  const remaining = ref(0)
  let timer: ReturnType<typeof setInterval> | undefined
  let base = 0
  let initial = 0

  function stop() {
    if (timer !== undefined) {
      clearInterval(timer)
      timer = undefined
    }
  }
  function tick() {
    const next = Math.max(0, initial - Math.floor((now() - base) / 1000))
    remaining.value = next
    if (next === 0) {
      stop()
      opts.onZero?.()
    }
  }
  function start(seconds: number) {
    stop()
    initial = Math.max(0, Math.floor(seconds))
    base = now()
    remaining.value = initial
    if (initial > 0) timer = setInterval(tick, 1000)
  }
  if (getCurrentScope()) onScopeDispose(stop)
  return { remaining, start, stop }
}
