import { flushPromises, mount } from '@vue/test-utils'
import ElementPlus from 'element-plus'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter, RouterView } from 'vue-router'
import type { ConsoleClusterApp, ConsoleClusterView } from '../../logic/cluster'

// 挂载测试：api 打桩；验证成员表、降级 / RDMA / stale 提示、按钮状态、自动续费、释放与集群应用部署请求。
const { get, post, put, del } = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn(), put: vi.fn(), del: vi.fn() }))
vi.mock('../../api', () => ({ api: { get, post, put, del }, onUnauthorized: vi.fn() }))

import { session } from '../../store/session'
import ClusterDetailView from '../ClusterDetailView.vue'

const member = (rank: number, over = {}) => ({
  rank, instance_id: `ins-${'abcdefgh'[rank].repeat(8)}`, state: 'running', suspended: false, ip: `10.210.7.${11 + rank}`, machine_label: `A-0${rank + 1}`,
  net_status: 'ok', net_reason: '', expires_at: '2026-10-27T10:00:00Z', ...over,
})
const view = (over: Partial<ConsoleClusterView> = {}): ConsoleClusterView => ({
  id: 'clu-1', size: 2, status: 'ready', network: 'rdma', fabric_kind: 'cx7', subnet: '10.210.7.0/24', head_ip: '10.210.7.11', batch_id: 'bat-1',
  expires_at: '2026-10-27T10:00:00Z', expires_mismatch: false, auto_renew: false, auto_renew_duration: '1d', auto_renew_failed_reason: '',
  renewable_until: null, renew_options: [{ duration: '1d', quantity_max: 90 }], members: [member(0), member(1)] as ConsoleClusterView['members'],
  app: null, ready_at: '2026-10-26T10:00:00Z', ended_at: null, created_at: '2026-10-26T09:55:00Z', ...over,
})
const envSpec = [
  { name: 'MODEL_ID', label: '模型', type: 'string', required: true },
  { name: 'TENSOR_PARALLEL_SIZE', label: 'TP', type: 'int', min: 1, max: 8 },
  { name: 'SERVED_MODEL_NAME', type: 'string', default: 'model' },
]
const clusterApp = (over: Partial<ConsoleClusterApp> = {}): ConsoleClusterApp => ({
  id: 'acd-1', state: 'active', app_id: 9, app_slug: 'vllm-cluster', app_name: 'vLLM 集群', app_icon: '', version_id: 3, version: '1.0.0',
  latest_version_id: 3, latest_version: '1.0.0', update_available: false, status: 'running', health: 'ok', phase: '', progress: 100,
  url: 'https://ins-aaaaaaaa-8000.example.com/docs', env: { MODEL_ID: 'Qwen/Qwen3-32B' }, secret_env: {},
  spec: { ports: [{ port: 8000, path: '/docs' }], env: envSpec as ConsoleClusterApp['spec']['env'] },
  members: [
    { rank: 0, role: 'head', instance_id: 'ins-aaaaaaaa', deployment_id: 'dep-1', state: 'running', health: 'ok', phase: '', progress: 100, message: '', current: true },
    { rank: 1, role: 'worker', instance_id: 'ins-bbbbbbbb', deployment_id: 'dep-2', state: 'running', health: 'ok', phase: '', progress: 100, message: '', current: true },
  ],
  created_at: '', updated_at: '', ...over,
})
const catalog = [
  { id: 1, slug: 'vllm', name: 'vLLM', summary: '', category: 'llm', icon: '', images: [{ id: 3, name: 'LLM' }], version: { id: 11, version: '1.0.0', env: [], ports: [], links: [], timeout_minutes: 30 } },
  { id: 9, slug: 'vllm-cluster', name: 'vLLM 集群', summary: '', category: 'llm', icon: '', images: [{ id: 3, name: 'LLM' }], version: { id: 3, version: '1.0.0', env: envSpec, ports: [{ port: 8000 }], links: [], timeout_minutes: 360, cluster: { mode: 'required', min_size: 2, max_size: 4 } } },
]

let current: ConsoleClusterView
let wrapper: ReturnType<typeof mount> | undefined
async function mountAt() {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/clusters/:id', component: ClusterDetailView, meta: { title: '集群详情' } },
      { path: '/:p(.*)*', component: { template: '<div />' }, meta: { title: 'x' } },
    ],
  })
  await router.push('/clusters/clu-1')
  await router.isReady()
  wrapper = mount(RouterView, { global: { plugins: [router, ElementPlus] }, attachTo: document.body })
  await flushPromises()
}
const q = (id: string) => document.querySelector(`[data-testid="${id}"]`) as HTMLElement | null
const qa = (id: string) => [...document.querySelectorAll<HTMLElement>(`[data-testid="${id}"]`)]
const btnDisabled = (id: string) => (q(id) as HTMLButtonElement).disabled

beforeEach(() => {
  vi.stubGlobal('matchMedia', (m: string) => ({ matches: false, media: m, addEventListener: () => {}, removeEventListener: () => {} }))
  current = view()
  get.mockReset()
  post.mockReset()
  put.mockReset()
  del.mockReset()
  get.mockImplementation(async (path: string) => {
    if (path === '/clusters/clu-1') return current
    if (path.startsWith('/instances/')) return { id: path.split('/')[2], product_id: 1, image_id: 3 }
    if (path === '/products') return { items: [{ id: 1, name: 'Spark', prices: [{ duration: '1d', amount: 10000 }, { duration: '1w', amount: 60000 }] }] }
    if (path === '/apps') return { items: catalog }
    if (path === '/wallet') return { balance: 100000 }
    throw new Error(`unexpected ${path}`)
  })
  session.status = 'authed'
  session.balance = 100000
})
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  session.status = 'unknown'
  document.body.innerHTML = ''
  vi.unstubAllGlobals()
})

describe('ClusterDetailView', () => {
  it('状态、网络、成员表（rank / 集群 IP / 网络状态）、连接信息', async () => {
    await mountAt()
    expect(q('cluster-status')!.textContent).toBe('就绪')
    expect(q('cluster-network')!.textContent).toBe('RDMA 高速互联')
    const rows = qa('cluster-member')
    expect(rows.map((r) => r.dataset.rank)).toEqual(['0', '1'])
    expect(qa('cluster-member-ip').map((e) => e.textContent)).toEqual(['10.210.7.11', '10.210.7.12'])
    expect(qa('cluster-member-net').map((e) => e.textContent!.trim())).toEqual(['正常', '正常'])
    expect(q('cluster-connect-head')!.textContent).toBe('10.210.7.11')
    expect(q('cluster-connect-ray')!.textContent).toContain('export RAY_ADDRESS=$SPARK_CLUSTER_HEAD_IP:6379')
    expect(q('cluster-connect-env')!.textContent).toContain('. /.spark/cluster.env')
    expect(btnDisabled('cluster-renew')).toBe(false)
    expect(btnDisabled('cluster-release')).toBe(false)
    expect(q('cluster-app-deploy')).not.toBeNull()
    expect(q('cluster-notice-degraded')).toBeNull()
  })

  it('降级 + RDMA 退回 + 成员网络异常 + 到期不一致：各自提示', async () => {
    current = view({ status: 'degraded', network: 'rdma_degraded', expires_mismatch: true, members: [member(0), member(1, { net_status: 'error', net_reason: '集群网络异常，平台已告警处理', suspended: true })] as ConsoleClusterView['members'] })
    await mountAt()
    expect(q('cluster-status')!.textContent).toBe('降级')
    expect(q('cluster-notice-degraded')).not.toBeNull()
    expect(q('cluster-notice-rdma_degraded')).not.toBeNull()
    expect(q('cluster-notice-expires_mismatch')!.textContent).toContain('成员到期时间不一致')
    expect(q('cluster-expires-mismatch')).not.toBeNull()
    expect(qa('cluster-member-net')[1].textContent).toContain('异常：集群网络异常，平台已告警处理')
    expect(q('cluster-member-suspended')).not.toBeNull()
  })

  it('组建中且有成员开通中：释放置灰并说明', async () => {
    current = view({ status: 'forming', ready_at: null, members: [member(0, { state: 'creating' }), member(1)] as ConsoleClusterView['members'] })
    await mountAt()
    expect(q('cluster-notice-forming')).not.toBeNull()
    expect(btnDisabled('cluster-release')).toBe(true)
    expect(q('cluster-release-reason')!.textContent).toContain('开通')
  })

  it('已释放：没有续费 / 释放 / 自动续费 / 部署入口', async () => {
    current = view({ status: 'released', renew_options: [], ended_at: '2026-10-27T10:00:00Z', members: [member(0, { state: 'released' })] as ConsoleClusterView['members'] })
    await mountAt()
    expect(q('cluster-notice-released')).not.toBeNull()
    for (const id of ['cluster-renew', 'cluster-release', 'cluster-auto-renew', 'cluster-app-deploy', 'cluster-connect']) expect(q(id), id).toBeNull()
  })

  it('集群应用：聚合状态、成员角色表、调用方式；stale 时提示重新部署', async () => {
    current = view({ app: clusterApp() })
    await mountAt()
    expect(q('cluster-app-status')!.textContent).toBe('运行中')
    const ms = qa('cluster-app-member')
    expect(ms.map((m) => m.dataset.role)).toEqual(['head', 'worker'])
    expect(q('cluster-app-call-model')!.textContent).toBe('model')
    expect(q('cluster-app-call-public')!.textContent).toBe('https://ins-aaaaaaaa-8000.example.com/v1')
    expect(q('cluster-app-stop')).not.toBeNull()
    expect(q('cluster-app-stale')).toBeNull()
    wrapper!.unmount()
    document.body.innerHTML = ''
    current = view({ status: 'degraded', app: clusterApp({ status: 'stale', url: null, members: [{ ...clusterApp().members[0], current: false }, clusterApp().members[1]] }) })
    await mountAt()
    expect(q('cluster-app-status')!.textContent).toBe('需要重新部署')
    expect(q('cluster-app-stale')!.textContent).toContain('成员已更换，请重新部署')
    expect(q('cluster-app-member-stale')).not.toBeNull()
    expect(q('cluster-app-redeploy')).not.toBeNull()
    expect(q('cluster-app-open')).toBeNull()
  })

  it('部署集群应用：只列支持集群的应用，TP 默认 = 台数，POST /clusters/{id}/apps', async () => {
    await mountAt()
    q('cluster-app-deploy')!.click()
    await flushPromises()
    expect(q('cluster-app-deploy-pick-vllm')).toBeNull()
    expect(q('cluster-app-deploy-pick-vllm-cluster')).not.toBeNull()
    // 只有一个候选：自动选中
    const tp = q('app-env-TENSOR_PARALLEL_SIZE')!.querySelector('input') as HTMLInputElement
    expect(tp.value).toBe('2')
    const mi = q('app-env-MODEL_ID')!
    const model = (mi.tagName === 'INPUT' ? mi : mi.querySelector('input')) as HTMLInputElement
    model.value = 'Qwen/Qwen3-32B'
    model.dispatchEvent(new Event('input'))
    await flushPromises()
    post.mockResolvedValueOnce(clusterApp({ status: 'installing', progress: 0 }))
    q('cluster-app-deploy-ok')!.click()
    await flushPromises()
    expect(post).toHaveBeenCalledWith('/clusters/clu-1/apps', { app_id: 9, env: { MODEL_ID: 'Qwen/Qwen3-32B', TENSOR_PARALLEL_SIZE: '2', SERVED_MODEL_NAME: 'model' } })
  })

  it('自动续费：PUT /clusters/{id}/auto-renew；释放：二次确认后 POST release', async () => {
    await mountAt()
    put.mockResolvedValueOnce(view({ auto_renew: true }))
    q('cluster-auto-renew')!.click()
    await flushPromises()
    expect(put).toHaveBeenCalledWith('/clusters/clu-1/auto-renew', { enabled: true, duration: '1d' })
    q('cluster-release')!.click()
    await flushPromises()
    expect(q('cluster-release-text')!.textContent).toContain('全部 2 台')
    expect(post).not.toHaveBeenCalled()
    post.mockResolvedValueOnce({ ok: true })
    q('cluster-release-ok')!.click()
    await flushPromises()
    expect(post).toHaveBeenCalledWith('/clusters/clu-1/release', {})
  })

  it('自动续费时长：商品没加载时用集群现有时长，不硬写 1d；都没有则开关禁用（终审 B-12）', async () => {
    const base = get.getMockImplementation()!
    get.mockImplementation(async (path: string, params?: unknown) => {
      if (path === '/products') throw new Error('products down')
      return base(path, params)
    })
    current = view({ auto_renew_duration: '1w' })
    await mountAt()
    put.mockResolvedValueOnce(view({ auto_renew: true, auto_renew_duration: '1w' }))
    q('cluster-auto-renew')!.click()
    await flushPromises()
    expect(put).toHaveBeenCalledWith('/clusters/clu-1/auto-renew', { enabled: true, duration: '1w' })
    wrapper?.unmount()
    wrapper = undefined
    document.body.innerHTML = ''
    put.mockReset()

    current = view({ auto_renew_duration: '' })
    await mountAt()
    const sw = q('cluster-auto-renew')!
    expect(sw.classList.contains('is-disabled')).toBe(true)
    sw.click()
    await flushPromises()
    expect(put).not.toHaveBeenCalled()
  })

  it('自动续费时长：集群没有现有时长时取商品第一个档位；关闭不带时长', async () => {
    current = view({ auto_renew_duration: '' })
    await mountAt()
    put.mockResolvedValueOnce(view({ auto_renew: true }))
    q('cluster-auto-renew')!.click()
    await flushPromises()
    expect(put).toHaveBeenCalledWith('/clusters/clu-1/auto-renew', { enabled: true, duration: '1d' })
    put.mockResolvedValueOnce(view({ auto_renew: false }))
    q('cluster-auto-renew')!.click()
    await flushPromises()
    expect(put).toHaveBeenLastCalledWith('/clusters/clu-1/auto-renew', { enabled: false })
  })

  it('释放后在位成员不足（旧成员洗机中不再列出）：每 5 秒刷新，洗完即显示已释放', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
    try {
      current = view({ status: 'degraded', members: [] })
      await mountAt()
      const calls = () => get.mock.calls.filter(([p]) => p === '/clusters/clu-1').length
      const before = calls()
      current = view({ status: 'released', members: [], ended_at: '2026-10-26T11:00:00Z', renew_options: [] })
      vi.advanceTimersByTime(5_000)
      await flushPromises()
      expect(calls()).toBe(before + 1)
      expect(q('cluster-status')!.dataset.status).toBe('released')
    } finally {
      vi.useRealTimers()
    }
  })

  it('续费：总价 = 单价 × 台数 × 数量，带 Idempotency-Key', async () => {
    await mountAt()
    q('cluster-renew')!.click()
    await flushPromises()
    expect(q('cluster-renew-price')!.textContent).toContain('¥200.00')
    post.mockResolvedValueOnce({ orders: [{}, {}], cluster: view() })
    q('cluster-renew-ok')!.click()
    await flushPromises()
    expect(post).toHaveBeenCalledWith('/clusters/clu-1/renew', { duration: '1d', quantity: 1 }, { idempotencyKey: expect.any(String) })
  })
})
