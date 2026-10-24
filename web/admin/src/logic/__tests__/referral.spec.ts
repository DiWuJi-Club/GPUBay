import type { RescanResult } from '@spark/shared'
import { describe, expect, it } from 'vitest'
import { fmtRate, flagLabel, maskPhone, parseRateForm, parseRescanDays, rescanMessage } from '../referral'

describe('maskPhone', () => {
  it('11 位手机号只留前 3 后 4', () => {
    expect(maskPhone('13800001234')).toBe('138****1234')
  })
  it('非 11 位整体打码，不泄露原文', () => {
    expect(maskPhone('1380000')).toBe('****')
    expect(maskPhone('')).toBe('')
  })
})

describe('fmtRate', () => {
  it.each([
    [1500, '15%'],
    [1050, '10.5%'],
    [1234, '12.34%'],
    [5, '0.05%'],
    [0, '0%'],
  ])('%d bp → %s', (bp, out) => expect(fmtRate(bp)).toBe(out))
})

describe('flagLabel', () => {
  it('四种标记中文，未知原样', () => {
    expect(['same_ip', 'same_device', 'burst', 'same_identity', 'x'].map(flagLabel)).toEqual(['同IP', '同设备', '爆发', '同身份', 'x'])
  })
})

describe('parseRateForm', () => {
  const ok = { rate: '15', invitee: '500', inviter: '5000', hold: '7', period: '365', effective: null as Date | null, note: ' e2e ' }
  it('百分比 → bp，元 → 分（字符串换算，不经浮点），生效时间缺省省略', () => {
    expect(parseRateForm(ok)).toEqual({
      body: { rate_bp: 1500, max_per_invitee: 50000, max_per_inviter_30d: 500000, hold_days: 7, period_days: 365, note: 'e2e' },
    })
    expect(parseRateForm({ ...ok, rate: '12.34', invitee: '0.29' })).toMatchObject({ body: { rate_bp: 1234, max_per_invitee: 29 } })
  })
  it('生效时间提交 RFC3339', () => {
    const d = new Date('2026-10-21T00:00:00Z')
    expect(parseRateForm({ ...ok, effective: d })).toMatchObject({ body: { effective_from: '2026-10-21T00:00:00.000Z' } })
  })
  it.each([
    [{ rate: '15.123' }, '返利比例最多两位小数'],
    [{ rate: '' }, '返利比例最多两位小数'],
    [{ invitee: '-1' }, '单个被邀请人上限格式不正确'],
    [{ inviter: 'abc' }, '单个邀请人 30 天上限格式不正确'],
    [{ hold: '1.5' }, '发放延迟需为整数天'],
    [{ period: '' }, '返利期需为整数天'],
  ])('格式错误 %o → %s', (patch, error) => expect(parseRateForm({ ...ok, ...patch })).toEqual({ error }))
})

describe('parseRescanDays', () => {
  it.each([
    ['3', 3],
    ['1', 1],
    ['30', 30],
  ])('%s 合法', (v, n) => expect(parseRescanDays(v)).toBe(n))
  it.each(['0', '31', '1.5', '', 'a'])('%s 不合法', (v) => expect(parseRescanDays(v)).toBeNull())
})

describe('rescanMessage', () => {
  const r = (over: Partial<RescanResult> = {}): RescanResult => ({ watermark: '2026-10-17T00:00:00Z', processed: 5, failed: 0, released: 2, incomplete: false, ...over })
  it('全部成功：已重扫 + 处理 / 发放数量', () => {
    expect(rescanMessage(r())).toEqual({ type: 'success', text: '已重扫：处理 5 行，发放 2 笔' })
  })
  it('部分行失败：已重扫，N 行仍失败（提醒，不是错误）', () => {
    expect(rescanMessage(r({ failed: 4 }))).toEqual({ type: 'warning', text: '已重扫，4 行仍失败（处理 5 行，发放 2 笔）' })
  })
  it('未全部完成：提醒，带失败行数', () => {
    expect(rescanMessage(r({ incomplete: true }))).toEqual({ type: 'warning', text: '已重扫，但本轮未全部完成（处理 5 行，发放 2 笔），之后的清扫会继续' })
    expect(rescanMessage(r({ incomplete: true, failed: 1 }))).toEqual({ type: 'warning', text: '已重扫，但本轮未全部完成（处理 5 行，发放 2 笔，1 行失败），之后的清扫会继续' })
  })
})
