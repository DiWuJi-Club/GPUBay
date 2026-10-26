import { flushPromises, mount } from '@vue/test-utils'
import ElementPlus from 'element-plus'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter, RouterView, type Router } from 'vue-router'

// 计划 13：实例列表的集群折叠、成员实例详情的横幅与按钮置灰、成员上的集群部署只读、「添加应用」排除 required 集群应用。
const { get, post } = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn() }))
vi.mock('../../api', () => ({ api: { get, post, put: vi.fn(), del: vi.fn() }, onUnauthorized: vi.fn() }))

import { session } from '../../store/session'
import InstanceDetailView from '../InstanceDetailView.vue'
import InstancesView from '../InstancesView.vue'

const inst = (id: string, over: Record<string, unknown> = {}) => ({
  id, state: 'running', product_id: 1, image_id: 3, product_name: 'Spark', image_name: 'LLM', suspended: false, expires_at: '2026-10-27T10:00:00Z',
  remaining_seconds: 3600, pending_seconds: 0, retain_until: null, ssh_command: `ssh ${id}@ssh.x`, password_auth: true, ssh_host: 'ssh.x', ssh_port: 22,
  ports_count: 0, scheduled_start_at: null, scheduled_end_at: null, cancellable_until: null, cancellable_seconds: 0, starts_in_seconds: 0,
  group_id: null, group_name: '', machine_label: 'A-01', batch_id: null, reassign_count: 0, auto_renew: false, auto_renew_duration: '1d',
  auto_renew_failed_reason: '', renewable_until: null, renew_options: [], apps_summary: [], created_at: '2026-10-26T10:00:00Z', cluster: null, ...over,
})
const ref = (rank: number) => ({ id: 'clu-1', size: 2, rank, status: 'ready' })
const catalog = [
  { id: 1, slug: 'vllm', name: 'vLLM', summary: '', category: 'llm', icon: '', images: [{ id: 3, name: 'LLM' }], version: { id: 11, version: '1.0.0', env: [], ports: [], links: [], timeout_minutes: 30 } },
  { id: 9, slug: 'vllm-cluster', name: 'vLLM 集群', summary: '', category: 'llm', icon: '', images: [{ id: 3, name: 'LLM' }], version: { id: 3, version: '1.0.0', env: [], ports: [], links: [], timeout_minutes: 360, cluster: { mode: 'required', min_size: 2, max_size: 4 } } },
]
const clusterDep = {
  id: 'dep-1', instance_id: 'ins-c0', app_id: 9, app_slug: 'vllm-cluster', app_name: 'vLLM 集群', app_icon: '', version_id: 3, version: '1.0.0',
  latest_version_id: 3, latest_version: '1.0.0', update_available: false, state: 'running', desired: 'running', phase: '', progress: 100, message: '',
  health: 'ok', error_code: '', env: {}, secret_env: {}, spec: { ports: [{ port: 8000 }], env: [] }, port: 8000, url: 'https://ins-c0-8000.x/', links: [],
  attempt: 1, cleanup_pending: false, started_at: null, finished_at: null, created_at: '', updated_at: '', cluster_role: 'head', cluster_deployment_id: 'acd-1',
}

let router: Router
let wrapper: ReturnType<typeof mount> | undefined
async function mountAt(url: string) {
  router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/instances', component: InstancesView, meta: { title: '我的实例' } },
      { path: '/instances/:id', component: InstanceDetailView, meta: { title: '实例详情' } },
      { path: '/:p(.*)*', component: { template: '<div />' }, meta: { title: 'x' } },
    ],
  })
  await router.push(url)
  await router.isReady()
  wrapper = mount(RouterView, { global: { plugins: [router, ElementPlus] }, attachTo: document.body })
  await flushPromises()
}
const q = (id: string) => document.querySelector(`[data-testid="${id}"]`) as HTMLElement | null
const qa = (id: string) => [...document.querySelectorAll<HTMLElement>(`[data-testid="${id}"]`)]

let mobile = false
beforeEach(() => {
  mobile = false
  vi.stubGlobal('matchMedia', (m: string) => ({ matches: mobile, media: m, addEventListener: () => {}, removeEventListener: () => {} }))
  get.mockReset()
  post.mockReset()
  get.mockImplementation(async (path: string) => {
    if (path === '/instances') return { items: [inst('ins-1'), inst('ins-c1', { cluster: ref(1), batch_id: 'bat-1' }), inst('ins-c0', { cluster: ref(0), batch_id: 'bat-1' })] }
    if (path === '/instances/ins-c0') return inst('ins-c0', { cluster: ref(0), auto_renew: false })
    if (path === '/instances/ins-c0/apps') return { items: [clusterDep] }
    if (path.endsWith('/events') || path.endsWith('/ports')) return { items: [] }
    if (path === '/apps') return { items: catalog }
    if (path === '/products') return { items: [{ id: 1, name: 'Spark', prices: [{ duration: '1d', amount: 10000 }] }] }
    throw new Error(`unexpected ${path}`)
  })
  session.status = 'authed'
})
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  session.status = 'unknown'
  document.body.innerHTML = ''
  vi.unstubAllGlobals()
})

describe('实例列表：集群折叠', () => {
  for (const m of [false, true]) {
    it(`${m ? '手机' : '电脑'}：成员折叠为一张集群卡（按 rank），单机实例照常`, async () => {
      mobile = m
      await mountAt('/instances')
      const cards = qa('instances-cluster-card')
      expect(cards).toHaveLength(1)
      expect(cards[0].dataset.id).toBe('clu-1')
      expect(q('instances-cluster-status')!.textContent).toBe('就绪')
      expect(qa('instances-cluster-member').map((e) => e.dataset.id)).toEqual(['ins-c0', 'ins-c1'])
      expect(qa('instance-id').map((e) => e.textContent)).toEqual(['ins-1'])
      cards[0].click()
      await flushPromises()
      expect(router.currentRoute.value.path).toBe('/clusters/clu-1')
    })
  }
})

describe('成员实例详情', () => {
  it('横幅 + 续费 / 释放 / 自动续费置灰；集群部署只读；添加应用不含 required', async () => {
    await mountAt('/instances/ins-c0')
    const banner = q('instance-cluster-banner')!
    expect(banner.textContent).toContain('属于集群 clu-1（rank 0 / 2）')
    expect(q('instance-cluster-link')!.getAttribute('href')).toBe('/clusters/clu-1')
    expect((q('detail-renew-btn') as HTMLButtonElement).disabled).toBe(true)
    expect((q('detail-release-btn') as HTMLButtonElement).disabled).toBe(true)
    expect(q('detail-cluster-guard')).not.toBeNull()
    expect(q('detail-auto-renew')!.classList.contains('is-disabled')).toBe(true)
    // 集群部署：标记角色、只读（没有操作菜单与修改参数），可看日志
    expect(q('detail-app-cluster')!.textContent).toContain('主节点')
    expect(q('detail-app-cluster-hint')).not.toBeNull()
    expect(q('detail-app-actions')).toBeNull()
    expect(q('detail-app-log')).not.toBeNull()
    // 添加应用：required 集群应用不出现
    q('detail-app-add')!.click()
    await flushPromises()
    expect(q('app-deploy-pick-vllm')).not.toBeNull()
    expect(q('app-deploy-pick-vllm-cluster')).toBeNull()
  })
})
