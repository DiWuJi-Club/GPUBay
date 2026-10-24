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
   *  未接入的机器会先被接入。fail=true 时每条指令都按失败回报（result 为原因）。
   *  images 给出时先把该（些）Agent 的已缓存镜像设为这份清单（整体替换），本次及之后的状态上报都带上（覆盖率 / 机器详情）。 */
  agent(opts: { machine_id?: number; fail?: boolean; result?: string; images?: string[] } = {}) {
    return post<{ completed: number }>('/hooks/agent', opts)
  },
  /** 造已接入并连上 fake Agent 的空闲机器；rack 缺省为 E2E；group_id 指定时登记后加入机组。 */
  async machines(count = 1, serial_prefix?: string, rack?: string, group_id?: number): Promise<number[]> {
    return (await post<{ ids: number[] }>('/hooks/machines', { count, serial_prefix, rack, group_id })).ids
  },
  /** 建一个机组（visible 缺省 true）。 */
  groups(name: string, visible = true) {
    return post<{ id: number }>('/hooks/groups', { name, visible })
  },
  /** fake Agent 是否仍连着网关。机器已被撤销接入时会顺手停掉并移除它的 fake Agent：
   *  撤销接入的用例结束前必须调用一次，否则之后的全局 hooks.agent() 会把这台机器悄悄重新接入。 */
  async agentStatus(machine_id: number): Promise<{ connected: boolean }> {
    const r = await (await api()).get(`/hooks/agent/status?machine_id=${machine_id}`)
    if (!r.ok()) throw new Error(`hook /hooks/agent/status -> ${r.status()} ${await r.text()}`)
    return (await r.json()) as { connected: boolean }
  },
  /** 造用户。带 invite_code / device_id / ip 任一时走真实注册路径（发码 + 登录，ip 缺省 127.0.0.1），invite 为绑定结果；
   *  手机号已存在时只是再登录一次（记设备），不改实名、不入账。 */
  users(opts: { phone?: string; verified?: boolean; balance?: number; ledger_rows?: number; invite_code?: string; device_id?: string; ip?: string } = {}) {
    return post<{ id: number; phone: string; invite?: { bound: boolean; reason: string; inviter?: string } | null }>('/hooks/users', opts)
  },
  /** 插一版费率（生效时间 = 现在），未给的字段沿用当前版本。费率是全局的，改了要在 finally 里恢复（helpers/referral.ts restoreRate）。 */
  referralConfig(opts: { rate_bp?: number; max_per_invitee?: number; max_per_inviter_30d?: number; hold_days?: number; period_days?: number }) {
    return post<{ id: number }>('/hooks/referral/config', opts)
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
  /** 让假容器 sshd 在容器内 127.0.0.1:<port> 回显 `body|host=<Host>|path=<RequestURI>|cookie=<到达容器的 Cookie 头>`，并立即刷新入口路由表。
   *  先经 API 加端口、再调本钩子（路由表只收录已登记的端口）。所有实例共用同一个假容器：同一端口后登记的覆盖先登记的。 */
  async ingressUpstream(port: number, body: string): Promise<void> {
    await post('/hooks/ingress-upstream', { port, body })
  },
  /** 创建并发布四个 fake 应用（vllm / open-webui / comfyui / jupyterlab，幂等），并把各应用端口在假容器里转到 fake 应用服务
   *  （回 `spark-app-ok|path=<path>`）。每次调用都重新登记端口：ingressUpstream 会覆盖同端口的目标。 */
  appsSeed() {
    return post<{ apps: { id: number; slug: string; version_id: number; port: number }[] }>('/hooks/apps/seed', {})
  },
  /** 让应用部署 Runner 执行 n 轮（1～200）：每轮先把假时钟推进 5 秒（PollInterval）、真实等 200ms，再 Tick。 */
  appsTick(n = 1) {
    return post<{ rounds: number; processed: number }>('/hooks/apps/tick', { n })
  },
  /** 在假容器 sshd 的端口（缺省 8000）挂一个 FakeVLLM（替换该端口上之前的服务）。served 缺省 = 调用时全部已上线模型的 slug，
   *  所以要先把模型上线再调用；usage 缺省 100 / 50 token；require_key 缺省 ""（接受任意节点密钥）。 */
  llmUpstream(opts: { port?: number; served?: string[]; prompt_tokens?: number; completion_tokens?: number; chunks?: string[]; fail_first_n?: number; delay_ms?: number } = {}) {
    return post<{ ok: boolean; port: number; served: string[] }>('/hooks/llm/upstream', opts)
  },
  /** 在 Token 服务池内的机器（空闲、Agent 在线、没有实例）上直接造一个 ready 节点（系统实例 running，不建部署）。 */
  llmNode(model_id: number, machine_id: number) {
    return post<{ node_id: string; instance_id: string }>('/hooks/llm/node', { model_id, machine_id })
  },
  /** 跑一轮节点 Controller（probe 为真时再跑一轮探测）并刷新网关路由表。钩子造的节点没有部署，探测会对它计失败
   *  （3 轮后不健康、再一轮失败），所以缺省 probe=false：只推进排空 / 移除。 */
  llmTick(probe = false) {
    return post<{ provisioned: number; drained: number; failed: number; removed: number; orphans: number; error: string }>('/hooks/llm/tick', { probe })
  },
}
