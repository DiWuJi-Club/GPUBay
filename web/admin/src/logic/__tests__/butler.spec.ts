import { describe, expect, it } from 'vitest'
import { adoptRange, fmtCount, hasAudit, stepView, tabOf, userIdOf, validateSettingsForm } from '../butler'

describe('tabOf', () => {
  it.each([
    [{}, true, 'usage'],
    [{ tab: 'settings' }, false, 'settings'],
    [{ tab: 'conversations' }, true, 'conversations'],
    [{ tab: 'conversations' }, false, 'usage'],
    [{ user_id: '7' }, true, 'conversations'],
    [{ user_id: '7' }, false, 'usage'],
    [{ user_id: 'x' }, true, 'usage'],
    [{ tab: 'usage', user_id: '7' }, true, 'usage'],
    [{ tab: 'bogus' }, true, 'usage'],
  ] as const)('%o (canConversations=%s) → %s', (q, can, want) => {
    expect(tabOf(q, can)).toBe(want)
  })
})

describe('userIdOf', () => {
  it('只接受正整数', () => {
    expect(userIdOf('12')).toBe('12')
    expect(userIdOf(['5', '6'])).toBe('5')
    for (const v of ['0', '-1', '1.5', 'abc', '', undefined, null]) expect(userIdOf(v)).toBe('')
  })
})

describe('adoptRange', () => {
  const out = { from: '2026-09-26', to: '2026-10-25' }
  it('两端都空：采用后端默认', () => {
    const r = { from: '', to: '' }
    expect(adoptRange(r, out)).toBe(true)
    expect(r).toEqual(out)
  })
  it('用户已选过日期：不覆盖', () => {
    const r = { from: '2026-10-01', to: '' }
    expect(adoptRange(r, out)).toBe(false)
    expect(r).toEqual({ from: '2026-10-01', to: '' })
  })
})

describe('stepView / hasAudit', () => {
  it('待确认的调用不显示失败', () => {
    expect(stepView('error', 'pending')).toEqual({ mark: '…', cls: 'pending', suffix: '（待确认）' })
    expect(stepView('pending', undefined).cls).toBe('pending')
    expect(stepView('ok', 'executed').mark).toBe('✓')
    expect(stepView('error', 'failed').mark).toBe('✗')
  })
  it('只有 executed / failed 的写调用有审计', () => {
    expect(hasAudit('write', 'executed')).toBe(true)
    expect(hasAudit('write', 'failed')).toBe(true)
    for (const s of ['pending', 'approved', 'rejected', 'expired']) expect(hasAudit('write', s)).toBe(false)
    expect(hasAudit('read', 'executed')).toBe(false)
  })
})

describe('fmtCount', () => {
  it('千分位', () => {
    expect(fmtCount(1234567)).toBe('1,234,567')
    expect(fmtCount(0)).toBe('0')
    expect(fmtCount(undefined)).toBe('0')
  })
})

describe('validateSettingsForm', () => {
  const ok = { enabled: false, daily: '300000', turns: '60', notice: '  维护中\n18:00 恢复 ' }
  it('合法 → 请求体（notice 去首尾空白）', () => {
    expect(validateSettingsForm(ok)).toEqual({ input: { enabled: false, daily_token_limit: 300000, max_turns: 60, notice: '维护中\n18:00 恢复' } })
  })
  it.each([
    [{ daily: '9999' }, '每用户每日 token 限额需在 10000～100000000 之间'],
    [{ daily: '100000001' }, '每用户每日 token 限额需在 10000～100000000 之间'],
    [{ daily: '' }, '每用户每日 token 限额需在 10000～100000000 之间'],
    [{ turns: '4' }, '每对话消息数上限需在 5～1000 之间'],
    [{ turns: '1001' }, '每对话消息数上限需在 5～1000 之间'],
    [{ notice: '字'.repeat(201) }, '关闭说明不能超过 200 字'],
    [{ notice: 'a\u0007b' }, '关闭说明含非法字符'],
  ])('%o → %s', (patch, msg) => {
    expect(validateSettingsForm({ ...ok, ...patch })).toEqual({ error: msg })
  })
  it('200 字（按字符计）可以', () => {
    expect('input' in validateSettingsForm({ ...ok, notice: '😀'.repeat(200) })).toBe(true)
  })
})
