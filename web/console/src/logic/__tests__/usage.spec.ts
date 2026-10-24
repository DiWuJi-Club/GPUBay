import { formatMicroFen, formatTokens, ledgerKindType, type LLMSettlement } from '@spark/shared'
import { describe, expect, it } from 'vitest'
import {
  addDays, daySpan, defaultUsageRange, latencyLabel, settlementDetail, settlementLedgerLink, tokenCount, usageStatusTag, USAGE_MAX_DAYS, validateUsageRange,
} from '../tokens'

const settlement = (over: Partial<LLMSettlement> = {}): LLMSettlement => ({
  id: 1, created_at: '2026-10-24T02:00:00Z', requests: 3, prompt_tokens: 1000, completion_tokens: 500, usage_amount: 120, debt_amount: 0, charged: 120,
  wallet_ledger_id: 77, period_from: '2026-10-24T01:50:00Z', period_to: '2026-10-24T01:59:00Z', ...over,
})

describe('金额与 token 显示', () => {
  it.each([
    [0, '¥0.00'],
    [999_999, '< ¥0.01'],
    [1_250_000, '¥0.0125'],
    [123_456_789, '¥1.234568'],
  ])('formatMicroFen(%i 微分) → %s', (micro, want) => {
    expect(formatMicroFen(micro)).toBe(want)
  })

  it.each([
    [999, '999'],
    [12_345, '12.3K'],
    [2_500_000, '2.5M'],
  ])('formatTokens(%i) → %s', (n, want) => {
    expect(formatTokens(n)).toBe(want)
  })

  it('tokenCount：千分位整数；估算值前缀 ≈', () => {
    expect(tokenCount(0)).toBe('0')
    expect(tokenCount(1234567)).toBe('1,234,567')
    expect(tokenCount(1234, true)).toBe('≈1,234')
    expect(tokenCount(0, true)).toBe('≈0')
  })

  it('latencyLabel：< 1 秒按毫秒，其余两位小数秒；null → —', () => {
    expect(latencyLabel(null)).toBe('—')
    expect(latencyLabel(850)).toBe('850 ms')
    expect(latencyLabel(1234)).toBe('1.23 s')
    expect(latencyLabel(65_000)).toBe('65.00 s')
  })

  it('usageStatusTag：成功绿、客户端中断橙、上游错误 / 超时 / 无节点红', () => {
    expect(usageStatusTag('ok')).toEqual({ label: '成功', type: 'success' })
    expect(usageStatusTag('client_abort')).toEqual({ label: '客户端中断', type: 'warning' })
    expect(usageStatusTag('upstream_error')).toEqual({ label: '上游错误', type: 'danger' })
    expect(usageStatusTag('upstream_timeout')).toEqual({ label: '上游超时', type: 'danger' })
    expect(usageStatusTag('no_node')).toEqual({ label: '无可用节点', type: 'danger' })
  })

  it('钱包流水：Token 消费与扣款同色', () => {
    expect(ledgerKindType.llm_pay).toBe(ledgerKindType.order_pay)
    expect(ledgerKindType.recharge).not.toBe(ledgerKindType.order_pay)
  })
})

describe('请求日志的日期范围（北京时间，含首尾两天）', () => {
  it('addDays / daySpan 跨月、跨年', () => {
    expect(addDays('2026-10-24', -6)).toBe('2026-10-18')
    expect(addDays('2026-03-01', -1)).toBe('2026-02-28')
    expect(addDays('2026-12-31', 1)).toBe('2027-01-01')
    expect(daySpan('2026-10-18', '2026-10-24')).toBe(7)
    expect(daySpan('2026-10-24', '2026-10-24')).toBe(1)
  })

  it('默认最近 7 天：北京时间今天及之前 6 天', () => {
    // UTC 2026-10-24 17:00 = 北京时间 10-25 01:00
    expect(defaultUsageRange(new Date('2026-10-24T17:00:00Z'))).toEqual({ from: '2026-10-19', to: '2026-10-25' })
  })

  it('最长 90 天，起始不能晚于结束', () => {
    expect(USAGE_MAX_DAYS).toBe(90)
    expect(validateUsageRange('2026-10-18', '2026-10-24')).toBeNull()
    expect(validateUsageRange(addDays('2026-10-24', -89), '2026-10-24')).toBeNull()
    expect(validateUsageRange(addDays('2026-10-24', -90), '2026-10-24')).toBe('查询范围最长 90 天')
    expect(validateUsageRange('2026-10-25', '2026-10-24')).toBe('开始日期不能晚于结束日期')
    expect(validateUsageRange('', '2026-10-24')).toBe('请选择开始和结束日期')
    expect(validateUsageRange('2026-10-24', 'x')).toBe('请选择开始和结束日期')
  })
})

describe('结算记录', () => {
  it('只有用量：一行「用量 ¥x」', () => {
    expect(settlementDetail(settlement())).toEqual(['用量 ¥1.20'])
  })

  it('补扣欠费、余额不足转欠费都写明', () => {
    expect(settlementDetail(settlement({ usage_amount: 120, debt_amount: 50, charged: 170 }))).toEqual(['用量 ¥1.20', '补扣欠费 ¥0.50'])
    expect(settlementDetail(settlement({ usage_amount: 120, debt_amount: 0, charged: 20 }))).toEqual(['用量 ¥1.20', '余额不足，¥1.00 计入欠费'])
    // 纯补扣欠费（没有新用量）
    expect(settlementDetail(settlement({ usage_amount: 0, debt_amount: 300, charged: 100, requests: 0 }))).toEqual(['补扣欠费 ¥3.00', '余额不足，¥2.00 计入欠费'])
  })

  it('流水链接：有 wallet_ledger_id 才有', () => {
    expect(settlementLedgerLink(settlement())).toEqual({ path: '/wallet', query: { ledger: '77' } })
    expect(settlementLedgerLink(settlement({ wallet_ledger_id: null, charged: 0 }))).toBeNull()
  })
})
