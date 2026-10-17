import { toHalfWidthDigits } from '@spark/shared'

export const PHONE_RE = /^1[3-9]\d{9}$/

/** 去空白与连字符、全角数字转半角（从微信复制的「138 0000 1234」）。 */
export function normalizePhone(s: string): string {
  return toHalfWidthDigits(s).replace(/[\s-]/g, '')
}

/** 只保留数字，最多 6 位（短信里的「000 000」）。 */
export function normalizeCode(s: string): string {
  return toHalfWidthDigits(s).replace(/\D/g, '').slice(0, 6)
}

export function isValidPhone(s: string): boolean {
  return PHONE_RE.test(normalizePhone(s))
}

export function maskPhone(p: string): string {
  return p.length === 11 ? `${p.slice(0, 3)}****${p.slice(7)}` : p
}
