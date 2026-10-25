import { mount } from '@vue/test-utils'
import ElementPlus from 'element-plus'
import type { LLMModelPublic } from '@spark/shared'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter } from 'vue-router'
import ConsoleLayout from '../../layouts/ConsoleLayout.vue'
import {
  contextLabel, costYuan, curlExample, DEFAULT_LLM_LIMITS, exampleModel, keyHintMask, llmErrorHelp, llmErrorStatus, lowestInputPrice,
  pricePerMillionYuan, pythonExample,
} from '../tokens'

const model = (slug: string, over: Partial<LLMModelPublic> = {}): LLMModelPublic => ({
  slug, name: slug, description: '', kind: 'chat', context_len: 32768, max_output_tokens: 8192, input_price: 100, output_price: 200,
  features: [], available: true, ...over,
})

describe('价格与费用', () => {
  it.each([
    [50, '0.50'],
    [0, '0.00'],
    [1234, '12.34'],
  ])('pricePerMillionYuan(%i 分) → %s（元 / 百万 token 两位小数）', (fen, want) => {
    expect(pricePerMillionYuan(fen)).toBe(want)
  })

  it.each([
    [12500, '0.000125'],
    [0, '0.000000'],
    [1_000_000, '0.010000'],
    [123_456_789, '1.234568'],
    [49, '0.000000'],
    [50, '0.000001'],
  ])('costYuan(%i 微分) → %s（元 6 位小数）', (micro, want) => {
    expect(costYuan(micro)).toBe(want)
  })

  it('lowestInputPrice：最低输入价；没有模型返回 null', () => {
    expect(lowestInputPrice([])).toBeNull()
    expect(lowestInputPrice([model('a', { input_price: 300 }), model('b', { input_price: 50 }), model('c', { input_price: 80 })])).toBe(50)
  })

  it('contextLabel：1024 的整数倍按 K 显示，其余按 formatTokens', () => {
    expect(contextLabel(32768)).toBe('32K')
    expect(contextLabel(131072)).toBe('128K')
    expect(contextLabel(1000)).toBe('1K')
    expect(contextLabel(500)).toBe('500')
  })
})

describe('快速开始示例', () => {
  const base = 'https://spark.example.com/v1'

  it('curl 示例含 base_url、模型与占位 Key', () => {
    const s = curlExample(base, 'qwen3-4b')
    expect(s).toContain('https://spark.example.com/v1/chat/completions')
    expect(s).toContain('"model":"qwen3-4b"')
    expect(s).toContain('Authorization: Bearer sk-spark-xxxx')
    expect(s).toContain('Content-Type: application/json')
  })

  it('base_url 末尾的斜杠不重复', () => {
    expect(curlExample(base + '/', 'm')).toContain('https://spark.example.com/v1/chat/completions')
    expect(pythonExample(base + '/', 'm')).toContain('base_url="https://spark.example.com/v1"')
  })

  it('Python 示例用 openai SDK、流式并读取 usage 块', () => {
    const s = pythonExample(base, 'qwen3-4b')
    expect(s).toContain('from openai import OpenAI')
    expect(s).toContain('base_url="https://spark.example.com/v1"')
    expect(s).toContain('api_key="sk-spark-xxxx"')
    expect(s).toContain('model="qwen3-4b"')
    expect(s).toContain('stream=True')
    expect(s).toContain('chunk.usage')
  })

  it('exampleModel：优先第一个对话模型，否则第一个；没有模型用占位符', () => {
    expect(exampleModel([model('emb', { kind: 'embedding' }), model('chat-a'), model('chat-b')])).toBe('chat-a')
    expect(exampleModel([model('emb', { kind: 'embedding' })])).toBe('emb')
    expect(exampleModel([])).toBe('<model>')
  })

  it('默认限额 RPM 60 / TPM 200,000 / 并发 8', () => {
    expect(DEFAULT_LLM_LIMITS).toEqual({ rpm: 60, tpm: 200_000, concurrency: 8 })
  })

  it('错误码表覆盖网关的全部错误码，且每个都有状态码', () => {
    for (const code of [
      'invalid_api_key', 'account_banned', 'kyc_required', 'account_blocked', 'insufficient_quota', 'model_not_allowed', 'model_not_found', 'not_found',
      'invalid_request_error', 'request_too_large', 'rate_limit_exceeded', 'monthly_cap_exceeded', 'model_unavailable', 'server_overloaded', 'upstream_error',
    ]) {
      expect(llmErrorHelp[code], code).toBeTruthy()
      expect(llmErrorStatus[code], code).toBeGreaterThanOrEqual(400)
    }
    expect(Object.keys(llmErrorStatus).sort()).toEqual(Object.keys(llmErrorHelp).sort())
  })
})

describe('keyHintMask', () => {
  it('后端给的 sk-spark-…末 4 位原样显示；只有末 4 位时补前缀；空串显示 —', () => {
    expect(keyHintMask('sk-spark-…a1b2')).toBe('sk-spark-…a1b2')
    expect(keyHintMask('a1b2')).toBe('sk-spark-…a1b2')
    expect(keyHintMask('sk-spark-abcdefgh1234')).toBe('sk-spark-…1234')
    expect(keyHintMask('')).toBe('—')
  })
})

describe('ConsoleLayout 导航', () => {
  afterEach(() => vi.unstubAllGlobals())

  function mountLayout(mobile: boolean) {
    vi.stubGlobal('matchMedia', (q: string) => ({ matches: mobile, media: q, addEventListener: () => {}, removeEventListener: () => {} }))
    const router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/:p(.*)*', component: { template: '<div />' }, meta: { title: 't' } }] })
    return mount(ConsoleLayout, { global: { plugins: [router, ElementPlus] } })
  }

  it('电脑侧边菜单最后一项是 Token 商城 → /tokens', () => {
    const w = mountLayout(false)
    const items = w.findAll('[data-testid^="menu-"]')
    expect(items.map((i) => i.attributes('data-testid'))).toEqual(['menu-market', 'menu-workflows', 'menu-instances', 'menu-wallet', 'menu-account', 'menu-tokens'])
    const tokens = w.get('[data-testid="menu-tokens"]')
    expect(tokens.text()).toContain('Token 商城')
    expect(tokens.attributes('href')).toBe('/tokens')
  })

  it('手机底部导航 6 项，Token 商城与工作流市场用短名', () => {
    const w = mountLayout(true)
    expect(w.findAll('[data-testid^="tabbar-"]')).toHaveLength(6)
    expect(w.get('[data-testid="tabbar-tokens"]').text()).toBe('Token')
    expect(w.get('[data-testid="tabbar-workflows"]').text()).toBe('工作流')
  })
})
