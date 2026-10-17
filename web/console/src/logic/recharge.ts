import { RECHARGE_MAX_CENTS, RECHARGE_MIN_CENTS, validateYuanRange } from '@spark/shared'

export const PRESETS = [50, 100, 500, 1000] as const
export const MIN_CENTS = RECHARGE_MIN_CENTS
export const MAX_CENTS = RECHARGE_MAX_CENTS

/** 手机端拉起支付宝扫一扫：把二维码内容整体编码后放进 qrcode 参数。 */
export function alipayLink(qr: string): string {
  return `alipays://platformapi/startapp?saId=10000007&qrcode=${encodeURIComponent(qr)}`
}

/** 开发 / 测试环境的模拟渠道二维码以 mock:// 开头。 */
export const isMockQR = (qr: string) => qr.startsWith('mock://')

export function mmss(seconds: number): string {
  const s = Math.max(0, Math.floor(seconds))
  return `${String(Math.floor(s / 60)).padStart(2, '0')}:${String(s % 60).padStart(2, '0')}`
}

export function validateAmount(input: string): { cents: number } | { error: string } {
  return validateYuanRange(input, MIN_CENTS, MAX_CENTS, { range: '充值金额需在 1 元到 100 万元之间' })
}
