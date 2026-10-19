import type { Instance, Order } from './instance'

export interface Group {
  id: number
  name: string
  region: string
  description: string
  machine_count: number
  idle_now: number
}

export interface GroupMachine {
  id: number
  label: string
  busy_until: string | null
}

export interface CalendarDay {
  date: string
  free: number
  total: number
}

export interface CalendarPeriod {
  lower: string
  upper: string
  mine: boolean
  instance_id?: string
  user_id?: number
}

export interface Availability {
  available: number
  lower: string
  upper: string
}

export interface Batch {
  id: string
  count: number
  created_at: string
}

export interface BookingResult {
  batch: Batch | null
  orders: Order[]
  instances: Instance[]
}

export interface CancelQuote {
  refund: number
  percent: number
  deadline: string
  max: number
}

export interface UserEvent {
  to_state: string
  reason_kind: string
  at: string
  detail: string
}
