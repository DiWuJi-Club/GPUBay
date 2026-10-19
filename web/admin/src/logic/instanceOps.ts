import { validateYuanRange, yuanToCents, type InstanceState, type Role } from '@spark/shared'

/** 与后端 rental.MinExtendSeconds / MaxExtendSeconds 一致 */
export const MIN_EXTEND = 60
export const MAX_EXTEND = 30 * 86400

/** 补偿时长换算为秒；amount 为 null（数字框被清空）按 0 处理，交给 validateExtend 拦截。 */
export function extendSeconds(amount: number | null | undefined, unit: 'hour' | 'day'): number {
  return Math.round((amount ?? 0) * (unit === 'day' ? 86400 : 3600))
}

export function validateExtend(seconds: number): string | null {
  return Number.isFinite(seconds) && seconds >= MIN_EXTEND && seconds <= MAX_EXTEND ? null : '补偿时长需在 1 分钟到 30 天之间'
}

/**
 * 退款金额（元，最多两位小数）→ 分，范围 0～可退上限；负数、多于两位小数、科学计数等一律「金额格式不正确」。
 * 0 合法：表示只释放、不退款（后端允许；可退上限为 0 时这是唯一的释放方式）。
 */
export function validateRefund(yuan: string, maxCents: number): { cents: number } | { error: string } {
  return validateYuanRange(yuan, 0, maxCents, { max: '退款金额不能超过可退上限' })
}

/** 金额输入恰好是 0（含 0.00）：提示「只释放、不退款」 */
export const isZeroRefund = (yuan: string) => yuanToCents(yuan) === 0

const opsRole = (r: Role) => r === 'ops' || r === 'super'
const finRole = (r: Role) => r === 'finance' || r === 'super'
/** 后端 addTime 允许的状态（后台免费延长允许 reserved） */
const extendable = (s: InstanceState) => s === 'reserved' || s === 'creating' || s === 'running' || s === 'stopped'
/** 后端 RefundRelease 允许的状态 */
const refundable = (s: InstanceState) => s === 'creating' || s === 'running' || s === 'stopped'

export const showExtend = (r: Role, s: InstanceState) => opsRole(r) && extendable(s)
export const showMigrate = (r: Role, s: InstanceState) => opsRole(r) && s === 'running'
export const showRewipe = (r: Role, s: InstanceState) => opsRole(r) && s === 'wiping'
export const showRefund = (r: Role, s: InstanceState) => finRole(r) && refundable(s)
/** 预订（reserved）改派 / 取消：运维与超管。 */
export const showReassign = (r: Role, s: InstanceState) => opsRole(r) && s === 'reserved'
export const showCancelBooking = (r: Role, s: InstanceState) => opsRole(r) && s === 'reserved'
/** 端口处置（强制私有 / 删除）：运维与超管。 */
export const showPortActions = (role: Role): boolean => role === 'ops' || role === 'super'
