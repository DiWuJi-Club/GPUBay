import type { AppsSummaryItem } from './apps'
import type { Duration } from './catalog'

export type InstanceState = 'reserved' | 'creating' | 'running' | 'stopped' | 'wiping' | 'released' | 'failed' | 'cancelled'

export interface RenewOption {
  duration: Duration
  quantity_max: number
}

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
  password_auth: boolean
  ssh_host: string
  ssh_port: number
  ports_count: number
  scheduled_start_at: string | null
  scheduled_end_at: string | null
  cancellable_until: string | null
  /** 按服务器时钟距取消截止的秒数（0 = 不可取消）；前端按拿到数据的时刻递减 */
  cancellable_seconds: number
  /** 预订已开机但未到开始时刻：按服务器时钟距开始的秒数，在此之前剩余时长不递减；其余为 0 */
  starts_in_seconds: number
  group_id: number | null
  group_name: string
  machine_label: string
  batch_id: string | null
  reassign_count: number
  auto_renew: boolean
  auto_renew_duration: Duration
  auto_renew_failed_reason: string
  renewable_until: string | null
  renew_options: RenewOption[]
  /** 非终态应用部署的摘要（列表 / 详情；后端总是返回数组，旧数据与测试夹具可省略） */
  apps_summary?: AppsSummaryItem[]
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
  quantity: number
  amount: number
  status: OrderStatus
  refunded_amount: number
  batch_id: string | null
  auto: boolean
  created_at: string
}
