export type WorkflowCategory = 'txt2img' | 'img2img' | 'video' | 'upscale' | 'other'
export type WorkflowStatus = 'draft' | 'published' | 'retired'
export type WorkflowInputType = 'text' | 'textarea' | 'int' | 'float' | 'bool' | 'enum' | 'seed' | 'image'
/** deployment_workflows.state */
export type WorkflowInstallState = 'pending' | 'installed' | 'failed'
/** 快速出图运行状态；running 只来自接口（ComfyUI 队列正在执行），不落库。 */
export type QuickRunState = 'queued' | 'running' | 'done' | 'error'

export interface WorkflowRepoFile { repo: string; file: string }
export interface WorkflowModel {
  name: string
  dir: string
  size_gb: number
  modelscope?: WorkflowRepoFile
  hf?: WorkflowRepoFile
  sha256: string
}
export interface WorkflowNode { name: string; git: string; commit: string; node_types: string[] }

/** 快速出图表单的一项（manifest.inputs[]）。 */
export interface WorkflowInput {
  key: string
  label: string
  type: WorkflowInputType
  node: string
  input: string
  default?: string | number | boolean
  min?: number
  max?: number
  step?: number
  max_len?: number
  options?: string[]
}
export interface WorkflowOutput { node: string }

/** 控制台可见的清单：不含 api_prompt 本体（只有后台版本详情接口返回它）。 */
export interface WorkflowManifestPublic {
  comfyui_min?: string
  models: WorkflowModel[]
  nodes: WorkflowNode[]
  inputs: WorkflowInput[]
  outputs: WorkflowOutput[]
}

/** 列表里的当前版本摘要。 */
export interface WorkflowVersionSummary {
  id: number
  version: string
  download_gb: number
  /** 模型数 */
  models: number
  /** 自定义节点数 */
  nodes: number
  quick_run: boolean
  comfyui_min: string
}

/** GET /workflows 的条目（市场页）。 */
export interface WorkflowCatalogItem {
  id: number
  slug: string
  name: string
  summary: string
  category: WorkflowCategory
  tags: string[]
  cover_url: string | null
  sort: number
  version: WorkflowVersionSummary
  installs: number
}

/** GET /workflows/{slug}（详情页）。 */
export interface WorkflowDetail extends Omit<WorkflowCatalogItem, 'version'> {
  description: string
  version: WorkflowVersionSummary & { manifest: WorkflowManifestPublic; changelog: string; published_at: string | null }
  versions: { id: number; version: string; published_at: string | null }[]
}

/** 部署上已装的工作流（Deployment.workflows[] 与 GET …/apps/{dep}/workflows）。 */
export interface InstalledWorkflow {
  workflow_id: number
  slug: string
  name: string
  category: WorkflowCategory
  version_id: number
  version: string
  state: WorkflowInstallState
  latest_version_id: number | null
  update_available: boolean
  quick_run: boolean
}

export interface QuickRunFile { index: number; filename: string; kind: 'image' | 'video'; url: string }

/** POST / GET …/workflows/{wid}/runs[/{run}] */
export interface QuickRun {
  id: string
  prompt_id: string
  seed: number | null
  state: QuickRunState
  files: QuickRunFile[]
  message: string
}
