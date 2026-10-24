import type { InviteResult } from './referral'

export type KycStatus = 'none' | 'pending' | 'verified' | 'rejected'
export type UserKind = 'personal' | 'enterprise'
export type UserStatus = 'active' | 'banned'

export interface User {
  id: number
  phone: string
  kind: UserKind
  kyc_status: KycStatus
  status: UserStatus
  created_at: string
  /** 仅 POST /auth/login 的响应：带 invite_code 时的绑定结果 */
  invite?: InviteResult
  /** 仅 GET /me：本月返利到账（分），0 时省略 */
  referral_month_settled?: number
}
