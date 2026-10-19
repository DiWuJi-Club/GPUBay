import { fmtDuration, fmtTime, type Duration, type Instance, type InstanceState } from '@spark/shared'
import { MAX_QUANTITY } from './booking'

/** 以拿到数据的本地时刻为基准递减；暂停中与非运行状态不递减。 */
export function remainingNow(i: Instance, fetchedAt: number, now: number): number {
  if (i.state !== 'running' || i.suspended) return i.remaining_seconds
  return Math.max(0, i.remaining_seconds - Math.floor((now - fetchedAt) / 1000))
}

export function instanceTimeText(i: Instance, remaining: number): string {
  if (i.state === 'reserved' && i.scheduled_start_at) return `${fmtTime(i.scheduled_start_at)} 开始 · 共 ${fmtDuration(i.pending_seconds)}`
  if (i.state === 'cancelled') return '已取消'
  if (i.state === 'running') return `剩余 ${fmtDuration(remaining)}`
  if (i.state === 'stopped' && i.retain_until) return `数据保留至 ${fmtTime(i.retain_until)}`
  if (i.pending_seconds > 0) return `已付 ${fmtDuration(i.pending_seconds)} 待开始`
  if (i.state === 'failed') return '费用已原路退回余额'
  return ''
}

/** 停机且有待开始的续费：释放会作废这部分时长（计划 1 遗留）。 */
export function releaseWarning(i: Instance): string | null {
  if (i.state === 'stopped' && i.pending_seconds > 0) {
    return `该实例有已付费但尚未开始计时的 ${fmtDuration(i.pending_seconds)}，释放后这部分时长也会作废`
  }
  return null
}

export const canRenew = (s: InstanceState) => s === 'reserved' || s === 'creating' || s === 'running' || s === 'stopped'
export const canRelease = (s: InstanceState) => s === 'running' || s === 'stopped'
export const canSetAutoRenew = (s: InstanceState) => s === 'reserved' || s === 'creating' || s === 'running' || s === 'stopped'
/** 预订开通前（开始前 1 小时）可取消。 */
export const canCancel = (i: Instance, nowMs: number) => i.state === 'reserved' && !!i.cancellable_until && nowMs < Date.parse(i.cancellable_until)

/** 某档位最多能买的单位数：后端 renew_options 为准，没有时按 90 天上限。 */
export function renewQuantityMax(i: Instance, d: Duration): number {
  const opt = i.renew_options?.find((o) => o.duration === d)
  return opt ? opt.quantity_max : MAX_QUANTITY[d]
}

export function autoRenewFailureText(i: Instance): string {
  if (i.auto_renew_failed_reason === 'insufficient_balance') return '自动续费失败：余额不足，请充值后系统会自动重试'
  if (i.auto_renew_failed_reason === 'renew_blocked') return `自动续费失败：机器已有后续预订，可续至 ${i.renewable_until ? fmtTime(i.renewable_until) : '—'}`
  return ''
}
