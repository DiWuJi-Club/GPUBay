import type { Duration } from './catalog'
import type { InstanceState, OrderKind, OrderStatus } from './instance'
import type { KycResult } from './kyc'
import type { LedgerKind, LedgerRow } from './wallet'
import type { RechargeChannel, RechargeStatus } from './recharge'
import type { User } from './user'

export interface Paged<T> {
  items: T[]
  total: number
}
export interface Listed<T> {
  items: T[]
}

export type Role = 'super' | 'ops' | 'finance'

export interface Admin {
  id: number
  username: string
  role: Role
  status: 'active' | 'disabled'
  totp_confirmed: boolean
  last_login_at: string | null
  last_login_ip: string
  created_at: string
}

export interface AdminLoginResult {
  next: 'totp' | 'totp_setup'
  otpauth_uri: string
}

export type MachineState = 'idle' | 'allocated' | 'maintenance' | 'faulty'

export interface Machine {
  id: number
  serial: string
  rack: string
  slot: string
  lan_ip: string
  product_id: number
  state: MachineState
  agent_online: boolean
  last_seen_at: string | null
  pdu_id: number | null
  pdu_outlet: number | null
  created_at: string
  enrolled: boolean
  fingerprint_prefix: string
  connected: boolean
  agent_version: string
  agent_connected_at: string | null
  state_reported_at: string | null
  last_metrics: MachineMetrics | null
}

export interface MachineMetrics {
  gpu_util: number
  gpu_temp_c: number
  gpu_power_w: number
  mem_total: number
  mem_used: number
  root_disk_total: number
  root_disk_used: number
  data_disk_used: number
  load1: number
}

export interface EnrollCommand {
  command: string
  token_id: number
  expires_at: string
}

export interface AdminInstance {
  id: string
  user_id: number
  machine_id: number
  product_id: number
  image_id: number
  state: InstanceState
  suspended: boolean
  pending_seconds: number
  remaining_seconds: number
  expires_at: string | null
  stopped_at: string | null
  created_at: string
}

export interface MachineDetail {
  machine: Machine
  current_instance: AdminInstance | null
  history: AdminInstance[]
}

export interface ImportError {
  line: number
  message: string
}

export interface PDU {
  id: number
  name: string
  driver: 'snmp' | 'mock'
  address: string
  has_community: boolean
  oid_prefix: string
  cycle_value: number
  on_value: number
  created_at: string
}

export interface PDUInput {
  name: string
  driver: 'snmp' | 'mock'
  address: string
  community: string
  oid_prefix: string
  cycle_value: number
  on_value: number
}

export interface Alert {
  id: number
  level: 'warning' | 'critical'
  kind: string
  machine_id: number | null
  instance_id: string
  message: string
  created_at: string
  resolved_at: string | null
  resolved_by: number | null
}

export interface InstanceEvent {
  id: number
  from_state: string
  to_state: string
  reason: string
  at: string
}

export interface CommandRow {
  id: string
  machine_id: number
  type: string
  status: string
  result: string
  attempts: number
  created_at: string
}

export interface AdminOrder {
  id: string
  user_id: number
  kind: OrderKind
  instance_id: string
  product_id: number
  duration: Duration
  amount: number
  refunded_amount: number
  status: OrderStatus
  created_at: string
}

export interface AdminInstanceDetail {
  instance: AdminInstance
  events: InstanceEvent[]
  orders: AdminOrder[]
  commands: CommandRow[]
}

export interface RefundQuote {
  instance_id: string
  remaining_seconds: number
  unit_order_id: string
  suggested: number
  max: number
}

export interface AdminLedgerRow extends LedgerRow {
  user_id: number
  operator_id: number | null
  kind: LedgerKind
}

export interface AdminRecharge {
  id: string
  user_id: number
  channel: RechargeChannel
  amount: number
  status: RechargeStatus
  channel_trade_no?: string
  operator_id?: number | null
  remark?: string
  expires_at: string
  paid_at: string | null
  closed_at: string | null
  created_at: string
}

export interface AdminKYCRecord {
  id: number
  user_id: number
  kind: 'personal' | 'enterprise'
  real_name: string
  id_number_masked?: string
  license_no?: string
  license_file?: string
  result: KycResult
  reason: string
  reviewer_id: number | null
  created_at: string
  updated_at: string
}

export interface UserDetail {
  user: User
  balance: number
  instances: AdminInstance[]
  orders: AdminOrder[]
  ledger: LedgerRow[]
  kyc: AdminKYCRecord[]
}

export interface AdminPrice {
  duration: Duration
  amount: number
  enabled: boolean
  updated_at: string
}

export interface AdminProduct {
  id: number
  code: string
  name: string
  spec: Record<string, unknown>
  status: 'active' | 'inactive'
  prices: AdminPrice[]
}

export interface AdminImage {
  id: number
  name: string
  ref: string
  description: string
  enabled: boolean
  sort: number
  created_at: string
  updated_at: string
}

export interface ImageInput {
  name: string
  ref: string
  description: string
  enabled: boolean
  sort: number
}

export interface AuditEntry {
  id: number
  actor_type: 'admin' | 'user' | 'system'
  actor_id: number
  actor_name: string
  action: string
  target_type: string
  target_id: string
  diff: unknown
  ip: string
  created_at: string
}

export interface Dashboard {
  machines: { by_state: Partial<Record<MachineState, number>>; online: number; total: number }
  instances: Partial<Record<InstanceState, number>>
  recharge_today: number
  recharge_month: number
  order_today: number
  order_month: number
  utilization: { date: string; rate: number }[]
}
