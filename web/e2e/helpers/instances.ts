import { type APIRequestContext, type Locator, type Page, type TestInfo } from '@playwright/test'
import { isMobile, placeOrder, userApi } from './console'
import { hooks } from './hooks'

export const stateLabels: Record<string, { text: string; type: string }> = {
  creating: { text: '开通中', type: 'primary' },
  running: { text: '运行中', type: 'success' },
  stopped: { text: '已到期停机', type: 'warning' },
  wiping: { text: '洗机中', type: 'info' },
  released: { text: '已释放', type: 'info' },
  failed: { text: '开通失败', type: 'danger' },
}

/** 列表里某个实例所在的行（电脑 el-table 的 row）或卡片（手机）。 */
export function instanceRow(page: Page, info: TestInfo, id: string): Locator {
  const idCell = page.getByTestId('instance-id').filter({ hasText: id })
  return isMobile(info) ? page.getByTestId('instance-card').filter({ has: idCell }) : page.getByRole('row').filter({ has: idCell })
}

/** API 下单并让 Agent 开通成功，返回运行中的实例。 */
export async function runningInstance(phone: string, opts: { duration?: '1d' | '1w' | '1m' } = {}): Promise<{ api: APIRequestContext; id: string }> {
  const api = await userApi(phone)
  await hooks.machines(1)
  const id = await placeOrder(api, opts)
  await hooks.agent()
  return { api, id }
}

/** 控制台实例视图里用例会读到的字段。 */
export type InstanceJSON = {
  state: string
  remaining_seconds: number
  retain_until: string | null
  pending_seconds: number
  suspended: boolean
  password_auth: boolean
  auto_renew: boolean
  auto_renew_duration: '1d' | '1w' | '1m'
  auto_renew_failed_reason: string
  scheduled_start_at: string | null
}

export async function instanceOf(api: APIRequestContext, id: string): Promise<InstanceJSON> {
  const r = await api.get(`/api/v1/instances/${id}`)
  if (!r.ok()) throw new Error(`GET instance ${id} ${r.status()} ${await r.text()}`)
  return (await r.json()) as InstanceJSON
}
