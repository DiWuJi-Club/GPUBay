import { flushPromises, mount } from '@vue/test-utils'
import ElementPlus from 'element-plus'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter, RouterView, type Router } from 'vue-router'

const { get } = vi.hoisted(() => ({ get: vi.fn() }))
vi.mock('../../api', () => ({ api: { get, post: vi.fn(), put: vi.fn(), del: vi.fn(), upload: vi.fn() }, onUnauthorized: vi.fn(), onTotpSetupRequired: vi.fn() }))

import WorkflowDetailView from '../WorkflowDetailView.vue'
import WorkflowInstallsView from '../WorkflowInstallsView.vue'

const wf = (id: number) => ({ id, slug: `wf-${id}`, name: `工作流 ${id}`, summary: '', description: '', category: 'txt2img', tags: [], has_cover: false, cover_url: null, sort: 0, status: 'draft' })

let router: Router
let wrapper: ReturnType<typeof mount> | undefined
async function mountAt(url: string) {
  router = createRouter({
    history: createMemoryHistory(),
    routes: [{ path: '/workflows/:id', component: WorkflowDetailView }, { path: '/workflows/:id/installs', component: WorkflowInstallsView }, { path: '/:p(.*)*', component: { template: '<div />' } }],
  })
  await router.push(url)
  await router.isReady()
  wrapper = mount(RouterView, { global: { plugins: [router, ElementPlus] }, attachTo: document.body })
  await flushPromises()
}
const text = (id: string) => document.querySelector(`[data-testid="${id}"]`)?.textContent ?? ''

beforeEach(() => {
  get.mockReset()
  get.mockImplementation(async (path: string) => {
    const m = /^\/workflows\/(\d+)(\/.*)?$/.exec(path)
    if (path === '/workflows') return { items: [] }
    if (m && !m[2]) return wf(Number(m[1]))
    if (m?.[2] === '/versions') return { items: [] }
    if (m?.[2] === '/installs') return { items: [], total: Number(m[1]) }
    throw new Error(path)
  })
})
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  document.body.innerHTML = ''
})

describe('工作流详情 / 安装列表随路由参数重新加载', () => {
  it('详情：/workflows/1 → /workflows/2', async () => {
    await mountAt('/workflows/1')
    expect(text('adm-wf-detail-name')).toBe('工作流 1')
    await router.push('/workflows/2')
    await flushPromises()
    expect(text('adm-wf-detail-name')).toBe('工作流 2')
    expect(get).toHaveBeenCalledWith('/workflows/2/versions')
  })
  it('安装列表：/workflows/1/installs → /workflows/3/installs', async () => {
    await mountAt('/workflows/1/installs')
    expect(text('adm-wf-installs-back')).toBe('工作流 1')
    await router.push('/workflows/3/installs')
    await flushPromises()
    expect(text('adm-wf-installs-back')).toBe('工作流 3')
    expect(text('adm-wf-installs-total')).toContain('3')
  })
})
