import { ApiError, brand, workflowErrorLabel, type WorkflowCatalogItem } from '@spark/shared'
import { flushPromises, mount } from '@vue/test-utils'
import ElementPlus, { ElMessage } from 'element-plus'
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
import WorkflowDetailView from '../../views/WorkflowDetailView.vue'
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
  vi.restoreAllMocks()
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

describe('WorkflowPicker（单选，实例详情「添加工作流」）', () => {
  it('点可选项 emit select，已装的不 emit；selectedId 高亮', async () => {
    const onSelect = vi.fn()
    await mountWith(defineComponent({ setup: () => () => h(WorkflowPicker, { mode: 'single', items: catalog, disabledIds: [1], selectedId: 2, testid: 's', onSelect }) }))
    expect(byTestId('s-a')!.dataset.disabled).toBe('true')
    expect(byTestId('s-b')!.getAttribute('aria-pressed')).toBe('true')
    expect(byTestId('s-total')).toBeNull()
    byTestId('s-a')!.click()
    byTestId('s-c')!.click()
    await flushPromises()
    expect(onSelect).toHaveBeenCalledTimes(1)
    expect(onSelect.mock.calls[0][0].id).toBe(3)
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
    expect(byTestId('wf-install-text')!.textContent).toContain('重启实例上的 ComfyUI')
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

describe('WorkflowInstallDialog：错误与边界（T12 评审）', () => {
  const inst = (id: string) => ({ id, state: 'running', product_name: 'DGX Spark', apps_summary: [{ id: 'x', name: 'ComfyUI', state: 'running', progress: 0, url: null }] })
  const mountDialog = () =>
    mountWith(defineComponent({ setup: () => () => h(WorkflowInstallDialog, { workflow: catalog[0], modelValue: true }) }))
  it('实例列表加载失败：显示错误与重试，不显示「没有 ComfyUI」；重试成功后列出', async () => {
    let fail = true
    get.mockImplementation(async (p: string) => {
      if (p === '/instances') {
        if (fail) throw new ApiError(500, 'internal', '服务器开小差了')
        return { items: [inst('ins-a')] }
      }
      return { items: [{ id: 'd1', app_slug: 'comfyui', state: 'running', workflows: [] }] }
    })
    await mountDialog()
    expect(byTestId('wf-install-load-error')!.textContent).toContain('服务器开小差了')
    expect(byTestId('wf-install-empty')).toBeNull()
    fail = false
    byTestId('wf-install-retry')!.click()
    await flushPromises()
    expect(byTestId('wf-install-load-error')).toBeNull()
    expect(byTestId('wf-target-d1')).not.toBeNull()
  })
  it('workflow_exists 等冲突：保留错误文案并重新拉取目标', async () => {
    let installed: unknown[] = []
    get.mockImplementation(async (p: string) => (p === '/instances' ? { items: [inst('ins-a')] } : { items: [{ id: 'd1', app_slug: 'comfyui', state: 'running', workflows: installed }] }))
    post.mockImplementation(async () => {
      installed = [{ workflow_id: 1 }] // 别处刚装上
      throw new ApiError(409, 'workflow_exists', '该工作流已安装在此 ComfyUI 上')
    })
    await mountDialog()
    byTestId('wf-install-confirm')!.click()
    await flushPromises()
    expect(get.mock.calls.filter((c) => c[0] === '/instances')).toHaveLength(2)
    expect(byTestId('wf-install-error')!.textContent).toContain('该工作流已安装在此 ComfyUI 上')
    expect(byTestId('wf-target-d1')!.textContent).toContain('已安装')
    expect(byTestId('wf-install-confirm')!.hasAttribute('disabled')).toBe(true)
  })
  it('超过 20 台：只查前 20 台并提示；并发不超过 6；取消按钮关闭', async () => {
    let running = 0
    let peak = 0
    get.mockImplementation(async (p: string) => {
      if (p === '/instances') return { items: Array.from({ length: 25 }, (_, i) => inst(`ins-${i}`)) }
      running++
      peak = Math.max(peak, running)
      await new Promise((r) => setTimeout(r, 1))
      running--
      return { items: [] }
    })
    const visible = ref(true)
    await mountWith(defineComponent({ setup: () => () => h(WorkflowInstallDialog, { workflow: catalog[0], modelValue: visible.value, 'onUpdate:modelValue': (v: boolean) => (visible.value = v) }) }))
    await new Promise((r) => setTimeout(r, 30))
    await flushPromises()
    expect(get.mock.calls.filter((c) => String(c[0]).endsWith('/apps'))).toHaveLength(20)
    expect(peak).toBeLessThanOrEqual(6)
    expect(byTestId('wf-install-truncated')).not.toBeNull()
    byTestId('wf-install-cancel')!.click()
    await flushPromises()
    expect(visible.value).toBe(false)
  })
})

describe('WorkflowInstallFlow「新开一台实例」（T12 评审 I4）', () => {
  it('ComfyUI 当前版本不支持工作流：提示 workflow_app_unsupported，不打开下单', async () => {
    const warn = vi.spyOn(ElMessage, 'warning')
    get.mockImplementation(async (p: string) => {
      if (p === '/workflows') return { items: catalog }
      if (p === '/apps') return { items: [{ id: 5, slug: 'comfyui', images: [{ id: 7 }], version: { supports_workflows: false, env: [] } }] }
      if (p === '/images') return { items: [{ id: 7, name: 'img', description: '' }] }
      if (p === '/products') return { items: [{ id: 1, prices: [{ duration: '1d', amount: 1 }], idle_count: 1 }] }
      return { items: [] }
    })
    await mountWith(WorkflowsView)
    byTestId('wf-card-a')!.querySelector<HTMLElement>('[data-testid="wf-install-btn"]')!.click()
    await flushPromises()
    byTestId('wf-rent-new')!.click()
    await flushPromises()
    expect(warn).toHaveBeenCalledWith(workflowErrorLabel.workflow_app_unsupported)
    expect(byTestId('rent-dialog')).toBeNull()
  })
})

describe('RentDialog 预装工作流：目录刷新与下架提示（T12 评审 I4 / I5）', () => {
  const comfy = {
    id: 5, slug: 'comfyui', name: 'ComfyUI', summary: '', category: 'image', icon: '', images: [{ id: 7, name: 'ComfyUI 镜像' }],
    version: { id: 50, version: '1.1.0', ports: [], links: [], timeout_minutes: 90, supports_workflows: true, env: [] },
  }
  const product = { id: 1, code: 'spark', name: 'DGX Spark', spec: {}, prices: [{ duration: '1d', amount: 100 }], idle_count: 3 }
  it('预选的工作流已下架：去掉并提示；重新打开时重新拉目录', async () => {
    const warn = vi.spyOn(ElMessage, 'warning')
    get.mockImplementation(async (p: string) => {
      if (p === '/workflows') return { items: catalog }
      if (p === '/groups') return { items: [], now: new Date().toISOString() }
      if (p === '/availability') return { available: 3 }
      return { items: [] }
    })
    const visible = ref(true)
    await mountWith(defineComponent({
      setup: () => () => h(RentDialog, {
        modelValue: visible.value, 'onUpdate:modelValue': (v: boolean) => (visible.value = v), product, images: [{ id: 7, name: 'ComfyUI 镜像', description: '' }],
        mode: 'immediate', duration: '1d', imageId: 7, apps: [comfy], appId: 5, presetWorkflowIds: [99, 2],
      } as never),
    }))
    expect(warn).toHaveBeenCalledWith('所选工作流已下架，已从预装列表移除')
    expect(byTestId('rent-workflows-b')!.getAttribute('aria-pressed')).toBe('true')
    const calls = () => get.mock.calls.filter((c) => c[0] === '/workflows').length
    expect(calls()).toBe(1)
    visible.value = false
    await flushPromises()
    visible.value = true
    await flushPromises()
    expect(calls()).toBe(2)
  })
})

describe('WorkflowDetailView（T12 评审 M2 / M7）', () => {
  const detail = {
    ...catalog[0], description: '介绍', versions: [{ id: 10, version: '1.0.0', published_at: null }],
    version: { ...catalog[0].version, changelog: '', published_at: null, manifest: { models: [], inputs: [], outputs: [], nodes: [
      { name: 'good', git: 'https://github.com/a/b.git', commit: '0123456789abcdef0123456789abcdef01234567', node_types: ['X'] },
      { name: 'bad', git: 'javascript:alert(1)', commit: '0123456789abcdef0123456789abcdef01234567', node_types: ['Y'] },
    ] } },
  }
  async function mountDetail(url: string) {
    router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/workflows/:slug', component: WorkflowDetailView }, { path: '/workflows', component: { render: () => null } }, { path: '/instances/:id', component: { render: () => null } }] })
    router.afterEach(() => { document.title = `工作流详情 - ${brand.name}` }) // 同 router.ts
    await router.push(url)
    await router.isReady()
    wrapper = mount({ render: () => h(RouterViewStub) }, { global: { plugins: [router, ElementPlus] }, attachTo: document.body })
    await flushPromises()
  }
  const RouterViewStub = defineComponent({ setup: () => () => h(WorkflowDetailView) })
  beforeEach(() => {
    get.mockImplementation(async (p: string) => (p === '/workflows/a' ? detail : { items: [] }))
  })
  it('节点 Git 只有 https 渲染成链接', async () => {
    await mountDetail('/workflows/a')
    const links = [...byTestId('wf-detail-nodes')!.querySelectorAll('a')].map((a) => a.getAttribute('href'))
    expect(links).toEqual(['https://github.com/a/b.git'])
    expect(byTestId('wf-detail-nodes')!.textContent).toContain('javascript:alert(1)')
  })
  it('登录回来 ?install= 去掉查询后标题仍是工作流名称', async () => {
    await mountDetail('/workflows/a?install=a')
    await flushPromises()
    expect(router.currentRoute.value.query.install).toBeUndefined()
    expect(document.title).toBe(`${detail.name} - ${brand.name}`)
    expect(byTestId('wf-install-dialog')).not.toBeNull()
  })
})
