import type { Instance } from '@spark/shared'
import { describe, expect, it } from 'vitest'
import { canRelease, canRenew, instanceTimeText, releaseWarning, remainingNow } from '../instance'

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
