import { flushPromises, mount } from '@vue/test-utils'
import { ApiError, type LLMKey, type LLMModelPublic, type User } from '@spark/shared'
import ElementPlus from 'element-plus'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter, RouterView, type Router } from 'vue-router'

const { get, post, put, del, confirm } = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn(), put: vi.fn(), del: vi.fn(), confirm: vi.fn() }))
vi.mock('../../../api', () => ({ api: { get, post, put, del }, onUnauthorized: vi.fn() }))
vi.mock('element-plus', async (importOriginal) => {
  const mod = await importOriginal<typeof import('element-plus')>()
  return { ...mod, ElMessageBox: { confirm } }
})

import { beijingToday } from '../../../logic/tokens'
import { session } from '../../../store/session'
import TokensView from '../../../views/TokensView.vue'

const model = (slug: string, over: Partial<LLMModelPublic> = {}): LLMModelPublic => ({
  slug, name: slug, description: '', kind: 'chat', context_len: 32768, max_output_tokens: 8192, input_price: 100, output_price: 200,
  features: [], available: true, ...over,
})
const key = (id: number, over: Partial<LLMKey> = {}): LLMKey => ({
  id, name: `key-${id}`, key_hint: `sk-spark-…${String(id).padStart(4, '0')}`, status: 'active', model_slugs: [], monthly_cap: null, month_cost: 0,
  expires_at: null, last_used_at: null, created_at: '2026-10-01T00:00:00Z', ...over,
})
const PLAIN = 'sk-spark-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789abcd'
let keys: LLMKey[] = []
const writeText = vi.fn()

let router: Router
let wrapper: ReturnType<typeof mount> | undefined
async function mountKeys() {
  router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/tokens', component: TokensView },
      { path: '/kyc', component: { template: '<div />' } },
    ],
  })
  await router.push('/tokens?tab=keys')
  await router.isReady()
  wrapper = mount(RouterView, { global: { plugins: [router, ElementPlus] }, attachTo: document.body })
  await flushPromises()
}

const byId = (id: string) => document.querySelector<HTMLElement>(`[data-testid="${id}"]`)
const click = async (id: string) => {
  const el = byId(id)
  expect(el, id).not.toBeNull()
  el!.click()
  await flushPromises()
}
async function type(id: string, value: string) {
  const el = byId(id) as HTMLInputElement
  expect(el, id).not.toBeNull()
  el.value = value
  el.dispatchEvent(new Event('input'))
  await flushPromises()
}
/** el-dialog 关闭后只是 v-show 隐藏（离场动画在 jsdom 里不结束），按遮罩是否显示判断。ElMessageBox 已 mock，页面上只有这一个遮罩。 */
const dialogOpen = () => {
  const o = document.querySelector<HTMLElement>('.el-overlay')
  return !!o && o.style.display !== 'none'
}
const keysGets = () => get.mock.calls.filter(([p]) => p === '/llm/keys').length

beforeEach(() => {
  vi.stubGlobal('matchMedia', (q: string) => ({ matches: false, media: q, addEventListener: () => {}, removeEventListener: () => {} }))
  writeText.mockReset().mockResolvedValue(undefined)
  vi.stubGlobal('navigator', { ...navigator, clipboard: { writeText } })
  for (const f of [get, post, put, del, confirm]) f.mockReset()
  keys = [
    key(1, { name: '生产', model_slugs: [], monthly_cap: null, month_cost: 1234, last_used_at: '2026-10-24T02:30:00Z' }),
    key(2, { name: '测试', status: 'disabled', model_slugs: ['qwen3-4b'], monthly_cap: 1000, expires_at: '2099-12-31T15:59:59Z' }),
    key(3, { name: '旧的', status: 'revoked' }),
  ]
  get.mockImplementation(async (path: string) => {
    if (path === '/llm/models') return { items: [model('qwen3-4b'), model('bge-m3', { kind: 'embedding' })], base_url: 'https://x/v1' }
    if (path === '/llm/keys') return { items: keys }
    if (path === '/llm/account') return { debt: 0, unsettled: 0, today_cost: 0, month_cost: 0, month_tokens: 0, limits: { rpm: 60, tpm: 200000, concurrency: 8 }, blocked: false, blocked_reason: '', kyc_verified: true, base_url: '' }
    throw new Error(`unexpected ${path}`)
  })
  session.status = 'authed'
  session.user = { id: 1, phone: '138****0000', kyc_status: 'verified' } as User
})
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  document.body.innerHTML = ''
  session.status = 'unknown'
  session.user = null
  vi.unstubAllGlobals()
})

describe('KeysTab 列表', () => {
  it('表格：名称、Key 提示、允许模型（空 = 全部）、月上限 / 本月已用、最近使用、到期、状态', async () => {
    await mountKeys()
    expect(get).toHaveBeenCalledWith('/llm/keys')
    const row1 = byId('tokens-key-row-1')!.closest('tr')!
    expect(row1.textContent).toContain('生产')
    expect(row1.querySelector('[data-testid="tokens-key-hint"]')!.textContent).toBe('sk-spark-…0001')
    expect(row1.querySelector('[data-testid="tokens-key-models-all"]')!.textContent).toBe('全部')
    expect(row1.querySelector('[data-testid="tokens-key-cap-cell"]')!.textContent).toBe('不限')
    expect(row1.querySelector('[data-testid="tokens-key-cost"]')!.textContent).toBe('¥12.34')
    expect(row1.textContent).toContain('2026-10-24 10:30')
    expect(row1.textContent).toContain('永不过期')
    expect(byId('tokens-key-status-1')!.textContent).toBe('启用')
    expect(byId('tokens-key-disable-1')!.textContent!.trim()).toBe('停用')

    const row2 = byId('tokens-key-row-2')!.closest('tr')!
    expect(row2.textContent).toContain('qwen3-4b')
    expect(row2.querySelector('[data-testid="tokens-key-cap-cell"]')!.textContent).toBe('¥10.00')
    expect(row2.textContent).toContain('从未使用')
    expect(row2.textContent).toContain('2099-12-31')
    expect(byId('tokens-key-status-2')!.textContent).toBe('已停用')
    expect(byId('tokens-key-disable-2')!.textContent!.trim()).toBe('启用')

    // 撤销的 Key：显示「已撤销」，没有任何操作
    expect(byId('tokens-key-status-3')!.textContent).toBe('已撤销')
    for (const a of ['disable', 'edit', 'revoke']) expect(byId(`tokens-key-${a}-3`), a).toBeNull()
    expect(byId('tokens-key-count')!.textContent).toBe('2 / 20')
  })

  it('手机：卡片列表，撤销的 Key 同样没有操作', async () => {
    vi.stubGlobal('matchMedia', (q: string) => ({ matches: true, media: q, addEventListener: () => {}, removeEventListener: () => {} }))
    await mountKeys()
    expect(byId('tokens-keys-table')).toBeNull()
    const card = byId('tokens-key-row-2')!
    expect(card.textContent).toContain('测试')
    expect(card.textContent).toContain('¥10.00')
    expect(card.querySelector('[data-testid="tokens-key-disable-2"]')!.textContent!.trim()).toBe('启用')
    expect(byId('tokens-key-status-3')!.textContent).toBe('已撤销')
    expect(byId('tokens-key-revoke-3')).toBeNull()
  })

  it('未实名：创建按钮禁用，引导链接到 /kyc', async () => {
    session.user = { id: 1, phone: '138****0000', kyc_status: 'none' } as User
    await mountKeys()
    expect((byId('tokens-key-create') as HTMLButtonElement).disabled).toBe(true)
    expect(byId('tokens-key-kyc-link')!.getAttribute('href')).toBe('/kyc?redirect=/tokens?tab=keys')
    byId('tokens-key-create')!.click()
    await flushPromises()
    expect(byId('tokens-key-name')).toBeNull()
  })

  it('已实名：没有实名引导', async () => {
    await mountKeys()
    expect((byId('tokens-key-create') as HTMLButtonElement).disabled).toBe(false)
    expect(byId('tokens-key-kyc-link')).toBeNull()
  })
})

describe('创建 Key', () => {
  it('提交表单 → 只显示一次的明文面板 → 复制 → 二次确认后关闭', async () => {
    await mountKeys()
    post.mockResolvedValue({ key: PLAIN, item: key(4, { name: '新 Key' }) })
    await click('tokens-key-create')
    await type('tokens-key-name', '  新 Key ')
    // 模型多选：选 qwen3-4b
    byId('tokens-key-model-option-qwen3-4b')!.click()
    await flushPromises()
    await type('tokens-key-cap', '19.99')
    await click('tokens-key-submit')
    expect(post).toHaveBeenCalledWith('/llm/keys', { name: '新 Key', model_slugs: ['qwen3-4b'], monthly_cap: 1999 })
    expect(keysGets()).toBe(2) // 创建成功即刷新列表

    expect(byId('tokens-key-plain')!.textContent).toBe(PLAIN)
    expect(byId('tokens-key-warning')!.textContent).toContain('关闭后不再显示，请立即复制保存')
    await click('tokens-key-copy')
    expect(writeText).toHaveBeenCalledWith(PLAIN)

    // 取消二次确认：面板仍在
    confirm.mockRejectedValueOnce('cancel')
    await click('tokens-key-done')
    expect(confirm).toHaveBeenCalledTimes(1)
    expect(byId('tokens-key-plain')).not.toBeNull()

    // 点右上角关闭同样要确认
    confirm.mockRejectedValueOnce('cancel')
    document.querySelector<HTMLElement>('.el-dialog__headerbtn')!.click()
    await flushPromises()
    expect(confirm).toHaveBeenCalledTimes(2)
    expect(byId('tokens-key-plain')).not.toBeNull()

    confirm.mockResolvedValueOnce('confirm')
    await click('tokens-key-done')
    expect(dialogOpen()).toBe(false)
    // 离场动画期间仍是明文面板（不闪回创建表单），但明文已清空
    expect(byId('tokens-key-plain')!.textContent).toBe('')
    expect(byId('tokens-key-name')).toBeNull()

    // 再次打开是空表单，没有上一次的明文
    await click('tokens-key-create')
    expect((byId('tokens-key-name') as HTMLInputElement).value).toBe('')
    expect(byId('tokens-key-plain')).toBeNull()
  })

  it('明文面板显示时离开页面要确认：取消留在原页，确认才跳转；刷新 / 关标签页触发 beforeunload 拦截', async () => {
    await mountKeys()
    post.mockResolvedValue({ key: PLAIN, item: key(4) })
    await click('tokens-key-create')
    await type('tokens-key-name', 'ci')
    await click('tokens-key-submit')
    expect(byId('tokens-key-plain')!.textContent).toBe(PLAIN)

    const unload = new Event('beforeunload', { cancelable: true })
    window.dispatchEvent(unload)
    expect(unload.defaultPrevented).toBe(true)

    confirm.mockRejectedValueOnce('cancel')
    await router.push('/kyc')
    expect(confirm).toHaveBeenCalledTimes(1)
    expect(router.currentRoute.value.path).toBe('/tokens')
    expect(byId('tokens-key-plain')!.textContent).toBe(PLAIN)

    confirm.mockResolvedValueOnce('confirm')
    await router.push('/kyc')
    await flushPromises()
    expect(confirm).toHaveBeenCalledTimes(2)
    expect(router.currentRoute.value.path).toBe('/kyc')

    // 组件已卸载：监听已移除，不再拦截
    const later = new Event('beforeunload', { cancelable: true })
    window.dispatchEvent(later)
    expect(later.defaultPrevented).toBe(false)
  })

  it('没有明文面板时离开页面不打扰；表单阶段 beforeunload 不拦截', async () => {
    await mountKeys()
    await click('tokens-key-create')
    const unload = new Event('beforeunload', { cancelable: true })
    window.dispatchEvent(unload)
    expect(unload.defaultPrevented).toBe(false)
    await router.push('/kyc')
    expect(confirm).not.toHaveBeenCalled()
    expect(router.currentRoute.value.path).toBe('/kyc')
  })

  it('到期日期：原生日期框，最早今天（北京时间），选了就带 expires_at', async () => {
    await mountKeys()
    post.mockResolvedValue({ key: PLAIN, item: key(4) })
    await click('tokens-key-create')
    const input = byId('tokens-key-expires') as HTMLInputElement
    expect(input.type).toBe('date')
    expect(input.min).toBe(beijingToday(new Date()))
    await type('tokens-key-name', 'ci')
    await type('tokens-key-expires', '2099-01-31')
    await click('tokens-key-submit')
    expect(post).toHaveBeenCalledWith('/llm/keys', { name: 'ci', model_slugs: [], expires_at: '2099-01-31T23:59:59+08:00' })
  })

  it('最小输入只带名称与空模型列表（= 全部模型）', async () => {
    await mountKeys()
    post.mockResolvedValue({ key: PLAIN, item: key(4) })
    await click('tokens-key-create')
    await type('tokens-key-name', 'ci')
    await click('tokens-key-submit')
    expect(post).toHaveBeenCalledWith('/llm/keys', { name: 'ci', model_slugs: [] })
  })

  it('前端校验失败不发请求', async () => {
    await mountKeys()
    await click('tokens-key-create')
    await type('tokens-key-cap', '1.234')
    await click('tokens-key-submit')
    expect(post).not.toHaveBeenCalled()
    // el-form-item 的错误提示有 100ms 防抖
    await vi.waitFor(() => {
      const text = document.querySelector('.el-dialog')!.textContent!
      expect(text).toContain('请填写名称')
      expect(text).toContain('月上限须大于 0，最多两位小数')
    })
  })

  it('后端错误文案直接显示（llm_key_limit），对话框不关', async () => {
    await mountKeys()
    post.mockRejectedValue(new ApiError(409, 'llm_key_limit', '最多创建 20 个 API Key'))
    await click('tokens-key-create')
    await type('tokens-key-name', '第 21 个')
    await click('tokens-key-submit')
    expect(byId('tokens-key-error')!.textContent).toBe('最多创建 20 个 API Key')
    expect(dialogOpen()).toBe(true)
    expect(byId('tokens-key-plain')).toBeNull()
    expect(dialogOpen()).toBe(true)
  })
})

describe('行操作', () => {
  it('停用 / 启用：PUT status 后刷新', async () => {
    await mountKeys()
    put.mockResolvedValue(key(1, { status: 'disabled' }))
    await click('tokens-key-disable-1')
    expect(put).toHaveBeenCalledWith('/llm/keys/1', { status: 'disabled' })
    await click('tokens-key-disable-2')
    expect(put).toHaveBeenLastCalledWith('/llm/keys/2', { status: 'active' })
    expect(keysGets()).toBe(3)
  })

  it('编辑：回填名称 / 模型 / 月上限；清空月上限发 null；没有到期日字段', async () => {
    await mountKeys()
    put.mockResolvedValue(key(2))
    await click('tokens-key-edit-2')
    expect((byId('tokens-key-name') as HTMLInputElement).value).toBe('测试')
    expect((byId('tokens-key-cap') as HTMLInputElement).value).toBe('10.00')
    expect(byId('tokens-key-expires')).toBeNull()
    await type('tokens-key-name', '测试 2')
    await type('tokens-key-cap', '')
    await click('tokens-key-submit')
    expect(put).toHaveBeenCalledWith('/llm/keys/2', { name: '测试 2', model_slugs: ['qwen3-4b'], monthly_cap: null })
    expect(post).not.toHaveBeenCalled()
    expect(keysGets()).toBe(2)
    expect(dialogOpen()).toBe(false)
  })

  it('编辑失败显示后端文案（llm_key_invalid）', async () => {
    await mountKeys()
    put.mockRejectedValue(new ApiError(400, 'llm_key_invalid', '模型 foo 不存在或未上线'))
    await click('tokens-key-edit-1')
    await click('tokens-key-submit')
    expect(byId('tokens-key-error')!.textContent).toBe('模型 foo 不存在或未上线')
  })

  it('撤销：确认后 DELETE 并刷新；取消则不请求', async () => {
    await mountKeys()
    confirm.mockRejectedValueOnce('cancel')
    await click('tokens-key-revoke-1')
    expect(confirm).toHaveBeenCalledTimes(1)
    expect(confirm.mock.calls[0][0]).toContain('生产')
    expect(del).not.toHaveBeenCalled()

    confirm.mockResolvedValueOnce('confirm')
    del.mockResolvedValue({})
    await click('tokens-key-revoke-1')
    expect(del).toHaveBeenCalledWith('/llm/keys/1')
    expect(keysGets()).toBe(2)
  })
})
