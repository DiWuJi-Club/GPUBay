import type { LLMKey } from '@spark/shared'
import { describe, expect, it } from 'vitest'
import { emptyKeyForm, keyFormOf, keyStatusTag, KEY_NAME_MAX, monthlyCapYuanToFen, MONTHLY_CAP_MAX_FEN, validateKeyInput, type KeyForm } from '../tokens'

// 2026-10-24 12:00 北京时间
const NOW = new Date('2026-10-24T04:00:00Z')
const form = (over: Partial<KeyForm> = {}): KeyForm => ({ ...emptyKeyForm(), name: '生产', ...over })
const key = (over: Partial<LLMKey> = {}): LLMKey => ({
  id: 1, name: '生产', key_hint: 'sk-spark-…a1b2', status: 'active', model_slugs: [], monthly_cap: null, month_cost: 0,
  expires_at: null, last_used_at: null, created_at: '2026-10-01T00:00:00Z', ...over,
})

describe('monthlyCapYuanToFen', () => {
  it.each([
    ['10', 1000],
    ['10.5', 1050],
    ['0.01', 1],
    ['19.99', 1999], // 浮点乘法会得到 1998.9999…，这里按字符串换算
    ['0.29', 29],
    [' 1.10 ', 110],
    ['1000000', MONTHLY_CAP_MAX_FEN],
  ])('%s 元 → %d 分', (input, fen) => {
    expect(monthlyCapYuanToFen(input)).toEqual({ fen })
  })

  it('空 = 不限（null）', () => {
    expect(monthlyCapYuanToFen('')).toEqual({ fen: null })
    expect(monthlyCapYuanToFen('  ')).toEqual({ fen: null })
  })

  it.each(['0', '0.00', '-1', '1.234', '1e3', '１０', 'abc', '1,000', '.5'])('非法：%s', (input) => {
    expect(monthlyCapYuanToFen(input)).toHaveProperty('error')
  })

  it('超过 100 万元', () => {
    expect(monthlyCapYuanToFen('1000000.01')).toEqual({ error: '月上限不能超过 ¥1,000,000.00' })
  })
})

describe('validateKeyInput（创建）', () => {
  it('最小输入：只有名称，其余字段不带', () => {
    expect(validateKeyInput(form({ name: '  生产  ' }), NOW)).toEqual({ body: { name: '生产', model_slugs: [] } })
  })

  it('完整输入：模型去重、月上限换算为分、到期日为北京时间当天结束', () => {
    const r = validateKeyInput(form({ modelSlugs: ['qwen3-4b', 'bge-m3', 'qwen3-4b'], monthlyCap: '19.99', expiresOn: '2026-11-30' }), NOW)
    expect(r).toEqual({ body: { name: '生产', model_slugs: ['qwen3-4b', 'bge-m3'], monthly_cap: 1999, expires_at: '2026-11-30T23:59:59+08:00' } })
  })

  it('名称必填、≤ 32 个字符（按 Unicode 码点计，与后端 rune 一致）', () => {
    expect(validateKeyInput(form({ name: '   ' }), NOW)).toEqual({ errors: { name: '请填写名称' } })
    expect(validateKeyInput(form({ name: '键'.repeat(KEY_NAME_MAX) }), NOW)).toHaveProperty('body')
    expect(validateKeyInput(form({ name: '😀'.repeat(KEY_NAME_MAX) }), NOW)).toHaveProperty('body') // 32 个 emoji = 64 个 UTF-16 单元
    expect(validateKeyInput(form({ name: 'a'.repeat(KEY_NAME_MAX + 1) }), NOW)).toEqual({ errors: { name: '名称最多 32 个字符' } })
  })

  it('到期日期：今天（北京时间）可选，过去不可选；格式必须是 YYYY-MM-DD', () => {
    expect(validateKeyInput(form({ expiresOn: '2026-10-24' }), NOW)).toEqual({ body: { name: '生产', model_slugs: [], expires_at: '2026-10-24T23:59:59+08:00' } })
    expect(validateKeyInput(form({ expiresOn: '2026-10-23' }), NOW)).toEqual({ errors: { expiresOn: '到期日期不能早于今天' } })
    expect(validateKeyInput(form({ expiresOn: '2026/11/30' }), NOW)).toEqual({ errors: { expiresOn: '日期格式不正确' } })
    // 北京时间已是 10-25 凌晨（UTC 仍是 10-24）：10-24 已过去
    expect(validateKeyInput(form({ expiresOn: '2026-10-24' }), new Date('2026-10-24T16:30:00Z'))).toEqual({ errors: { expiresOn: '到期日期不能早于今天' } })
  })

  it('多个字段同时出错一并返回', () => {
    expect(validateKeyInput(form({ name: '', monthlyCap: '0', expiresOn: '2020-01-01' }), NOW)).toEqual({
      errors: { name: '请填写名称', monthlyCap: '月上限须大于 0，最多两位小数', expiresOn: '到期日期不能早于今天' },
    })
  })
})

describe('validateKeyInput（编辑）', () => {
  it('清空月上限 → monthly_cap: null；不带到期日（PUT 不支持修改）', () => {
    expect(validateKeyInput(form({ monthlyCap: '', expiresOn: '2020-01-01', modelSlugs: ['qwen3-4b'] }), NOW, 'edit')).toEqual({
      body: { name: '生产', model_slugs: ['qwen3-4b'], monthly_cap: null },
    })
    expect(validateKeyInput(form({ monthlyCap: '5' }), NOW, 'edit')).toEqual({ body: { name: '生产', model_slugs: [], monthly_cap: 500 } })
  })
})

describe('keyFormOf / keyStatusTag', () => {
  it('编辑表单回填：月上限分 → 元字符串，无上限为空', () => {
    expect(keyFormOf(key({ name: 'a', model_slugs: ['x'], monthly_cap: 1999 }))).toEqual({ name: 'a', modelSlugs: ['x'], monthlyCap: '19.99', expiresOn: '' })
    expect(keyFormOf(key()).monthlyCap).toBe('')
  })

  it('状态标签：启用 / 已停用 / 已撤销；启用但已过期显示「已过期」', () => {
    expect(keyStatusTag(key(), NOW)).toEqual({ label: '启用', type: 'success' })
    expect(keyStatusTag(key({ status: 'disabled' }), NOW)).toEqual({ label: '已停用', type: 'warning' })
    expect(keyStatusTag(key({ status: 'revoked' }), NOW)).toEqual({ label: '已撤销', type: 'info' })
    expect(keyStatusTag(key({ expires_at: '2026-10-24T03:59:59Z' }), NOW)).toEqual({ label: '已过期', type: 'danger' })
    expect(keyStatusTag(key({ expires_at: '2026-10-24T15:59:59Z' }), NOW)).toEqual({ label: '启用', type: 'success' })
    expect(keyStatusTag(key({ status: 'revoked', expires_at: '2020-01-01T00:00:00Z' }), NOW)).toEqual({ label: '已撤销', type: 'info' })
  })
})
