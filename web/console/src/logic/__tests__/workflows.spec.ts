import type { AppCatalogItem, Deployment, Instance, Product, WorkflowCatalogItem, WorkflowModel } from '@spark/shared'
import { describe, expect, it } from 'vitest'
import { workflowAddConfirmText } from '../apps'
import {
  canInstallTo, mapLimit, safeGitUrl, targetHint, coverOrPlaceholder, filterWorkflows, installCandidates, installConfirmText, installTargets, isWorkflowApp,
  markdownBlocks, MAX_WORKFLOWS, modelSource, presetEnvFor, rentChoice, shortCommit, sortWorkflows, toggleWorkflow, totalDownloadGB,
  type InstallTarget,
} from '../workflows'

const wf = (over: Partial<WorkflowCatalogItem> & { gb?: number } = {}): WorkflowCatalogItem => {
  const { gb = 6.94, ...rest } = over
  return {
    id: 1, slug: 'sdxl-txt2img', name: 'SDXL 文生图', summary: '入门首选', category: 'txt2img', tags: ['SDXL', '入门'], cover_url: null, sort: 10, installs: 0,
    version: { id: 11, version: '1.0.0', download_gb: gb, models: 1, nodes: 0, quick_run: true, comfyui_min: '0.3.60' },
    ...rest,
  }
}

const inst = (over: Partial<Instance> = {}): Instance => ({
  id: 'ins-a', state: 'running', product_id: 1, image_id: 1, product_name: 'DGX Spark', image_name: 'Ubuntu', suspended: false, expires_at: null,
  remaining_seconds: 0, pending_seconds: 0, retain_until: null, ssh_command: '', password_auth: true, ssh_host: '', ssh_port: 22, ports_count: 0,
  scheduled_start_at: null, scheduled_end_at: null, cancellable_until: null, cancellable_seconds: 0, starts_in_seconds: 0, group_id: null, group_name: '',
  machine_label: '', batch_id: null, reassign_count: 0, auto_renew: false, auto_renew_duration: '1d', auto_renew_failed_reason: '', renewable_until: null,
  renew_options: [], apps_summary: [{ id: 'dep-1', name: 'ComfyUI', state: 'running', progress: 100, url: null }], created_at: '2026-10-24T00:00:00Z', ...over,
})

const dep = (over: Partial<Deployment> = {}): Deployment => ({
  id: 'dep-1', instance_id: 'ins-a', app_id: 3, app_slug: 'comfyui', app_name: 'ComfyUI', app_icon: '', version_id: 5, version: '1.1.0',
  latest_version_id: null, latest_version: '', update_available: false, state: 'running', desired: 'running', phase: '', progress: 100, message: '',
  health: 'ok', error_code: '', env: {}, secret_env: {}, spec: { ports: [{ port: 8188 }] }, port: 8188, url: null, links: [], attempt: 1,
  cleanup_pending: false, started_at: null, finished_at: null, created_at: '', updated_at: '', workflows: [], ...over,
})

const installed = (workflowId: number) => ({
  workflow_id: workflowId, slug: `w${workflowId}`, name: `W${workflowId}`, category: 'txt2img' as const, version_id: 1, version: '1.0.0', state: 'installed' as const,
  latest_version_id: 1, latest_version: '1.0.0', update_available: false, quick_run: false, added_at: '', installed_at: null,
})

describe('filterWorkflows / sortWorkflows', () => {
  const items = [
    wf({ id: 1, slug: 'a', name: 'SDXL 文生图', category: 'txt2img', tags: ['SDXL'], sort: 10 }),
    wf({ id: 2, slug: 'b', name: 'SDXL 图生图', category: 'img2img', tags: ['重绘'], sort: 20 }),
    wf({ id: 3, slug: 'c', name: 'Wan 视频', category: 'video', tags: ['Wan2.2'], sort: 5 }),
  ]
  it('全部 + 空搜索：原样', () => {
    expect(filterWorkflows(items, { category: 'all', query: '' }).map((w) => w.id)).toEqual([1, 2, 3])
  })
  it('按分类', () => {
    expect(filterWorkflows(items, { category: 'img2img', query: '' }).map((w) => w.id)).toEqual([2])
    expect(filterWorkflows(items, { category: 'upscale', query: '' })).toEqual([])
  })
  it('搜索名称与标签，忽略大小写与首尾空白', () => {
    expect(filterWorkflows(items, { category: 'all', query: ' sdxl ' }).map((w) => w.id)).toEqual([1, 2])
    expect(filterWorkflows(items, { category: 'all', query: '重绘' }).map((w) => w.id)).toEqual([2])
    expect(filterWorkflows(items, { category: 'all', query: 'wan2.2' }).map((w) => w.id)).toEqual([3])
    expect(filterWorkflows(items, { category: 'txt2img', query: '重绘' })).toEqual([])
  })
  it('sort 升序，同 sort 按名称；不改原数组', () => {
    const list = [wf({ id: 1, name: 'B', sort: 10 }), wf({ id: 2, name: 'A', sort: 10 }), wf({ id: 3, name: 'Z', sort: 1 })]
    expect(sortWorkflows(list).map((w) => w.id)).toEqual([3, 2, 1])
    expect(list.map((w) => w.id)).toEqual([1, 2, 3])
  })
})

describe('totalDownloadGB / toggleWorkflow', () => {
  const items = [wf({ id: 1, gb: 6.94 }), wf({ id: 2, gb: 17.24 }), wf({ id: 3, gb: 0.1 })]
  it('只算选中的，两位小数', () => {
    expect(totalDownloadGB(items, [])).toBe(0)
    expect(totalDownloadGB(items, [1, 2])).toBe(24.18)
    expect(totalDownloadGB(items, [3, 1, 99])).toBe(7.04)
  })
  it('选中 / 取消，最多 MAX_WORKFLOWS 个', () => {
    expect(toggleWorkflow([], 1)).toEqual([1])
    expect(toggleWorkflow([1, 2], 1)).toEqual([2])
    const full = Array.from({ length: MAX_WORKFLOWS }, (_, i) => i + 1)
    expect(toggleWorkflow(full, 99)).toEqual(full)
    expect(toggleWorkflow(full, 3)).toHaveLength(MAX_WORKFLOWS - 1)
    expect(toggleWorkflow([1], 2, 1)).toEqual([1])
  })
})

describe('installCandidates / installTargets / canInstallTo', () => {
  it('候选实例：可管理应用的状态且有应用摘要，最多 20 台', () => {
    const list = [
      inst({ id: 'a' }), inst({ id: 'b', state: 'released' }), inst({ id: 'c', apps_summary: [] }), inst({ id: 'd', state: 'stopped' }),
      inst({ id: 'e', apps_summary: undefined }), inst({ id: 'f', state: 'reserved' }),
    ]
    expect(installCandidates(list)).toEqual({ instances: list.filter((i) => ['a', 'd', 'e', 'f'].includes(i.id)), truncated: false })
    const many = installCandidates(Array.from({ length: 30 }, (_, i) => inst({ id: `x${i}` })))
    expect(many.instances).toHaveLength(20)
    expect(many.truncated).toBe(true)
    expect(installCandidates(Array.from({ length: 20 }, (_, i) => inst({ id: `y${i}` }))).truncated).toBe(false)
  })
  it('目标：只挑 comfyui 部署，带已装数与是否已装', () => {
    const t = installTargets([
      { instance: inst({ id: 'ins-a', product_name: 'DGX Spark' }), deployments: [dep({ id: 'd1', workflows: [installed(7), installed(8)] }), dep({ id: 'd2', app_slug: 'vllm' })] },
      { instance: inst({ id: 'ins-b' }), deployments: [dep({ id: 'd3', instance_id: 'ins-b', state: 'installing', workflows: undefined })] },
      { instance: inst({ id: 'ins-c' }), deployments: [dep({ id: 'd4', instance_id: 'ins-c', state: 'removed' })] },
    ], 7)
    expect(t).toEqual([
      { instanceId: 'ins-a', instanceName: 'DGX Spark · ins-a', deploymentId: 'd1', state: 'running', installedCount: 2, installed: true },
      { instanceId: 'ins-b', instanceName: 'DGX Spark · ins-b', deploymentId: 'd3', state: 'installing', installedCount: 0, installed: false },
    ])
  })
  const target = (over: Partial<InstallTarget> = {}): InstallTarget => ({ instanceId: 'i', instanceName: 'n', deploymentId: 'd', state: 'running', installedCount: 0, installed: false, ...over })
  it('canInstallTo：状态、上限、已装', () => {
    for (const s of ['queued', 'running', 'stopped', 'failed'] as const) expect(canInstallTo(target({ state: s }))).toEqual({ ok: true, reason: '' })
    expect(canInstallTo(target({ state: 'installing' }))).toEqual({ ok: false, reason: 'ComfyUI 正在部署，请稍后再装' })
    expect(canInstallTo(target({ state: 'removing' })).ok).toBe(false)
    expect(canInstallTo(target({ installedCount: 8 }))).toEqual({ ok: false, reason: '已装满 8 个工作流' })
    expect(canInstallTo(target({ installedCount: 7 })).ok).toBe(true)
    expect(canInstallTo(target({ installed: true }))).toEqual({ ok: false, reason: '已安装' })
  })
  it('targetHint：排队 / 失败的部署给出说明', () => {
    expect(targetHint('queued')).toBe('排队中：随 ComfyUI 部署一起安装')
    expect(targetHint('failed')).toBe('ComfyUI 部署失败：安装时会重新部署')
    expect(targetHint('running')).toBe('')
  })
})

describe('installConfirmText / coverOrPlaceholder / 小工具', () => {
  it('确认文案含「重启 ComfyUI」与下载量', () => {
    const t = installConfirmText(wf({ gb: 17.24 }))
    expect(t).toBe(workflowAddConfirmText(wf({ gb: 17.24 }))) // 与实例详情「添加工作流」同一文案
    expect(t).toContain('重启实例上的 ComfyUI')
    expect(t).toContain('需下载约 17.2 GB')
    expect(installConfirmText(wf({ gb: 0 }))).not.toContain('下载')
  })
  it('封面：有 cover_url 用图，否则分类占位', () => {
    expect(coverOrPlaceholder(wf({ cover_url: '/api/v1/workflows/a/cover?v=1' }))).toEqual({ kind: 'image', url: '/api/v1/workflows/a/cover?v=1' })
    expect(coverOrPlaceholder(wf({ category: 'video' }))).toEqual({ kind: 'placeholder', category: 'video', label: '视频' })
  })
  it('shortCommit / modelSource', () => {
    expect(shortCommit('0123456789abcdef0123456789abcdef01234567')).toBe('01234567')
    const m: WorkflowModel = { name: 'a', dir: 'checkpoints', size_gb: 1, sha256: '', modelscope: { repo: 'AI/x', file: 'a.safetensors' }, hf: { repo: 'org/x', file: 'a.safetensors' } }
    expect(modelSource(m)).toBe('ModelScope AI/x · HuggingFace org/x')
    expect(modelSource({ ...m, modelscope: undefined })).toBe('HuggingFace org/x')
  })
  it('isWorkflowApp：comfyui 且版本未声明不支持', () => {
    const app = (slug: string, sw?: boolean) => ({ slug, version: { supports_workflows: sw } }) as unknown as AppCatalogItem
    expect(isWorkflowApp(app('comfyui', true))).toBe(true)
    expect(isWorkflowApp(app('comfyui'))).toBe(true)
    expect(isWorkflowApp(app('comfyui', false))).toBe(false)
    expect(isWorkflowApp(app('vllm', true))).toBe(false)
    expect(isWorkflowApp(null)).toBe(false)
  })
  it('presetEnvFor：只保留应用变量里存在且合法的预设', () => {
    const app = { version: { env: [{ name: 'MODEL_ID', type: 'enum', options: ['none', 'sdxl'] }, { name: 'X', type: 'string' }] } } as unknown as AppCatalogItem
    expect(presetEnvFor(app, { MODEL_ID: 'none', Y: '1' })).toEqual({ MODEL_ID: 'none' })
    expect(presetEnvFor(app, { MODEL_ID: 'flux' })).toEqual({}) // 旧版本没有 none：沿用默认
    expect(presetEnvFor(app, { X: 'v' })).toEqual({ X: 'v' })
  })
  it('rentChoice：优先有空闲的可租商品，否则预订，都没有为 null', () => {
    const p = (id: number, idle: number, prices = 1) => ({ id, idle_count: idle, prices: Array.from({ length: prices }, () => ({ duration: '1d', amount: 1 })) }) as unknown as Product
    expect(rentChoice([p(1, 0), p(2, 3)])).toEqual({ product: p(2, 3), mode: 'immediate' })
    expect(rentChoice([p(1, 0, 0), p(2, 0)])).toEqual({ product: p(2, 0), mode: 'booking' })
    expect(rentChoice([p(1, 5, 0)])).toBeNull()
    expect(rentChoice([])).toBeNull()
  })
})

describe('markdownBlocks（描述的 Markdown 子集）', () => {
  it('段落、列表、加粗、行内代码；不产生 HTML', () => {
    const blocks = markdownBlocks('第一段 **重点** 与 `code`\n续行\n\n- 项一\n- 项 **二**\n\n<script>x</script>')
    expect(blocks).toEqual([
      { type: 'p', inlines: [{ kind: 'text', text: '第一段 ' }, { kind: 'bold', text: '重点' }, { kind: 'text', text: ' 与 ' }, { kind: 'code', text: 'code' }, { kind: 'text', text: ' 续行' }] },
      { type: 'ul', items: [[{ kind: 'text', text: '项一' }], [{ kind: 'text', text: '项 ' }, { kind: 'bold', text: '二' }]] },
      { type: 'p', inlines: [{ kind: 'text', text: '<script>x</script>' }] },
    ])
  })
  it('空文本：没有块；未闭合的标记按原文', () => {
    expect(markdownBlocks('')).toEqual([])
    expect(markdownBlocks('a **b')).toEqual([{ type: 'p', inlines: [{ kind: 'text', text: 'a **b' }] }])
  })
})

describe('mapLimit / safeGitUrl', () => {
  it('并发上限内执行，结果按输入顺序、失败不影响其它', async () => {
    let running = 0
    let peak = 0
    const out = await mapLimit([1, 2, 3, 4, 5, 6, 7], 3, async (n) => {
      running++
      peak = Math.max(peak, running)
      await new Promise((r) => setTimeout(r, 5))
      running--
      if (n === 4) throw new Error('boom')
      return n * 10
    })
    expect(peak).toBe(3)
    expect(out.map((r) => (r.status === 'fulfilled' ? r.value : 'x'))).toEqual([10, 20, 30, 'x', 50, 60, 70])
    expect(await mapLimit([], 3, async () => 1)).toEqual([])
  })
  it('只把 https 地址当链接', () => {
    expect(safeGitUrl('https://github.com/a/b.git')).toBe('https://github.com/a/b.git')
    expect(safeGitUrl('javascript:alert(1)')).toBeNull()
    expect(safeGitUrl('http://github.com/a/b')).toBeNull()
    expect(safeGitUrl('HTTPS://x')).toBeNull()
  })
})
