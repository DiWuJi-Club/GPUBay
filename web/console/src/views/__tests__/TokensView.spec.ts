import { flushPromises, mount } from '@vue/test-utils'
import type { LLMModelPublic } from '@spark/shared'
import ElementPlus from 'element-plus'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter, RouterView, type Router } from 'vue-router'

const { get } = vi.hoisted(() => ({ get: vi.fn() }))
vi.mock('../../api', () => ({ api: { get, post: vi.fn() }, onUnauthorized: vi.fn() }))

import { session } from '../../store/session'
import MarketView from '../MarketView.vue'
import TokensView from '../TokensView.vue'

const model = (slug: string, over: Partial<LLMModelPublic> = {}): LLMModelPublic => ({
  slug, name: `${slug} 名称`, description: '', kind: 'chat', context_len: 32768, max_output_tokens: 8192, input_price: 100, output_price: 200,
  features: ['tools'], available: true, ...over,
})
const BASE = 'https://spark.example.com/v1'
let models: LLMModelPublic[] = []

let router: Router
let wrapper: ReturnType<typeof mount> | undefined
async function mountAt(url: string) {
  router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/tokens', component: TokensView, meta: { title: 'Token 商城' } },
      { path: '/market', component: MarketView, meta: { title: '算力市场' } },
      { path: '/login', component: { template: '<div />' }, meta: { title: '登录' } },
    ],
  })
  await router.push(url)
  await router.isReady()
  wrapper = mount(RouterView, { global: { plugins: [router, ElementPlus] }, attachTo: document.body })
  await flushPromises()
}

beforeEach(() => {
  vi.stubGlobal('matchMedia', (q: string) => ({ matches: false, media: q, addEventListener: () => {}, removeEventListener: () => {} }))
  get.mockReset()
  models = [model('bge-m3', { kind: 'embedding', input_price: 20, available: false }), model('qwen3-4b', { input_price: 50 })]
  get.mockImplementation(async (path: string) => {
    if (path === '/llm/models') return { items: models, base_url: BASE }
    if (path === '/products' || path === '/images' || path === '/apps') return { items: [] }
    if (path === '/llm/account') return { debt: 0, unsettled: 0, today_cost: 0, month_cost: 0, month_tokens: 0, limits: { rpm: 60, tpm: 200000, concurrency: 8 }, blocked: false, blocked_reason: '', kyc_verified: true, base_url: '' }
    throw new Error(`unexpected ${path}`)
  })
  session.status = 'anon'
  session.user = null
})
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  session.status = 'unknown'
  vi.unstubAllGlobals()
})

const byId = (id: string) => document.querySelector(`[data-testid="${id}"]`)

describe('TokensView（未登录）', () => {
  it('模型卡片：名称、slug、上下文、价格两行（向量模型只有输入价）、可用状态', async () => {
    await mountAt('/tokens')
    const qwen = byId('tokens-model-card-qwen3-4b')!
    expect(qwen.querySelector('[data-testid="tokens-model-name"]')!.textContent).toBe('qwen3-4b 名称')
    expect(qwen.querySelector('[data-testid="tokens-model-slug"]')!.textContent).toBe('qwen3-4b')
    expect(qwen.querySelector('[data-testid="tokens-model-context"]')!.textContent).toBe('32K')
    expect(qwen.querySelector('[data-testid="tokens-model-input-price"]')!.textContent).toBe('¥0.50')
    expect(qwen.querySelector('[data-testid="tokens-model-output-price"]')!.textContent).toBe('¥2.00')
    expect(qwen.querySelector('[data-testid="tokens-model-available"]')!.textContent).toBe('可用')
    expect(qwen.querySelector('[data-testid="tokens-model-feature"]')!.textContent).toBe('工具调用')
    const bge = byId('tokens-model-card-bge-m3')!
    expect(bge.querySelector('[data-testid="tokens-model-available"]')!.textContent).toBe('暂无节点')
    expect(bge.querySelector('[data-testid="tokens-model-output-price"]')).toBeNull()
    expect(get).toHaveBeenCalledWith('/llm/models')
  })

  it('五个 Tab；Keys / 用量 / 日志显示登录引导，点「去登录」带 redirect', async () => {
    await mountAt('/tokens')
    for (const t of ['models', 'keys', 'usage', 'logs', 'quickstart']) expect(byId(`tokens-tab-${t}`), t).not.toBeNull()
    expect(document.querySelectorAll('[data-testid="tokens-login-prompt"]')).toHaveLength(3)
    await router.replace('/tokens?tab=keys')
    await flushPromises()
    ;(document.querySelector('[data-testid="tokens-login-btn"]') as HTMLElement).click()
    await flushPromises()
    expect(router.currentRoute.value.path).toBe('/login')
    expect(router.currentRoute.value.query.redirect).toBe('/tokens?tab=keys')
  })

  it('快速开始：base_url 来自接口，示例用第一个对话模型与占位 Key，含默认限额与错误码表', async () => {
    await mountAt('/tokens?tab=quickstart')
    expect(byId('tokens-quickstart-base-url')!.textContent).toBe(BASE)
    expect(byId('tokens-quickstart-model')!.textContent).toBe('qwen3-4b')
    const curl = byId('tokens-quickstart-curl')!.textContent!
    expect(curl).toContain(`${BASE}/chat/completions`)
    expect(curl).toContain('"model":"qwen3-4b"')
    expect(curl).toContain('sk-spark-xxxx')
    expect(byId('tokens-quickstart-python')!.textContent).toContain(`base_url="${BASE}"`)
    expect(byId('tokens-quickstart-note-limits')!.textContent).toMatch(/60 次.*200K token.*8 个并发/s)
    expect(byId('tokens-quickstart-note-usage')!.textContent).toContain('usage')
    expect(byId('tokens-quickstart-note-debt')!.textContent).toContain('402')
    expect(byId('tokens-quickstart-errors')!.textContent).toContain('invalid_api_key')
  })

  it('已登录：不显示登录引导', async () => {
    session.status = 'authed'
    await mountAt('/tokens')
    expect(document.querySelectorAll('[data-testid="tokens-login-prompt"]')).toHaveLength(0)
  })
})

describe('MarketView 的 Token 商城横幅', () => {
  it('显示最低输入价并链接到 /tokens', async () => {
    await mountAt('/market')
    const banner = byId('market-tokens-banner')!
    expect(banner.getAttribute('href')).toBe('/tokens')
    expect(byId('market-tokens-banner-price')!.textContent).toBe('¥0.20')
  })

  it('没有在线模型时不显示', async () => {
    models = []
    await mountAt('/market')
    expect(byId('market-tokens-banner')).toBeNull()
  })
})
