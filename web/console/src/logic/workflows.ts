import {
  formatGB, labelOf, workflowCategoryLabel,
  type AppCatalogItem, type Deployment, type DeploymentState, type Duration, type Instance, type Product, type WorkflowCatalogItem, type WorkflowCategory, type WorkflowModel,
} from '@spark/shared'

import { MAX_WORKFLOWS } from './apps'

/** 每个 ComfyUI 部署最多装的工作流数（定义在 logic/apps.ts）；开通时预装同样上限。 */
export { MAX_WORKFLOWS }
/** 安装弹窗最多查询的实例数（每台一个 GET /instances/{id}/apps）。 */
export const INSTALL_SCAN_LIMIT = 20
/** 市场页分类标签顺序（规格 §4）。 */
export const WORKFLOW_CATEGORIES: WorkflowCategory[] = ['txt2img', 'img2img', 'video', 'upscale', 'other']
export type CategoryFilter = WorkflowCategory | 'all'
/** 「新开一台实例」时预选的应用与变量（D15：工作流自带模型，不再下预置模型）。 */
export const WORKFLOW_APP_SLUG = 'comfyui'
export const WORKFLOW_RENT_ENV: Record<string, string> = { MODEL_ID: 'none' }

/** 前端过滤：分类 + 名称 / 标签搜索（忽略大小写与首尾空白）。 */
export function filterWorkflows<T extends Pick<WorkflowCatalogItem, 'name' | 'tags' | 'category'>>(items: T[], f: { category: CategoryFilter; query: string }): T[] {
  const q = f.query.trim().toLowerCase()
  return items.filter((w) => {
    if (f.category !== 'all' && w.category !== f.category) return false
    if (!q) return true
    return w.name.toLowerCase().includes(q) || w.tags.some((t) => t.toLowerCase().includes(q))
  })
}

/** sort 升序，再按名称；返回新数组。 */
export function sortWorkflows<T extends Pick<WorkflowCatalogItem, 'sort' | 'name'>>(items: T[]): T[] {
  return [...items].sort((a, b) => a.sort - b.sort || a.name.localeCompare(b.name, 'zh'))
}

/** 选中工作流的合计下载量（GB，两位小数）。 */
export function totalDownloadGB(items: Pick<WorkflowCatalogItem, 'id' | 'version'>[], selectedIds: number[]): number {
  const sel = new Set(selectedIds)
  const sum = items.filter((w) => sel.has(w.id)).reduce((s, w) => s + (Number(w.version.download_gb) || 0), 0)
  return Math.round(sum * 100) / 100
}

/** 多选切换：已选 → 取消；未选且未满 max → 加到末尾；满了不变。 */
export function toggleWorkflow(selected: number[], id: number, max = MAX_WORKFLOWS): number[] {
  if (selected.includes(id)) return selected.filter((x) => x !== id)
  return selected.length >= max ? selected : [...selected, id]
}

// ---- 一键安装 ----

const CANDIDATE_STATES = new Set(['reserved', 'creating', 'running', 'stopped']) // 同实例详情「应用」卡片的可见状态
/** 可能有 ComfyUI 部署的实例：可管理应用的状态，且 apps_summary 非空（旧数据没有该字段时也查）；最多 INSTALL_SCAN_LIMIT 台。 */
export function installCandidates(instances: Instance[]): Instance[] {
  return instances.filter((i) => CANDIDATE_STATES.has(i.state) && (i.apps_summary == null || i.apps_summary.length > 0)).slice(0, INSTALL_SCAN_LIMIT)
}

export interface InstallTarget {
  instanceId: string
  instanceName: string
  deploymentId: string
  state: DeploymentState
  installedCount: number
  /** 该工作流已装在这个部署上 */
  installed: boolean
}

const LIVE_DEPLOYMENT = new Set<DeploymentState>(['queued', 'installing', 'running', 'stopped', 'failed'])
/** 从每台实例的部署列表里挑 comfyui（按 app_slug）且未在移除的部署。 */
export function installTargets(entries: { instance: Instance; deployments: Deployment[] }[], workflowId: number): InstallTarget[] {
  return entries.flatMap(({ instance, deployments }) =>
    deployments.filter((d) => d.app_slug === WORKFLOW_APP_SLUG && LIVE_DEPLOYMENT.has(d.state)).map((d) => ({
      instanceId: instance.id,
      instanceName: `${instance.product_name} · ${instance.id}`,
      deploymentId: d.id,
      state: d.state,
      installedCount: d.workflows?.length ?? 0,
      installed: d.workflows?.some((w) => w.workflow_id === workflowId) ?? false,
    })),
  )
}

const INSTALLABLE = new Set<DeploymentState>(['queued', 'running', 'stopped', 'failed'])
/** 能否装到该部署（与后端 app_invalid_state / workflow_limit / workflow_exists 同口径，后端为准）。 */
export function canInstallTo(t: InstallTarget): { ok: boolean; reason: string } {
  if (t.installed) return { ok: false, reason: '已安装' }
  if (!INSTALLABLE.has(t.state)) return { ok: false, reason: t.state === 'installing' ? 'ComfyUI 正在部署，请稍后再装' : '当前状态不能安装' }
  if (t.installedCount >= MAX_WORKFLOWS) return { ok: false, reason: `已装满 ${MAX_WORKFLOWS} 个工作流` }
  return { ok: true, reason: '' }
}

/** 安装确认文案（规格 §4）。 */
export function installConfirmText(w: Pick<WorkflowCatalogItem, 'version'>): string {
  const gb = Number(w.version.download_gb) || 0
  return `安装会重启 ComfyUI，实例上正在生成的任务会中断${gb > 0 ? `；需下载约 ${formatGB(gb)} 模型` : ''}。`
}

export type Cover = { kind: 'image'; url: string } | { kind: 'placeholder'; category: WorkflowCategory; label: string }
/** 封面：有 cover_url 用图片，否则按分类占位。 */
export function coverOrPlaceholder(w: Pick<WorkflowCatalogItem, 'cover_url' | 'category'>): Cover {
  if (w.cover_url) return { kind: 'image', url: w.cover_url }
  return { kind: 'placeholder', category: w.category, label: labelOf(workflowCategoryLabel, w.category) }
}

export const shortCommit = (c: string) => c.slice(0, 8)

/** 模型来源：ModelScope 优先（容器内先试 ModelScope 再试 hf-mirror）。 */
export function modelSource(m: WorkflowModel): string {
  const parts: string[] = []
  if (m.modelscope) parts.push(`ModelScope ${m.modelscope.repo}`)
  if (m.hf) parts.push(`HuggingFace ${m.hf.repo}`)
  return parts.join(' · ')
}

// ---- 开通时预装（RentDialog）----

/** 该应用能否预装工作流：comfyui 且版本没声明不支持（后端恒给出 supports_workflows；旧夹具缺省按支持）。 */
export function isWorkflowApp(app: Pick<AppCatalogItem, 'slug' | 'version'> | null | undefined): boolean {
  return !!app && app.slug === WORKFLOW_APP_SLUG && app.version.supports_workflows !== false
}

/** 预设变量里应用确实有、且 enum 值在可选项内的部分（旧版本没有 MODEL_ID=none 时沿用默认）。 */
export function presetEnvFor(app: Pick<AppCatalogItem, 'version'>, preset: Record<string, string>): Record<string, string> {
  const out: Record<string, string> = {}
  for (const e of app.version.env) {
    const v = preset[e.name]
    if (v == null) continue
    if (e.type === 'enum' && !e.options?.includes(v)) continue
    out[e.name] = v
  }
  return out
}

/** 「新开一台实例」用哪个商品：第一个有价格且有空闲的 → 立即租用；否则第一个有价格的 → 预订；都没有 → null。 */
export function rentChoice(products: Product[]): { product: Product; mode: 'immediate' | 'booking' } | null {
  const priced = products.filter((p) => p.prices.length > 0)
  const idle = priced.find((p) => p.idle_count > 0)
  if (idle) return { product: idle, mode: 'immediate' }
  return priced[0] ? { product: priced[0], mode: 'booking' } : null
}
export const firstDuration = (p: Product): Duration => p.prices[0]?.duration ?? '1d'

// ---- 描述：Markdown 子集（段落、`- ` 列表、**加粗**、`代码`），只产出结构，模板渲染为文本节点（不用 v-html）----

export type Inline = { kind: 'text' | 'bold' | 'code'; text: string }
export type MdBlock = { type: 'p'; inlines: Inline[] } | { type: 'ul'; items: Inline[][] }

const INLINE_RE = /\*\*([^*]+)\*\*|`([^`]+)`/g
function inlines(s: string): Inline[] {
  const out: Inline[] = []
  const push = (kind: Inline['kind'], text: string) => {
    const last = out[out.length - 1]
    if (kind === 'text' && last?.kind === 'text') last.text += text
    else if (text) out.push({ kind, text })
  }
  let at = 0
  for (const m of s.matchAll(INLINE_RE)) {
    push('text', s.slice(at, m.index))
    if (m[1] != null) push('bold', m[1])
    else push('code', m[2])
    at = m.index + m[0].length
  }
  push('text', s.slice(at))
  return out
}

export function markdownBlocks(text: string): MdBlock[] {
  const blocks: MdBlock[] = []
  for (const chunk of text.replace(/\r\n?/g, '\n').split(/\n\s*\n/)) {
    const lines = chunk.split('\n').map((l) => l.trim()).filter(Boolean)
    let para: string[] = []
    let list: Inline[][] = []
    const flushPara = () => {
      if (para.length) blocks.push({ type: 'p', inlines: inlines(para.join(' ')) })
      para = []
    }
    const flushList = () => {
      if (list.length) blocks.push({ type: 'ul', items: list })
      list = []
    }
    for (const l of lines) {
      const item = /^[-*]\s+(.*)$/.exec(l)
      if (item) {
        flushPara()
        list.push(inlines(item[1]))
      } else {
        flushList()
        para.push(l)
      }
    }
    flushPara()
    flushList()
  }
  return blocks
}
