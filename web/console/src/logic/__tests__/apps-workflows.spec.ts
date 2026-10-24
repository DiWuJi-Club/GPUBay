import type { Deployment, InstalledWorkflow, WorkflowCatalogItem } from '@spark/shared'
import { describe, expect, it } from 'vitest'
import {
  MAX_WORKFLOWS, addWorkflowReason, canChangeWorkflows, deploymentWorkflows, isWorkflowHost, workflowAddConfirmText, workflowPanelNotice,
  workflowRemoveConfirmText, workflowRowActions, workflowUpdateConfirmText,
} from '../apps'

const dep = (over: Partial<Deployment> = {}): Deployment => ({
  id: 'dep-1', instance_id: 'ins-1', app_id: 2, app_slug: 'comfyui', app_name: 'ComfyUI', app_icon: 'image', version_id: 20, version: '1.1.0',
  latest_version_id: 20, latest_version: '1.1.0', update_available: false, state: 'running', desired: 'running', phase: '', progress: 0, message: '',
  health: 'ok', error_code: '', env: {}, secret_env: {}, spec: { ports: [] }, port: 8188, url: 'https://x.example/', links: [], attempt: 1, cleanup_pending: false,
  started_at: null, finished_at: null, created_at: '2026-10-20T00:00:00Z', updated_at: '2026-10-20T00:00:00Z', ...over,
})
const wf = (over: Partial<InstalledWorkflow> = {}): InstalledWorkflow => ({
  workflow_id: 5, slug: 'sdxl-txt2img', name: 'SDXL 文生图', category: 'txt2img', version_id: 50, version: '1.0.0', state: 'installed',
  latest_version_id: 50, latest_version: '1.0.0', update_available: false, quick_run: true, added_at: '2026-10-24T00:00:00Z', installed_at: '2026-10-24T00:10:00Z',
  ...over,
})
const item = (over: Partial<WorkflowCatalogItem> = {}): WorkflowCatalogItem => ({
  id: 5, slug: 'sdxl-txt2img', name: 'SDXL 文生图', summary: '', category: 'txt2img', tags: [], cover_url: null, sort: 0, installs: 0,
  version: { id: 50, version: '1.0.0', download_gb: 6.94, models: 1, nodes: 0, quick_run: true, comfyui_min: '' }, ...over,
})

describe('部署视图里的 workflows（计划 11）', () => {
  it('只有 comfyui 部署显示工作流子区块', () => {
    expect(isWorkflowHost(dep())).toBe(true)
    expect(isWorkflowHost(dep({ app_slug: 'vllm' }))).toBe(false)
  })
  it('workflows 缺省 / null 视为空数组', () => {
    expect(deploymentWorkflows(dep())).toEqual([])
    expect(deploymentWorkflows(dep({ workflows: null as unknown as InstalledWorkflow[] }))).toEqual([])
    expect(deploymentWorkflows(dep({ workflows: [wf()] }))).toEqual([wf()])
  })
  it('上限 8 个', () => expect(MAX_WORKFLOWS).toBe(8))
  it('增删改只在部署 queued / running / stopped / failed 时可用（与后端 Installer 一致）', () => {
    for (const s of ['queued', 'running', 'stopped', 'failed'] as const) expect(canChangeWorkflows(dep({ state: s }))).toBe(true)
    for (const s of ['installing', 'removing', 'removed', 'lost'] as const) expect(canChangeWorkflows(dep({ state: s }))).toBe(false)
  })
  it('部署中时子区块提示「正在安装工作流…」（有工作流时）', () => {
    expect(workflowPanelNotice(dep({ state: 'installing', workflows: [wf({ state: 'pending' })] }))).toBe('正在安装工作流…（随应用进度）')
    expect(workflowPanelNotice(dep({ state: 'installing', workflows: [] }))).toBeNull()
    expect(workflowPanelNotice(dep({ state: 'running', workflows: [wf()] }))).toBeNull()
  })
})

describe('workflowRowActions', () => {
  it('运行中且已安装、支持快速出图：打开 + 快速出图 + 移除', () => {
    expect(workflowRowActions(wf(), dep(), 'running')).toEqual({ open: true, quickRun: true, quickRunDisabled: false, update: null, remove: true })
  })
  it('快速出图需要 quick_run、行已安装、部署运行中；实例非运行时置灰', () => {
    expect(workflowRowActions(wf({ quick_run: false }), dep(), 'running').quickRun).toBe(false)
    expect(workflowRowActions(wf({ state: 'pending' }), dep(), 'running').quickRun).toBe(false)
    expect(workflowRowActions(wf({ state: 'failed' }), dep(), 'running').quickRun).toBe(false)
    expect(workflowRowActions(wf(), dep({ state: 'stopped' }), 'running').quickRun).toBe(false)
    expect(workflowRowActions(wf(), dep(), 'stopped')).toMatchObject({ quickRun: true, quickRunDisabled: true })
  })
  it('打开 ComfyUI 需要部署运行中且有 url', () => {
    expect(workflowRowActions(wf(), dep({ url: null }), 'running').open).toBe(false)
    expect(workflowRowActions(wf(), dep({ state: 'queued' }), 'running').open).toBe(false)
  })
  it('有新版本 → 更新；安装失败且无新版本 → 重试安装', () => {
    expect(workflowRowActions(wf({ update_available: true, latest_version_id: 51, latest_version: '1.1.0' }), dep(), 'running').update).toBe('update')
    expect(workflowRowActions(wf({ state: 'failed' }), dep(), 'running').update).toBe('retry')
    expect(workflowRowActions(wf({ state: 'failed', update_available: true }), dep(), 'running').update).toBe('update')
  })
  it('部署中：不能更新 / 移除', () => {
    expect(workflowRowActions(wf({ update_available: true }), dep({ state: 'installing' }), 'running')).toMatchObject({ update: null, remove: false })
  })
})

describe('添加 / 更新 / 移除的文案', () => {
  it('已装的灰显', () => {
    expect(addWorkflowReason(item(), [wf()])).toBe('已安装')
    expect(addWorkflowReason(item({ id: 6 }), [wf()])).toBeNull()
  })
  it('安装确认：重启 ComfyUI 与下载量（D9）', () => {
    expect(workflowAddConfirmText(item())).toBe('安装「SDXL 文生图」会重启实例上的 ComfyUI，正在生成的任务会中断；需下载约 6.94 GB 模型')
    expect(workflowAddConfirmText(item({ version: { ...item().version, download_gb: 0 } }))).toBe('安装「SDXL 文生图」会重启实例上的 ComfyUI，正在生成的任务会中断')
  })
  it('更新确认：新版本 + 重启', () => {
    const w = wf({ update_available: true, latest_version_id: 51, latest_version: '1.1.0' })
    expect(workflowUpdateConfirmText(w)).toBe('将「SDXL 文生图」从 v1.0.0 更新到 v1.1.0，会重启实例上的 ComfyUI，正在生成的任务会中断')
    expect(workflowUpdateConfirmText(wf({ state: 'failed' }))).toBe('重新安装「SDXL 文生图」v1.0.0，会重启实例上的 ComfyUI，正在生成的任务会中断')
  })
  it('移除确认：模型与节点保留（D8）+ 重启', () => {
    expect(workflowRemoveConfirmText(wf())).toBe('移除「SDXL 文生图」后模型与节点保留在 /root/data/comfyui（可手动删除释放空间）；会重启实例上的 ComfyUI，正在生成的任务会中断')
  })
})
