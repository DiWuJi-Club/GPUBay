/** 登录响应里的邀请结果（POST /auth/login 带 invite_code 时返回）。inviter 为脱敏手机号。 */
export interface InviteResult {
  bound: boolean
  reason: 'ok' | 'invalid_code' | 'inviter_frozen' | 'existing_user'
  inviter?: string
}

/** 当前生效的返利条款（金额单位：分）。 */
export interface RebateRule {
  rate_bp: number
  period_days: number
  hold_days: number
  max_per_invitee: number
  max_per_inviter_30d: number
}

export type ReferralAccountStatus = 'active' | 'frozen'

/** GET /referral/me（金额单位：分） */
export interface ReferralMe {
  code: string
  link: string
  status: ReferralAccountStatus
  kyc_verified: boolean
  rebate: RebateRule
  stats: {
    invitees: number
    verified_invitees: number
    /** 待发放 */
    pending: number
    /** 累计净返利：已发放 − 已扣回，含抵扣欠账的部分 */
    settled_net: number
    /** 本月实际到账（钱包 referral_rebate 流水之和） */
    month_settled: number
    debt: number
  }
}

export type BindingStatus = 'active' | 'held' | 'revoked'

/** GET /referral/invitees 的一项（手机号已脱敏） */
export interface Invitee {
  phone_masked: string
  bound_at: string
  status: BindingStatus
  kyc_verified: boolean
  contributed: number
  rebate_until: string
}

export type RewardStatus = 'pending' | 'settled' | 'void'

/** GET /referral/rewards 的一项（被邀请人手机号已脱敏） */
export interface RewardItem {
  order_id: string
  invitee_masked: string
  paid_at: string
  order_amount: number
  refunded_amount: number
  reward: number
  status: RewardStatus
  settled_at: string | null
  clawed_amount: number
}
