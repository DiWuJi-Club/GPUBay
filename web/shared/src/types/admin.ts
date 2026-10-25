import type { InstancePort, SshKey } from './access'
import type { AppCategory, AppSpec, AppStatus, Deployment, DeploymentEvent, DeploymentState } from './apps'
import type { CalendarPeriod } from './booking'
import type { Duration } from './catalog'
import type { InstanceState, OrderKind, OrderStatus } from './instance'
import type { KycResult } from './kyc'
import type { LedgerKind, LedgerRow } from './wallet'
import type { RechargeChannel, RechargeStatus } from './recharge'
import type { BindingStatus, RebateRule, ReferralAccountStatus, RewardStatus } from './referral'
import type { User } from './user'
import type { InstalledWorkflow, WorkflowCategory, WorkflowInstallState, WorkflowManifestPublic, WorkflowStatus } from './workflows'

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
  /** 每个实例容器的 cgroup 用量（计划 10，≤ 16 项；mem_max 0 = 不限）；升级前写入的 last_metrics 没有此字段 */
  instances?: {
    instance_id: string
    mem_current: number
    mem_max: number
    cpu_percent: number
    data_disk_used: number
  }[]
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
  /** queued + installing */
  apps_installing: number
  apps_running: number
  apps_failed_24h: number
  /** 计划 11：已装好（installed）且部署非终态的工作流安装数 */
  workflow_installs: number
  /** 计划 11：近 24 小时快速出图次数 */
  workflow_runs_24h: number
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
  /** 计划 11：部署上的工作流集合（非 ComfyUI 部署为 []；旧后端缺省） */
  workflows?: InstalledWorkflow[]
}

// ---- 计划 11：ComfyUI 工作流目录（/admin/api/v1/workflows*）----

/** 后台工作流（POST / PUT / GET /workflows/{id}、封面上传的响应）。cover_url 是后台预览地址（带 ?v=），无封面为 null。 */
export interface AdminWorkflow {
  id: number
  slug: string
  name: string
  summary: string
  description: string
  category: WorkflowCategory
  tags: string[]
  has_cover: boolean
  cover_url: string | null
  sort: number
  status: WorkflowStatus
  created_at: string
  updated_at: string
}

/** GET /workflows 的一行：+ 版本数、在用安装数、近 7 日快速出图次数。 */
export interface AdminWorkflowRow extends AdminWorkflow {
  versions: number
  installs_live: number
  runs_7d: number
}

/** POST / PUT /workflows 的请求体（slug 创建后不可改；PUT 的 status 省略 = 不改）。 */
export interface AdminWorkflowInput {
  slug: string
  name: string
  summary: string
  description: string
  category: WorkflowCategory
  tags: string[]
  sort: number
  status?: WorkflowStatus
}

/** GET /workflows/{id}/versions 的一项（不含 graph / api_prompt / manifest 本体），按创建倒序。 */
export interface AdminWorkflowVersionRow {
  id: number
  workflow_id: number
  version: string
  status: WorkflowStatus
  download_gb: number
  manifest_hash: string
  changelog: string
  /** 模型数 */
  models: number
  /** 自定义节点数 */
  nodes: number
  quick_run: boolean
  comfyui_min: string
  /** 该版本的在用安装数 */
  installs: number
  published_at: string | null
  created_at: string
  updated_at: string
}

/** API 格式的工作流（ComfyUI「导出 (API)」）。 */
export type ComfyApiPrompt = Record<string, { class_type: string; inputs: Record<string, unknown>; _meta?: unknown }>

/** GET /workflows/{id}/versions/{vid}（唯一带 graph / api_prompt / manifest 全文的接口）；api_prompt 为 null = 不支持快速出图。 */
export interface AdminWorkflowVersion {
  id: number
  workflow_id: number
  version: string
  status: WorkflowStatus
  graph: unknown
  api_prompt: ComfyApiPrompt | null
  manifest: WorkflowManifestPublic
  download_gb: number
  manifest_hash: string
  changelog: string
  published_at: string | null
  created_at: string
  updated_at: string
}

/**
 * POST / PUT /workflows/{id}/versions[/{vid}] 的请求体。PUT 草稿：省略的字段沿用当前值，api_prompt 显式 null = 去掉快速出图；
 * PUT 已发布：只能改 changelog 或 status = retired。
 */
export interface AdminWorkflowVersionInput {
  version?: string
  graph?: unknown
  api_prompt?: ComfyApiPrompt | null
  manifest?: WorkflowManifestPublic
  changelog?: string
  copy_from?: number
  status?: WorkflowStatus
}

/** ?validate=1 的响应（只校验不保存）。 */
export interface WorkflowValidateResult {
  ok: boolean
  manifest_hash: string
  download_gb: number
  warnings: string[]
}

/** GET /workflows/{id}/installs 的一行（按加入时间倒序）。 */
export interface AdminWorkflowInstall {
  deployment_id: string
  deployment_state: DeploymentState
  instance_id: string
  user_id: number
  phone: string
  version_id: number
  version: string
  state: WorkflowInstallState
  added_at: string
  installed_at: string | null
}
