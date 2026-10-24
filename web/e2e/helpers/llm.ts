import { expect, request, type APIRequestContext } from '@playwright/test'
import { adminApi, adminPost, createAdmin, getJSON, type SeededAdmin } from './admin'
import { CONSOLE_URL, hooks } from './hooks'
import { runStamp } from './phone'

// Token 商城（计划 9）用例的公共部分。
// - 模型经后台真实 API 建（super；上线要动态码，adminPost 自动补）；节点不经 Controller，用 /hooks/llm/node 直接造 ready 节点
//   （系统实例 running、没有部署），上游是 /hooks/llm/upstream 挂在假容器 8000 端口的 FakeVLLM（所有节点共用）。
// - 模型与节点是全局状态：网关、控制台模型列表、Controller 对账都看全部已上线模型。所以每个用例造的模型 / 池机器都登记在这里，
//   afterEach 的 cleanupLLM 把模型下线 → 节点排空 → 释放系统实例（fake Agent 洗机）→ removed，再把机器移出 Token 服务池
//   （变回未分组的空闲机器，之后的租用用例照常可用）。不清理的话，之后任何一次 /hooks/llm/tick 都会为「已上线但缺节点」的
//   模型在池内空闲机器上补建节点、或告警「Token 服务池机器不足」。
// - /hooks/llm/tick 一律 probe=false：钩子节点没有部署，探测会计失败，3 轮后转不健康、再一轮失败（与是否修复探测无关，用例都成立）。

export type LLMModelJSON = {
  id: number
  slug: string
  name: string
  kind: string
  status: string
  input_price: number
  output_price: number
  replicas: number
  sort: number
}

export type SeededModel = LLMModelJSON & { admin: SeededAdmin }

let modelSeq = 0
/** 小写字母 / 数字 / 连字符，满足后端 slug 规则；runStamp 保证跨 worker 重启唯一。 */
export function uniqueSlug(): string {
  modelSeq += 1
  return `e2e-${runStamp}-${modelSeq}`
}

const ownedModels: SeededModel[] = []
const ownedMachines: number[] = []

/** LLM Serving 镜像（/hooks/apps/seed 建的，vllm-node 的兼容镜像）。 */
async function servingImage(ctx: APIRequestContext): Promise<number> {
  const images = (await getJSON<{ items: { id: number; name: string; enabled: boolean }[] }>(ctx, '/admin/api/v1/images')).items
  const img = images.find((i) => i.name === 'LLM Serving' && i.enabled)
  if (!img) throw new Error('seedLLMModel: 找不到镜像 LLM Serving（/hooks/apps/seed 应已创建）')
  return img.id
}

/**
 * 经后台 API 建一个模型（super 管理员），缺省上线（动态码由 adminPost 补）、1 个副本、价格 ¥0.50 / ¥1.50 每百万 token。
 * 先调 /hooks/apps/seed（幂等）：建模型要求内部应用 vllm-node 已发布。模型登记到 cleanupLLM。
 */
export async function seedLLMModel(opts: { slug?: string; name?: string; input_price?: number; output_price?: number; online?: boolean; replicas?: number; sort?: number; kind?: 'chat' | 'embedding'; features?: string[] } = {}): Promise<SeededModel> {
  await hooks.appsSeed()
  const admin = await createAdmin('super')
  const ctx = await adminApi(admin)
  const slug = opts.slug ?? uniqueSlug()
  const body = {
    slug, name: opts.name ?? `E2E 模型 ${slug}`, kind: opts.kind ?? 'chat', source: 'modelscope', source_id: 'Qwen/Qwen3-8B',
    image_id: await servingImage(ctx), context_len: 32768, max_output_tokens: 8192, default_max_tokens: 1024,
    input_price: opts.input_price ?? 50, output_price: opts.output_price ?? 150, replicas: opts.replicas ?? 1, max_concurrency: 32,
    features: opts.features ?? [], sort: opts.sort ?? 0, node_env: {},
  }
  let m = await adminPost<LLMModelJSON>(ctx, admin, '/admin/api/v1/llm/models', body)
  const seeded: SeededModel = { ...m, admin }
  ownedModels.push(seeded)
  if (opts.online ?? true) {
    m = await adminPost<LLMModelJSON>(ctx, admin, `/admin/api/v1/llm/models/${m.id}/status`, { status: 'online' })
    Object.assign(seeded, m)
  }
  await ctx.dispose()
  return seeded
}

/** Token 服务池的机组 ID。 */
export async function poolGroupId(ctx: APIRequestContext): Promise<number> {
  return (await getJSON<{ group_id: number }>(ctx, '/admin/api/v1/llm/pool')).group_id
}

/** 在 Token 服务池里登记一台新机器（fake Agent 在线、空闲），登记到 cleanupLLM（结束时移出池）。 */
export async function poolMachine(admin: SeededAdmin, rack?: string): Promise<number> {
  const ctx = await adminApi(admin)
  const pool = await poolGroupId(ctx)
  await ctx.dispose()
  const [id] = await hooks.machines(1, undefined, rack, pool)
  ownedMachines.push(id)
  return id
}

/** 登记一台池外机器给 cleanupLLM：用例经界面把它加入池时用（结束时同样移出池）。 */
export function ownPoolMachine(id: number) {
  ownedMachines.push(id)
}

/**
 * 池里加一台新机器、挂上游 FakeVLLM（served = 当前全部已上线模型，所以模型要先上线）、在机器上造 ready 节点。
 * upstream 给出时按它配置 FakeVLLM（如 usage token 数、前 N 次失败）。
 */
export async function createPoolMachineAndNode(model: SeededModel, upstream: Parameters<typeof hooks.llmUpstream>[0] = {}) {
  const machineId = await poolMachine(model.admin)
  await hooks.llmUpstream(upstream)
  const { node_id, instance_id } = await hooks.llmNode(model.id, machineId)
  return { machineId, nodeId: node_id, instanceId: instance_id }
}

export type NodeJSON = { id: string; model_id: number; machine_id: number; instance_id: string | null; state: string }

export async function nodesOf(ctx: APIRequestContext, modelId: number): Promise<NodeJSON[]> {
  return (await getJSON<{ items: NodeJSON[] }>(ctx, `/admin/api/v1/llm/nodes?model_id=${modelId}`)).items
}

const TERMINAL = new Set(['removed', 'failed'])

/** 推进 Controller（probe=false）直到 modelId 的节点全部终结：每轮推进 5 秒（排空要等 2 × 路由刷新 = 4 秒）、tick、
 *  让对应机器的 fake Agent 执行停机 / 洗机。 */
export async function settleNodes(ctx: APIRequestContext, modelId: number, rounds = 12) {
  for (let i = 0; ; i++) {
    const live = (await nodesOf(ctx, modelId)).filter((n) => !TERMINAL.has(n.state))
    if (!live.length) return
    if (i >= rounds) throw new Error(`settleNodes: 模型 ${modelId} 推进 ${rounds} 轮后仍有节点 ${live.map((n) => `${n.id}=${n.state}`).join(', ')}`)
    await hooks.advance(5)
    const rep = await hooks.llmTick()
    if (rep.error) throw new Error(`llm tick: ${rep.error}`)
    for (const n of live) await hooks.agent({ machine_id: n.machine_id })
  }
}

/** afterEach：本用例的模型全部下线并把节点推进到终态，池机器移出池（失败的用例也清理）。 */
export async function cleanupLLM() {
  const models = ownedModels.splice(0)
  const machines = ownedMachines.splice(0)
  if (!models.length && !machines.length) return
  const admin = models[0]?.admin ?? (await createAdmin('super'))
  const ctx = await adminApi(admin) // 重新登录：用例里推进过假时钟，后台会话可能已空闲失效
  try {
    const list = (await getJSON<{ items: LLMModelJSON[] }>(ctx, '/admin/api/v1/llm/models')).items
    for (const m of models) {
      if (list.find((x) => x.id === m.id)?.status === 'online') await adminPost(ctx, admin, `/admin/api/v1/llm/models/${m.id}/status`, { status: 'offline' })
    }
    for (const m of models) await settleNodes(ctx, m.id)
    if (machines.length) {
      const pool = await poolGroupId(ctx)
      const inPool = new Set((await getJSON<{ machines: { machine: { id: number } }[] }>(ctx, '/admin/api/v1/llm/pool')).machines.map((x) => x.machine.id))
      const ids = machines.filter((id) => inPool.has(id))
      if (ids.length) {
        const r = await ctx.delete(`/admin/api/v1/groups/${pool}/machines`, { data: { machine_ids: ids } })
        if (!r.ok()) throw new Error(`移出 Token 服务池 ${ids.join(',')}: ${r.status()} ${await r.text()}`)
      }
    }
  } finally {
    await ctx.dispose()
  }
}

/** 用户 API 建一把 Key（用户须已实名），返回明文与 Key ID。 */
export async function createKey(api: APIRequestContext, name = 'e2e', extra: Record<string, unknown> = {}): Promise<{ key: string; id: number; hint: string }> {
  const r = await api.post('/api/v1/llm/keys', { data: { name, ...extra } })
  if (r.status() !== 201) throw new Error(`POST /llm/keys ${r.status()} ${await r.text()}`)
  const out = (await r.json()) as { key: string; item: { id: number; key_hint: string } }
  return { key: out.key, id: out.item.id, hint: out.item.key_hint }
}

/** 直接调网关 POST /v1/chat/completions（控制台同源的 /v1）；返回状态码、响应体文本与 JSON（非 JSON 为 null）。 */
export async function callChat(key: string, model: string, opts: { stream?: boolean; max_tokens?: number } = {}) {
  const ctx = await request.newContext({ baseURL: CONSOLE_URL })
  try {
    const r = await ctx.post('/v1/chat/completions', {
      headers: { Authorization: `Bearer ${key}` },
      data: { model, messages: [{ role: 'user', content: '你好' }], stream: opts.stream ?? false, max_tokens: opts.max_tokens ?? 16 },
    })
    const text = await r.text()
    let json: Record<string, unknown> | null = null
    try {
      json = JSON.parse(text) as Record<string, unknown>
    } catch {
      json = null
    }
    return { status: r.status(), text, json, requestId: r.headers()['x-request-id'] ?? '' }
  } finally {
    await ctx.dispose()
  }
}

/** 网关的在线模型表是进程内缓存（真实时间 2 秒，llmgw modelCacheTTL）：刚上线的模型要等缓存刷新后才能调用，
 *  否则 404 model_not_found。用这把 Key 轮询 GET /v1/models 直到出现 slug（第一次调用前调用）。 */
export async function waitGatewayModel(key: string, slug: string) {
  const ctx = await request.newContext({ baseURL: CONSOLE_URL })
  try {
    await expect
      .poll(async () => {
        const r = await ctx.get('/v1/models', { headers: { Authorization: `Bearer ${key}` } })
        return r.ok() ? ((await r.json()) as { data: { id: string }[] }).data.map((m) => m.id) : []
      }, { timeout: 10_000 })
      .toContain(slug)
  } finally {
    await ctx.dispose()
  }
}

/** 跑一轮清扫（含 Token 结算）。 */
export const sweep = () => hooks.sweep()
