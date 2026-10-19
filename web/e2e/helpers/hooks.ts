import { request, type APIRequestContext } from '@playwright/test'

export const HOOKS_URL = 'http://127.0.0.1:18090'
export const CONSOLE_URL = 'http://127.0.0.1:18080'
export const ADMIN_URL = 'http://127.0.0.1:18081'
/** 控制台的对外地址（SPARK_PUBLIC_BASE_URL）：实例 Web 入口的 authorize / 登录跳转都回到这里。
 *  spark.test 与 ins-*-<port>.spark.test 由 console project 的 --host-resolver-rules 解析到 127.0.0.1。 */
export const CONSOLE_SITE_URL = 'http://spark.test:18080'

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
  /** 用进程内 ssh 客户端经 SSH 网关执行命令：验证页面上显示的密码 / 公钥真的能登录。
   *  error 非空表示连接或认证失败（exit_status 为 -1）；网关的 Banner 并在 output 开头。 */
  ssh(opts: { instance_id: string; password?: string; key_pem?: string; command: string }) {
    return post<{ output: string; exit_status: number; error: string }>('/hooks/ssh', opts)
  },
  /** 让假容器 sshd 在容器内 127.0.0.1:<port> 回显 `body|host=<Host>|path=<RequestURI>`，并立即刷新入口路由表。
   *  先经 API 加端口、再调本钩子（路由表只收录已登记的端口）。所有实例共用同一个假容器：同一端口后登记的覆盖先登记的。 */
  async ingressUpstream(port: number, body: string): Promise<void> {
    await post('/hooks/ingress-upstream', { port, body })
  },
}
