import type { WorkflowCategory, WorkflowInput, WorkflowInputType, WorkflowManifestPublic, WorkflowModel, WorkflowNode } from '@spark/shared'

// 后台工作流版本编辑页的纯逻辑（计划 11 规格 §3.2 / §12）：manifest 结构化表单 ↔ JSON、graph / api_prompt 文件解析、
// 前端即时校验（与 internal/comfywf/manifest.go 顶部同样的正则，返回同样的字段路径）。一切以后端校验为准，这里只做表单提示。

// ---- 规格 §3.2 的正则（复制自 internal/comfywf/manifest.go）----
export const SLUG_RE = /^[a-z][a-z0-9-]{1,47}$/
export const VERSION_RE = /^[0-9A-Za-z][0-9A-Za-z.-]{0,31}$/
export const COMFY_MIN_RE = /^\d+\.\d+\.\d+$/
export const MODEL_NAME_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/
export const REPO_RE = /^[A-Za-z0-9][A-Za-z0-9._-]*\/[A-Za-z0-9][A-Za-z0-9._-]*$/
export const FILE_SEG_RE = /^[A-Za-z0-9][A-Za-z0-9._-]*$/
export const SHA256_RE = /^[0-9a-f]{64}$/
export const NODE_NAME_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/
export const GIT_PATH_RE = /^\/[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+(\.git)?$/
export const COMMIT_RE = /^[0-9a-f]{40}$/
export const NODE_TYPE_RE = /^[A-Za-z0-9_.: -]{1,64}$/
export const INPUT_KEY_RE = /^[a-z][a-z0-9_]{0,31}$/

export const MODEL_DIRS = [
  'checkpoints', 'diffusion_models', 'unet', 'text_encoders', 'clip', 'vae', 'loras',
  'upscale_models', 'controlnet', 'clip_vision', 'embeddings', 'style_models', 'vae_approx',
] as const
export const GIT_HOSTS = ['github.com', 'gitee.com', 'gitcode.com'] as const
export const INPUT_TYPES: readonly WorkflowInputType[] = ['text', 'textarea', 'int', 'float', 'bool', 'enum', 'seed', 'image']
export const WORKFLOW_CATEGORIES: readonly WorkflowCategory[] = ['txt2img', 'img2img', 'video', 'upscale', 'other']
/** manifest.outputs[].node 允许的 class_type。 */
export const OUTPUT_CLASS_TYPES = ['SaveImage', 'SaveAnimatedWEBP', 'SaveWEBM', 'VHS_VideoCombine', 'SaveVideo'] as const

export const LIMITS = {
  models: 16, nodes: 8, inputs: 24, outputs: 4, nodeTypes: 64, images: 2, options: 32, optionLen: 64, label: 32,
  maxLen: 4000, defaultMaxLen: 1000, graphBytes: 262144, apiBytes: 262144, changelog: 1000, repoLen: 128, fileLen: 256,
  coverBytes: 2 << 20, name: 48, summary: 120, description: 8000, tags: 8, tagLen: 16, sort: 10000,
} as const
/** 种子上限（不含）：2^53。 */
const MAX_SEED = 2 ** 53

/** 模型加载节点（D16）：UI 格式 widgets_values 里模型文件名的位置、API 格式里的入参名、该节点读取的 models/<dir>。 */
export const MODEL_LOADERS: Record<string, { widgets: number[]; inputs: string[]; dirs: string[] }> = {
  CheckpointLoaderSimple: { widgets: [0], inputs: ['ckpt_name'], dirs: ['checkpoints'] },
  UNETLoader: { widgets: [0], inputs: ['unet_name'], dirs: ['diffusion_models', 'unet'] },
  VAELoader: { widgets: [0], inputs: ['vae_name'], dirs: ['vae'] },
  CLIPLoader: { widgets: [0], inputs: ['clip_name'], dirs: ['text_encoders', 'clip'] },
  DualCLIPLoader: { widgets: [0, 1], inputs: ['clip_name1', 'clip_name2'], dirs: ['text_encoders', 'clip'] },
  TripleCLIPLoader: { widgets: [0, 1, 2], inputs: ['clip_name1', 'clip_name2', 'clip_name3'], dirs: ['text_encoders', 'clip'] },
  LoraLoader: { widgets: [0], inputs: ['lora_name'], dirs: ['loras'] },
  LoraLoaderModelOnly: { widgets: [0], inputs: ['lora_name'], dirs: ['loras'] },
  UpscaleModelLoader: { widgets: [0], inputs: ['model_name'], dirs: ['upscale_models'] },
  ControlNetLoader: { widgets: [0], inputs: ['control_net_name'], dirs: ['controlnet'] },
  CLIPVisionLoader: { widgets: [0], inputs: ['clip_name'], dirs: ['clip_vision'] },
}

export interface FieldIssue {
  path: string
  reason: string
}

// ---- 小工具 ----
const runes = (s: string) => [...s].length
export const utf8Bytes = (s: string) => new TextEncoder().encode(s).length
const round2 = (n: number) => Math.round(n * 100) / 100
const isObj = (v: unknown): v is Record<string, unknown> => typeof v === 'object' && v !== null && !Array.isArray(v)
const isLink = (v: unknown) => Array.isArray(v)

/** 字节数 → `12.3 KB`。 */
export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${round1(n / 1024)} KB`
  return `${round1(n / 1024 / 1024)} MB`
}
const round1 = (n: number) => Math.round(n * 10) / 10

/** 节点 id 排序：都是十进制数字时按数值（"9" < "10"），否则按字典序（同后端 compareNodeID）。 */
export function compareNodeID(a: string, b: string): number {
  const dig = (s: string) => /^\d+$/.test(s)
  if (dig(a) && dig(b)) {
    const ta = a.replace(/^0+/, ''), tb = b.replace(/^0+/, '')
    if (ta.length !== tb.length) return ta.length - tb.length
    if (ta !== tb) return ta < tb ? -1 : 1
  }
  return a < b ? -1 : a > b ? 1 : 0
}

// ---- graph.json / api.json ----

export interface GraphInfo {
  ok: boolean
  /** 空文本 */
  empty: boolean
  error: string
  /** 顶层 nodes 数 */
  nodes: number
  /** 紧凑形式的字节数（解析失败时为原文字节数；后端按紧凑形式计 256KB 上限） */
  bytes: number
  value: unknown
}

/** 解析 graph.json（ComfyUI「保存」的 UI 格式）：须是对象，带 nodes 数组与 links 数组。 */
export function parseGraphFile(text: string): GraphInfo {
  const out: GraphInfo = { ok: false, empty: false, error: '', nodes: 0, bytes: utf8Bytes(text), value: undefined }
  if (!text.trim()) return { ...out, empty: true, error: '不能为空', bytes: 0 }
  let v: unknown
  try {
    v = JSON.parse(text)
  } catch {
    return { ...out, error: '不是合法的 JSON' }
  }
  out.bytes = utf8Bytes(JSON.stringify(v))
  if (!isObj(v)) return { ...out, error: '应为 ComfyUI 保存的工作流 JSON 对象（UI 格式）' }
  if (!Array.isArray(v.nodes)) return { ...out, error: '缺少 nodes 数组（是不是选成了「导出 (API)」的文件？）' }
  if (!Array.isArray(v.links)) return { ...out, error: '缺少 links 数组' }
  out.nodes = v.nodes.length
  out.value = v
  if (out.bytes > LIMITS.graphBytes) return { ...out, error: `不能超过 ${LIMITS.graphBytes / 1024} KB` }
  return { ...out, ok: true }
}

export interface ApiNode {
  id: string
  class_type: string
  /** 非连线的入参名（可作为快速出图参数），保持文件中的顺序 */
  inputs: string[]
  /** 全部入参的现值（连线为 [node_id, slot]） */
  values: Record<string, unknown>
}

export interface ApiInfo {
  ok: boolean
  /** 空文本 = 不支持快速出图（api_prompt 为 null） */
  empty: boolean
  error: string
  nodes: ApiNode[]
  bytes: number
  value: Record<string, unknown> | null
}

/** 解析 api.json（ComfyUI「导出 (API)」）：`{<node_id>: {class_type, inputs}}`；空文本 = 无快速出图。 */
export function parseApiFile(text: string): ApiInfo {
  const out: ApiInfo = { ok: false, empty: false, error: '', nodes: [], bytes: utf8Bytes(text), value: null }
  if (!text.trim() || text.trim() === 'null') return { ...out, ok: true, empty: true, bytes: 0 }
  let v: unknown
  try {
    v = JSON.parse(text)
  } catch {
    return { ...out, error: '不是合法的 JSON' }
  }
  out.bytes = utf8Bytes(JSON.stringify(v))
  if (!isObj(v)) return { ...out, error: '应为 API 格式的 JSON 对象（ComfyUI「导出 (API)」）' }
  const ids = Object.keys(v).sort(compareNodeID)
  if (!ids.length) return { ...out, error: '至少要有一个节点' }
  if (Array.isArray(v.nodes) && Array.isArray(v.links)) return { ...out, error: '这是 UI 格式的工作流；请用 ComfyUI「导出 (API)」' }
  for (const id of ids) {
    const n = v[id]
    if (!isObj(n)) return { ...out, error: `节点 ${id} 应为 {class_type, inputs} 对象` }
    if (typeof n.class_type !== 'string' || !n.class_type.trim()) return { ...out, error: `节点 ${id} 缺少 class_type` }
    if (!isObj(n.inputs)) return { ...out, error: `节点 ${id} 的 inputs 应为对象` }
    const values = n.inputs
    out.nodes.push({ id, class_type: n.class_type, inputs: Object.keys(values).filter((k) => !isLink(values[k])), values })
  }
  out.value = v
  if (out.bytes > LIMITS.apiBytes) return { ...out, error: `不能超过 ${LIMITS.apiBytes / 1024} KB` }
  return { ...out, ok: true }
}

// ---- D16：graph / api_prompt 引用的模型与自定义节点 ----

export interface ModelRef {
  name: string
  loader: string
  /** 出处（如 graph 的「节点 #4 CheckpointLoaderSimple」/ api_prompt 的「api_prompt.4」） */
  where: string
}
export interface GraphRefs {
  models: ModelRef[]
  /** graph 里标为非内置（properties.cnr_id ≠ comfy-core 或有 aux_id）的节点类型，去重 */
  customTypes: string[]
}

function loaderNames(type: string, widgets: unknown): string[] {
  const l = MODEL_LOADERS[type]
  if (!l) return []
  if (Array.isArray(widgets)) return l.widgets.map((k) => widgets[k]).filter((s): s is string => typeof s === 'string' && s !== '')
  if (isObj(widgets)) return l.inputs.map((k) => widgets[k]).filter((s): s is string => typeof s === 'string' && s !== '')
  return []
}

/**
 * 收集 graph（含 definitions.subgraphs）与 api_prompt 引用的模型文件名和非内置节点类型。静音 / 旁路（mode 2 / 4）的节点跳过
 * （与后端一致）。内置节点表只在后端（几百项）：没有 cnr_id / aux_id 的未知节点这里查不出来，以后端校验为准。
 */
export function collectRefs(graph: unknown, api: ApiNode[]): GraphRefs {
  const models: ModelRef[] = []
  const custom = new Set<string>()
  const walk = (nodes: unknown) => {
    if (!Array.isArray(nodes)) return
    for (const n of nodes) {
      if (!isObj(n) || typeof n.type !== 'string' || !n.type) continue
      if (n.mode === 2 || n.mode === 4) continue
      const props = isObj(n.properties) ? n.properties : {}
      if (('cnr_id' in props && props.cnr_id !== 'comfy-core') || 'aux_id' in props) custom.add(n.type)
      for (const name of loaderNames(n.type, n.widgets_values)) models.push({ name, loader: n.type, where: `节点 #${String(n.id)} ${n.type}` })
    }
  }
  if (isObj(graph)) {
    walk(graph.nodes)
    const defs = graph.definitions
    if (isObj(defs) && Array.isArray(defs.subgraphs)) for (const sg of defs.subgraphs) if (isObj(sg)) walk(sg.nodes)
  }
  for (const n of api) {
    for (const k of MODEL_LOADERS[n.class_type]?.inputs ?? []) {
      const s = n.values[k]
      if (typeof s === 'string' && s) models.push({ name: s, loader: n.class_type, where: `api_prompt.${n.id}` })
    }
  }
  return { models, customTypes: [...custom] }
}

export interface MissingRefs {
  /** 引用了但 manifest.models 没声明的模型（去重），dir 为建议目录（该加载节点读取的第一个目录） */
  models: { name: string; dir: string; where: string }[]
  /** graph 里的非内置节点类型，但没有出现在任何 manifest.nodes[].node_types 里 */
  nodeTypes: string[]
}

/** D16 的前端提示：列出全部缺失（后端只报第一个）。 */
export function missingRefs(m: WorkflowManifestPublic, refs: GraphRefs): MissingRefs {
  const declared = new Set(m.models.map((x) => x.name.trim()))
  const seen = new Set<string>()
  const models: MissingRefs['models'] = []
  for (const r of refs.models) {
    if (declared.has(r.name) || seen.has(r.name)) continue
    seen.add(r.name)
    models.push({ name: r.name, dir: MODEL_LOADERS[r.loader]?.dirs[0] ?? 'checkpoints', where: r.where })
  }
  const types = new Set(m.nodes.flatMap((n) => n.node_types.map((t) => t.trim())))
  return { models, nodeTypes: refs.customTypes.filter((t) => !types.has(t)) }
}

// ---- 结构化表单 ----

export interface ModelRow {
  name: string
  dir: string
  size_gb: string
  ms_repo: string
  ms_file: string
  hf_repo: string
  hf_file: string
  sha256: string
}
export interface NodeRow {
  name: string
  git: string
  commit: string
  /** 逗号或换行分隔 */
  node_types: string
}
export interface InputRow {
  key: string
  label: string
  type: WorkflowInputType
  node: string
  input: string
  /** bool 为 'true' / 'false'；image 不用 */
  default: string
  min: string
  max: string
  step: string
  max_len: string
  /** enum：一行一个 */
  options: string
}
export interface OutputRow {
  node: string
}
export interface ManifestForm {
  comfyui_min: string
  models: ModelRow[]
  nodes: NodeRow[]
  inputs: InputRow[]
  outputs: OutputRow[]
}

export const emptyManifest = (): WorkflowManifestPublic => ({ models: [], nodes: [], inputs: [], outputs: [] })
export const emptyModelRow = (): ModelRow => ({ name: '', dir: 'checkpoints', size_gb: '', ms_repo: '', ms_file: '', hf_repo: '', hf_file: '', sha256: '' })
export const emptyNodeRow = (): NodeRow => ({ name: '', git: '', commit: '', node_types: '' })
export const emptyInputRow = (): InputRow => ({ key: '', label: '', type: 'text', node: '', input: '', default: '', min: '', max: '', step: '', max_len: '', options: '' })
export const emptyOutputRow = (): OutputRow => ({ node: '' })

/** 各类型的输入有哪些列。 */
export const inputHas = {
  range: (t: WorkflowInputType) => t === 'int' || t === 'float',
  maxLen: (t: WorkflowInputType) => t === 'text' || t === 'textarea',
  options: (t: WorkflowInputType) => t === 'enum',
  default: (t: WorkflowInputType) => t !== 'image',
}

const str = (v: unknown) => (v === undefined || v === null ? '' : String(v))

export function manifestToForm(m: WorkflowManifestPublic): ManifestForm {
  return {
    comfyui_min: m.comfyui_min ?? '',
    models: (m.models ?? []).map((x) => ({
      name: x.name, dir: x.dir, size_gb: str(x.size_gb), ms_repo: x.modelscope?.repo ?? '', ms_file: x.modelscope?.file ?? '',
      hf_repo: x.hf?.repo ?? '', hf_file: x.hf?.file ?? '', sha256: x.sha256 ?? '',
    })),
    nodes: (m.nodes ?? []).map((n) => ({ name: n.name, git: n.git, commit: n.commit, node_types: (n.node_types ?? []).join(', ') })),
    inputs: (m.inputs ?? []).map((i) => ({
      key: i.key, label: i.label, type: i.type, node: i.node, input: i.input, default: str(i.default), min: str(i.min), max: str(i.max),
      step: str(i.step), max_len: str(i.max_len), options: (i.options ?? []).join('\n'),
    })),
    outputs: (m.outputs ?? []).map((o) => ({ node: o.node })),
  }
}

/** 空串 → undefined；否则 Number（非数字为 NaN，由 manifestIssues 报「需为数字」）。 */
const num = (s: string): number | undefined => (s.trim() === '' ? undefined : Number(s.trim()))
export const splitList = (s: string, sep: RegExp) => s.split(sep).map((x) => x.trim()).filter(Boolean)

/**
 * 表单 → manifest JSON（与后端规范化一致：去空白、dir / sha256 / commit 小写）。
 * 默认值：text / textarea 一律带（可为空串）；其余类型空串 = 不设默认；image 没有默认值。
 */
export function formToManifest(f: ManifestForm): WorkflowManifestPublic {
  const m: WorkflowManifestPublic = {
    models: f.models.map((r) => {
      const md: WorkflowModel = { name: r.name.trim(), dir: r.dir.trim().toLowerCase(), size_gb: num(r.size_gb) ?? NaN, sha256: r.sha256.trim().toLowerCase() }
      if (r.ms_repo.trim() || r.ms_file.trim()) md.modelscope = { repo: r.ms_repo.trim(), file: r.ms_file.trim() }
      if (r.hf_repo.trim() || r.hf_file.trim()) md.hf = { repo: r.hf_repo.trim(), file: r.hf_file.trim() }
      return md
    }),
    nodes: f.nodes.map((r): WorkflowNode => ({ name: r.name.trim(), git: r.git.trim(), commit: r.commit.trim().toLowerCase(), node_types: splitList(r.node_types, /[,\n，]/) })),
    inputs: f.inputs.map((r) => {
      const t = r.type
      const i: WorkflowInput = { key: r.key.trim(), label: r.label.trim(), type: t, node: r.node.trim(), input: r.input.trim() }
      if (t === 'text' || t === 'textarea') i.default = r.default
      else if (t === 'bool') {
        const d = r.default.trim()
        if (d) i.default = d === 'true' ? true : d === 'false' ? false : d
      } else if (t === 'enum') {
        if (r.default.trim()) i.default = r.default.trim()
      } else if (t !== 'image') {
        const d = num(r.default)
        if (d !== undefined) i.default = d
      }
      if (inputHas.range(t)) {
        const [min, max, step] = [num(r.min), num(r.max), num(r.step)]
        if (min !== undefined) i.min = min
        if (max !== undefined) i.max = max
        if (step !== undefined) i.step = step
      }
      if (inputHas.maxLen(t)) {
        const ml = num(r.max_len)
        if (ml !== undefined) i.max_len = ml
      }
      if (inputHas.options(t)) i.options = splitList(r.options, /\n/)
      return i
    }),
    outputs: f.outputs.map((o) => ({ node: o.node.trim() })),
  }
  const min = f.comfyui_min.trim()
  return min ? { comfyui_min: min, ...m } : m
}

/**
 * 在输入表里选定「节点 / 输入」后的建议（只填还空着的格子）：key 取入参名、类型按现值猜（seed / noise_seed → seed，
 * 整数 → int，小数 → float，布尔 → bool，LoadImage.image → image，模型文件名 → enum，其余字符串 → text），
 * 默认值取现值（image / seed 除外），数值给一个包含现值的范围。
 */
export function suggestInput(row: InputRow, node: ApiNode | undefined): Partial<InputRow> {
  if (!node || !(row.input in node.values)) return {}
  const cur = node.values[row.input]
  const out: Partial<InputRow> = {}
  if (!row.key && INPUT_KEY_RE.test(row.input)) out.key = row.input
  if (!row.label) out.label = row.input.slice(0, LIMITS.label)
  let type: WorkflowInputType = 'text'
  if (/^(noise_)?seed$/.test(row.input) && typeof cur === 'number') type = 'seed'
  else if (typeof cur === 'number') type = Number.isInteger(cur) ? 'int' : 'float'
  else if (typeof cur === 'boolean') type = 'bool'
  else if (node.class_type === 'LoadImage' && row.input === 'image') type = 'image'
  else if (MODEL_LOADERS[node.class_type]?.inputs.includes(row.input)) type = 'enum'
  out.type = type
  if (type === 'image' || type === 'seed') out.default = ''
  else out.default = str(cur)
  if (type === 'enum' && !row.options) out.options = str(cur)
  if ((type === 'int' || type === 'float') && typeof cur === 'number') {
    if (!row.min) out.min = String(Math.min(0, cur))
    if (!row.max) out.max = String(Math.max(cur * 4, type === 'int' ? 100 : 10))
  }
  return out
}

/** 表单里有没法发给后端的值（非数字的大小 / 范围 / 默认值，会被 JSON 序列化成 null）。 */
export function hasNaN(m: WorkflowManifestPublic): boolean {
  const bad = (v: unknown) => typeof v === 'number' && Number.isNaN(v)
  return m.models.some((x) => bad(x.size_gb)) || m.inputs.some((i) => [i.default, i.min, i.max, i.step, i.max_len].some(bad))
}

/** 下载量（GB）：Σ size_gb，两位小数（与后端 DownloadGB 一致；非数字的行不计）。 */
export function downloadGB(models: { size_gb: number | string }[]): number {
  let sum = 0
  for (const md of models) {
    const v = typeof md.size_gb === 'string' ? Number(md.size_gb) : md.size_gb
    if (Number.isFinite(v)) sum += round2(v)
  }
  return round2(sum)
}

// ---- 即时校验（规格 §3.2；路径与后端一致）----

function validRepoFile(f: string): boolean {
  if (!f || f.length > LIMITS.fileLen || f.includes('..')) return false
  return f.split('/').every((seg) => FILE_SEG_RE.test(seg))
}

/** Git 地址问题（空串 = 合法）：https、主机白名单（D5）、/owner/repo(.git)。 */
export function gitIssue(s: string): string {
  let u: URL
  try {
    u = new URL(s)
  } catch {
    return '必须是 https:// 地址'
  }
  if (!s.startsWith('https://') || u.protocol !== 'https:') return '必须是 https:// 地址'
  if (u.username || u.password || u.port || u.search || u.hash || s.includes('#') || s.includes('?')) return '不能带用户名、端口、查询参数或片段'
  if (!(GIT_HOSTS as readonly string[]).includes(u.hostname)) return `Git 主机只能是 ${GIT_HOSTS.join(' / ')}`
  if (!GIT_PATH_RE.test(u.pathname)) return '路径应形如 /owner/repo(.git)'
  if (u.pathname.slice(1).split('/').some((seg) => seg.startsWith('.'))) return '路径的每一段不能以 . 开头'
  return ''
}

const isInt = (n: number) => Number.isInteger(n)

function inputAttrIssues(p: string, i: WorkflowInput, out: FieldIssue[]) {
  const push = (path: string, reason: string) => out.push({ path, reason })
  if (inputHas.range(i.type)) {
    if (i.min === undefined) return push(`${p}.min`, '数值参数必须设 min')
    if (Number.isNaN(i.min)) return push(`${p}.min`, '需为数字')
    if (i.max === undefined) return push(`${p}.max`, '数值参数必须设 max')
    if (Number.isNaN(i.max)) return push(`${p}.max`, '需为数字')
    if (i.min > i.max) return push(`${p}.min`, 'min 不能大于 max')
    if (i.type === 'int' && !isInt(i.min)) return push(`${p}.min`, 'int 参数的 min 必须是整数')
    if (i.type === 'int' && !isInt(i.max)) return push(`${p}.max`, 'int 参数的 max 必须是整数')
    if (i.step !== undefined && (Number.isNaN(i.step) || i.step <= 0 || (i.type === 'int' && !isInt(i.step)))) return push(`${p}.step`, 'step 必须为正（int 参数为正整数）')
  }
  if (inputHas.maxLen(i.type) && i.max_len !== undefined && (!isInt(i.max_len) || i.max_len < 1 || i.max_len > LIMITS.maxLen)) {
    return push(`${p}.max_len`, `max_len 应在 1～${LIMITS.maxLen}`)
  }
  if (i.type === 'enum') {
    const opts = i.options ?? []
    if (opts.length < 1 || opts.length > LIMITS.options) return push(`${p}.options`, `选项应有 1～${LIMITS.options} 项`)
    const seen = new Set<string>()
    for (let j = 0; j < opts.length; j++) {
      if (runes(opts[j]) > LIMITS.optionLen || seen.has(opts[j])) return push(`${p}.options[${j}]`, `选项不能为空、不能重复，≤ ${LIMITS.optionLen} 字符`)
      seen.add(opts[j])
    }
  }
  const d = i.default
  if (d === undefined) return
  switch (i.type) {
    case 'text':
    case 'textarea': {
      const ml = i.max_len ?? LIMITS.defaultMaxLen
      if (typeof d === 'string' && runes(d) > ml) push(`${p}.default`, `默认值不能超过 ${ml} 字`)
      return
    }
    case 'int':
    case 'float': {
      if (typeof d !== 'number' || Number.isNaN(d)) return push(`${p}.default`, '默认值需为数字')
      if (i.type === 'int' && !isInt(d)) return push(`${p}.default`, '默认值必须是整数')
      if ((i.min !== undefined && d < i.min) || (i.max !== undefined && d > i.max)) return push(`${p}.default`, `默认值应在 ${i.min}～${i.max}`)
      if (i.type === 'int' && i.step !== undefined && i.step > 0 && (d - (i.min ?? 0)) % i.step !== 0) {
        return push(`${p}.default`, `默认值必须是 ${i.min ?? 0} 加 ${i.step} 的整数倍`)
      }
      return
    }
    case 'seed':
      if (typeof d !== 'number' || !isInt(d) || d < -1 || d >= MAX_SEED) push(`${p}.default`, '种子默认值应为 -1（随机）或 0～2^53-1 的整数')
      return
    case 'bool':
      if (typeof d !== 'boolean') push(`${p}.default`, '默认值必须是 true 或 false')
      return
    case 'enum':
      if (!(i.options ?? []).includes(String(d))) push(`${p}.default`, '默认值不是可选值')
      return
  }
}

function valueMatches(type: WorkflowInputType, cur: unknown): boolean {
  switch (type) {
    case 'text':
    case 'textarea':
    case 'enum':
    case 'image':
      return typeof cur === 'string'
    case 'int':
    case 'float':
      return typeof cur === 'number'
    case 'seed':
      return typeof cur === 'number' && isInt(cur)
    case 'bool':
      return typeof cur === 'boolean'
  }
  return false
}

/**
 * manifest 的即时校验：返回全部发现的问题（每个字段最多一条），路径与后端 workflow_version_invalid 一致
 * （如 manifest.models[0].modelscope.repo）。api 为 null 表示没有 api_prompt（不支持快速出图）。
 * D16 的交叉校验另见 missingRefs；内置节点表与「模型目录与加载节点相符」只在后端校验。
 */
export function manifestIssues(m: WorkflowManifestPublic, api: ApiNode[] | null): FieldIssue[] {
  const out: FieldIssue[] = []
  const push = (path: string, reason: string) => out.push({ path, reason })
  if (m.comfyui_min && !COMFY_MIN_RE.test(m.comfyui_min)) push('manifest.comfyui_min', '格式应为 0.3.60')

  if (m.models.length > LIMITS.models) push('manifest.models', `最多 ${LIMITS.models} 个模型`)
  const names = new Set<string>()
  m.models.forEach((md, i) => {
    const p = `manifest.models[${i}]`
    if (!MODEL_NAME_RE.test(md.name)) push(`${p}.name`, '文件名只能含字母、数字、. _ -，不含 /，≤ 128 字符')
    else if (names.has(md.name)) push(`${p}.name`, `模型 ${md.name} 重复`)
    names.add(md.name)
    if (!(MODEL_DIRS as readonly string[]).includes(md.dir)) push(`${p}.dir`, `目录只能是 ${MODEL_DIRS.join(' / ')}`)
    if (Number.isNaN(md.size_gb)) push(`${p}.size_gb`, '请填写大小（GB）')
    else if (round2(md.size_gb) < 0.01 || md.size_gb > 200) push(`${p}.size_gb`, '大小应在 0.01～200 GB')
    if (!md.modelscope && !md.hf) push(p, 'modelscope 与 hf 至少给一个')
    for (const [key, rf] of [['modelscope', md.modelscope], ['hf', md.hf]] as const) {
      if (!rf) continue
      if (rf.repo.length > LIMITS.repoLen || !REPO_RE.test(rf.repo)) push(`${p}.${key}.repo`, `仓库应形如 owner/name，≤ ${LIMITS.repoLen} 字符`)
      if (!validRepoFile(rf.file)) push(`${p}.${key}.file`, '应为仓库内相对路径（每段以字母或数字开头，不含 ..，≤ 256 字符）')
    }
    if (md.sha256 && !SHA256_RE.test(md.sha256)) push(`${p}.sha256`, '应为空或 64 位十六进制')
  })

  if (m.nodes.length > LIMITS.nodes) push('manifest.nodes', `最多 ${LIMITS.nodes} 个自定义节点`)
  const nodeNames = new Set<string>()
  m.nodes.forEach((n, i) => {
    const p = `manifest.nodes[${i}]`
    if (!NODE_NAME_RE.test(n.name)) push(`${p}.name`, '目录名只能含字母、数字、. _ -，≤ 64 字符')
    else if (nodeNames.has(n.name)) push(`${p}.name`, `节点 ${n.name} 重复`)
    nodeNames.add(n.name)
    const g = gitIssue(n.git)
    if (g) push(`${p}.git`, g)
    if (!COMMIT_RE.test(n.commit)) push(`${p}.commit`, '必须是 40 位十六进制 commit')
    if (n.node_types.length < 1 || n.node_types.length > LIMITS.nodeTypes) push(`${p}.node_types`, `应有 1～${LIMITS.nodeTypes} 项`)
    else {
      const bad = n.node_types.findIndex((t) => !NODE_TYPE_RE.test(t))
      if (bad >= 0) push(`${p}.node_types[${bad}]`, '节点类型只能含字母、数字、_ . : - 与空格，≤ 64 字符')
    }
  })

  if (!api) {
    if (m.inputs.length) push('manifest.inputs', '没有 api_prompt 时不能定义快速出图参数')
    if (m.outputs.length) push('manifest.outputs', '没有 api_prompt 时不能定义输出节点')
    return out
  }
  const byId = new Map(api.map((n) => [n.id, n]))
  if (m.inputs.length > LIMITS.inputs) push('manifest.inputs', `最多 ${LIMITS.inputs} 个参数`)
  const keys = new Set<string>()
  const targets = new Set<string>()
  let images = 0
  m.inputs.forEach((i, idx) => {
    const p = `manifest.inputs[${idx}]`
    if (!INPUT_KEY_RE.test(i.key)) push(`${p}.key`, '只能是小写字母开头的 a-z0-9_，≤ 32 字符')
    else if (keys.has(i.key)) push(`${p}.key`, `参数 ${i.key} 重复`)
    keys.add(i.key)
    if (!i.label || runes(i.label) > LIMITS.label) push(`${p}.label`, `名称必填，≤ ${LIMITS.label} 字符`)
    if (!INPUT_TYPES.includes(i.type)) push(`${p}.type`, `类型只能是 ${INPUT_TYPES.join(' / ')}`)
    const node = byId.get(i.node)
    if (!node) push(`${p}.node`, i.node ? `节点 ${i.node} 不在 api_prompt 里` : '请选择节点')
    else if (!(i.input in node.values)) push(`${p}.input`, i.input ? `节点 ${i.node} 没有输入 ${i.input}` : '请选择输入')
    else if (isLink(node.values[i.input])) push(`${p}.input`, `节点 ${i.node} 的 ${i.input} 是连线，不能作为参数`)
    else {
      const t = `${i.node}\u0000${i.input}`
      if (targets.has(t)) push(`${p}.input`, `节点 ${i.node} 的 ${i.input} 已被其它参数使用`)
      else if (INPUT_TYPES.includes(i.type) && !valueMatches(i.type, node.values[i.input])) push(`${p}.input`, `节点 ${i.node} 的 ${i.input} 现值类型与参数类型 ${i.type} 不符`)
      else if (MODEL_LOADERS[node.class_type]?.inputs.includes(i.input) && i.type !== 'enum') {
        push(`${p}.type`, `${node.class_type} 的 ${i.input} 是模型文件名，只能用 enum 类型`)
      }
      targets.add(t)
    }
    if (i.type === 'image' && ++images > LIMITS.images) push('manifest.inputs', `image 类型最多 ${LIMITS.images} 项`)
    inputAttrIssues(p, i, out)
  })

  if (m.outputs.length > LIMITS.outputs) push('manifest.outputs', `最多 ${LIMITS.outputs} 个输出节点`)
  const outs = new Set<string>()
  m.outputs.forEach((o, i) => {
    const p = `manifest.outputs[${i}].node`
    const n = byId.get(o.node)
    if (!n) push(p, o.node ? `节点 ${o.node} 不在 api_prompt 里` : '请选择节点')
    else if (!(OUTPUT_CLASS_TYPES as readonly string[]).includes(n.class_type)) push(p, `节点 ${o.node} 的类型 ${n.class_type} 不是保存图片 / 视频的节点`)
    else if (outs.has(o.node)) push(p, `输出节点 ${o.node} 重复`)
    outs.add(o.node)
  })
  return out
}

export interface VersionFields {
  version: string
  changelog: string
  graph: GraphInfo
  api: ApiInfo
}

/** 版本号、更新说明、graph、api_prompt 的即时校验（路径同后端）。 */
export function versionIssues(v: VersionFields): FieldIssue[] {
  const out: FieldIssue[] = []
  if (!VERSION_RE.test(v.version.trim())) out.push({ path: 'version', reason: '只能由字母、数字、点和连字符组成（最多 32 个字符），且以字母或数字开头' })
  if (runes(v.changelog.trim()) > LIMITS.changelog) out.push({ path: 'changelog', reason: `最多 ${LIMITS.changelog} 个字符` })
  if (!v.graph.ok) out.push({ path: 'graph', reason: v.graph.error })
  if (!v.api.ok) out.push({ path: 'api_prompt', reason: v.api.error })
  return out
}

// ---- 后端错误定位 ----

/** 拆出 workflow_version_invalid 消息的「路径：原因」（全角冒号）；不是这种格式返回 null。 */
export function splitFieldError(message: string): FieldIssue | null {
  const m = /^((?:manifest|graph|api_prompt|version|changelog)(?:[.[][^：\s]*)?)：([\s\S]*)$/.exec(message)
  return m ? { path: m[1], reason: m[2] } : null
}

export type ManifestTable = 'models' | 'nodes' | 'inputs' | 'outputs'
export interface CellRef {
  table: ManifestTable
  index: number
  /** 表单列名（modelscope.repo → ms_repo、node_types[2] → node_types、options[1] → options）；整行为 '' */
  field: string
}

const FIELD_ALIAS: Record<string, string> = { 'modelscope.repo': 'ms_repo', 'modelscope.file': 'ms_file', 'hf.repo': 'hf_repo', 'hf.file': 'hf_file' }

/** 字段路径 → 表格单元（manifest.models[0].modelscope.repo → {models, 0, ms_repo}）；不是表格行返回 null。 */
export function pathToCell(path: string): CellRef | null {
  const m = /^manifest\.(models|nodes|inputs|outputs)\[(\d+)\](?:\.(.+))?$/.exec(path)
  if (!m) return null
  const raw = (m[3] ?? '').replace(/\[\d+\]$/, '')
  return { table: m[1] as ManifestTable, index: Number(m[2]), field: FIELD_ALIAS[raw] ?? raw }
}

// ---- 工作流基本信息 / 封面 ----

export interface WorkflowFormValue {
  slug: string
  name: string
  summary: string
  description: string
  category: WorkflowCategory
  tags: string
  sort: number
}

/** 标签：中英文逗号分隔，去空白、去重。 */
export const parseTags = (s: string) => [...new Set(splitList(s, /[,，]/))]

/** 工作流基本信息的即时校验，返回第一条问题（空串 = 合法）；checkSlug 为 false（编辑）时不查 slug。 */
export function workflowIssue(f: WorkflowFormValue, checkSlug: boolean): string {
  const tags = parseTags(f.tags)
  if (checkSlug && !SLUG_RE.test(f.slug.trim())) return 'slug 只能是小写字母开头的 2～48 位小写字母、数字或连字符'
  if (!f.name.trim() || runes(f.name.trim()) > LIMITS.name) return `请填写工作流名称（最多 ${LIMITS.name} 个字符）`
  if (runes(f.summary.trim()) > LIMITS.summary) return `简介最多 ${LIMITS.summary} 个字符`
  if (runes(f.description.trim()) > LIMITS.description) return `说明最多 ${LIMITS.description} 个字符`
  if (!WORKFLOW_CATEGORIES.includes(f.category)) return '请选择分类'
  if (tags.length > LIMITS.tags) return `标签最多 ${LIMITS.tags} 个`
  const long = tags.find((t) => runes(t) > LIMITS.tagLen)
  if (long) return `标签「${long}」超过 ${LIMITS.tagLen} 个字符`
  if (!Number.isInteger(f.sort) || Math.abs(f.sort) > LIMITS.sort) return '排序值需在 -10000～10000 之间'
  return ''
}

export const COVER_TYPES = ['image/png', 'image/jpeg', 'image/webp'] as const

/** 封面文件的即时校验（D13：PNG / JPG / WebP，≤ 2MB）；空串 = 合法。 */
export function coverIssue(file: { type: string; size: number }): string {
  if (!(COVER_TYPES as readonly string[]).includes(file.type)) return '封面只能是 PNG、JPG 或 WebP 图片'
  if (file.size > LIMITS.coverBytes) return '封面不能超过 2MB'
  return ''
}
