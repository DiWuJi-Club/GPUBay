import type { Duration } from './catalog'

export type InstanceState = 'creating' | 'running' | 'stopped' | 'wiping' | 'released' | 'failed'

export interface Instance {
  id: string
  state: InstanceState
  product_id: number
  image_id: number
  product_name: string
  image_name: string
  suspended: boolean
  expires_at: string | null
  remaining_seconds: number
  pending_seconds: number
  retain_until: string | null
  ssh_command: string
  created_at: string
}

export type OrderKind = 'new' | 'renew'
export type OrderStatus = 'paid' | 'refunded' | 'partially_refunded'

export interface Order {
  id: string
  kind: OrderKind
  instance_id: string
  product_id: number
  duration: Duration
  amount: number
  status: OrderStatus
  refunded_amount: number
  created_at: string
}
