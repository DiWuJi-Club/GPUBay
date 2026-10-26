import { flushPromises, mount } from '@vue/test-utils'
import ElementPlus from 'element-plus'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h, ref } from 'vue'
import { createMemoryHistory, createRouter } from 'vue-router'

// RentDialog 的集群模式（计划 13）：租用方式、网络类型与可开数量（/availability?cluster_size=）、总价、请求体与 placed.clusterId。
const { get, post } = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn() }))
vi.mock('../../api', () => ({ api: { get, post }, onUnauthorized: vi.fn() }))

import { session } from '../../store/session'
import RentDialog, { type PlacedResult } from '../RentDialog.vue'

const product = (clusters: unknown) => ({
  id: 1, code: 'spark', name: 'DGX Spark', spec: { gpu: 'GB10', memory_gb: 128, disk_tb: 4, cpu: 'ARM' }, idle_count: 4,
  prices: [{ duration: '1d', amount: 10000 }, { duration: '1w', amount: 60000 }], clusters,
})
const apps = [
  { id: 1, slug: 'vllm', name: 'vLLM', summary: '', category: 'llm', icon: '', images: [{ id: 3, name: 'LLM' }], version: { id: 11, version: '1.0.0', env: [], ports: [], links: [], timeout_minutes: 30 } },
  { id: 9, slug: 'vllm-cluster', name: 'vLLM 集群', summary: '', category: 'llm', icon: '', images: [{ id: 3, name: 'LLM' }], version: { id: 3, version: '1.0.0', env: [], ports: [], links: [], timeout_minutes: 360, cluster: { mode: 'required', min_size: 2, max_size: 4 } } },
]
const q = (id: string) => document.querySelector(`[data-testid="${id}"]`) as HTMLElement | null
const wait = (ms: number) => new Promise((r) => setTimeout(r, ms))
let placed: PlacedResult[] = []
let wrapper: ReturnType<typeof mount> | undefined

async function open(clusters: unknown) {
  const router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/:p(.*)*', component: { template: '<div />' } }] })
  await router.push('/market')
  await router.isReady()
  const Host = defineComponent(() => {
    const visible = ref(true)
    return () => h(RentDialog, {
      modelValue: visible.value, 'onUpdate:modelValue': (v: boolean) => (visible.value = v),
      product: product(clusters) as never, images: [{ id: 3, name: 'LLM', description: '' }] as never, mode: 'booking', duration: '1d', imageId: 3, apps: apps as never,
      onPlaced: (r: PlacedResult) => placed.push(r),
    })
  })
  wrapper = mount(Host, { global: { plugins: [router, ElementPlus] }, attachTo: document.body })
  await flushPromises()
}
const clickRadio = async (id: string) => {
  const el = q(id)!
  ;(el.querySelector('input') ?? el).click()
  await flushPromises()
}

beforeEach(() => {
  vi.stubGlobal('matchMedia', (m: string) => ({ matches: false, media: m, addEventListener: () => {}, removeEventListener: () => {} }))
  placed = []
  get.mockReset()
  post.mockReset()
  get.mockImplementation(async (path: string, params?: Record<string, unknown>) => {
    if (path === '/groups') return { items: [], now: new Date().toISOString() }
    if (path === '/availability') {
      if (params?.cluster_size === 2) return { available: 1, rdma: true }
      if (params?.cluster_size === 4) return { available: 0, rdma: false }
      return { available: 4, lower: '', upper: '' }
    }
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

describe('RentDialog 集群模式', () => {
  it('租用方式按 /products 的 clusters 禁用；预装应用里没有 required 集群应用', async () => {
    await open({ '2': { available: 1, rdma: true }, '4': { available: 0, rdma: false } })
    expect(q('rent-mode')).not.toBeNull()
    expect((q('rent-mode-cluster-4')!.querySelector('input') as HTMLInputElement).disabled).toBe(true)
    expect((q('rent-mode-cluster-2')!.querySelector('input') as HTMLInputElement).disabled).toBe(false)
    expect(q('rent-app')).not.toBeNull()
    q('rent-app')!.click()
    await flushPromises()
    const opts = [...document.querySelectorAll('.el-select-dropdown__item')].map((e) => e.textContent!.trim())
    expect(opts).toContain('vLLM')
    expect(opts).not.toContain('vLLM 集群')
  })

  it('选集群 2 台：数量 / 开始 / 预装 / 指定机器隐藏；显示网络与可开数量；总价 = 2 × 单价；POST /bookings 带 cluster', async () => {
    await open({ '2': { available: 1, rdma: true }, '4': { available: 0, rdma: false } })
    await clickRadio('rent-mode-cluster-2')
    await wait(350) // 可用性查询有 300ms 防抖
    await flushPromises()
    for (const id of ['rent-count', 'rent-start-later', 'rent-app', 'rent-machine']) expect(q(id), id).toBeNull()
    expect(q('rent-cluster-network')!.textContent).toBe('同一高速网络（RDMA）')
    expect(q('rent-cluster-available')!.textContent).toBe('可开 1 个')
    expect(get).toHaveBeenCalledWith('/availability', { product_id: 1, group_id: undefined, cluster_size: 2 })
    q('rent-next')!.click()
    await flushPromises()
    expect(q('market-confirm-count')!.textContent).toBe('集群 2 台')
    expect(q('market-confirm-price')!.textContent).toContain('¥200.00')
    post.mockResolvedValueOnce({ batch: { id: 'bat-1', count: 2, created_at: '' }, orders: [], instances: [{ id: 'ins-1' }, { id: 'ins-2' }], cluster: { id: 'clu-1' } })
    q('market-confirm-ok')!.click()
    await flushPromises()
    expect(post).toHaveBeenCalledWith('/bookings', {
      product_id: 1, image_id: 3, group_id: null, machine_id: null, count: 2, duration: '1d', quantity: 1, auto_renew: false, auto_renew_duration: '1d', cluster: true,
    }, { idempotencyKey: expect.any(String) })
    expect(placed).toEqual([{ instances: [{ id: 'ins-1' }, { id: 'ins-2' }], batchId: 'bat-1', immediate: true, clusterId: 'clu-1' }])
  })

  it('实时查询无可用集群：下一步禁用并说明', async () => {
    await open(undefined) // 旧后端没有 clusters：不预先禁用
    await clickRadio('rent-mode-cluster-4')
    await wait(350)
    await flushPromises()
    expect(q('rent-cluster-network')!.textContent).toBe('普通网络（性能有限）')
    expect(q('rent-cluster-available')!.textContent).toContain('暂无')
    expect((q('rent-next') as HTMLButtonElement).disabled).toBe(true)
    expect(q('rent-cluster-note')!.textContent).toContain('暂不支持预订')
  })
})
