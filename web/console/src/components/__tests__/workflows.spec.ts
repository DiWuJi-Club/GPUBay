import type { WorkflowCatalogItem } from '@spark/shared'
import { flushPromises, mount } from '@vue/test-utils'
import ElementPlus from 'element-plus'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h, ref } from 'vue'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'

const { get, post } = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn() }))
vi.mock('../../api', () => ({
  api: { get, post },
  onUnauthorized: vi.fn(),
  fetchWorkflows: () => get('/workflows').then((r: { items: unknown[] }) => r.items),
  fetchWorkflow: (slug: string) => get(`/workflows/${slug}`),
  installWorkflow: (i: string, d: string, w: number) => post(`/instances/${i}/apps/${d}/workflows`, { workflow_id: w }),
}))

import { session } from '../../store/session'
import RentDialog from '../RentDialog.vue'
import WorkflowInstallDialog from '../WorkflowInstallDialog.vue'
import WorkflowPicker from '../WorkflowPicker.vue'
import WorkflowsView from '../../views/WorkflowsView.vue'

const wf = (id: number, slug: string, over: Partial<WorkflowCatalogItem> = {}): WorkflowCatalogItem => ({
  id, slug, name: `工作流${id}`, summary: '', category: 'txt2img', tags: [], cover_url: null, sort: id, installs: 0,
  version: { id: id * 10, version: '1.0.0', download_gb: 2.5, models: 1, nodes: 0, quick_run: false, comfyui_min: '' }, ...over,
})
const catalog = [wf(1, 'a'), wf(2, 'b', { category: 'video', tags: ['Wan'] }), wf(3, 'c')]

let router: Router
let wrapper: ReturnType<typeof mount> | undefined
async function mountWith(comp: ReturnType<typeof defineComponent>) {
  router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/', component: comp }, { path: '/instances/:id', component: { render: () => null } }, { path: '/login', component: { render: () => null } }, { path: '/workflows/:slug', component: { render: () => null } }, { path: '/help', component: { render: () => null } }] })
  await router.push('/')
  await router.isReady()
  wrapper = mount({ render: () => h('div', [h(comp)]) }, { global: { plugins: [router, ElementPlus] }, attachTo: document.body })
  await flushPromises()
  return wrapper
}
const byTestId = (id: string) => document.querySelector<HTMLElement>(`[data-testid="${id}"]`)

beforeEach(() => {
  window.matchMedia = ((q: string) => ({ matches: false, media: q, addEventListener: () => {}, removeEventListener: () => {} })) as never // jsdom 没有 matchMedia（useMobile）
  get.mockReset()
  post.mockReset()
  session.status = 'authed'
  session.user = { id: 1, phone: '13800000000', kyc_status: 'verified' } as never
  session.balance = 100000
})
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  document.body.innerHTML = ''
})

describe('WorkflowPicker（多选内嵌）', () => {
  it('选中 / 取消、上限、灰显已装、合计下载量', async () => {
    const picked = ref<number[]>([])
    await mountWith(defineComponent({
      setup: () => () => h(WorkflowPicker, { items: catalog, max: 2, disabledIds: [3], modelValue: picked.value, 'onUpdate:modelValue': (v: number[]) => (picked.value = v), testid: 'p' }),
    }))
    byTestId('p-a')!.click()
    await flushPromises()
    byTestId('p-b')!.click()
    await flushPromises()
    expect(picked.value).toEqual([1, 2])
    expect(byTestId('p-total')!.textContent).toContain('已选 2/2')
    expect(byTestId('p-total')!.textContent).toContain('5 GB')
    byTestId('p-c')!.click() // 已装：不可选
    await flushPromises()
    expect(picked.value).toEqual([1, 2])
    expect(byTestId('p-c')!.textContent).toContain('已安装')
    byTestId('p-a')!.click()
    await flushPromises()
    expect(picked.value).toEqual([2])
  })
})

describe('WorkflowsView（市场页）', () => {
  it('分类标签与搜索在前端过滤', async () => {
    get.mockImplementation(async (p: string) => (p === '/workflows' ? { items: catalog } : { items: [] }))
    await mountWith(WorkflowsView)
    expect(byTestId('wf-card-a')).not.toBeNull()
    byTestId('wf-category-video')!.click()
    await flushPromises()
    expect(byTestId('wf-card-a')).toBeNull()
    expect(byTestId('wf-card-b')).not.toBeNull()
    byTestId('wf-category-all')!.click()
    await flushPromises()
    const el = byTestId('wf-search')!
    const input = (el.tagName === 'INPUT' ? el : el.querySelector('input')!) as HTMLInputElement
    input.value = 'wan'
    input.dispatchEvent(new Event('input'))
    await flushPromises()
    expect(byTestId('wf-card-a')).toBeNull()
    expect(byTestId('wf-card-b')).not.toBeNull()
  })
  it('未登录点「一键安装」跳登录，redirect 带 install', async () => {
    session.status = 'anon'
    get.mockImplementation(async () => ({ items: catalog }))
    await mountWith(WorkflowsView)
    byTestId('wf-card-a')!.querySelector<HTMLElement>('[data-testid="wf-install-btn"]')!.click()
    await flushPromises()
    expect(router.currentRoute.value.path).toBe('/login')
    expect(router.currentRoute.value.query.redirect).toBe('/?install=a')
  })
})

describe('WorkflowInstallDialog', () => {
  const inst = (id: string, state = 'running') => ({ id, state, product_name: 'DGX Spark', apps_summary: [{ id: 'x', name: 'ComfyUI', state: 'running', progress: 0, url: null }] })
  const dep = (id: string, over: Record<string, unknown> = {}) => ({ id, app_slug: 'comfyui', state: 'running', workflows: [], ...over })
  it('列出 ComfyUI 部署，灰显不可装的，安装后跳实例详情并高亮', async () => {
    get.mockImplementation(async (p: string) => {
      if (p === '/instances') return { items: [inst('ins-a'), inst('ins-b'), inst('ins-c', 'released')] }
      if (p === '/instances/ins-a/apps') return { items: [dep('d1', { workflows: [{ workflow_id: 1 }] }), dep('d9', { app_slug: 'vllm' })] }
      if (p === '/instances/ins-b/apps') return { items: [dep('d2')] }
      throw new Error(`unexpected ${p}`)
    })
    post.mockResolvedValue({ deployment: {}, workflows: [] })
    const visible = ref(true)
    await mountWith(defineComponent({ setup: () => () => h(WorkflowInstallDialog, { workflow: catalog[0], modelValue: visible.value, 'onUpdate:modelValue': (v: boolean) => (visible.value = v) }) }))
    expect(get.mock.calls.map((c) => c[0])).not.toContain('/instances/ins-c/apps')
    expect(byTestId('wf-target-d1')!.textContent).toContain('已安装')
    expect(byTestId('wf-target-d9')).toBeNull()
    expect(byTestId('wf-install-text')!.textContent).toContain('重启 ComfyUI')
    byTestId('wf-target-d2')!.click()
    await flushPromises()
    byTestId('wf-install-confirm')!.click()
    await flushPromises()
    expect(post).toHaveBeenCalledWith('/instances/ins-b/apps/d2/workflows', { workflow_id: 1 })
    expect(router.currentRoute.value.path).toBe('/instances/ins-b')
    expect(router.currentRoute.value.query.highlight).toBe('workflows')
  })
  it('「新开一台实例」emit rent', async () => {
    get.mockImplementation(async () => ({ items: [] }))
    const onRent = vi.fn()
    await mountWith(defineComponent({ setup: () => () => h(WorkflowInstallDialog, { workflow: catalog[1], modelValue: true, onRent }) }))
    expect(byTestId('wf-install-empty')).not.toBeNull()
    byTestId('wf-rent-new')!.click()
    await flushPromises()
    expect(onRent).toHaveBeenCalledWith(2)
  })
})

describe('RentDialog 预装工作流', () => {
  const comfy = {
    id: 5, slug: 'comfyui', name: 'ComfyUI', summary: '', category: 'image', icon: '', images: [{ id: 7, name: 'ComfyUI 镜像' }],
    version: { id: 50, version: '1.1.0', ports: [], links: [], timeout_minutes: 90, supports_workflows: true, env: [{ name: 'MODEL_ID', type: 'enum', options: ['none', 'sdxl'], default: 'sdxl' }] },
  }
  const product = { id: 1, code: 'spark', name: 'DGX Spark', spec: {}, prices: [{ duration: '1d', amount: 100 }], idle_count: 3 }
  it('预选 ComfyUI + 工作流：显示多选、确认页列出、请求体带 app_workflow_ids 与 MODEL_ID=none', async () => {
    get.mockImplementation(async (p: string) => {
      if (p === '/workflows') return { items: catalog }
      if (p === '/groups') return { items: [], now: new Date().toISOString() }
      if (p === '/availability') return { available: 3 }
      return { items: [] }
    })
    post.mockResolvedValue({ order: {}, instance: { id: 'ins-new' } })
    const onPlaced = vi.fn()
    await mountWith(defineComponent({
      setup: () => () => h(RentDialog, {
        modelValue: true, product, images: [{ id: 7, name: 'ComfyUI 镜像', description: '' }], mode: 'immediate', duration: '1d', imageId: 7,
        apps: [comfy], appId: 5, presetEnv: { MODEL_ID: 'none' }, presetWorkflowIds: [2], onPlaced,
      } as never),
    }))
    expect(byTestId('rent-workflows')).not.toBeNull()
    expect(byTestId('rent-workflows-b')!.getAttribute('aria-pressed')).toBe('true')
    byTestId('rent-workflows-a')!.click()
    await flushPromises()
    byTestId('rent-next')!.click()
    await flushPromises()
    expect(byTestId('rent-confirm-workflows')!.textContent).toContain('工作流2、工作流1')
    expect(byTestId('rent-confirm-workflows')!.textContent).toContain('5 GB')
    byTestId('market-confirm-ok')!.click()
    await flushPromises()
    expect(post).toHaveBeenCalledTimes(1)
    const [path, body] = post.mock.calls[0]
    expect(path).toBe('/orders')
    expect(body).toMatchObject({ app_id: 5, app_env: { MODEL_ID: 'none' }, app_workflow_ids: [2, 1] })
    expect(onPlaced).toHaveBeenCalled()
  })
})
