import { request, type APIRequestContext } from '@playwright/test'

export const HOOKS_URL = 'http://127.0.0.1:18090'
export const CONSOLE_URL = 'http://127.0.0.1:18080'
export const ADMIN_URL = 'http://127.0.0.1:18081'

let ctx: APIRequestContext | undefined
async function api(): Promise<APIRequestContext> {
  ctx ??= await request.newContext({ baseURL: HOOKS_URL })
  return ctx
}

async function post<T>(path: string, body: unknown): Promise<T> {
  const r = await (await api()).post(path, { data: body })
  if (!r.ok()) throw new Error(`hook ${path} -> ${r.status()} ${await r.text()}`)
  return (await r.json()) as T
}

export type Role = 'super' | 'ops' | 'finance'

export const hooks = {
  async clock(): Promise<Date> {
    const r = await (await api()).get('/hooks/clock')
    if (!r.ok()) throw new Error(`hook /hooks/clock -> ${r.status()} ${await r.text()}`)
    return new Date(((await r.json()) as { now: string }).now)
  },
  async advance(seconds: number): Promise<Date> {
    return new Date((await post<{ now: string }>('/hooks/clock/advance', { seconds })).now)
  },
  /** heartbeat（默认 true）：让每个 fake Agent 通过真实流发一次心跳，再跑一轮清扫。 */
  async sweep(opts: { heartbeat?: boolean } = {}): Promise<void> {
    await post('/hooks/sweep', opts)
  },
  /** 让步进式 fake Agent 执行该机器（缺省全部）当前队列里的指令并上报；指令经真实网关投递，返回时平台已处理完结果。
   *  未接入的机器会先被接入。fail=true 时每条指令都按失败回报（result 为原因）。 */
  agent(opts: { machine_id?: number; fail?: boolean; result?: string } = {}) {
    return post<{ completed: number }>('/hooks/agent', opts)
  },
  /** 造已接入并连上 fake Agent 的空闲机器；rack 缺省为 E2E。 */
  async machines(count = 1, serial_prefix?: string, rack?: string): Promise<number[]> {
    return (await post<{ ids: number[] }>('/hooks/machines', { count, serial_prefix, rack })).ids
  },
  /** fake Agent 是否仍连着网关。机器已被撤销接入时会顺手停掉并移除它的 fake Agent：
   *  撤销接入的用例结束前必须调用一次，否则之后的全局 hooks.agent() 会把这台机器悄悄重新接入。 */
  async agentStatus(machine_id: number): Promise<{ connected: boolean }> {
    const r = await (await api()).get(`/hooks/agent/status?machine_id=${machine_id}`)
    if (!r.ok()) throw new Error(`hook /hooks/agent/status -> ${r.status()} ${await r.text()}`)
    return (await r.json()) as { connected: boolean }
  },
  users(opts: { phone?: string; verified?: boolean; balance?: number; ledger_rows?: number } = {}) {
    return post<{ id: number; phone: string }>('/hooks/users', opts)
  },
  admins(opts: { username: string; role: Role; bind_totp: boolean }) {
    return post<{ id: number; password: string; totp_secret?: string; recovery_codes?: string[] }>('/hooks/admins', opts)
  },
  async kyc(reject: boolean): Promise<void> {
    await post('/hooks/kyc', { reject })
  },
}
