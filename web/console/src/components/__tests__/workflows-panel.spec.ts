import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import ElementPlus from 'element-plus'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, type QuickRun } from '@spark/shared'

// 挂载测试：api 全部打桩，验证 data-testid、增删改请求参数、快速出图的校验 → 上传 → 运行 → 2 秒轮询 → 输出渲染，以及关闭 / 卸载 / 出错时的状态。
// jsdom 没有 matchMedia（useMobile 需要）与 URL.createObjectURL（上传缩略图）。
Object.defineProperty(window, 'matchMedia', { value: (q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {} }) })

const m = vi.hoisted(() => ({
  fetchWorkflows: vi.fn(),
  fetchWorkflow: vi.fn(),
  getInstalledWorkflow: vi.fn(),
  installWorkflow: vi.fn(),
  updateDeploymentWorkflow: vi.fn(),
  removeDeploymentWorkflow: vi.fn(),
  uploadQuickRunImage: vi.fn(),
  startQuickRun: vi.fn(),
  getQuickRun: vi.fn(),
}))
vi.mock('../../api', () => m)

import QuickRunDrawer from '../QuickRunDrawer.vue'
import WorkflowsPanel from '../WorkflowsPanel.vue'

const catalogItem = (id: number, slug: string, gb: number) => ({
  id, slug, name: slug.toUpperCase(), summary: 's', category: 'txt2img', tags: [], cover_url: null, sort: id, installs: 0,
  version: { id: id * 10, version: '1.0.0', download_gb: gb, models: 1, nodes: 0, quick_run: true, comfyui_min: '' },
})
const detail = (versionId = 50) => ({ version: { id: versionId, version: versionId === 50 ? '1.0.0' : '1.1.0', changelog: '修复', manifest: { inputs: [
  { key: 'prompt', label: '提示词', type: 'textarea', node: '6', input: 'text', default: 'cat' },
  { key: 'width', label: '宽', type: 'int', node: '5', input: 'width', default: 1024, min: 512, max: 2048, step: 64 },
  { key: 'seed', label: '种子', type: 'seed', node: '3', input: 'seed' },
  { key: 'image', label: '参考图', type: 'image', node: '10', input: 'image' },
] } } })
/** 已装版本的快速出图参数（GET …/workflows/{wid}）。 */
const installed = (quickRun = true) => ({ workflow: wf({ update_available: false, latest_version_id: 50 }), inputs: detail().version.manifest.inputs, outputs: [], quick_run: quickRun })
const IMG = 'spark-quickrun/abcdef012345.png'
const runView = (over: Partial<QuickRun> = {}): QuickRun => ({
  id: 'r1', prompt_id: 'p', seed: 77, state: 'queued', files: [], message: '', created_at: '2026-10-24T10:00:00Z', finished_at: null, ...over,
})
const doneRun = () => runView({ state: 'done', finished_at: '2026-10-24T10:00:12Z', files: [
  { index: 0, filename: 'a.png', kind: 'image', url: '/api/v1/instances/ins-1/apps/dep-1/workflows/5/runs/r1/files/0' },
  { index: 1, filename: 'b.mp4', kind: 'video', url: '/api/v1/instances/ins-1/apps/dep-1/workflows/5/runs/r1/files/1' },
  { index: 2, filename: 'c.png', kind: 'image', url: 'https://evil.example/c.png' },
] })
const deferred = <T>() => {
  let resolve!: (v: T) => void
  const promise = new Promise<T>((r) => { resolve = r })
  return { promise, resolve }
}

const wf = (o = {}) => ({ workflow_id: 5, slug: 'sdxl', name: 'SDXL', category: 'txt2img', version_id: 50, version: '1.0.0', state: 'installed', latest_version_id: 51, latest_version: '1.1.0', update_available: true, quick_run: true, added_at: '', installed_at: '', ...o })
const dep = (o = {}) => ({ id: 'dep-1', app_slug: 'comfyui', app_name: 'ComfyUI', state: 'running', url: 'https://c.example/', workflows: [wf()], ...o })
const inst = (state = 'running') => ({ id: 'ins-1', state })
const q = (s: string) => document.querySelector(`[data-testid="${s}"]`) as HTMLElement | null
const plugins = { global: { plugins: [ElementPlus] }, attachTo: document.body }

let wrappers: VueWrapper[] = []
const mountPanel = (deployment = dep(), instance = inst()) => {
  const w = mount(WorkflowsPanel, { props: { instance: instance as never, deployment: deployment as never }, ...plugins })
  wrappers.push(w)
  return w
}
const mountDrawer = (workflow = wf({ update_available: false, latest_version_id: 50 })) => {
  const w = mount(QuickRunDrawer, { props: { modelValue: true, instanceId: 'ins-1', deployment: { id: 'dep-1', url: 'u', app_name: 'C' }, workflow: workflow as never }, ...plugins })
  wrappers.push(w)
  return w
}
/** 打开抽屉、填好图片（经表单 v-model），返回 wrapper。 */
async function readyDrawer() {
  const w = mountDrawer()
  await flushPromises()
  const form = w.findComponent({ name: 'QuickRunForm' })
  form.vm.$emit('update:modelValue', { ...(form.props('modelValue') as object), image: IMG })
  await flushPromises()
  return w
}
const runButtonBusy = () => q('qr-run')!.classList.contains('is-loading')

beforeEach(() => {
  document.body.innerHTML = ''
  vi.resetAllMocks()
  m.fetchWorkflows.mockResolvedValue([catalogItem(5, 'sdxl', 6.9), catalogItem(6, 'flux', 17)])
  m.fetchWorkflow.mockResolvedValue(detail())
  m.getInstalledWorkflow.mockResolvedValue(installed())
  m.installWorkflow.mockResolvedValue({})
  m.updateDeploymentWorkflow.mockResolvedValue({})
  m.removeDeploymentWorkflow.mockResolvedValue({})
  m.uploadQuickRunImage.mockResolvedValue({ name: IMG })
  m.startQuickRun.mockResolvedValue(runView())
  m.getQuickRun.mockResolvedValueOnce(runView({ state: 'running' })).mockResolvedValue(doneRun())
})
afterEach(() => {
  for (const w of wrappers) w.unmount()
  wrappers = []
  vi.useRealTimers()
})

describe('WorkflowsPanel', () => {
  it('行、状态、快速出图、上限、部署中', async () => {
    const w = mountPanel()
    await flushPromises()
    expect(q('wf-panel')).toBeTruthy()
    expect(q('wf-row-sdxl')).toBeTruthy()
    expect(q('wf-state-sdxl')!.textContent).toBe('已安装')
    expect(q('wf-quickrun-sdxl')).toBeTruthy()
    expect(q('wf-update-tag-sdxl')!.textContent).toBe('有新版本 v1.1.0')
    expect(q('wf-add')).toBeTruthy()
    await w.setProps({ deployment: dep({ workflows: Array.from({ length: 8 }, (_, i) => wf({ workflow_id: i, slug: `s${i}` })) }) as never })
    expect(q('wf-limit')!.textContent).toContain('最多安装 8 个')
    expect(q('wf-add')).toBeNull()
    await w.setProps({ deployment: dep({ state: 'installing', workflows: [wf({ state: 'pending' })] }) as never })
    expect(q('wf-installing')!.textContent).toBe('正在安装工作流…（随应用进度）')
    expect(q('wf-quickrun-sdxl')).toBeNull()
    expect(q('wf-add')).toBeNull()
  })

  it('更新（指定新版本）与移除：确认文案、请求参数、refresh', async () => {
    m.fetchWorkflow.mockResolvedValue(detail(51))
    const w = mountPanel()
    await w.find('[data-testid="wf-more-sdxl"]').trigger('click')
    await flushPromises()
    expect(q('wf-update-sdxl')!.textContent).toContain('更新到 v1.1.0')
    q('wf-update-sdxl')!.click()
    await flushPromises()
    expect(q('wf-confirm-text')!.textContent).toContain('从 v1.0.0 更新到 v1.1.0，会重启实例上的 ComfyUI')
    expect(q('wf-confirm-changelog')!.textContent).toContain('修复')
    q('wf-confirm-ok')!.click()
    await flushPromises()
    expect(m.updateDeploymentWorkflow).toHaveBeenCalledWith('ins-1', 'dep-1', 5, 51)
    q('wf-remove-sdxl')!.click()
    await flushPromises()
    expect(q('wf-confirm-text')!.textContent).toContain('移除「SDXL」后模型与节点保留在 /root/data/comfyui')
    q('wf-confirm-ok')!.click()
    await flushPromises()
    expect(m.removeDeploymentWorkflow).toHaveBeenCalledWith('ins-1', 'dep-1', 5)
    expect(w.emitted('refresh')).toHaveLength(2)
  })

  it('安装失败且无新版本：「重试安装」= PUT 不带 version_id', async () => {
    mountPanel(dep({ workflows: [wf({ state: 'failed', update_available: false, latest_version_id: 50, latest_version: '1.0.0' })] }))
    await flushPromises()
    expect(q('wf-failed-sdxl')).toBeTruthy()
    ;(q('wf-more-sdxl') as HTMLElement).click()
    await flushPromises()
    expect(q('wf-update-sdxl')!.textContent).toContain('重试安装')
    q('wf-update-sdxl')!.click()
    await flushPromises()
    expect(q('wf-confirm-text')!.textContent).toContain('重新安装「SDXL」v1.0.0')
    q('wf-confirm-ok')!.click()
    await flushPromises()
    expect(m.updateDeploymentWorkflow).toHaveBeenCalledWith('ins-1', 'dep-1', 5, null)
  })

  it('实例停机：添加 / 更新 / 移除置灰并说明原因，点了也不发请求', async () => {
    mountPanel(dep({ state: 'stopped' }), inst('stopped'))
    await flushPromises()
    expect(q('wf-add')!.hasAttribute('disabled')).toBe(true)
    expect(q('wf-add-blocked')!.textContent).toBe('实例已停机，续费后可操作')
    ;(q('wf-more-sdxl') as HTMLElement).click()
    await flushPromises()
    expect(q('wf-remove-sdxl')!.getAttribute('aria-disabled')).toBe('true')
    q('wf-remove-sdxl')!.click()
    await flushPromises()
    expect(q('wf-confirm-dialog')).toBeNull()
    expect(m.removeDeploymentWorkflow).not.toHaveBeenCalled()
  })

  it('添加：已装灰显，选中后确认安装', async () => {
    const w = mountPanel()
    await w.find('[data-testid="wf-add"]').trigger('click')
    await flushPromises()
    expect(q('wf-add-item-sdxl')!.dataset.disabled).toBe('true')
    q('wf-add-item-sdxl')!.click()
    await flushPromises()
    expect(q('wf-add-confirm-text')).toBeNull()
    q('wf-add-item-flux')!.click()
    await flushPromises()
    expect(q('wf-add-confirm-text')!.textContent).toContain('需下载约 17 GB')
    q('wf-add-confirm')!.click()
    await flushPromises()
    expect(m.installWorkflow).toHaveBeenCalledWith('ins-1', 'dep-1', 6)
    expect(w.emitted('refresh')).toBeTruthy()
  })
})

describe('QuickRunDrawer', () => {
  it('校验 → 运行 → 轮询 → 输出；只渲染本站文件地址；workflow_busy', async () => {
    vi.useFakeTimers()
    const w = mountDrawer()
    await flushPromises()
    expect(q('qr-drawer')).toBeTruthy()
    expect(q('qr-input-prompt')).toBeTruthy()
    q('qr-run')!.click()
    await flushPromises()
    expect(q('qr-input-image-error')!.textContent).toBe('必填')
    expect(m.startQuickRun).not.toHaveBeenCalled()
    const form = w.findComponent({ name: 'QuickRunForm' })
    form.vm.$emit('update:modelValue', { ...(form.props('modelValue') as object), image: IMG })
    await flushPromises()
    q('qr-run')!.click()
    await flushPromises()
    expect(m.startQuickRun).toHaveBeenCalledWith('ins-1', 'dep-1', 5, { prompt: 'cat', width: 1024, image: IMG })
    expect(q('qr-status')!.textContent).toBe('排队中')
    expect(q('qr-status')!.getAttribute('aria-live')).toBe('polite')
    expect(q('qr-seed')!.textContent).toBe('本次种子：77')
    expect(runButtonBusy()).toBe(true)
    await vi.advanceTimersByTimeAsync(2000)
    expect(q('qr-status')!.textContent).toBe('生成中')
    await vi.advanceTimersByTimeAsync(2000)
    expect(q('qr-status')!.textContent).toBe('完成（12 秒）')
    expect(runButtonBusy()).toBe(false)
    expect(q('qr-file-0')!.querySelector('img')!.getAttribute('src')).toBe('/api/v1/instances/ins-1/apps/dep-1/workflows/5/runs/r1/files/0')
    expect(q('qr-file-1')!.querySelector('video')!.getAttribute('aria-label')).toBe('b.mp4')
    expect(q('qr-file-0-download')!.getAttribute('download')).toBe('a.png')
    expect(q('qr-file-2')!.querySelector('img')).toBeNull()
    expect(q('qr-file-2-download')).toBeNull()
    await vi.advanceTimersByTimeAsync(10_000)
    expect(m.getQuickRun).toHaveBeenCalledTimes(2)
    m.startQuickRun.mockRejectedValueOnce(new ApiError(429, 'workflow_busy', 'x'))
    q('qr-run')!.click()
    await flushPromises()
    expect(q('qr-error')!.textContent).toContain('ComfyUI 正在处理其它任务，请稍后再试')
  })

  it('轮询遇到 4xx：停止轮询、显示错误、按钮恢复可用', async () => {
    vi.useFakeTimers()
    m.getQuickRun.mockReset().mockRejectedValue(new ApiError(409, 'app_invalid_state', 'ComfyUI 未运行'))
    await readyDrawer()
    q('qr-run')!.click()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(2000)
    expect(q('qr-error')!.textContent).toContain('ComfyUI 未运行')
    expect(runButtonBusy()).toBe(false)
    expect(q('qr-run')!.textContent).toContain('运行')
    await vi.advanceTimersByTimeAsync(10_000)
    expect(m.getQuickRun).toHaveBeenCalledTimes(1)
  })

  it('轮询遇到 5xx / 网络错误：提示后继续，成功后提示消失', async () => {
    vi.useFakeTimers()
    m.getQuickRun.mockReset().mockRejectedValueOnce(new ApiError(503, 'upstream_unreachable', '无法连接实例内的 ComfyUI')).mockResolvedValue(doneRun())
    await readyDrawer()
    q('qr-run')!.click()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(2000)
    expect(q('qr-poll-warn')!.textContent).toContain('正在重试')
    expect(runButtonBusy()).toBe(true)
    await vi.advanceTimersByTimeAsync(2000)
    expect(q('qr-poll-warn')).toBeNull()
    expect(q('qr-status')!.textContent).toBe('完成（12 秒）')
  })

  it('满 30 分钟停止轮询并提示', async () => {
    vi.useFakeTimers()
    m.getQuickRun.mockReset().mockResolvedValue(runView({ state: 'running' }))
    await readyDrawer()
    q('qr-run')!.click()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(30 * 60 * 1000 + 4000)
    expect(q('qr-poll-stopped')!.textContent).toContain('已等待 30 分钟')
    expect(runButtonBusy()).toBe(false)
    const n = m.getQuickRun.mock.calls.length
    await vi.advanceTimersByTimeAsync(10_000)
    expect(m.getQuickRun).toHaveBeenCalledTimes(n)
  })

  it('关闭抽屉 / 卸载后停止轮询', async () => {
    vi.useFakeTimers()
    m.getQuickRun.mockReset().mockResolvedValue(runView({ state: 'running' }))
    const w = await readyDrawer()
    q('qr-run')!.click()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(2000)
    expect(m.getQuickRun).toHaveBeenCalledTimes(1)
    await w.setProps({ modelValue: false })
    await vi.advanceTimersByTimeAsync(10_000)
    expect(m.getQuickRun).toHaveBeenCalledTimes(1)
    w.unmount()
    wrappers = []
    document.body.innerHTML = ''

    const w2 = await readyDrawer()
    q('qr-run')!.click()
    await flushPromises()
    w2.unmount()
    wrappers = wrappers.filter((x) => x !== w2)
    await vi.advanceTimersByTimeAsync(10_000)
    expect(m.getQuickRun).toHaveBeenCalledTimes(1)
  })

  it('POST /runs 未返回时关闭再打开：状态复位，迟到的响应被丢弃', async () => {
    const d = deferred<QuickRun>()
    m.startQuickRun.mockReturnValueOnce(d.promise)
    const w = await readyDrawer()
    q('qr-run')!.click()
    await flushPromises()
    expect(runButtonBusy()).toBe(true)
    await w.setProps({ modelValue: false })
    await w.setProps({ modelValue: true })
    await flushPromises()
    expect(runButtonBusy()).toBe(false)
    d.resolve(runView())
    await flushPromises()
    expect(q('qr-status')).toBeNull()
    expect(runButtonBusy()).toBe(false)
    expect(m.getQuickRun).not.toHaveBeenCalled()
  })

  it('上传图片：调上传接口、显示缩略图、运行时带上返回的名字', async () => {
    const created: string[] = []
    URL.createObjectURL = vi.fn(() => { created.push('blob:1'); return 'blob:1' })
    URL.revokeObjectURL = vi.fn()
    mountDrawer()
    await flushPromises()
    const input = q('qr-input-image')!.querySelector('input[type=file]') as HTMLInputElement
    const file = new File([new Uint8Array([137, 80, 78, 71])], 'cat.png', { type: 'image/png' })
    Object.defineProperty(input, 'files', { value: [file] })
    input.dispatchEvent(new Event('change'))
    await flushPromises()
    expect(m.uploadQuickRunImage).toHaveBeenCalledWith('ins-1', 'dep-1', 5, expect.any(File))
    expect(q('qr-input-image-preview')!.getAttribute('src')).toBe('blob:1')
    q('qr-run')!.click()
    await flushPromises()
    expect(m.startQuickRun).toHaveBeenCalledWith('ins-1', 'dep-1', 5, { prompt: 'cat', width: 1024, image: IMG })
  })

  it('上传在卸载后才返回：不再创建缩略图 URL', async () => {
    URL.createObjectURL = vi.fn(() => 'blob:2')
    URL.revokeObjectURL = vi.fn()
    const d = deferred<{ name: string }>()
    m.uploadQuickRunImage.mockReturnValueOnce(d.promise)
    const w = mountDrawer()
    await flushPromises()
    const input = q('qr-input-image')!.querySelector('input[type=file]') as HTMLInputElement
    Object.defineProperty(input, 'files', { value: [new File([new Uint8Array([1])], 'a.png', { type: 'image/png' })] })
    input.dispatchEvent(new Event('change'))
    await flushPromises()
    w.unmount()
    wrappers = []
    d.resolve({ name: IMG })
    await flushPromises()
    expect(URL.createObjectURL).not.toHaveBeenCalled()
  })

  it('表单按已装版本的 inputs 渲染（GET …/workflows/{wid}）', async () => {
    mountDrawer()
    await flushPromises()
    expect(m.getInstalledWorkflow).toHaveBeenCalledWith('ins-1', 'dep-1', 5)
    expect(m.fetchWorkflow).not.toHaveBeenCalled()
    expect(q('qr-input-width')).toBeTruthy()
    expect(q('qr-load-error')).toBeNull()
    expect(q('qr-run')!.hasAttribute('disabled')).toBe(false)
  })

  it('参数加载失败（含 404 workflow_not_found）或版本不支持快速出图：不可运行', async () => {
    m.getInstalledWorkflow.mockRejectedValueOnce(new ApiError(404, 'workflow_not_found', '工作流不存在或未上架'))
    const w = mountDrawer()
    await flushPromises()
    expect(q('qr-load-error')!.textContent).toContain('参数表单加载失败：工作流不存在或未上架')
    expect(q('qr-run')!.hasAttribute('disabled')).toBe(true)
    w.unmount()
    wrappers = []
    document.body.innerHTML = ''
    m.getInstalledWorkflow.mockResolvedValueOnce(installed(false))
    mountDrawer()
    await flushPromises()
    expect(q('qr-load-error')!.textContent).toContain('不支持快速出图')
    expect(q('qr-run')!.hasAttribute('disabled')).toBe(true)
  })
})
