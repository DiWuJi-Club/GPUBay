import type { InstancePort, SshKey } from './access'
import type { AppCategory, AppSpec, AppStatus, Deployment, DeploymentEvent } from './apps'
import type { CalendarPeriod } from './booking'
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
  group_id: number | null
  display_name: string
  /** Agent 上报的本机镜像（repository:tag，≤ 200 项），只用于覆盖率展示 */
  cached_images: string[]
  cached_images_at: string | null
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
  /** 该实例的全部部署（含已卸载 / 已失效的历史，按创建时间） */
  apps?: AdminDeployment[]
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
  /** queued + installing */
  apps_installing: number
  apps_running: number
  apps_failed_24h: number
}

/** 后台应用列表的一行（GET /admin/api/v1/apps）；POST / PUT 返回的应用不带 versions / deployments_live。 */
export interface AdminApp {
  id: number
  slug: string
  name: string
  summary: string
  description: string
  category: AppCategory
  icon: string
  sort: number
  status: AppStatus
  created_at: string
  updated_at: string
  versions: number
  /** 非终态（不是 removed / lost）的部署数 */
  deployments_live: number
}

/** 兼容镜像在在线机器上的缓存情况（unknown：缓存上报超过 5 分钟或旧 Agent 未上报）。 */
export interface AppCoverage {
  image_id: number
  image_name: string
  cached: number
  online: number
  unknown: number
}

/** 后台版本（GET /apps/{id}/versions 的一项；脚本走 /script 单独取）。已退役版本的 coverage 为空。 */
export interface AdminAppVersion {
  id: number
  app_id: number
  version: string
  status: AppStatus
  image_ids: number[]
  spec: AppSpec
  spec_hash: string
  changelog: string
  published_at: string | null
  created_at: string
  updated_at: string
  coverage: AppCoverage[]
  /** 该版本的非终态部署数（后端尚未提供时缺省） */
  deployments_live?: number
}

/** 后台部署视图：多 log_tail（已脱敏）与 user_id。 */
export interface AdminDeployment extends Deployment {
  log_tail: string
  user_id: number
}

/** GET /deployments/{dep}：事件按时间倒序（最近 100 条）；log_tail 只在 deployment 里。 */
export interface AdminDeploymentDetail {
  deployment: AdminDeployment
  events: DeploymentEvent[]
}
