import { createHttp } from '@spark/shared'

const handlers = { unauthorized: () => {} }

/** 控制台 API 客户端。401 的处理由 store/session 注册，避免循环引用。 */
export const api = createHttp({ base: '/api/v1', onUnauthorized: () => handlers.unauthorized() })

export function onUnauthorized(fn: () => void) {
  handlers.unauthorized = fn
}

// --- workflows market (plan 11 Task 12) ---
// 类型用 import('…') 内联引用：不新增顶层 import，避免与其它任务的同名 import 合并冲突

/** 市场列表（匿名）：GET /workflows（分类在前端过滤，这里不带 category）。 */
export const fetchWorkflows = () => api.get<import('@spark/shared').Listed<import('@spark/shared').WorkflowCatalogItem>>('/workflows').then((r) => r.items)
/** 详情（匿名）：GET /workflows/{slug}；不存在 / 未上架 → workflow_not_found。 */
export const fetchWorkflow = (slug: string) => api.get<import('@spark/shared').WorkflowDetail>(`/workflows/${encodeURIComponent(slug)}`)
/** 安装到某个 ComfyUI 部署：POST /instances/{id}/apps/{dep}/workflows（不带 version_id = 最新已发布版本）。 */
export const installWorkflow = (instanceId: string, deploymentId: string, workflowId: number, versionId?: number) =>
  api.post<{ deployment: import('@spark/shared').Deployment; workflows: import('@spark/shared').InstalledWorkflow[] }>(`/instances/${instanceId}/apps/${deploymentId}/workflows`, {
    workflow_id: workflowId, ...(versionId != null ? { version_id: versionId } : {}),
  })
// --- end workflows market (plan 11 Task 12) ---
