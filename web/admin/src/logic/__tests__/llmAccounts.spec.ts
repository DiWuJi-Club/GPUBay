import { formatMicroFen, type LLMDailyAdminRow } from '@spark/shared'
import { describe, expect, it } from 'vitest'
import {
  accountFilterFromQuery, accountsQuery, blockBody, buildLimitPatch, currentYM, dailyQuery, defaultUsageRange, emptyAccountFilter, emptyUsageFilter,
  keyMonthCostText, limitFormOf, limitText, settlementsQuery, sumDaily, summarizeDaily, unblockBody, usageQuery, validateReason, validateUsageRange,
} from '../llm'

const acc = (over: Partial<{ rpm_limit: number | null; tpm_limit: number | null; concurrency_limit: number | null }> = {}) => ({
  rpm_limit: null as number | null, tpm_limit: 500_000 as number | null, concurrency_limit: null as number | null, ...over,
})

describe('限额表单的空值语义', () => {
  it('表单初值：覆盖值 → 字符串，null → 空串', () => {
    expect(limitFormOf(acc())).toEqual({ rpm: '', tpm: '500000', concurrency: '' })
  })

  it('没有改动 → 空 patch（不发任何字段）', () => {
    expect(buildLimitPatch(acc(), limitFormOf(acc()))).toEqual({ ok: true, patch: {} })
    // 首尾空白不算改动
    expect(buildLimitPatch(acc(), { rpm: ' ', tpm: ' 500000 ', concurrency: '' })).toEqual({ ok: true, patch: {} })
  })

  it('清空已有覆盖值 → 发 null（恢复默认）；空且原本就是默认 → 省略', () => {
    const r = buildLimitPatch(acc(), { rpm: '', tpm: '', concurrency: '' })
    expect(r).toEqual({ ok: true, patch: { tpm_limit: null } })
    expect(r.ok && 'rpm_limit' in r.patch).toBe(false)
    expect(r.ok && 'concurrency_limit' in r.patch).toBe(false)
  })

  it('只发改了的字段；全角数字按半角', () => {
    expect(buildLimitPatch(acc(), { rpm: '120', tpm: '500000', concurrency: '１６' })).toEqual({ ok: true, patch: { rpm_limit: 120, concurrency_limit: 16 } })
  })

  it('越界 / 非整数 → 字段错误，不产生 patch', () => {
    const r = buildLimitPatch(acc(), { rpm: '0', tpm: '100000001', concurrency: '1.5' })
    expect(r.ok).toBe(false)
    if (!r.ok) expect(Object.keys(r.errors).sort()).toEqual(['concurrency', 'rpm', 'tpm'])
    expect(buildLimitPatch(acc(), { rpm: '-1', tpm: '', concurrency: '' }).ok).toBe(false)
    expect(buildLimitPatch(acc(), { rpm: '1e3', tpm: '', concurrency: '' }).ok).toBe(false)
    // 边界值可接受
    expect(buildLimitPatch(acc(), { rpm: '100000', tpm: '100000000', concurrency: '512' })).toEqual({
      ok: true, patch: { rpm_limit: 100000, tpm_limit: 100000000, concurrency_limit: 512 },
    })
    expect(buildLimitPatch(acc(), { rpm: '1', tpm: '1', concurrency: '1' }).ok).toBe(true)
  })

  it('表格：null 显示生效值并标默认', () => {
    const a = { ...acc(), limits: { rpm: 60, tpm: 500000, concurrency: 8 } }
    expect(limitText(a, 'rpm')).toBe('60（默认）')
    expect(limitText(a, 'tpm')).toBe('500,000')
  })
})

describe('停用 / 原因', () => {
  it('原因必填、≤ 200 字符；请求体', () => {
    expect(validateReason('  ')).toBe('请填写原因')
    expect(validateReason('x'.repeat(201))).toMatch('200')
    expect(validateReason('好'.repeat(200))).toBeNull()
    expect(blockBody('  欠费不还 ')).toEqual({ blocked: true, blocked_reason: '欠费不还' })
    expect(unblockBody()).toEqual({ blocked: false })
  })
})

describe('筛选与查询串', () => {
  it('账户：空值省略，欠费 / 停用按 true / false', () => {
    expect(accountsQuery(emptyAccountFilter(), 1, 20)).toEqual({ q: undefined, user_id: undefined, blocked: undefined, debt: undefined, page: 1, page_size: 20 })
    expect(accountsQuery({ q: '138', user_id: '', blocked: 'false', debt: true }, 2, 50)).toEqual({ q: '138', user_id: undefined, blocked: 'false', debt: 'true', page: 2, page_size: 50 })
  })

  it('账户：?user_id= → 精确 user_id 筛选（只留数字），不放进 q', () => {
    expect(accountFilterFromQuery({ user_id: '42' })).toEqual({ q: '', user_id: '42', blocked: '', debt: false })
    expect(accountFilterFromQuery({ user_id: ['4a2'] }).user_id).toBe('42')
    expect(accountFilterFromQuery({}).user_id).toBe('')
    expect(accountsQuery(accountFilterFromQuery({ user_id: '42' }), 1, 20)).toMatchObject({ q: undefined, user_id: '42' })
  })

  it('用量：默认最近 7 天（北京时间）；空值省略', () => {
    const now = new Date('2026-10-24T17:00:00Z') // 北京 10-25 01:00
    expect(defaultUsageRange(now)).toEqual({ from: '2026-10-19', to: '2026-10-25' })
    const f = emptyUsageFilter({ user_id: '7' }, now)
    expect(usageQuery(f)).toEqual({ user_id: '7', model_id: undefined, key_id: undefined, status: undefined, from: '2026-10-19', to: '2026-10-25' })
    expect(usageQuery({ ...f, model_id: 3, key_id: '9', status: 'ok' })).toMatchObject({ model_id: 3, key_id: '9', status: 'ok' })
    expect(settlementsQuery({ ...f, model_id: 3, status: 'ok' })).toEqual({ user_id: '7', from: '2026-10-19', to: '2026-10-25' })
    expect(dailyQuery(f, 'day')).toEqual({ from: '2026-10-19', to: '2026-10-25', group_by: 'model' })
    expect(dailyQuery(f, 'user').group_by).toBe('user')
  })

  it('日期范围：必填、先后、≤ 90 天（含首尾）', () => {
    expect(validateUsageRange({ from: '', to: '2026-10-24' })).toBe('请选择起止日期')
    expect(validateUsageRange({ from: '2026-10-25', to: '2026-10-24' })).toBe('开始日期不能晚于结束日期')
    expect(validateUsageRange({ from: '2026-10-24', to: '2026-10-24' })).toBeNull()
    expect(validateUsageRange({ from: '2026-07-27', to: '2026-10-24' })).toBeNull() // 90 天
    expect(validateUsageRange({ from: '2026-07-26', to: '2026-10-24' })).toBe('查询范围最多 90 天')
  })
})

describe('汇总与费用', () => {
  const rows: LLMDailyAdminRow[] = [
    { day: '2026-10-23', model: 'a', requests: 1, prompt_tokens: 10, completion_tokens: 5, cost_micro: 100 },
    { day: '2026-10-23', model: 'b', requests: 2, prompt_tokens: 20, completion_tokens: 6, cost_micro: 900 },
    { day: '2026-10-24', model: 'a', requests: 3, prompt_tokens: 30, completion_tokens: 7, cost_micro: 50 },
  ]
  it('按天 / 按模型 合计', () => {
    expect(summarizeDaily(rows, 'day').map((r) => [r.day, r.requests, r.cost_micro])).toEqual([['2026-10-24', 3, 50], ['2026-10-23', 3, 1000]])
    expect(summarizeDaily(rows, 'model').map((r) => [r.model, r.requests, r.cost_micro])).toEqual([['b', 2, 900], ['a', 4, 150]])
    expect(sumDaily(rows)).toEqual({ requests: 6, prompt_tokens: 60, completion_tokens: 18, cost_micro: 1050 })
  })
  it('按用户合计', () => {
    const u = summarizeDaily([{ day: 'd1', user_id: 5, phone: '138', requests: 1, prompt_tokens: 1, completion_tokens: 1, cost_micro: 1 }, { day: 'd2', user_id: 5, phone: '138', requests: 1, prompt_tokens: 1, completion_tokens: 1, cost_micro: 2 }], 'user')
    expect(u).toEqual([{ key: '5', user_id: 5, phone: '138', requests: 2, prompt_tokens: 2, completion_tokens: 2, cost_micro: 3 }])
  })
  it('Key 本月消费：优先 month_cost（分）；否则 month_ym 不是北京时间当前月时为 0', () => {
    const now = new Date('2026-10-31T16:30:00Z') // 北京 11-01 00:30
    expect(currentYM(now)).toBe('2026-11')
    expect(keyMonthCostText({ month_ym: '2026-10', month_cost_micro: 5_000_000 }, now)).toBe('¥0.00')
    expect(keyMonthCostText({ month_ym: '2026-11', month_cost_micro: 5_000_000 }, now)).toBe('¥0.05')
    expect(keyMonthCostText({ month_ym: '2026-10', month_cost_micro: 5_000_000, month_cost: 0 }, now)).toBe('¥0.00')
    expect(keyMonthCostText({ month_ym: '2026-11', month_cost_micro: 5_000_000, month_cost: 1234 }, now)).toBe('¥12.34')
  })
  it('微分 → 元（后台与控制台共用 shared 的 formatMicroFen）', () => {
    expect(formatMicroFen(0)).toBe('¥0.00')
    expect(formatMicroFen(999_999)).toBe('< ¥0.01')
    expect(formatMicroFen(1_000_000)).toBe('¥0.01')
    expect(formatMicroFen(1_250_000)).toBe('¥0.0125')
    expect(formatMicroFen(123_456_789_000_000)).toBe('¥1,234,567.89')
    expect(formatMicroFen(-2_000_000)).toBe('-¥0.02')
  })
})
