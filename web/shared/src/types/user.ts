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
}
