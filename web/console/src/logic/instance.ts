import { fmtDuration, fmtTime, type Instance, type InstanceState } from '@spark/shared'

/** 以拿到数据的本地时刻为基准递减；暂停中与非运行状态不递减。 */
export function remainingNow(i: Instance, fetchedAt: number, now: number): number {
  if (i.state !== 'running' || i.suspended) return i.remaining_seconds
  return Math.max(0, i.remaining_seconds - Math.floor((now - fetchedAt) / 1000))
}

export function instanceTimeText(i: Instance, remaining: number): string {
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

export const canRenew = (s: InstanceState) => s === 'creating' || s === 'running' || s === 'stopped'
export const canRelease = (s: InstanceState) => s === 'running' || s === 'stopped'
