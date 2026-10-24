import { describe, expect, it } from 'vitest'
import { defaultUsageRange, fmtCount, tabOf, userIdOf, validateSettingsForm } from '../butler'

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

describe('defaultUsageRange', () => {
  it('北京时间今天往前共 30 天', () => {
    // 2026-10-25 01:00 北京时间 = 2026-10-24T17:00Z
    expect(defaultUsageRange(new Date('2026-10-24T17:00:00Z'))).toEqual({ from: '2026-09-26', to: '2026-10-25' })
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
