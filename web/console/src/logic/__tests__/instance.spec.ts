import type { Instance } from '@spark/shared'
import { describe, expect, it } from 'vitest'
import { autoRenewFailureText, canCancel, canRelease, canRenew, canSetAutoRenew, instanceTimeText, releaseWarning, remainingNow, renewQuantityMax } from '../instance'

const base: Instance = {
  id: 'ins-abc12345', state: 'running', product_id: 1, image_id: 1, product_name: 'DGX Spark 整机', image_name: 'PyTorch', suspended: false,
  expires_at: '2026-10-18T06:00:00Z', remaining_seconds: 86399, pending_seconds: 0, retain_until: null, ssh_command: 'ssh ins-abc12345@ssh.spark.test',
  password_auth: true, ssh_host: 'ssh.spark.test', ssh_port: 22, ports_count: 0,
  scheduled_start_at: null, scheduled_end_at: null, cancellable_until: null, group_id: null, group_name: '', machine_label: '', batch_id: null,
  reassign_count: 0, auto_renew: false, auto_renew_duration: '1d', auto_renew_failed_reason: '', renewable_until: null, renew_options: [],
  created_at: '2026-10-17T06:00:00Z',
}

describe('releaseWarning (U-11)', () => {
  it('仅 stopped 且 pending_seconds > 0 时返回追加文案', () => {
    expect(releaseWarning({ ...base, state: 'stopped', pending_seconds: 86400 })).toBe('该实例有已付费但尚未开始计时的 1 天，释放后这部分时长也会作废')
    expect(releaseWarning({ ...base, state: 'stopped', pending_seconds: 0 })).toBeNull()
    expect(releaseWarning({ ...base, state: 'running', pending_seconds: 86400 })).toBeNull()
  })
})

describe('instance time text', () => {
  it('运行中按剩余秒递减', () => {
    expect(remainingNow(base, 1000, 4000)).toBe(86396)
    expect(instanceTimeText(base, 86396)).toBe('剩余 23 小时 59 分')
  })
  it('暂停中不递减', () => {
    expect(remainingNow({ ...base, suspended: true }, 1000, 99_000)).toBe(86399)
  })
  it('停机显示保留时间，待开始显示已付时长，失败显示退款', () => {
    expect(instanceTimeText({ ...base, state: 'stopped', retain_until: '2026-10-18T12:00:00Z' }, 0)).toBe('数据保留至 2026-10-18 20:00')
    expect(instanceTimeText({ ...base, state: 'stopped', retain_until: null, pending_seconds: 604800 }, 0)).toBe('已付 7 天 待开始')
    expect(instanceTimeText({ ...base, state: 'creating', pending_seconds: 86400 }, 0)).toBe('已付 1 天 待开始')
    expect(instanceTimeText({ ...base, state: 'failed', pending_seconds: 0 }, 0)).toBe('费用已原路退回余额')
    expect(instanceTimeText({ ...base, state: 'released', pending_seconds: 0 }, 0)).toBe('')
  })
})

describe('按钮与状态匹配 (C-DET-09 的逻辑)', () => {
  it.each([
    ['creating', true, false],
    ['running', true, true],
    ['stopped', true, true],
    ['wiping', false, false],
    ['released', false, false],
    ['failed', false, false],
  ] as const)('%s renew=%s release=%s', (state, renew, release) => {
    expect(canRenew(state)).toBe(renew)
    expect(canRelease(state)).toBe(release)
  })
})

describe('booking states (U-21)', () => {
  const reserved: Instance = { ...base, state: 'reserved', expires_at: null, remaining_seconds: 0, pending_seconds: 86400, ssh_command: '',
    scheduled_start_at: '2026-10-20T02:00:00Z', scheduled_end_at: '2026-10-21T02:00:00Z', cancellable_until: '2026-10-20T01:00:00Z',
    renewable_until: '2026-10-25T00:00:00Z', renew_options: [{ duration: '1d', quantity_max: 3 }, { duration: '1w', quantity_max: 0 }, { duration: '1m', quantity_max: 0 }] }
  it('预订 / 取消的时间文案', () => {
    expect(instanceTimeText(reserved, 0)).toBe('2026-10-20 10:00 开始 · 共 1 天')
    expect(instanceTimeText({ ...base, state: 'cancelled' }, 0)).toBe('已取消')
  })
  it('可取消：reserved 且未过截止', () => {
    expect(canCancel(reserved, Date.parse('2026-10-20T00:59:00Z'))).toBe(true)
    expect(canCancel(reserved, Date.parse('2026-10-20T01:00:00Z'))).toBe(false)
    expect(canCancel({ ...reserved, state: 'creating' }, 0)).toBe(false)
  })
  it('续费数量上限来自 renew_options', () => {
    expect(renewQuantityMax(reserved, '1d')).toBe(3)
    expect(renewQuantityMax(reserved, '1w')).toBe(0)
    expect(renewQuantityMax({ ...reserved, renew_options: [] }, '1m')).toBe(3)
  })
  it('自动续费失败文案', () => {
    expect(autoRenewFailureText({ ...base, auto_renew_failed_reason: 'insufficient_balance' })).toBe('自动续费失败：余额不足，请充值后系统会自动重试')
    expect(autoRenewFailureText({ ...reserved, auto_renew_failed_reason: 'renew_blocked' })).toBe('自动续费失败：机器已有后续预订，可续至 2026-10-25 08:00')
    expect(autoRenewFailureText(base)).toBe('')
  })
  it('按钮矩阵含 reserved / cancelled', () => {
    expect(canRenew('reserved')).toBe(true)
    expect(canRelease('reserved')).toBe(false)
    expect(canRenew('cancelled')).toBe(false)
    expect(canSetAutoRenew('reserved')).toBe(true)
    expect(canSetAutoRenew('released')).toBe(false)
  })
})
