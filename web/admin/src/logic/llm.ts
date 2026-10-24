import {
  centsToYuanInput, fmtYuan, yuanToCents,
  deploymentPhaseLabel, deploymentStateLabel, fmtTime, labelOf,
  type AdminAppVersion, type AdminGroup, type AdminGroupBrief, type AdminImage, type AppEnvSpec, type LLMModelAdmin, type LLMModelInput, type LLMModelKind, type LLMModelSource,
  type LLMNode, type LLMNodeState, type LLMPoolMachine, type Role,
} from '@spark/shared'
import { ALL_ROLES, FIN_ROLES, OPS_ROLES } from '../menu'

// Token 商城后台（规格 §3.1、§10.2、§11）：价格单位「分 / 百万 token」，表单里按「元 / 百万 token」两位小数输入。

// ---- Tab ----

export type LlmTabKey = 'overview' | 'models' | 'nodes' | 'accounts' | 'usage'
export interface LlmTab { key: LlmTabKey; label: string; roles: readonly Role[] }

/** 页内 Tab（路由本身 ALL_ROLES；接口层仍按规格 §10.2 的角色 403）。 */
export const LLM_TABS: readonly LlmTab[] = [
  { key: 'overview', label: '总览', roles: ALL_ROLES },
  { key: 'models', label: '模型', roles: ALL_ROLES },
  { key: 'nodes', label: '节点与机器池', roles: OPS_ROLES },
  { key: 'accounts', label: '账户', roles: ALL_ROLES }, // ops 改限额 / 停用，finance 查看
  { key: 'usage', label: '用量', roles: FIN_ROLES },
]

export const visibleTabs = (role: Role): LlmTab[] => LLM_TABS.filter((t) => t.roles.includes(role))

/** ?tab= → 当前角色可见的 Tab；缺省、未知或无权时回到总览。 */
export function tabFromQuery(q: unknown, role: Role): LlmTabKey {
  const v = Array.isArray(q) ? q[0] : q
  const t = visibleTabs(role).find((x) => x.key === v)
  return t ? t.key : 'overview'
}

// ---- 价格 ----

/** 元（字符串，≤ 2 位小数，≥ 0）→ 分；不合法返回 null（不经浮点乘法）。 */
export const yuanInputToFen = (s: string): number | null => yuanToCents(s)
/** 分 → 输入框的元字符串（`1.50`）。 */
export const fenToYuanInput = (fen: number): string => centsToYuanInput(fen)
/** 表格显示：`¥1.50`（元 / 百万 token）。 */
export const fmtPrice = (fen: number): string => fmtYuan(fen)

// ---- 模型表单 ----

export const LLM_FEATURES = ['tools', 'reasoning', 'vision', 'long-context'] as const
export const llmFeatureLabel: Record<string, string> = { tools: '工具调用', reasoning: '推理', vision: '视觉', 'long-context': '长上下文' }
export const llmSourceLabel: Record<string, string> = { modelscope: 'ModelScope', 'hf-mirror': 'HF 镜像' }

/** 由平台在建节点时填写的 vllm-node 变量（规格 §3.1 五个 + 按 kind 填的 TASK），表单不展示、不提交。 */
export const RESERVED_NODE_ENV: readonly string[] = ['MODEL_ID', 'MODEL_SOURCE', 'SERVED_MODEL_NAME', 'MAX_MODEL_LEN', 'API_KEY', 'TASK']

export const SLUG_RE = /^[a-z0-9][a-z0-9.-]{1,63}$/
export const SOURCE_ID_RE = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/
export const LIMITS = {
  name: 64,
  description: 2000,
  context_len: [1024, 1048576],
  max_output_tokens: [1, 262144],
  replicas: [0, 200],
  max_concurrency: [1, 512],
} as const

/** vllm-node spec 里可由模型覆盖的变量。 */
export function nodeEnvSpec(env: AppEnvSpec[] | undefined): AppEnvSpec[] {
  return (env ?? []).filter((e) => !RESERVED_NODE_ENV.includes(e.name))
}

export interface ModelForm {
  slug: string
  name: string
  description: string
  kind: LLMModelKind
  source: LLMModelSource
  source_id: string
  image_id: number | undefined
  /** null = 建节点时取 vllm-node 最新已发布版本 */
  app_version_id: number | null
  context_len: number | undefined
  max_output_tokens: number | undefined
  default_max_tokens: number | undefined
  /** 元 / 百万 token */
  input_price: string
  output_price: string
  /**
   * spec 里每个可覆盖变量一项（空串 = 用 spec 默认值，不提交），另含已存的、不在所选版本 spec 里的变量（extraEnvKeys）：
   * 后者只读展示「将被移除」，drop_extra_env 确认后才从请求里去掉，否则不能保存。
   */
  node_env: Record<string, string>
  drop_extra_env: boolean
  replicas: number | undefined
  max_concurrency: number | undefined
  features: string[]
  sort: number | undefined
}

export function emptyModelForm(envSpec: AppEnvSpec[] = []): ModelForm {
  return {
    slug: '', name: '', description: '', kind: 'chat', source: 'modelscope', source_id: '', image_id: undefined, app_version_id: null,
    context_len: 32768, max_output_tokens: 8192, default_max_tokens: 4096, input_price: '', output_price: '',
    node_env: Object.fromEntries(envSpec.map((e) => [e.name, ''])), drop_extra_env: false, replicas: 0, max_concurrency: 32, features: [], sort: 0,
  }
}

export function modelToForm(m: LLMModelAdmin, envSpec: AppEnvSpec[]): ModelForm {
  return {
    slug: m.slug, name: m.name, description: m.description, kind: m.kind, source: m.source, source_id: m.source_id, image_id: m.image_id,
    app_version_id: m.app_version_id, context_len: m.context_len, max_output_tokens: m.max_output_tokens, default_max_tokens: m.default_max_tokens,
    input_price: fenToYuanInput(m.input_price), output_price: fenToYuanInput(m.output_price),
    node_env: { ...Object.fromEntries(envSpec.map((e) => [e.name, ''])), ...(m.node_env ?? {}) },
    drop_extra_env: false,
    replicas: m.replicas, max_concurrency: m.max_concurrency, features: [...m.features], sort: m.sort,
  }
}

// 与 internal/appdeploy ValidateEnv 一致：max_len 缺省 256、secret 至少 8 字符、pattern 全匹配
const DEFAULT_MAX_LEN = 256
const MIN_SECRET_LEN = 8
const byteLen = (v: string) => new TextEncoder().encode(v).length

/** pattern 按 Go 的全匹配编译；JS 编译不了的（RE2 专有语法）跳过前端校验，交给服务器。 */
function fullMatch(pattern: string): RegExp | null {
  try {
    return new RegExp(`^(?:${pattern})$`, 'u')
  } catch {
    return null
  }
}

/** 单个变量值的前端预校验（服务器仍是准绳）；null = 通过。空值：有默认值或非必填即通过。 */
export function validateEnvValue(e: AppEnvSpec, v: string): string | null {
  if (v === '') return e.required && !e.default ? `请填写 ${e.label || e.name}` : null
  if (/[\n\r\0]/.test(v)) return '不能包含换行或 NUL'
  const maxLen = e.max_len || DEFAULT_MAX_LEN
  if (byteLen(v) > maxLen) return `最多 ${maxLen} 字节`
  switch (e.type) {
    case 'int': {
      if (!/^[+-]?[0-9]+$/.test(v)) return '必须是整数'
      const n = BigInt(v)
      if ((e.min != null && n < BigInt(e.min)) || (e.max != null && n > BigInt(e.max))) return '超出允许范围'
      return null
    }
    case 'bool':
      return v === 'true' || v === 'false' ? null : '只能是 true 或 false'
    case 'enum':
      return e.options?.includes(v) ? null : '不在可选项内'
    case 'secret':
      if (byteLen(v) < MIN_SECRET_LEN) return `至少 ${MIN_SECRET_LEN} 个字符`
    // falls through：secret 同样检查 pattern
    case 'string': {
      const re = e.pattern ? fullMatch(e.pattern) : null
      return re && !re.test(v) ? '格式不正确' : null
    }
  }
  return null
}

/** 表单里有值、但不在所选版本 spec（非保留变量）里的变量名：版本换了或 spec 删了变量。 */
export function extraEnvKeys(f: Pick<ModelForm, 'node_env'>, envSpec: AppEnvSpec[]): string[] {
  const names = new Set(envSpec.map((e) => e.name))
  return Object.entries(f.node_env).filter(([k, v]) => v !== '' && !names.has(k)).map(([k]) => k).sort()
}

const isInt = (v: number | undefined | null): v is number => typeof v === 'number' && Number.isInteger(v)
const inRange = (v: number | undefined | null, [lo, hi]: readonly [number, number]) => isInt(v) && v >= lo && v <= hi
const charLen = (s: string) => [...s].length

export type ModelFormResult = { ok: true; input: LLMModelInput } | { ok: false; errors: Record<string, string> }

/**
 * 表单 → 请求体，并按规格 §3.1 预校验（服务器仍会再校验）。errors 的键是字段名，node_env 为 `node_env.<变量>`。
 * node_env 只提交 spec 里声明的非保留变量、去掉空值（空 = 用 spec 默认值）；spec 外的已存变量须 drop_extra_env 确认才去掉。编辑时不校验 slug（不可改，PUT 不带它）。
 */
export function validateModelForm(
  f: ModelForm,
  opt: { creating: boolean; envSpec: AppEnvSpec[]; /** 所选 vllm-node 版本兼容且启用的镜像；不传则不在前端校验 */ allowedImageIds?: number[] },
): ModelFormResult {
  const errors: Record<string, string> = {}
  const name = f.name.trim()
  const sourceId = f.source_id.trim()
  if (opt.creating && !SLUG_RE.test(f.slug)) errors.slug = '2～64 位小写字母、数字、点或连字符，以字母或数字开头'
  if (!name) errors.name = '请填写名称'
  else if (charLen(name) > LIMITS.name) errors.name = `名称最多 ${LIMITS.name} 个字`
  if (charLen(f.description) > LIMITS.description) errors.description = `介绍最多 ${LIMITS.description} 个字`
  if (f.kind !== 'chat' && f.kind !== 'embedding') errors.kind = '请选择类型'
  if (f.source !== 'modelscope' && f.source !== 'hf-mirror') errors.source = '请选择下载源'
  if (!SOURCE_ID_RE.test(sourceId)) errors.source_id = '格式为「组织/模型」，如 Qwen/Qwen3-32B'
  if (!isInt(f.image_id)) errors.image_id = '请选择镜像'
  else if (opt.allowedImageIds && !opt.allowedImageIds.includes(f.image_id)) errors.image_id = '该镜像不兼容所选 vllm-node 版本或已停用，请重新选择'
  if (!inRange(f.context_len, LIMITS.context_len)) errors.context_len = '上下文长度需在 1024～1048576 之间'
  if (!inRange(f.max_output_tokens, LIMITS.max_output_tokens)) errors.max_output_tokens = '最大输出需在 1～262144 之间'
  if (!isInt(f.default_max_tokens) || f.default_max_tokens < 1) errors.default_max_tokens = '默认输出需为正整数'
  else if (isInt(f.max_output_tokens) && f.default_max_tokens > f.max_output_tokens) errors.default_max_tokens = '默认输出不能超过最大输出'
  const input = yuanInputToFen(f.input_price)
  const output = yuanInputToFen(f.output_price)
  if (input == null) errors.input_price = '价格格式不正确（元，最多两位小数）'
  if (output == null) errors.output_price = '价格格式不正确（元，最多两位小数）'
  if (!inRange(f.replicas, LIMITS.replicas)) errors.replicas = '副本数需在 0～200 之间'
  if (!inRange(f.max_concurrency, LIMITS.max_concurrency)) errors.max_concurrency = '单节点并发需在 1～512 之间'
  if (f.features.some((x) => !(LLM_FEATURES as readonly string[]).includes(x))) errors.features = '标签不在可选项内'
  if (f.sort !== undefined && f.sort !== null && !isInt(f.sort)) errors.sort = '排序需为整数'

  const nodeEnv: Record<string, string> = {}
  for (const e of opt.envSpec) {
    const v = f.node_env[e.name] ?? ''
    const err = validateEnvValue(e, v)
    if (err) errors[`node_env.${e.name}`] = err
    else if (v !== '') nodeEnv[e.name] = v
  }
  const extra = extraEnvKeys(f, opt.envSpec)
  if (extra.length && !f.drop_extra_env) errors.node_env_extra = `${extra.join('、')} 不在所选 vllm-node 版本的变量里，保存时将被移除：请勾选确认`
  if (Object.keys(errors).length) return { ok: false, errors }
  return {
    ok: true,
    input: {
      slug: f.slug, name, description: f.description, kind: f.kind, source: f.source, source_id: sourceId,
      image_id: f.image_id!, app_version_id: f.app_version_id ?? null, context_len: f.context_len!, max_output_tokens: f.max_output_tokens!,
      default_max_tokens: f.default_max_tokens!, input_price: input!, output_price: output!, node_env: nodeEnv,
      replicas: f.replicas!, max_concurrency: f.max_concurrency!, features: [...f.features], sort: f.sort ?? 0,
    },
  }
}

function sameEnv(a: Record<string, string> | undefined, b: Record<string, string> | undefined): boolean {
  const x = Object.entries(a ?? {}).filter(([, v]) => v !== '')
  const y = Object.entries(b ?? {}).filter(([, v]) => v !== '')
  return x.length === y.length && x.every(([k, v]) => b?.[k] === v)
}

/** 只影响之后新建节点的参数（规格 §3.1）是否有变化。 */
export function nodeParamsChanged(before: LLMModelAdmin, after: LLMModelInput): boolean {
  return (
    before.source !== after.source ||
    before.source_id !== after.source_id ||
    before.image_id !== after.image_id ||
    (before.app_version_id ?? null) !== (after.app_version_id ?? null) ||
    before.context_len !== after.context_len ||
    before.max_output_tokens !== after.max_output_tokens ||
    !sameEnv(before.node_env, after.node_env)
  )
}

/** 保存时的提示：节点参数变了且已有节点（新建 / 没有节点时为 null）。 */
export function staleNodesWarning(before: LLMModelAdmin | null, after: LLMModelInput): string | null {
  if (!before || before.nodes.total <= 0 || !nodeParamsChanged(before, after)) return null
  return `已有 ${before.nodes.total} 个节点仍按旧参数运行，可逐个重建`
}

// ---- vllm-node 版本与镜像 ----

/** 与 appdeploy.Catalog.LatestPublished 一致：已发布版本中 published_at 最新、同时刻取 id 大。 */
export function latestPublished(vs: AdminAppVersion[]): AdminAppVersion | undefined {
  return vs
    .filter((v) => v.status === 'published')
    .sort((a, b) => Date.parse(b.published_at ?? '') - Date.parse(a.published_at ?? '') || b.id - a.id)[0]
}

/**
 * 镜像下拉：所选 vllm-node 版本兼容且启用的镜像；当前已选的即使停用 / 不兼容也保留（imageNote 标出，validateModelForm 拦下）。
 * 没有版本信息时列出全部启用镜像。
 */
export function compatibleImages(v: AdminAppVersion | undefined, images: AdminImage[], current?: number): AdminImage[] {
  return images.filter((i) => i.id === current || (i.enabled && (!v || v.image_ids.includes(i.id))))
}

/** 镜像下拉里的标注：不兼容所选版本 / 已停用；可选时为空串。 */
export function imageNote(v: AdminAppVersion | undefined, img: AdminImage): string {
  if (v && !v.image_ids.includes(img.id)) return '不兼容所选版本'
  if (!img.enabled) return '已停用'
  return ''
}

/** 实际期望副本：只有上线的模型按 replicas 建节点（草稿 / 下线为 0）。总览与模型表共用。 */
export const wantedReplicas = (m: Pick<LLMModelAdmin, 'status' | 'replicas'>): number => (m.status === 'online' ? m.replicas : 0)

// ---- 节点与机器池（规格 §5.1、§10.2、§11）----

/** 节点 Tab 自动刷新间隔（Tab 可见时）。 */
export const NODES_REFRESH_MS = 10_000

/** 机器所在机组是不是 Token 服务池：详情里的 group 带 purpose 时直接用，否则按 GET /groups 的 purpose 找。 */
export function isPoolGroup(groupId: number | null, brief: Pick<AdminGroupBrief, 'id' | 'purpose'> | null, groups: Pick<AdminGroup, 'id' | 'purpose'>[]): boolean {
  if (groupId == null) return false
  if (brief?.id === groupId && brief.purpose) return brief.purpose === 'llm'
  return groups.find((g) => g.id === groupId)?.purpose === 'llm'
}

/** 排空：ready / unhealthy / provisioning → draining（后端同规则，其余 llm_node_invalid_state）。 */
export const canDrainNode = (s: LLMNodeState) => s === 'ready' || s === 'unhealthy' || s === 'provisioning'
/** 重建：ready / unhealthy → Redeploy；failed → 在同机器新建节点行。 */
export const canRebuildNode = (s: LLMNodeState) => s === 'ready' || s === 'unhealthy' || s === 'failed'
/** 移除：任何状态都可立即释放；已在移除 / 已移除的不再显示。 */
export const canRemoveNode = (s: LLMNodeState) => s !== 'removing' && s !== 'removed'

/** 移除原因上限（字符，去首尾空白后），与强制卸载一致。 */
export const NODE_REMOVE_REASON_MAX = 200
export function validateNodeRemoveReason(reason: string): string | null {
  const r = reason.trim()
  if (!r) return '请填写移除原因'
  if ([...r].length > NODE_REMOVE_REASON_MAX) return `原因最多 ${NODE_REMOVE_REASON_MAX} 个字符`
  return null
}

/** 节点表「部署进度」：未建部署「—」；部署中「阶段 百分比」；排队且在等上一次部署「阶段」；其余显示部署状态中文。 */
export function nodeDeployText(n: Pick<LLMNode, 'deployment'>): string {
  const d = n.deployment
  if (!d) return '—'
  if (d.state === 'installing') return `${d.phase ? labelOf(deploymentPhaseLabel, d.phase) + ' ' : ''}${d.progress}%`
  if (d.state === 'queued' && d.phase) return labelOf(deploymentPhaseLabel, d.phase)
  return labelOf(deploymentStateLabel, d.state)
}

/** 机器池「租约到期」：有租约且还没有节点 →「等待租约结束（时间）」；有节点时只显示时间；没有租约「—」。 */
export function poolLeaseText(m: Pick<LLMPoolMachine, 'lease_until' | 'node'>): string {
  if (!m.lease_until) return '—'
  return m.node ? fmtTime(m.lease_until) : `等待租约结束（${fmtTime(m.lease_until)}）`
}

/** 加入机器对话框的候选来源：'none' = 未分组；数字 = 某个出租机组（加入会把机器移出原机组）。 */
export type PoolCandidateSource = 'none' | number
export const POOL_CANDIDATE_PAGE = 50
export function poolCandidateQuery(source: PoolCandidateSource, rack: string, page: number): Record<string, string | number | undefined> {
  return { group_id: source, rack: rack.trim() || undefined, page, page_size: POOL_CANDIDATE_PAGE }
}
