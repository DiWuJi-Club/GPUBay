import type { InstanceState, Role } from '@spark/shared'
import { describe, expect, it } from 'vitest'
import { extendSeconds, isZeroRefund, showCancelBooking, showExtend, showMigrate, showPortActions, showReassign, showRefund, showRewipe, validateExtend, validateRefund } from '../instanceOps'

const RANGE = '补偿时长需在 1 分钟到 30 天之间'

describe('instance ops', () => {
  it('延长时长换算与范围', () => {
    expect(extendSeconds(2, 'day')).toBe(172800)
    expect(extendSeconds(1, 'hour')).toBe(3600)
    expect(extendSeconds(1.5, 'hour')).toBe(5400)
    expect(extendSeconds(30, 'day')).toBe(2592000)
    expect(validateExtend(extendSeconds(31, 'day'))).toBe(RANGE)
    expect(validateExtend(30)).toBe(RANGE)
    expect(validateExtend(59)).toBe(RANGE)
    expect(validateExtend(0)).toBe(RANGE)
    expect(validateExtend(Number.NaN)).toBe(RANGE)
    expect(validateExtend(60)).toBeNull()
    expect(validateExtend(30 * 86400)).toBeNull()
    expect(validateExtend(30 * 86400 + 1)).toBe(RANGE)
  })
  it('延长：空输入（el-input-number 清空为 null）按 0 处理并被拦截', () => {
    expect(validateExtend(extendSeconds(null, 'day'))).toBe(RANGE)
  })
  it('退款金额：0 ≤ 金额 ≤ 上限', () => {
    expect(validateRefund('69', 6900)).toEqual({ cents: 6900 })
    expect(validateRefund(' 12.5 ', 6900)).toEqual({ cents: 1250 })
    expect(validateRefund('0', 6900)).toEqual({ cents: 0 }) // 只释放、不退款
    expect(validateRefund('0.00', 6900)).toEqual({ cents: 0 })
    expect(validateRefund('0', 0)).toEqual({ cents: 0 }) // 可退上限为 0 时仍可释放
    expect(validateRefund('0.01', 0)).toEqual({ error: '退款金额不能超过可退上限' })
    expect(validateRefund('0.01', 6900)).toEqual({ cents: 1 })
    expect(isZeroRefund('0')).toBe(true)
    expect(isZeroRefund(' 0.00 ')).toBe(true)
    expect(isZeroRefund('0.01')).toBe(false)
    expect(isZeroRefund('')).toBe(false)
    expect(isZeroRefund('abc')).toBe(false)
    expect(validateRefund('69.01', 6900)).toEqual({ error: '退款金额不能超过可退上限' })
    expect(validateRefund('-1', 6900)).toEqual({ error: '金额格式不正确' })
    expect(validateRefund('abc', 6900)).toEqual({ error: '金额格式不正确' })
    expect(validateRefund('1.234', 6900)).toEqual({ error: '金额格式不正确' })
    expect(validateRefund('', 6900)).toEqual({ error: '金额格式不正确' })
    expect(validateRefund('1e3', 6900)).toEqual({ error: '金额格式不正确' })
  })
  it('按钮按角色与状态', () => {
    expect(showExtend('ops', 'running')).toBe(true)
    expect(showExtend('finance', 'running')).toBe(false)
    expect(showExtend('super', 'wiping')).toBe(false)
    expect(showMigrate('ops', 'running')).toBe(true)
    expect(showMigrate('ops', 'stopped')).toBe(false)
    expect(showRewipe('ops', 'wiping')).toBe(true)
    expect(showRewipe('finance', 'wiping')).toBe(false)
    expect(showRefund('finance', 'stopped')).toBe(true)
    expect(showRefund('ops', 'running')).toBe(false)
    expect(showRefund('super', 'released')).toBe(false)
  })
  it('按钮完整矩阵：与后端允许的角色、状态一致', () => {
    const states: InstanceState[] = ['creating', 'running', 'stopped', 'wiping', 'released', 'failed']
    const visible = (role: Role) =>
      Object.fromEntries(states.map((s) => [s, [showExtend(role, s) && 'extend', showMigrate(role, s) && 'migrate', showRewipe(role, s) && 'rewipe', showRefund(role, s) && 'refund'].filter(Boolean)]))
    expect(visible('ops')).toEqual({ creating: ['extend'], running: ['extend', 'migrate'], stopped: ['extend'], wiping: ['rewipe'], released: [], failed: [] })
    expect(visible('finance')).toEqual({ creating: ['refund'], running: ['refund'], stopped: ['refund'], wiping: [], released: [], failed: [] })
    expect(visible('super')).toEqual({
      creating: ['extend', 'refund'], running: ['extend', 'migrate', 'refund'], stopped: ['extend', 'refund'], wiping: ['rewipe'], released: [], failed: [],
    })
  })
  it('预订操作只对 reserved 且 ops/super 可见', () => {
    expect(showReassign('ops', 'reserved')).toBe(true)
    expect(showReassign('super', 'reserved')).toBe(true)
    expect(showReassign('finance', 'reserved')).toBe(false)
    expect(showReassign('ops', 'creating')).toBe(false)
    expect(showCancelBooking('ops', 'reserved')).toBe(true)
    expect(showCancelBooking('ops', 'running')).toBe(false)
    expect(showMigrate('ops', 'reserved')).toBe(false)
    expect(showExtend('ops', 'reserved')).toBe(true)
    expect(showRefund('finance', 'reserved')).toBe(false)
  })
})

describe('showPortActions', () => {
  it('只有运维与超管能处置端口', () => {
    expect(showPortActions('ops')).toBe(true)
    expect(showPortActions('super')).toBe(true)
    expect(showPortActions('finance')).toBe(false)
  })
})

describe('系统实例（平台节点，规格 §4.2）', () => {
  const states: InstanceState[] = ['reserved', 'creating', 'running', 'stopped', 'wiping', 'released', 'failed']
  it('任何角色、任何状态都不显示退款 / 延长 / 换机 / 改派 / 取消预订', () => {
    for (const role of ['super', 'ops', 'finance'] as Role[]) {
      for (const s of states) {
        expect(showExtend(role, s, true), `${role} ${s} extend`).toBe(false)
        expect(showMigrate(role, s, true), `${role} ${s} migrate`).toBe(false)
        expect(showRefund(role, s, true), `${role} ${s} refund`).toBe(false)
        expect(showReassign(role, s, true), `${role} ${s} reassign`).toBe(false)
        expect(showCancelBooking(role, s, true), `${role} ${s} cancel`).toBe(false)
      }
    }
  })
  it('system=false / 缺省时与原判定一致', () => {
    expect(showExtend('ops', 'running', false)).toBe(true)
    expect(showMigrate('ops', 'running', false)).toBe(true)
    expect(showRefund('finance', 'running', false)).toBe(true)
    expect(showReassign('ops', 'reserved', false)).toBe(true)
    expect(showCancelBooking('ops', 'reserved', undefined)).toBe(true)
  })
  it('重新洗机不受影响（系统实例也会洗机）', () => {
    expect(showRewipe('ops', 'wiping')).toBe(true)
  })
})
