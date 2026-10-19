import { describe, expect, it } from 'vitest'
import { parseSignedYuan, validateAdjust, validateManualRecharge } from '../money'

describe('parseSignedYuan', () => {
  it.each([
    ['10', 1000],
    ['-5', -500],
    ['-0.5', -50],
    [' 12.34 ', 1234],
    ['0', null],
    ['abc', null],
    ['1.234', null],
    ['--1', null],
  ])('%s -> %s', (input, want) => {
    expect(parseSignedYuan(input)).toBe(want)
  })

  it.each([
    // 浮点陷阱：0.29 * 100 = 28.999999999999996、4.35 * 100 = 434.99999999999994，必须按字符串换算成整数分
    ['0.29', 29],
    ['-0.29', -29],
    ['-4.35', -435],
    ['1.15', 115],
    ['-0', null],
    ['-0.00', null],
    ['+5', null],
    ['- 5', null],
    ['5-', null],
    ['', null],
    ['-', null],
    ['1e3', null],
    ['１０', null],
    ['-.5', null],
  ])('边缘写法 %j -> %s', (input, want) => {
    expect(parseSignedYuan(input)).toBe(want)
  })

  it('结果总是整数分', () => {
    for (const s of ['0.01', '0.07', '-0.29', '19.99', '-1234.56']) expect(Number.isInteger(parseSignedYuan(s))).toBe(true)
  })
})

describe('validateAdjust', () => {
  it('正负金额与备注', () => {
    expect(validateAdjust('10', ' 活动补偿 ')).toEqual({ cents: 1000, remark: '活动补偿' })
    expect(validateAdjust('-5', '纠错')).toEqual({ cents: -500, remark: '纠错' })
  })
  it('金额格式不正确（含 0）先于备注检查', () => {
    for (const s of ['', '0', 'abc', '1.234', '--1']) expect(validateAdjust(s, '')).toEqual({ error: '金额格式不正确（可为负数，不能为 0）' })
  })
  it('±100 万元边界', () => {
    expect(validateAdjust('1000000', 'x')).toEqual({ cents: 100_000_000, remark: 'x' })
    expect(validateAdjust('-1000000', 'x')).toEqual({ cents: -100_000_000, remark: 'x' })
    expect(validateAdjust('1000000.01', 'x')).toEqual({ error: '调整金额需在 ±100 万元之间' })
    expect(validateAdjust('-1000000.01', 'x')).toEqual({ error: '调整金额需在 ±100 万元之间' })
  })
  it('备注必填（空白不算）', () => {
    expect(validateAdjust('10', '   ')).toEqual({ error: '请填写备注' })
  })
})

describe('validateManualRecharge', () => {
  it('合法金额', () => {
    expect(validateManualRecharge('100', ' 对公转账 ')).toEqual({ cents: 10000, remark: '对公转账' })
    expect(validateManualRecharge('1', 'x')).toEqual({ cents: 100, remark: 'x' })
    expect(validateManualRecharge('1000000', 'x')).toEqual({ cents: 100_000_000, remark: 'x' })
    expect(validateManualRecharge('0.29', 'x')).toEqual({ error: '入账金额需在 1 元到 100 万元之间' })
  })
  it('格式不正确：负数、三位小数、科学计数、空', () => {
    for (const s of ['-100', '1.234', '1e3', '']) expect(validateManualRecharge(s, 'x')).toEqual({ error: '金额格式不正确' })
  })
  it('范围', () => {
    expect(validateManualRecharge('0.99', 'x')).toEqual({ error: '入账金额需在 1 元到 100 万元之间' })
    expect(validateManualRecharge('1000000.01', 'x')).toEqual({ error: '入账金额需在 1 元到 100 万元之间' })
  })
  it('备注必填', () => {
    expect(validateManualRecharge('100', ' ')).toEqual({ error: '请填写备注（例如转账流水号）' })
  })
})
