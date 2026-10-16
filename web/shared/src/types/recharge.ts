export type RechargeStatus = 'pending' | 'paid' | 'closed'
export type RechargeChannel = 'wechat' | 'alipay' | 'manual' | (string & {})

export interface Recharge {
  id: string
  channel: RechargeChannel
  amount: number
  status: RechargeStatus
  qr_code?: string
  remaining_seconds: number
  expires_at: string
  paid_at: string | null
  closed_at: string | null
  created_at: string
}
