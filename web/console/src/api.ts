import {
  createHttp, type Deployment, type HttpOptions, type InstalledWorkflow, type Listed, type QuickRun, type WorkflowCatalogItem, type WorkflowDetail,
} from '@spark/shared'

const handlers = { unauthorized: () => {} }

/** 控制台 API 客户端。401 的处理由 store/session 注册，避免循环引用。 */
export const api = createHttp({ base: '/api/v1', onUnauthorized: () => handlers.unauthorized() })

export function onUnauthorized(fn: () => void) {
  handlers.unauthorized = fn
}

// --- workflows (plan 11)：市场目录 / 详情 / 安装（Task 12）+ 已装工作流增删改与快速出图（Task 13），规格 §4、§6.1、§10.1 ---

/** 安装 / 更新 / 移除的响应：部署视图（同 GET /instances/{id}/apps 的条目）与该部署的已装工作流。 */
export interface InstalledWorkflowsResult { deployment: Deployment; workflows: InstalledWorkflow[] }

const wfBase = (instanceId: string, depId: string) => `/instances/${instanceId}/apps/${depId}/workflows`

export const updateDeploymentWorkflow = (instanceId: string, depId: string, workflowId: number, versionId?: number | null) =>
  api.put<InstalledWorkflowsResult>(`${wfBase(instanceId, depId)}/${workflowId}`, versionId ? { version_id: versionId } : {})
export const removeDeploymentWorkflow = (instanceId: string, depId: string, workflowId: number) =>
  api.del<InstalledWorkflowsResult>(`${wfBase(instanceId, depId)}/${workflowId}`)
export const uploadQuickRunImage = (instanceId: string, depId: string, workflowId: number, file: File) =>
  api.upload<{ name: string }>(`${wfBase(instanceId, depId)}/${workflowId}/uploads`, file)
export const startQuickRun = (instanceId: string, depId: string, workflowId: number, inputs: Record<string, unknown>) =>
  api.post<QuickRun>(`${wfBase(instanceId, depId)}/${workflowId}/runs`, { inputs })
export const getQuickRun = (instanceId: string, depId: string, workflowId: number, runId: string, opt?: HttpOptions) =>
  api.get<QuickRun>(`${wfBase(instanceId, depId)}/${workflowId}/runs/${encodeURIComponent(runId)}`, undefined, opt)

/** 市场列表（匿名）：GET /workflows（分类在前端过滤，这里不带 category）。 */
export const fetchWorkflows = () => api.get<Listed<WorkflowCatalogItem>>('/workflows').then((r) => r.items)
/** 详情（匿名）：GET /workflows/{slug}；不存在 / 未上架 → workflow_not_found。 */
export const fetchWorkflow = (slug: string) => api.get<WorkflowDetail>(`/workflows/${encodeURIComponent(slug)}`)
/** 安装到某个 ComfyUI 部署：POST /instances/{id}/apps/{dep}/workflows（不带 version_id = 最新已发布版本）。 */
export const installWorkflow = (instanceId: string, deploymentId: string, workflowId: number, versionId?: number) =>
  api.post<InstalledWorkflowsResult>(wfBase(instanceId, deploymentId), {
    workflow_id: workflowId, ...(versionId != null ? { version_id: versionId } : {}),
  })
// --- end workflows (plan 11) ---
