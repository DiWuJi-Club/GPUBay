import { createHttp } from '@spark/shared'

const handlers = { unauthorized: () => {} }

/** 控制台 API 客户端。401 的处理由 store/session 注册，避免循环引用。 */
export const api = createHttp({ base: '/api/v1', onUnauthorized: () => handlers.unauthorized() })

export function onUnauthorized(fn: () => void) {
  handlers.unauthorized = fn
}

// --- workflows installed + quick run (plan 11 Task 13) ---
// 实例详情「应用」卡片的工作流子区块与快速出图抽屉（规格 §4、§6.1、§10.1）。
// 类型在本块内单独引入、目录 / 详情函数带 panel 前缀，避免与 Task 12（市场页）追加的块在合并时冲突 / 重名。
import type {
  Deployment, HttpOptions, InstalledWorkflow, Listed, QuickRun, WorkflowCatalogItem, WorkflowDetail,
} from '@spark/shared'

/** 安装 / 更新 / 移除的响应：部署视图（同 GET /instances/{id}/apps 的条目）与该部署的已装工作流。 */
export interface InstalledWorkflowsResult { deployment: Deployment; workflows: InstalledWorkflow[] }

const wfBase = (instanceId: string, depId: string) => `/instances/${instanceId}/apps/${depId}/workflows`

export const panelWorkflowCatalog = () => api.get<Listed<WorkflowCatalogItem>>('/workflows')
export const panelWorkflowDetail = (slug: string) => api.get<WorkflowDetail>(`/workflows/${encodeURIComponent(slug)}`)
export const addDeploymentWorkflow = (instanceId: string, depId: string, workflowId: number) =>
  api.post<InstalledWorkflowsResult>(wfBase(instanceId, depId), { workflow_id: workflowId })
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
