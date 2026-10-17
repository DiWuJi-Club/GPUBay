import { yuanToCents } from '@spark/shared'

/** 与后端 wallet.MaxAdjust 一致：单次调账 ±100 万元（分） */
export const MAX_ADJUST = 100_000_000
/** 与后端 billing.MinAmount / MaxAmount 一致：手动入账 1 元～100 万元（分） */
export const MIN_MANUAL_RECHARGE = 100
export const MAX_MANUAL_RECHARGE = 100_000_000

/**
 * 调账金额：元，允许一个前导 `-`（负号与数字之间不能有空格），不允许 0；返回整数分。
 * 数字部分交给 yuanToCents 按字符串换算（不经过浮点乘法：0.29 → 29 而不是 28.999…）。
 */
export function parseSignedYuan(input: string): number | null {
  const s = input.trim()
  const negative = s.startsWith('-')
  const body = negative ? s.slice(1) : s
  if (body !== body.trim()) return null // 「- 5」
  const cents = yuanToCents(body)
  if (cents == null || cents === 0) return null
  return negative ? -cents : cents
}

export type MoneyInput = { cents: number; remark: string } | { error: string }

/** 调账：金额（可为负，不能为 0，±100 万元以内）与备注（必填）。 */
export function validateAdjust(amount: string, remark: string): MoneyInput {
  const cents = parseSignedYuan(amount)
  if (cents == null) return { error: '金额格式不正确（可为负数，不能为 0）' }
  if (Math.abs(cents) > MAX_ADJUST) return { error: '调整金额需在 ±100 万元之间' }
  if (!remark.trim()) return { error: '请填写备注' }
  return { cents, remark: remark.trim() }
}

/** 手动入账：金额 1 元～100 万元（不接受负数）与备注（必填，例如转账流水号）。 */
export function validateManualRecharge(amount: string, remark: string): MoneyInput {
  const cents = yuanToCents(amount)
  if (cents == null) return { error: '金额格式不正确' }
  if (cents < MIN_MANUAL_RECHARGE || cents > MAX_MANUAL_RECHARGE) return { error: '入账金额需在 1 元到 100 万元之间' }
  if (!remark.trim()) return { error: '请填写备注（例如转账流水号）' }
  return { cents, remark: remark.trim() }
}
