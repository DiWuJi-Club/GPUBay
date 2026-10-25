import { flushPromises, mount } from '@vue/test-utils'
import ElementPlus from 'element-plus'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter, RouterView, type Router } from 'vue-router'

const { get } = vi.hoisted(() => ({ get: vi.fn() }))
vi.mock('../../api', () => ({ api: { get, post: vi.fn() }, onUnauthorized: vi.fn(), onTotpSetupRequired: vi.fn() }))

import DeploymentsView from '../DeploymentsView.vue'

const dep = (id: string) => ({
  id, instance_id: 'ins-a', user_id: 7, app_id: 1, app_slug: 'vllm', app_name: 'vLLM', app_icon: '', version_id: 1, version: '1.0.0',
  latest_version_id: null, latest_version: '', update_available: false, state: 'running', desired: 'running', phase: '', progress: 100,
  message: '', health: 'ok', error_code: '', env: {}, secret_env: {}, spec: { ports: [{ port: 8000 }] }, port: 8000, url: null, links: [],
  attempt: 1, cleanup_pending: false, started_at: null, finished_at: null, created_at: '2026-10-20T00:00:00Z', updated_at: '2026-10-20T00:00:00Z',
  log_tail: '',
})

/** 列表请求（GET /deployments）的查询参数 */
const listCalls = () => get.mock.calls.filter((c) => c[0] === '/deployments').map((c) => c[1] as Record<string, unknown>)
const detailCalls = () => get.mock.calls.filter((c) => String(c[0]).startsWith('/deployments/')).map((c) => c[0])

let router: Router
let wrapper: ReturnType<typeof mount> | undefined
async function mountAt(url: string) {
  router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/deployments', component: DeploymentsView }] })
  await router.push(url)
  await router.isReady()
  wrapper = mount(RouterView, { global: { plugins: [router, ElementPlus] }, attachTo: document.body })
  await flushPromises()
}

beforeEach(() => {
  get.mockReset()
  get.mockImplementation(async (path: string) => {
    if (path === '/apps') return { items: [] }
    if (path === '/deployments') return { items: [dep('dep-1'), dep('dep-2')], total: 45 }
    return { deployment: dep(path.split('/').pop()!), events: [] }
  })
})
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
})

describe('DeploymentsView 详情抽屉与查询串', () => {
  it('关闭经 ?dep= 打开的抽屉：去掉 dep 但不重新加载列表、不回到第一页', async () => {
    await mountAt('/deployments?instance_id=ins-a&dep=dep-1')
    expect(listCalls()).toHaveLength(1)
    expect(listCalls()[0]).toMatchObject({ instance_id: 'ins-a', page: 1 })
    expect(detailCalls()).toEqual(['/deployments/dep-1'])

    // 翻到第 3 页
    await (wrapper!.findComponent({ name: 'ElPagination' }).vm as unknown as { $emit: (e: string, v: number) => void }).$emit('update:current-page', 3)
    await flushPromises()
    expect(listCalls()).toHaveLength(2)
    expect(listCalls()[1]).toMatchObject({ page: 3 })

    // 关抽屉（el-drawer 的 close 事件 → closeDetail → router.replace 去掉 dep）
    wrapper!.findComponent({ name: 'ElDrawer' }).vm.$emit('close')
    await flushPromises()
    expect(router.currentRoute.value.query).toEqual({ instance_id: 'ins-a' })
    expect(listCalls()).toHaveLength(2) // 没有重新加载
    expect(wrapper!.findComponent({ name: 'ElPagination' }).props('currentPage')).toBe(3)
  })

  it('改了 dep 以外的参数仍按新筛选回到第一页重新加载；只改 dep 打开对应部署', async () => {
    await mountAt('/deployments?instance_id=ins-a')
    expect(listCalls()).toHaveLength(1)
    await router.push('/deployments?instance_id=ins-a&dep=dep-2')
    await flushPromises()
    expect(listCalls()).toHaveLength(1)
    expect(detailCalls()).toEqual(['/deployments/dep-2'])
    await router.push('/deployments?instance_id=ins-b')
    await flushPromises()
    expect(listCalls()).toHaveLength(2)
    expect(listCalls()[1]).toMatchObject({ instance_id: 'ins-b', page: 1 })
  })
})

describe('DeploymentsView 详情抽屉的工作流表（计划 11，A-INS-15）', () => {
  const wf = {
    workflow_id: 3, slug: 'sdxl-txt2img', name: 'SDXL 文生图', category: 'txt2img', version_id: 5, version: '1.0.0', state: 'installed',
    latest_version_id: 6, latest_version: '1.1.0', update_available: true, quick_run: true, added_at: '2026-10-20T00:00:00Z', installed_at: '2026-10-20T00:10:00Z',
  }
  it('ComfyUI 部署显示工作流表（状态、版本、有新版本）', async () => {
    get.mockImplementation(async (path: string) => {
      if (path === '/apps') return { items: [] }
      if (path === '/deployments') return { items: [], total: 0 }
      return { deployment: { ...dep('dep-c'), app_slug: 'comfyui', app_name: 'ComfyUI' }, events: [], workflows: [wf, { ...wf, workflow_id: 4, slug: 'b', name: 'B', state: 'failed', update_available: false }] }
    })
    await mountAt('/deployments?dep=dep-c')
    const table = document.querySelector('[data-testid="adm-dep-workflows"]')!
    expect(table).not.toBeNull()
    const rows = [...table.querySelectorAll('[data-testid="adm-dep-workflow-row"]')]
    expect(rows.map((r) => r.getAttribute('data-slug'))).toEqual(['sdxl-txt2img', 'b'])
    expect(rows[0].querySelector('[data-testid="adm-dep-workflow-state"]')!.textContent).toBe('已安装')
    expect(rows[0].querySelector('[data-testid="adm-dep-workflow-update"]')!.textContent).toContain('1.1.0')
    expect(rows[1].querySelector('[data-testid="adm-dep-workflow-state"]')!.getAttribute('data-state')).toBe('failed')
  })
  it('非 ComfyUI 部署且没有工作流：不显示这一节', async () => {
    await mountAt('/deployments?dep=dep-1')
    expect(document.querySelector('[data-testid="deployment-app"]')).not.toBeNull()
    expect(document.querySelector('[data-testid="adm-dep-workflows"]')).toBeNull()
  })
  it('ComfyUI 部署没有工作流：显示空表', async () => {
    get.mockImplementation(async (path: string) => {
      if (path === '/apps') return { items: [] }
      if (path === '/deployments') return { items: [], total: 0 }
      return { deployment: { ...dep('dep-c'), app_slug: 'comfyui' }, events: [], workflows: [] }
    })
    await mountAt('/deployments?dep=dep-c')
    expect(document.querySelector('[data-testid="adm-dep-workflows"]')!.textContent).toContain('未安装工作流')
  })
})
