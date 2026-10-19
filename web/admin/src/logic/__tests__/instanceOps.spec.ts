import type { InstanceState, Role } from '@spark/shared'
import { describe, expect, it } from 'vitest'
import { extendSeconds, isZeroRefund, showExtend, showMigrate, showPortActions, showRefund, showRewipe, validateExtend, validateRefund } from '../instanceOps'

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
})

describe('showPortActions', () => {
  it('只有运维与超管能处置端口', () => {
    expect(showPortActions('ops')).toBe(true)
    expect(showPortActions('super')).toBe(true)
    expect(showPortActions('finance')).toBe(false)
  })
})
