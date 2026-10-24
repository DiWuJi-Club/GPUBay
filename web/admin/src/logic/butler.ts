import { fmtDate, type ButlerSettingsInput, type LabelMap } from '@spark/shared'

// 后台 AI 管家页（计划 12 Task 14）的纯函数：tab 选择、默认日期、设置校验、标签。

export const butlerTabs = ['usage', 'conversations', 'settings'] as const
export type ButlerTab = (typeof butlerTabs)[number]

const first = (v: unknown): string => (Array.isArray(v) ? String(v[0] ?? '') : v == null ? '' : String(v))

/** 查询参数里的用户 ID：只接受正整数（其它一律视为没有）。 */
export function userIdOf(v: unknown): string {
  const s = first(v).trim()
  return /^[1-9]\d{0,17}$/.test(s) ? s : ''
}

/**
 * 当前 tab：?tab= 指定且有权看就用它；没指定但带了 ?user_id=（来自用户详情「查看对话」）→ 对话；否则用量。
 * 对话 tab 只有 ops / super 可见（canConversations），其它角色落回用量。
 */
export function tabOf(query: { tab?: unknown; user_id?: unknown }, canConversations: boolean): ButlerTab {
  const t = first(query.tab) as ButlerTab
  const ok = (x: ButlerTab) => x !== 'conversations' || canConversations
  if (butlerTabs.includes(t) && ok(t)) return t
  if (!first(query.tab) && userIdOf(query.user_id) && canConversations) return 'conversations'
  return 'usage'
}

/** 用量默认范围：北京时间今天往前共 30 天（两端含），与后端默认一致。 */
export function defaultUsageRange(now: Date = new Date()): { from: string; to: string } {
  return { from: fmtDate(new Date(now.getTime() - 29 * 86_400_000)), to: fmtDate(now) }
}

/** 北京时间今天 YYYY-MM-DD */
export const todayCST = (now: Date = new Date()) => fmtDate(now)

/** token 等计数加千分位 */
export function fmtCount(n: number | null | undefined): string {
  return String(Math.trunc(n ?? 0)).replace(/\B(?=(\d{3})+(?!\d))/g, ',')
}

// 与后端 butler 常量一致（MinDailyTokenLimit… / MaxNoticeRunes）
export const SETTINGS_LIMITS = { dailyMin: 10_000, dailyMax: 100_000_000, turnsMin: 5, turnsMax: 1000, noticeMax: 200 } as const

/** 设置表单：数字以字符串输入（只留数字），提交前校验成请求体；规则与后端 validateSettings 相同。 */
export interface SettingsForm {
  enabled: boolean
  daily: string
  turns: string
  notice: string
}

const badNotice = /[\u0000-\u0009\u000b-\u001f\u007f-\u009f]/

export function validateSettingsForm(f: SettingsForm): { input: ButlerSettingsInput } | { error: string } {
  const L = SETTINGS_LIMITS
  const daily = /^\d{1,12}$/.test(f.daily.trim()) ? Number(f.daily.trim()) : NaN
  const turns = /^\d{1,6}$/.test(f.turns.trim()) ? Number(f.turns.trim()) : NaN
  const notice = f.notice.trim()
  if (!(daily >= L.dailyMin && daily <= L.dailyMax)) return { error: `每用户每日 token 限额需在 ${L.dailyMin}～${L.dailyMax} 之间` }
  if (!(turns >= L.turnsMin && turns <= L.turnsMax)) return { error: `每对话消息数上限需在 ${L.turnsMin}～${L.turnsMax} 之间` }
  if ([...notice].length > L.noticeMax) return { error: `关闭说明不能超过 ${L.noticeMax} 字` }
  if (badNotice.test(notice)) return { error: '关闭说明含非法字符' }
  return { input: { enabled: f.enabled, daily_token_limit: daily, max_turns: turns, notice } }
}

export const convStatusLabel: LabelMap = { idle: '空闲', running: '处理中', awaiting_confirm: '待确认' }
export const toolCallStatusLabel: LabelMap = {
  executed: '已执行', failed: '失败', pending: '待确认', approved: '执行中', rejected: '已拒绝', expired: '已过期',
}
export const toolCallStatusType: LabelMap = {
  executed: 'success', failed: 'danger', pending: 'warning', approved: 'primary', rejected: 'info', expired: 'info',
}
export const roleLabel: LabelMap = { user: '用户', assistant: '管家', tool: '工具结果' }

/** 工具参数（落库的 *** 版本）格式化成多行 JSON；只做展示，不解析内容。 */
export function argsText(args: unknown): string {
  try {
    return JSON.stringify(args ?? {}, null, 2)
  } catch {
    return String(args)
  }
}
