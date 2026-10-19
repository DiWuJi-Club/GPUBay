import type { InstancePort, SshKey } from './access'
import type { CalendarPeriod } from './booking'
import type { Duration } from './catalog'
import type { InstanceState, OrderKind, OrderStatus } from './instance'
import type { KycResult } from './kyc'
import type { LedgerKind, LedgerRow } from './wallet'
import type { RechargeChannel, RechargeStatus } from './recharge'
import type { BindingStatus, RebateRule, ReferralAccountStatus, RewardStatus } from './referral'
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
  group_id: number | null
  display_name: string
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
  password_auth: boolean
  created_at: string
  scheduled_start_at: string | null
  scheduled_end_at: string | null
  group_id: number | null
  batch_id: string | null
  reassign_count: number
  auto_renew: boolean
  auto_renew_duration: Duration
  auto_renew_failed_reason: string
  provisioned_at: string | null
  cancelled_at: string | null
}

export interface MachineDetail {
  machine: Machine
  current_instance: AdminInstance | null
  history: AdminInstance[]
  group: AdminGroupBrief | null
  leases: CalendarPeriod[]
  upcoming_bookings: number
}

export interface AdminGroup {
  id: number
  name: string
  region: string
  description: string
  sort: number
  visible: boolean
  created_at: string
  updated_at: string
  machine_count: number
  idle_now: number
  reserved_upcoming: number
}
export type AdminGroupBrief = Pick<AdminGroup, 'id' | 'name' | 'region' | 'visible'>
export interface GroupInput {
  name: string
  region: string
  description: string
  sort: number
  visible: boolean
}
export interface UpcomingDay {
  date: string
  count: number
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
  quantity: number
  batch_id: string | null
  auto: boolean
}

export interface AdminInstanceDetail {
  instance: AdminInstance
  events: InstanceEvent[]
  orders: AdminOrder[]
  commands: CommandRow[]
  ports: InstancePort[]
  ssh_host_pubkey_fingerprint: string
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
  ssh_keys: SshKey[]
  /** 邀请区块：作为被邀请人时的邀请人、作为邀请人时的统计；两者都没有时为空对象 */
  referral: {
    inviter?: { user_id: number; phone: string; binding_status: BindingStatus }
    account?: { code: string; status: ReferralAccountStatus; invitees: number; settled_net: number; pending: number; debt: number }
  }
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
  ssh_sessions_open: number
  bookings_upcoming_7d: number
  /** 本月已发放返利净额（返利 − 扣回，分） */
  referral_month_settled: number
}

// ---- 邀请返利（计划 8，金额单位：分）。手机号为原文，页面显示前须脱敏；后端从不返回实名指纹 / 证件号 ----

export type RiskFlag = 'same_ip' | 'same_device' | 'burst' | 'same_identity'

/** GET /referral/summary */
export interface AdminReferralSummary {
  pending: number
  month_settled: number
  settled_net: number
  debt: number
  held_count: number
}

/** 费率版本（GET /referral/config 的 current 与 versions[]） */
export interface AdminRateVersion extends RebateRule {
  id: number
  effective_from: string
  note: string
  created_by: number | null
  created_at: string
}

/** 邀请账户（referral_accounts） */
export interface AdminReferralAccount {
  user_id: number
  code: string
  status: ReferralAccountStatus
  frozen_reason: string
  debt: number
  created_at: string
}

/** GET /referral/inviters 的一行 */
export interface AdminInviterRow extends AdminReferralAccount {
  phone: string
  kyc_status: string
  user_status: string
  invitees: number
  held_count: number
  pending: number
  settled_net: number
  last_bound_at: string | null
}

/** 绑定（GET /referral/bindings；ip / device_id / user_agent 只用于导出核查，页面不展示） */
export interface AdminBinding {
  id: number
  inviter_id: number
  invitee_id: number
  inviter_phone: string
  invitee_phone: string
  code: string
  status: BindingStatus
  risk_flags: RiskFlag[]
  hold_reason: string
  rate_version_id: number
  bound_at: string
  rebate_until: string
  ip: string
  device_id: string
  user_agent: string
  reviewed_by: number | null
  reviewed_at: string | null
  review_note: string
  pending: number
  contributed: number
  /** 邀请人或被邀请人有未处理的实名同身份核查失败（晚于绑定 / 上次审核），通过前须人工核查 */
  kyc_check_unresolved: boolean
}

/** GET /referral/rewards 的一行 */
export interface AdminReferralReward {
  order_id: string
  binding_id: number
  inviter_id: number
  invitee_id: number
  rate_version_id: number
  rate_bp: number
  order_amount: number
  refunded_amount: number
  reward_cap: number
  reward: number
  status: RewardStatus
  settle_after: string
  settled_amount: number
  clawed_amount: number
  settled_at: string | null
  paid_at: string
  created_at: string
  updated_at: string
  inviter_phone: string
  invitee_phone: string
}

export interface AdminReferralEvent {
  id: number
  order_id: string
  binding_id: number
  kind: string
  source_ledger_id: number | null
  wallet_ledger_id: number | null
  amount: number
  detail: string
  created_at: string
}

/** GET /referral/inviters/{id} */
export interface AdminInviterDetail {
  account: AdminReferralAccount
  phone: string
  /** 最近 100 个；全量个数见 bindings_total */
  bindings: AdminBinding[]
  bindings_total: number
  /** 最近 100 条；全量条数见 rewards_total */
  rewards: AdminReferralReward[]
  rewards_total: number
  /** 最近 50 条 */
  events: AdminReferralEvent[]
}

/**
 * POST /referral/rescan 的结果（200）。拨回水位后重扫已执行：failed 为处理失败的流水行数（之后每轮清扫重试），
 * incomplete 表示发现或发放本身出错 / 超时（之后的清扫轮继续）。返回错误时什么都没发生。
 */
export interface RescanResult {
  watermark: string
  processed: number
  failed: number
  released: number
  incomplete: boolean
}
