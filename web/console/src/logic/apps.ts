import {
  deploymentErrorLabel, deploymentPhaseLabel, deploymentStateLabel, deploymentStateType, formatGB, labelOf,
  type AppCatalogItem, type AppEnvSpec, type AppsSummaryItem, type Deployment, type DeploymentEvent, type Image, type InstalledWorkflow, type InstanceState,
  type UserEvent, type WorkflowCatalogItem,
} from '@spark/shared'

// 与 internal/appdeploy 保持一致：MinSecretLen、max_len 缺省值
const MIN_SECRET_LEN = 8
const DEFAULT_MAX_LEN = 256
const INT64_MIN = -(2n ** 63n)
const INT64_MAX = 2n ** 63n - 1n
const SECRET_CHARS = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789'
const SECRET_LEN = 32
// 判断 pattern 是否允许生成的密钥：含大小写与数字的 32 位探针
const SECRET_PROBE = 'Aa0Bb1Cc2Dd3Ee4Ff5Gg6Hh7Ii8Jj9Kk'

/** 应用兼容且当前可选的镜像，按 app.images 的顺序（第一个为默认）。 */
export function compatibleImages(app: AppCatalogItem, images: Image[]): Image[] {
  return app.images.map((a) => images.find((i) => i.id === a.id)).filter((i): i is Image => i != null)
}

/** 变量表单初值：default；bool 无默认为 'false'，其余为空串。 */
export function envDefaults(spec: AppEnvSpec[]): Record<string, string> {
  return Object.fromEntries(spec.map((e) => [e.name, e.default ?? (e.type === 'bool' ? 'false' : '')]))
}

/** 提交前去掉空串：空值交给服务器补默认值（与 ValidateEnv 对空值的处理一致）。 */
export function stripEmpty(values: Record<string, string>): Record<string, string> {
  return Object.fromEntries(Object.entries(values).filter(([, v]) => v !== ''))
}

const envLabel = (e: AppEnvSpec) => e.label || e.name
const byteLen = (v: string) => new TextEncoder().encode(v).length

/**
 * pattern 编译为全匹配（Go：`^(?:pattern)$`），只用 u 模式：u 模式对未知转义与孤立括号报错，
 * RE2 专有语法（`\z`、`\A`、`[[:alpha:]]`、`\pL` 等）因此编译失败 → 返回 null，跳过前端校验交给服务器；
 * 不带 u 时这些语法会被 JS 编译成别的含义（`\z` = 字母 z），误拦合法值。
 */
function fullMatch(pattern: string): RegExp | null {
  try {
    return new RegExp(`^(?:${pattern})$`, 'u')
  } catch {
    return null
  }
}

/**
 * 前端预校验单个值（镜像 internal/appdeploy/spec.go 的 ValidateEnv / checkValue，服务器仍是准绳）。
 * 返回中文原因；null 表示通过。空值：有默认值或非必填即通过（服务器补默认 / 不设置）。
 */
export function validateEnvValue(e: AppEnvSpec, v: string): string | null {
  if (v === '') return e.required && !e.default ? `请填写 ${envLabel(e)}` : null
  if (/[\n\r\0]/.test(v)) return '不能包含换行或 NUL'
  const maxLen = e.max_len || DEFAULT_MAX_LEN
  if (byteLen(v) > maxLen) return `最多 ${maxLen} 字节`
  switch (e.type) {
    case 'int': {
      if (!/^[+-]?[0-9]+$/.test(v)) return '必须是整数'
      const n = BigInt(v)
      if (n < INT64_MIN || n > INT64_MAX) return '必须是整数'
      if ((e.min != null && n < BigInt(e.min)) || (e.max != null && n > BigInt(e.max))) return '超出允许范围'
      return null
    }
    case 'bool':
      return v === 'true' || v === 'false' ? null : '只能是 true 或 false'
    case 'enum':
      return e.options?.includes(v) ? null : '不在可选项内'
    case 'secret':
      if (byteLen(v) < MIN_SECRET_LEN) return `至少 ${MIN_SECRET_LEN} 个字符`
      return checkPattern(e, v)
    case 'string':
      return checkPattern(e, v)
  }
  return null
}

function checkPattern(e: AppEnvSpec, v: string): string | null {
  if (!e.pattern) return null
  const re = fullMatch(e.pattern)
  return re && !re.test(v) ? '格式不正确' : null
}

/** secret 且规则允许 32 位字母数字时显示「生成」按钮。 */
export function canGenerate(e: AppEnvSpec): boolean {
  return e.type === 'secret' && validateEnvValue(e, SECRET_PROBE) === null
}

/** 生成 32 位 [A-Za-z0-9]（crypto.getRandomValues，拒绝采样无偏）；规则不允许时返回 ''。 */
export function generateSecret(e: AppEnvSpec, c: Pick<Crypto, 'getRandomValues'> = globalThis.crypto): string {
  if (!canGenerate(e)) return ''
  for (let attempt = 0; attempt < 10; attempt++) {
    let out = ''
    while (out.length < SECRET_LEN) {
      for (const b of c.getRandomValues(new Uint8Array(64))) {
        if (b < 248 && out.length < SECRET_LEN) out += SECRET_CHARS[b % 62] // 248 = 62 × 4
      }
    }
    if (validateEnvValue(e, out) === null) return out
  }
  return ''
}

/** 「模型」变量：spec.env 里第一个 enum。 */
const modelEnv = (app: AppCatalogItem) => app.version.env.find((e) => e.type === 'enum')

/** 预装应用下拉的选项：`名称 · 默认模型`（没有 enum 变量时只有名称）。 */
export function appOptionLabel(app: AppCatalogItem): string {
  const m = modelEnv(app)?.default
  return m ? `${app.name} · ${m}` : app.name
}

/** 确认页「预装应用」：`名称（所选模型）`。 */
export function appConfirmText(app: AppCatalogItem, values: Record<string, string>): string {
  const e = modelEnv(app)
  const m = e ? values[e.name] || e.default : ''
  return m ? `${app.name}（${m}）` : app.name
}

/** 部署状态文案与标签颜色（实例详情「应用」卡片与列表摘要共用）。 */
export function deploymentStatusText(d: Deployment, instanceState: InstanceState): { text: string; type: string } {
  if (instanceState === 'stopped') return { text: '实例已停机，续费后自动恢复', type: 'info' }
  if ((instanceState === 'reserved' || instanceState === 'creating') && d.state === 'queued') return { text: '将在开通后自动部署', type: 'info' }
  if (d.state === 'installing') {
    const phase = labelOf(deploymentPhaseLabel, d.phase)
    return { text: `部署中 ${d.progress}%${phase ? ` · ${phase}` : ''}`, type: 'primary' }
  }
  if (d.state === 'failed') return { text: d.error_code ? `失败：${labelOf(deploymentErrorLabel, d.error_code)}` : '失败', type: 'danger' }
  return { text: labelOf(deploymentStateLabel, d.state), type: labelOf(deploymentStateType, d.state) }
}

/**
 * 实例详情应用行的状态标签：只反映部署本身（实例非运行的原因单独一行 detail-app-instance-hint），
 * 但实例停机时仍记为 running 的部署（state 按规格 §6.3 不变）显示灰色「已停机」，与列表摘要一致（验收观察 O-4）。
 */
export function appRowStatus(d: Deployment, instanceState: InstanceState): { text: string; type: string } {
  if (instanceState === 'stopped' && d.state === 'running') return { text: '已停机', type: 'info' }
  return deploymentStatusText(d, 'running')
}

/** 每个实例最多部署的应用数（与 internal/appdeploy.MaxAppsPerInstance 一致）。 */
export const MAX_APPS = 3

/** 实例这些状态下可添加应用（非 running 时建 queued，开通 / 恢复后自动部署）。 */
export const canAddApp = (s: InstanceState) => s === 'reserved' || s === 'creating' || s === 'running' || s === 'stopped'

/** 实例不在运行时，每行部署下的提示；running 返回 null。 */
export function instanceAppHint(s: InstanceState): string | null {
  if (s === 'stopped') return '实例已停机，续费后自动恢复'
  if (s === 'reserved' || s === 'creating') return '将在开通后自动部署'
  return null
}

/** 重新部署 / 停止 / 启动只在实例运行中可用；返回不可用原因（null = 可用）。查看日志、卸载不受限。 */
export function appOpBlockReason(s: InstanceState): string | null {
  if (s === 'running') return null
  if (s === 'stopped') return '实例已停机，续费后可操作'
  if (s === 'reserved' || s === 'creating') return '实例开通后可操作'
  return '实例未运行'
}

/** 操作菜单按规格 §6.2 的允许状态显示。 */
export function deploymentActions(d: Pick<Deployment, 'state' | 'desired'>): { redeploy: boolean; stop: boolean; start: boolean; remove: boolean } {
  const live = d.state !== 'removed' && d.state !== 'lost'
  return {
    redeploy: d.state === 'running' || d.state === 'stopped' || d.state === 'failed',
    stop: (d.state === 'installing' || d.state === 'running') && d.desired === 'running',
    start: d.state === 'stopped',
    remove: live && d.state !== 'removing',
  }
}

/** 部署行需要较快刷新：Runner 正在推进（排队 / 部署中 / 卸载中 / 已请求停止）。 */
export const deploymentBusy = (d: Pick<Deployment, 'state' | 'desired'>) =>
  d.state === 'queued' || d.state === 'installing' || d.state === 'removing' || (d.state === 'running' && d.desired !== 'running')

/** 「添加应用」弹窗里应用卡片的灰显原因：已部署 / 实例镜像不兼容（null = 可选）。 */
export function addPickReason(app: AppCatalogItem, imageId: number, deployed: Pick<Deployment, 'app_id'>[]): string | null {
  if (deployed.some((d) => d.app_id === app.id)) return '已部署'
  if (!app.images.some((i) => i.id === imageId)) return `需要镜像：${app.images.map((i) => i.name).join('、') || '—'}`
  return null
}

/**
 * 实例列表的应用摘要标签：部署中 45%（蓝）/ 运行中（绿）/ 失败（红）/ 已停止（灰），其余按状态文案。
 * 实例停机时容器里的进程已不在，仍记为 running 的部署显示灰色「已停机」（续费后自动恢复）。
 */
export function summaryTag(a: AppsSummaryItem, instanceState: InstanceState): { text: string; type: string } {
  if (instanceState === 'stopped' && a.state === 'running') return { text: '已停机', type: 'info' }
  if (a.state === 'installing') return { text: `部署中 ${a.progress}%`, type: 'primary' }
  return { text: labelOf(deploymentStateLabel, a.state), type: labelOf(deploymentStateType, a.state) }
}

/**
 * 重新部署弹窗的变量初值：目标版本 spec 声明的普通变量取当前部署的值（没有则取默认值）；secret 一律空串（从不回显，留空沿用）。
 * secretSet 只保留目标版本里仍是 secret 的已设置项。
 */
export function redeployForm(d: Pick<Deployment, 'env' | 'secret_env'>, spec: AppEnvSpec[]): { values: Record<string, string>; secretSet: Record<string, boolean> } {
  const defaults = envDefaults(spec)
  const values: Record<string, string> = {}
  const secretSet: Record<string, boolean> = {}
  for (const e of spec) {
    if (e.type === 'secret') {
      values[e.name] = d.secret_env[e.name] ? '' : defaults[e.name]
      if (d.secret_env[e.name]) secretSet[e.name] = true
    } else values[e.name] = d.env[e.name] ?? defaults[e.name]
  }
  return { values, secretSet }
}

/**
 * 重新部署提交的 env（规格 §8）：空值不发送——普通变量由服务器补默认，留空的 secret 沿用旧值；
 * 用户明确勾了「清除」且没填新值的 secret 发送空串（服务器清除）。
 */
export function redeployEnv(values: Record<string, string>, cleared: Record<string, boolean>): Record<string, string> {
  const env = stripEmpty(values)
  for (const [name, on] of Object.entries(cleared)) if (on && !(name in env)) env[name] = ''
  return env
}

export interface RedeployVersion { id: number; label: string; env: AppEnvSpec[]; disabled: boolean }

/**
 * 重新部署可选的版本。「当前」用部署自带的 spec；「最新」取应用目录里的最新已发布版本——提交的就是这个版本 ID，
 * 变量表单也按它渲染（部署视图的 latest_version 可能与目录不一致时以目录为准）。目录里没有该应用（加载失败）时「最新」不可选。
 */
export function redeployVersions(d: Deployment, catalog: AppCatalogItem[]): RedeployVersion[] {
  const out: RedeployVersion[] = [{ id: d.version_id, label: `v${d.version}（当前）`, env: d.spec.env ?? [], disabled: false }]
  if (!d.update_available) return out
  const cat = catalog.find((a) => a.id === d.app_id)
  if (cat) {
    if (cat.version.id !== d.version_id) out.push({ id: cat.version.id, label: `v${cat.version.version}（最新）`, env: cat.version.env, disabled: false })
  } else if (d.latest_version_id != null) {
    out.push({ id: d.latest_version_id, label: `v${d.latest_version}（最新，应用目录加载失败，暂不可选）`, env: [], disabled: true })
  }
  return out
}

export interface TimelineItem { at: string; kind: string; text: string }

/** 「动态」卡片：实例事件与各部署事件合并，新到旧；部署事件 kind = app，文本前缀应用名。 */
export function mergeTimeline(inst: UserEvent[], apps: { name: string; events: DeploymentEvent[] }[]): TimelineItem[] {
  const out: TimelineItem[] = inst.map((e) => ({ at: e.at, kind: e.reason_kind, text: e.detail }))
  for (const a of apps) for (const e of [...a.events].reverse()) out.push({ at: e.at, kind: 'app', text: `${a.name}：${e.detail}` })
  // 稳定排序：同一时刻保持原顺序（实例事件本就新到旧，部署事件已反转为新到旧）
  return out.map((x, i) => ({ x, i, t: Date.parse(x.at) })).sort((a, b) => b.t - a.t || a.i - b.i).map((y) => y.x)
}

// ---- 计划 11：ComfyUI 部署上的工作流（实例详情「应用」卡片的工作流子区块） ----

/** 每个 ComfyUI 部署最多安装的工作流数（与 comfywf.MaxWorkflowsPerDeployment 一致；logic/workflows.ts 转出同一常量）。 */
export const MAX_WORKFLOWS = 8

/** 显示工作流子区块的部署：应用 slug 为 comfyui（与后端 Installer 按 slug 判断一致）。 */
export const isWorkflowHost = (d: Pick<Deployment, 'app_slug'>) => d.app_slug === 'comfyui'

/** 部署视图里的已装工作流；字段缺省（非 comfyui 部署 / 旧接口）或 null 时为空数组。 */
export function deploymentWorkflows(d: Pick<Deployment, 'workflows'>): InstalledWorkflow[] {
  return Array.isArray(d.workflows) ? d.workflows : []
}

/** 安装 / 更新 / 移除工作流只在部署 queued / running / stopped / failed 时可用（否则后端 app_invalid_state）。 */
export const canChangeWorkflows = (d: Pick<Deployment, 'state'>) =>
  d.state === 'queued' || d.state === 'running' || d.state === 'stopped' || d.state === 'failed'

/**
 * 增删改工作流的不可用原因（null = 可用）：部署进行中；或实例不在运行（会重新部署，同「重新部署」的限制）。
 * 排队中的部署（实例开通前 / 恢复前）只改集合、随部署安装，不受实例状态限制。
 */
export function workflowChangeBlockReason(d: Pick<Deployment, 'state'>, instanceState: InstanceState): string | null {
  if (!canChangeWorkflows(d)) return '部署进行中，完成后可操作'
  if (d.state === 'queued') return null
  return appOpBlockReason(instanceState)
}

/** 部署中时子区块顶部的提示（有工作流时；进度条沿用应用行的渲染）。 */
export function workflowPanelNotice(d: Pick<Deployment, 'state' | 'workflows'>): string | null {
  return d.state === 'installing' && deploymentWorkflows(d).length ? '正在安装工作流…（随应用进度）' : null
}

export interface WorkflowRowActions {
  /** 「打开 ComfyUI」：部署运行中且端口在 */
  open: boolean
  /** 「快速出图」：版本支持（quick_run）、该工作流已安装、部署运行中 */
  quickRun: boolean
  /** 实例不在运行（停机等）时快速出图置灰 */
  quickRunDisabled: boolean
  /** 更多菜单：有新版本 → 更新；安装失败且无新版本 → 重试安装（PUT 同版本 = 重试）；部署中、没有已发布版本时不提供 */
  update: 'update' | 'retry' | null
  remove: boolean
  /** 更新 / 移除暂不可用的原因（实例非运行等）：菜单项置灰并显示原因 */
  blocked: string | null
}

export function workflowRowActions(w: InstalledWorkflow, d: Pick<Deployment, 'state' | 'url'>, instanceState: InstanceState): WorkflowRowActions {
  const change = canChangeWorkflows(d)
  return {
    open: d.state === 'running' && !!d.url,
    quickRun: w.quick_run && w.state === 'installed' && d.state === 'running',
    quickRunDisabled: instanceState !== 'running',
    update: !change || w.latest_version_id == null ? null : w.update_available ? 'update' : w.state === 'failed' ? 'retry' : null,
    remove: change,
    blocked: change ? workflowChangeBlockReason(d, instanceState) : null,
  }
}

/** 「添加工作流」选择器里的灰显原因（null = 可选）。 */
export function addWorkflowReason(item: Pick<WorkflowCatalogItem, 'id'>, installed: Pick<InstalledWorkflow, 'workflow_id'>[]): string | null {
  return installed.some((w) => w.workflow_id === item.id) ? '已安装' : null
}

const RESTART_NOTE = '会重启实例上的 ComfyUI，正在生成的任务会中断' // D9

/** 安装确认文案（D9：重启 ComfyUI；需下载量）。 */
export function workflowAddConfirmText(item: Pick<WorkflowCatalogItem, 'name' | 'version'>): string {
  const gb = item.version.download_gb
  return `安装「${item.name}」${RESTART_NOTE}${gb > 0 ? `；需下载约 ${formatGB(gb)} 模型` : ''}`
}

/** 更新 / 重试确认文案。 */
export function workflowUpdateConfirmText(w: Pick<InstalledWorkflow, 'name' | 'version' | 'latest_version' | 'update_available'>): string {
  if (w.update_available) return `将「${w.name}」从 v${w.version} 更新到 v${w.latest_version}，${RESTART_NOTE}`
  return `重新安装「${w.name}」v${w.version}，${RESTART_NOTE}`
}

/** 移除确认文案（D8：模型与节点保留）。 */
export function workflowRemoveConfirmText(w: Pick<InstalledWorkflow, 'name'>): string {
  return `移除「${w.name}」后模型与节点保留在 /root/data/comfyui（可手动删除释放空间）；${RESTART_NOTE}`
}
