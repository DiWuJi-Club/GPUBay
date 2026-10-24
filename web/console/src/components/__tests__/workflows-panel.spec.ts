import { flushPromises, mount } from '@vue/test-utils'
import ElementPlus from 'element-plus'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError } from '@spark/shared'

// 挂载冒烟：api 全部打桩，验证 data-testid、增删改请求参数、快速出图的校验 → 运行 → 2 秒轮询 → 输出渲染。jsdom 没有 matchMedia（useMobile 需要）。

Object.defineProperty(window, 'matchMedia', { value: (q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {} }) })
const calls: string[] = []
const state = { polls: 0, startErr: null as unknown }
vi.mock('../../api', () => ({
  panelWorkflowCatalog: vi.fn(async () => ({ items: [
    { id: 5, slug: 'sdxl', name: 'SDXL', summary: 's', category: 'txt2img', tags: [], cover_url: null, sort: 1, installs: 0, version: { id: 50, version: '1.0.0', download_gb: 6.9, models: 1, nodes: 0, quick_run: true, comfyui_min: '' } },
    { id: 6, slug: 'flux', name: 'Flux', summary: 's', category: 'txt2img', tags: [], cover_url: null, sort: 2, installs: 0, version: { id: 60, version: '1.0.0', download_gb: 17, models: 2, nodes: 1, quick_run: true, comfyui_min: '' } },
  ] })),
  panelWorkflowDetail: vi.fn(async () => ({ version: { id: 50, version: '1.0.0', changelog: 'c', manifest: { inputs: [
    { key: 'prompt', label: '提示词', type: 'textarea', node: '6', input: 'text', default: 'cat' },
    { key: 'width', label: '宽', type: 'int', node: '5', input: 'width', default: 1024, min: 512, max: 2048, step: 64 },
    { key: 'seed', label: '种子', type: 'seed', node: '3', input: 'seed' },
    { key: 'image', label: '参考图', type: 'image', node: '10', input: 'image' },
  ] } } })),
  addDeploymentWorkflow: vi.fn(async (...a: unknown[]) => { calls.push('add:' + JSON.stringify(a)); return {} }),
  updateDeploymentWorkflow: vi.fn(async (...a: unknown[]) => { calls.push('update:' + JSON.stringify(a)); return {} }),
  removeDeploymentWorkflow: vi.fn(async (...a: unknown[]) => { calls.push('remove:' + JSON.stringify(a)); return {} }),
  uploadQuickRunImage: vi.fn(async () => ({ name: 'spark-quickrun/abcdef012345.png' })),
  startQuickRun: vi.fn(async (...a: unknown[]) => {
    calls.push('run:' + JSON.stringify(a))
    if (state.startErr) throw state.startErr
    return { id: 'r1', prompt_id: 'p', seed: 77, state: 'queued', files: [], message: '', created_at: '2026-10-24T10:00:00Z', finished_at: null }
  }),
  getQuickRun: vi.fn(async () => {
    state.polls++
    if (state.polls < 2) return { id: 'r1', prompt_id: 'p', seed: 77, state: 'running', files: [], message: '', created_at: '2026-10-24T10:00:00Z', finished_at: null }
    return { id: 'r1', prompt_id: 'p', seed: 77, state: 'done', files: [{ index: 0, filename: 'a.png', kind: 'image', url: '/api/v1/x/files/0' }, { index: 1, filename: 'b.mp4', kind: 'video', url: '/api/v1/x/files/1' }], message: '', created_at: '2026-10-24T10:00:00Z', finished_at: '2026-10-24T10:00:12Z' }
  }),
}))

import WorkflowsPanel from '../WorkflowsPanel.vue'
import QuickRunDrawer from '../QuickRunDrawer.vue'

const wf = (o = {}) => ({ workflow_id: 5, slug: 'sdxl', name: 'SDXL', category: 'txt2img', version_id: 50, version: '1.0.0', state: 'installed', latest_version_id: 51, latest_version: '1.1.0', update_available: true, quick_run: true, added_at: '', installed_at: '', ...o })
const dep = (o = {}) => ({ id: 'dep-1', app_slug: 'comfyui', app_name: 'ComfyUI', state: 'running', url: 'https://c.example/', workflows: [wf()], ...o })
const inst = { id: 'ins-1', state: 'running' }

beforeEach(() => { calls.length = 0; state.polls = 0; state.startErr = null; document.body.innerHTML = '' })
afterEach(() => vi.useRealTimers())

describe('工作流子区块与快速出图抽屉（挂载冒烟）', () => {
  it('panel rows + limit', async () => {
    const w = mount(WorkflowsPanel, { props: { instance: inst as never, deployment: dep() as never }, global: { plugins: [ElementPlus] }, attachTo: document.body })
    await flushPromises()
    const h = w.html()
    expect(h).toContain('data-testid="wf-panel"')
    expect(h).toContain('wf-row-sdxl')
    expect(w.find('[data-testid="wf-state-sdxl"]').text()).toBe('已安装')
    expect(h).toContain('wf-quickrun-sdxl')
    expect(h).toContain('有新版本 v1.1.0')
    expect(h).toContain('data-testid="wf-add"')
    const eight = Array.from({ length: 8 }, (_, i) => wf({ workflow_id: i, slug: 's' + i }))
    await w.setProps({ deployment: dep({ workflows: eight }) as never })
    expect(w.html()).toContain('wf-limit')
    expect(w.html()).not.toContain('data-testid="wf-add"')
    await w.setProps({ deployment: dep({ state: 'installing', workflows: [wf({ state: 'pending' })] }) as never })
    expect(w.text()).toContain('正在安装工作流…')
    expect(w.html()).not.toContain('wf-quickrun-sdxl')
    w.unmount()
  })
  it('update and remove confirm', async () => {
    const w = mount(WorkflowsPanel, { props: { instance: inst as never, deployment: dep() as never }, global: { plugins: [ElementPlus] }, attachTo: document.body })
    const q = (s: string) => document.querySelector(`[data-testid="${s}"]`) as HTMLElement | null
    await w.find('[data-testid="wf-more-sdxl"]').trigger('click')
    await flushPromises()
    expect(q('wf-update-sdxl')!.textContent).toContain('更新到 v1.1.0')
    q('wf-update-sdxl')!.click(); await flushPromises()
    expect(q('wf-confirm-text')!.textContent).toContain('从 v1.0.0 更新到 v1.1.0，会重启实例上的 ComfyUI')
    q('wf-confirm-ok')!.click(); await flushPromises()
    expect(calls).toEqual(['update:["ins-1","dep-1",5,51]'])
    q('wf-remove-sdxl')!.click(); await flushPromises()
    expect(q('wf-confirm-text')!.textContent).toContain('移除「SDXL」后模型与节点保留在 /root/data/comfyui')
    q('wf-confirm-ok')!.click(); await flushPromises()
    expect(calls[1]).toBe('remove:["ins-1","dep-1",5]')
    expect(w.emitted('refresh')!.length).toBe(2)
    w.unmount()
  })
  it('add dialog greys installed and posts', async () => {
    const w = mount(WorkflowsPanel, { props: { instance: inst as never, deployment: dep() as never }, global: { plugins: [ElementPlus] }, attachTo: document.body })
    await w.find('[data-testid="wf-add"]').trigger('click')
    await flushPromises()
    const sd = document.querySelector('[data-testid="wf-add-item-sdxl"]') as HTMLElement
    expect(sd.dataset.disabled).toBe('true')
    const fl = document.querySelector('[data-testid="wf-add-item-flux"]') as HTMLElement
    fl.click(); await flushPromises()
    expect(document.querySelector('[data-testid="wf-add-confirm-text"]')!.textContent).toContain('需下载约 17 GB')
    ;(document.querySelector('[data-testid="wf-add-confirm"]') as HTMLElement).click()
    await flushPromises()
    expect(calls).toEqual(['add:["ins-1","dep-1",6]'])
    expect(w.emitted('refresh')).toBeTruthy()
    w.unmount()
  })
  it('quick run drawer: validate, run, poll, render', async () => {
    vi.useFakeTimers()
    const w = mount(QuickRunDrawer, { props: { modelValue: true, instanceId: 'ins-1', deployment: { id: 'dep-1', url: 'u', app_name: 'C' }, workflow: wf({ update_available: false }) as never }, global: { plugins: [ElementPlus] }, attachTo: document.body })
    await flushPromises()
    const q = (s: string) => document.querySelector(`[data-testid="${s}"]`) as HTMLElement | null
    expect(q('qr-drawer')).toBeTruthy()
    expect(q('qr-input-prompt')).toBeTruthy()
    expect(q('qr-input-width')).toBeTruthy()
    q('qr-run')!.click(); await flushPromises()
    expect(q('qr-input-image-error')!.textContent).toBe('必填')
    expect(calls).toEqual([])
    // simulate upload via component internals: set values through form v-model
    const form = w.findComponent({ name: 'QuickRunForm' })
    form.vm.$emit('update:modelValue', { ...(form.props('modelValue') as object), image: 'spark-quickrun/abcdef012345.png', seed: '' })
    await flushPromises()
    q('qr-run')!.click(); await flushPromises()
    expect(calls[0]).toBe('run:["ins-1","dep-1",5,{"prompt":"cat","width":1024,"image":"spark-quickrun/abcdef012345.png"}]')
    expect(q('qr-status')!.textContent).toBe('排队中')
    expect(q('qr-seed')!.textContent).toBe('本次种子：77')
    await vi.advanceTimersByTimeAsync(2000); await flushPromises()
    expect(q('qr-status')!.textContent).toBe('生成中')
    await vi.advanceTimersByTimeAsync(2000); await flushPromises()
    expect(q('qr-status')!.textContent).toBe('完成（12 秒）')
    expect(q('qr-file-0')!.querySelector('img')!.getAttribute('src')).toBe('/api/v1/x/files/0')
    expect(q('qr-file-1')!.querySelector('video')).toBeTruthy()
    expect(q('qr-file-0-download')!.getAttribute('download')).toBe('a.png')
    await vi.advanceTimersByTimeAsync(10000)
    expect(state.polls).toBe(2)
    // busy error
    state.startErr = new ApiError(429, 'workflow_busy', 'x')
    q('qr-run')!.click(); await flushPromises()
    expect(q('qr-error')!.textContent).toContain('ComfyUI 正在处理其它任务，请稍后再试')
    w.unmount()
  })
})
