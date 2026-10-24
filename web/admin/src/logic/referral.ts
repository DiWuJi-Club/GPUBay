import { labelOf, yuanToCents, type LabelMap, type RescanResult } from '@spark/shared'

/** 邀请返利页只显示脱敏手机号（138****1234）；不是 11 位的一律整体打码，不回显原文。 */
export function maskPhone(p: string): string {
  if (!p) return ''
  return p.length === 11 ? `${p.slice(0, 3)}****${p.slice(7)}` : '****'
}

/** 风险标记（referral_bindings.risk_flags） */
export const riskFlagLabel: LabelMap = { same_ip: '同IP', same_device: '同设备', burst: '爆发', same_identity: '同身份' }
export const flagLabel = (f: string) => labelOf(riskFlagLabel, f)
export const accountStatusLabel: LabelMap = { active: '正常', frozen: '已冻结' }
export const eventKindLabel: LabelMap = {
  accrue: '计提',
  refund: '退款调整',
  settle: '发放',
  clawback: '扣回',
  debt_add: '记欠账',
  debt_offset: '抵扣欠账',
  void: '作废',
  capped: '封顶',
}
export const sortOptions = [
  { value: 'settled', label: '累计发放' },
  { value: 'invitees', label: '被邀请人数' },
  { value: 'bound_at', label: '最近绑定' },
]

/** 基点 → 百分比文案：1500 → 15%，1050 → 10.5%，1234 → 12.34%（整数运算，不经浮点）。 */
export function fmtRate(bp: number): string {
  const frac = String(bp % 100).padStart(2, '0').replace(/0+$/, '')
  return `${Math.floor(bp / 100)}${frac ? `.${frac}` : ''}%`
}

export interface RateForm {
  rate: string
  invitee: string
  inviter: string
  hold: string
  period: string
  effective: Date | null
  note: string
}
export interface RateBody {
  rate_bp: number
  max_per_invitee: number
  max_per_inviter_30d: number
  hold_days: number
  period_days: number
  effective_from?: string
  note: string
}

const intRe = /^\d+$/

/**
 * 新增费率版本表单 → 请求体。只做格式检查（范围由后端 referral_config_invalid 原文提示）：
 * 比例为百分数、最多两位小数（与元 → 分同一套字符串换算，15.5 → 1550 bp）；金额为元；天数为整数；
 * 生效时间留空则省略（后端按当前时刻生效），否则提交 RFC3339。
 */
export function parseRateForm(f: RateForm): { body: RateBody } | { error: string } {
  const rate = yuanToCents(f.rate)
  if (rate == null) return { error: '返利比例最多两位小数' }
  const invitee = yuanToCents(f.invitee)
  if (invitee == null) return { error: '单个被邀请人上限格式不正确' }
  const inviter = yuanToCents(f.inviter)
  if (inviter == null) return { error: '单个邀请人 30 天上限格式不正确' }
  if (!intRe.test(f.hold.trim())) return { error: '发放延迟需为整数天' }
  if (!intRe.test(f.period.trim())) return { error: '返利期需为整数天' }
  const body: RateBody = {
    rate_bp: rate,
    max_per_invitee: invitee,
    max_per_inviter_30d: inviter,
    hold_days: Number(f.hold.trim()),
    period_days: Number(f.period.trim()),
    note: f.note.trim(),
  }
  if (f.effective) body.effective_from = f.effective.toISOString()
  return { body }
}

export const RESCAN_MIN_DAYS = 1
export const RESCAN_MAX_DAYS = 30

/** 重扫天数：1～30 的整数，否则 null。 */
export function parseRescanDays(v: string): number | null {
  if (!intRe.test(v.trim())) return null
  const n = Number(v.trim())
  return n >= RESCAN_MIN_DAYS && n <= RESCAN_MAX_DAYS ? n : null
}

/**
 * 重扫结果提示（接口 200 即重扫已执行）。仍有处理失败的行或本轮未全部完成时用提醒（不是错误），
 * 并带上本次处理与发放的数量；失败行与未完成的部分由之后的清扫轮继续。
 */
export function rescanMessage(r: RescanResult): { type: 'success' | 'warning'; text: string } {
  const counts = `处理 ${r.processed} 行，发放 ${r.released} 笔`
  if (r.incomplete) return { type: 'warning', text: `已重扫，但本轮未全部完成（${counts}${r.failed > 0 ? `，${r.failed} 行失败` : ''}），之后的清扫会继续` }
  if (r.failed > 0) return { type: 'warning', text: `已重扫，${r.failed} 行仍失败（${counts}）` }
  return { type: 'success', text: `已重扫：${counts}` }
}
