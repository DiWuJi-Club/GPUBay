import type { Admin, LLMModelAdmin, Role } from '@spark/shared'
import { flushPromises, mount } from '@vue/test-utils'
import ElementPlus, { ElMessage } from 'element-plus'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter, RouterView, type Router } from 'vue-router'

const { get, post, put, del } = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn(), put: vi.fn(), del: vi.fn() }))
vi.mock('../../api', () => ({ api: { get, post, put, del }, onUnauthorized: vi.fn(), onTotpSetupRequired: vi.fn() }))

import { session } from '../../store/session'
import LlmView from '../LlmView.vue'

const model = (over: Partial<LLMModelAdmin> = {}): LLMModelAdmin => ({
  id: 3, slug: 'qwen3-32b', name: 'Qwen3 32B', description: '', kind: 'chat', source: 'modelscope', source_id: 'Qwen/Qwen3-32B',
  image_id: 5, app_version_id: null, context_len: 32768, max_output_tokens: 8192, default_max_tokens: 4096,
  input_price: 150, output_price: 600, node_env: { MAX_NUM_SEQS: '64' }, replicas: 1, max_concurrency: 32, features: ['tools'],
  status: 'draft', sort: 0, created_at: '2026-10-24T00:00:00Z', updated_at: '2026-10-24T00:00:00Z', nodes: { ready: 1, total: 2 },
  ...over,
})
const summary = {
  nodes: { ready: 3, total: 4, unhealthy: 1 },
  today: { requests: 1234, prompt_tokens: 50000, completion_tokens: 7000, settled_amount: 1250, unsettled_estimate: 30 },
  debt_total: 99, debt_users: 2,
}
const version = {
  id: 11, app_id: 1, version: '1.0.0', status: 'published', image_ids: [5], spec_hash: '', changelog: '', published_at: '2026-10-20T00:00:00Z',
  created_at: '', updated_at: '', coverage: [],
  spec: {
    ports: [{ port: 8000 }],
    env: [
      { name: 'MODEL_ID', type: 'string', required: true },
      { name: 'API_KEY', type: 'secret', required: true },
      { name: 'MAX_MODEL_LEN', type: 'int' },
      { name: 'TASK', type: 'enum', options: ['generate', 'embed'] },
      { name: 'MAX_NUM_SEQS', label: '最大并发序列', type: 'int', min: 1, max: 512, default: '64' },
      { name: 'QUANTIZATION', type: 'enum', options: ['none', 'fp8'], default: 'none' },
    ],
  },
}

let models: LLMModelAdmin[]
let router: Router
let wrapper: ReturnType<typeof mount> | undefined

function login(role: Role) {
  session.status = 'authed'
  session.admin = { id: 1, username: 'a', role, status: 'active', totp_confirmed: true, last_login_at: null, last_login_ip: '', created_at: '' } as Admin
}
async function mountAt(url: string) {
  router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/llm', component: LlmView }] })
  await router.push(url)
  await router.isReady()
  wrapper = mount(RouterView, { global: { plugins: [router, ElementPlus] }, attachTo: document.body })
  await flushPromises()
}
const byId = (id: string) => wrapper!.find(`[data-testid="${id}"]`)
const allById = (id: string) => wrapper!.findAll(`[data-testid="${id}"]`)
/** 表格行里的元素（el-table 还会在隐藏的 hidden-columns 里用空 row 渲染一遍列模板，要排除） */
const rowById = (id: string) => wrapper!.find(`.el-table__body [data-testid="${id}"]`)
const rowsById = (id: string) => wrapper!.findAll(`.el-table__body [data-testid="${id}"]`)
const getPaths = () => get.mock.calls.map((c) => c[0])

beforeEach(() => {
  models = [model()]
  for (const f of [get, post, put, del]) f.mockReset()
  get.mockImplementation(async (path: string) => {
    if (path === '/llm/summary') return summary
    if (path === '/llm/models') return { items: models }
    if (path === '/apps') return { items: [{ id: 2, slug: 'vllm' }, { id: 1, slug: 'vllm-node' }] }
    if (path === '/images') return { items: [{ id: 5, name: 'LLM Serving', ref: '', description: '', enabled: true, sort: 1 }, { id: 6, name: 'Other', enabled: true, sort: 2 }] }
    if (path === '/apps/1/versions') return { items: [version] }
    throw new Error(`unexpected GET ${path}`)
  })
  post.mockResolvedValue({})
  put.mockResolvedValue({})
  del.mockResolvedValue({})
})
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  vi.restoreAllMocks()
})

describe('LlmView Tab 与总览', () => {
  it('finance：没有节点 Tab；总览显示汇总与按模型节点卡', async () => {
    login('finance')
    await mountAt('/llm')
    expect(byId('llm-tab-overview').exists()).toBe(true)
    expect(byId('llm-tab-models').exists()).toBe(true)
    expect(byId('llm-tab-accounts').exists()).toBe(true)
    expect(byId('llm-tab-usage').exists()).toBe(true)
    expect(byId('llm-tab-nodes').exists()).toBe(false)
    expect(byId('llm-sum-nodes-ready').text()).toBe('3')
    expect(byId('llm-sum-nodes-total').text()).toBe('4')
    expect(byId('llm-sum-requests').text()).toBe('1,234')
    expect(byId('llm-sum-settled').text()).toBe('¥12.50')
    expect(byId('llm-sum-debt-users').text()).toBe('2')
    expect(allById('llm-overview-model')).toHaveLength(1)
    expect(byId('llm-overview-model-nodes').text()).toBe('1 / 2')
    expect(byId('llm-overview-model-wanted').text()).toBe('0') // 草稿：期望 0
  })

  it('?tab= 选中 Tab；无权的 Tab 回到总览；占位 Tab 可见', async () => {
    login('ops')
    await mountAt('/llm?tab=nodes')
    expect(byId('llm-nodes-placeholder').exists()).toBe(true)
    expect(getPaths()).not.toContain('/llm/summary') // 总览没打开就不加载
    wrapper!.unmount()
    login('finance')
    await mountAt('/llm?tab=nodes')
    expect(byId('llm-overview').exists()).toBe(true)
  })

  it('finance 看模型：只读（无新建 / 上下线），可查看详情', async () => {
    login('finance')
    await mountAt('/llm?tab=models')
    expect(rowsById('llm-model-row')).toHaveLength(1)
    expect(byId('llm-model-row-input-price').text()).toBe('¥1.50')
    expect(byId('llm-model-row-replicas').text()).toBe('1 / 1')
    expect(byId('llm-model-create').exists()).toBe(false)
    expect(rowById('llm-model-online').exists()).toBe(false)
    expect(rowById('llm-model-edit').exists()).toBe(false)
    await rowById('llm-model-view').trigger('click')
    await flushPromises()
    expect(byId('llm-model-save').exists()).toBe(false)
  })
})

describe('模型抽屉（super）', () => {
  it('新建：节点参数只渲染非保留变量；价格换算成分提交', async () => {
    login('super')
    await mountAt('/llm?tab=models')
    await byId('llm-model-create').trigger('click')
    await flushPromises()
    expect(byId('llm-model-env-MAX_NUM_SEQS').exists()).toBe(true)
    expect(byId('llm-model-env-QUANTIZATION').exists()).toBe(true)
    for (const n of ['MODEL_ID', 'API_KEY', 'MAX_MODEL_LEN', 'TASK']) expect(byId(`llm-model-env-${n}`).exists(), n).toBe(false)

    await byId('llm-model-slug').setValue('glm-4')
    await byId('llm-model-name').setValue('GLM 4')
    await byId('llm-model-source-id').setValue('ZhipuAI/glm-4-9b')
    await byId('llm-model-input-price').setValue('0.5')
    await byId('llm-model-output-price').setValue('2')
    await byId('llm-model-save').trigger('click')
    await flushPromises()
    expect(post).toHaveBeenCalledTimes(1)
    const [path, body] = post.mock.calls[0]
    expect(path).toBe('/llm/models')
    expect(body).toMatchObject({ slug: 'glm-4', name: 'GLM 4', source_id: 'ZhipuAI/glm-4-9b', image_id: 5, input_price: 50, output_price: 200, node_env: {}, app_version_id: null, replicas: 0 })
  })

  it('新建：表单错误显示在字段下，不发请求', async () => {
    login('super')
    await mountAt('/llm?tab=models')
    await byId('llm-model-create').trigger('click')
    await flushPromises()
    await byId('llm-model-slug').setValue('Bad Slug')
    await byId('llm-model-save').trigger('click')
    await flushPromises()
    expect(post).not.toHaveBeenCalled()
    // el-form-item 的错误状态有 100ms 防抖
    await vi.waitFor(() => {
      expect(wrapper!.text()).toContain('2～64 位小写字母')
      expect(wrapper!.text()).toContain('格式为「组织/模型」')
    })
  })

  it('编辑：改来源且已有节点 → 页内提示 + 保存后警告；PUT 不带 slug', async () => {
    login('super')
    const warn = vi.spyOn(ElMessage, 'warning')
    await mountAt('/llm?tab=models')
    await rowById('llm-model-edit').trigger('click')
    await flushPromises()
    expect(byId('llm-model-stale-warning').exists()).toBe(false)
    expect((byId('llm-model-input-price').element as HTMLInputElement).value).toBe('1.50')
    await byId('llm-model-source-id').setValue('Qwen/Qwen3-14B')
    expect(byId('llm-model-stale-warning').text()).toContain('已有 2 个节点仍按旧参数运行，可逐个重建')
    await byId('llm-model-save').trigger('click')
    await flushPromises()
    expect(put).toHaveBeenCalledTimes(1)
    const [path, body] = put.mock.calls[0]
    expect(path).toBe('/llm/models/3')
    expect(body).not.toHaveProperty('slug')
    expect(body).toMatchObject({ source_id: 'Qwen/Qwen3-14B', node_env: { MAX_NUM_SEQS: '64' }, input_price: 150 })
    expect(warn).toHaveBeenCalledWith(expect.objectContaining({ message: '已有 2 个节点仍按旧参数运行，可逐个重建' }))
  })

  it('编辑：只改价格不提示', async () => {
    login('super')
    const warn = vi.spyOn(ElMessage, 'warning')
    await mountAt('/llm?tab=models')
    await rowById('llm-model-edit').trigger('click')
    await flushPromises()
    await byId('llm-model-input-price').setValue('2.00')
    expect(byId('llm-model-stale-warning').exists()).toBe(false)
    await byId('llm-model-save').trigger('click')
    await flushPromises()
    expect(put.mock.calls[0][1]).toMatchObject({ input_price: 200 })
    expect(warn).not.toHaveBeenCalled()
  })

  it('vllm-node 未导入：提示且不能保存', async () => {
    login('super')
    const base = get.getMockImplementation()!
    get.mockImplementation(async (path: string) => (path === '/apps' ? { items: [] } : base(path)))
    await mountAt('/llm?tab=models')
    await byId('llm-model-create').trigger('click')
    await flushPromises()
    expect(byId('llm-model-catalog-error').text()).toContain('未找到内部应用 vllm-node')
    expect(byId('llm-model-save').attributes('disabled')).toBeDefined()
  })
})

describe('上下线与删除（super）', () => {
  it('上线：确认后 POST status（动态码由请求层弹框）', async () => {
    login('super')
    await mountAt('/llm?tab=models')
    await rowById('llm-model-online').trigger('click')
    await flushPromises()
    await byId('llm-model-status-ok').trigger('click')
    await flushPromises()
    expect(post).toHaveBeenCalledWith('/llm/models/3/status', { status: 'online' })
  })

  it('副本为 0 时上线被拦下', async () => {
    login('super')
    models = [model({ replicas: 0 })]
    await mountAt('/llm?tab=models')
    await rowById('llm-model-online').trigger('click')
    await flushPromises()
    expect(byId('llm-model-status-block').text()).toContain('上线前请设置至少 1 个副本')
    await byId('llm-model-status-ok').trigger('click')
    await flushPromises()
    expect(post).not.toHaveBeenCalled()
  })

  it('删除：只有无节点的草稿显示按钮；确认后 DELETE', async () => {
    login('super')
    models = [model(), model({ id: 4, slug: 'free', nodes: { ready: 0, total: 0 } }), model({ id: 5, slug: 'on', status: 'online', nodes: { ready: 0, total: 0 } })]
    await mountAt('/llm?tab=models')
    expect(rowsById('llm-model-delete')).toHaveLength(1)
    await rowById('llm-model-delete').trigger('click')
    await flushPromises()
    await byId('llm-model-delete-ok').trigger('click')
    await flushPromises()
    expect(del).toHaveBeenCalledWith('/llm/models/4', {})
  })
})
