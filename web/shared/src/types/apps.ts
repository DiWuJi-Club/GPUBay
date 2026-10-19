export type AppCategory = 'llm' | 'image' | 'notebook' | 'tool'
export type AppStatus = 'draft' | 'published' | 'retired'
export type AppEnvType = 'string' | 'int' | 'bool' | 'enum' | 'secret'

export interface AppEnvSpec {
  name: string
  label?: string
  type: AppEnvType
  required?: boolean
  options?: string[]
  default?: string
  pattern?: string
  max_len?: number
  min?: number
  max?: number
  help?: string
}
export interface AppPortSpec { port: number; label?: string; path?: string; health_path?: string }
export interface AppLinkSpec { label: string; path: string }
export interface AppSpec {
  ports: AppPortSpec[]
  timeout_minutes?: number
  min_data_free_gib?: number
  env?: AppEnvSpec[]
  links?: AppLinkSpec[]
}

/** GET /apps 的条目（市场页 / 添加应用弹窗）。 */
export interface AppCatalogItem {
  id: number
  slug: string
  name: string
  summary: string
  description?: string
  category: AppCategory
  icon: string
  version: { id: number; version: string; ports: AppPortSpec[]; env: AppEnvSpec[]; links: AppLinkSpec[]; timeout_minutes: number }
  images: { id: number; name: string }[]
}

export type DeploymentState = 'queued' | 'installing' | 'running' | 'stopped' | 'failed' | 'removing' | 'removed' | 'lost'
export type DeploymentHealth = 'unknown' | 'ok' | 'unhealthy'

/** GET /instances/{id}/apps 的条目。 */
export interface Deployment {
  id: string
  instance_id: string
  app_id: number
  app_slug: string
  app_name: string
  app_icon: string
  version_id: number
  version: string
  latest_version_id: number | null
  latest_version: string
  update_available: boolean
  state: DeploymentState
  desired: 'running' | 'stopped' | 'removed'
  phase: string
  progress: number
  message: string
  health: DeploymentHealth
  error_code: string
  env: Record<string, string>
  secret_env: Record<string, boolean>
  spec: AppSpec
  port: number
  url: string | null
  links: { label: string; url: string }[]
  attempt: number
  cleanup_pending: boolean
  started_at: string | null
  finished_at: string | null
  created_at: string
  updated_at: string
}

export interface DeploymentEvent { id: number; at: string; from_state: string; to_state: string; detail: string }

/** 实例列表 / 详情里的 apps_summary 条目。 */
export interface AppsSummaryItem { id: string; name: string; state: DeploymentState; progress: number; url: string | null }
