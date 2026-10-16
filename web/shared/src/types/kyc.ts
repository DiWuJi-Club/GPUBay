export type KycResult = 'pending' | 'verified' | 'rejected' | 'error'

/** 用户端 GET /kyc 返回的记录（个人 real_name 已脱敏）。 */
export interface KYCRecord {
  id: number
  kind: 'personal' | 'enterprise'
  real_name: string
  license_no?: string
  result: KycResult
  reason: string
  created_at: string
  updated_at: string
}

export interface Upload {
  key: string
  size: number
  content_type: string
  purpose: string
  created_at: string
}
