import { fmtTime, type AdminAppVersion, type AdminImage, type AppEnvSpec, type LLMModelAdmin, type LLMNodeState } from '@spark/shared'
import { describe, expect, it } from 'vitest'
import {
  compatibleImages, emptyModelForm, extraEnvKeys, fenToYuanInput, fmtPrice, imageNote, latestPublished, modelToForm, nodeEnvSpec, nodeParamsChanged, staleNodesWarning,
  tabFromQuery, validateEnvValue, validateModelForm, visibleTabs, wantedReplicas, yuanInputToFen, type ModelForm,
  canDrainNode, canRebuildNode, canRemoveNode, isPoolGroup, nodeDeployText, poolCandidateQuery, poolLeaseText, validateNodeRemoveReason,
} from '../llm'

const model = (over: Partial<LLMModelAdmin> = {}): LLMModelAdmin => ({
  id: 3, slug: 'qwen3-32b', name: 'Qwen3 32B', description: '', kind: 'chat', source: 'modelscope', source_id: 'Qwen/Qwen3-32B',
  image_id: 5, app_version_id: null, context_len: 32768, max_output_tokens: 8192, default_max_tokens: 4096,
  input_price: 150, output_price: 600, node_env: { MAX_NUM_SEQS: '64' }, replicas: 1, max_concurrency: 32, features: ['tools'],
  status: 'draft', sort: 0, created_at: '2026-10-24T00:00:00Z', updated_at: '2026-10-24T00:00:00Z', nodes: { ready: 1, total: 2 },
  ...over,
})

const env = (over: Partial<AppEnvSpec> & Pick<AppEnvSpec, 'name' | 'type'>): AppEnvSpec => ({ ...over })

const vllmEnv: AppEnvSpec[] = [
  env({ name: 'MODEL_ID', type: 'string', required: true }),
  env({ name: 'MODEL_SOURCE', type: 'enum', options: ['modelscope', 'hf-mirror'] }),
  env({ name: 'SERVED_MODEL_NAME', type: 'string', required: true }),
  env({ name: 'MAX_MODEL_LEN', type: 'int', min: 1024, max: 1048576 }),
  env({ name: 'API_KEY', type: 'secret', required: true }),
  env({ name: 'TASK', type: 'enum', options: ['generate', 'embed'], default: 'generate' }),
  env({ name: 'GPU_MEMORY_UTILIZATION', type: 'string', pattern: '^0\\.[3-9][0-9]?$', default: '0.85' }),
  env({ name: 'MAX_NUM_SEQS', type: 'int', min: 1, max: 512, default: '64' }),
  env({ name: 'QUANTIZATION', type: 'enum', options: ['none', 'fp8', 'awq', 'gptq'], default: 'none' }),
]

/** 合法的新建表单 */
const goodForm = (over: Partial<ModelForm> = {}): ModelForm => ({
  ...emptyModelForm(),
  slug: 'qwen3-32b', name: 'Qwen3 32B', source_id: 'Qwen/Qwen3-32B', image_id: 5, input_price: '1.50', output_price: '6', ...over,
})

describe('元 ↔ 分换算（价格：分 / 百万 token）', () => {
  it.each([
    ['1.5', 150], ['0', 0], ['0.01', 1], [' 12.34 ', 1234], ['0.29', 29], ['4.35', 435], ['100', 10000],
    ['', null], ['-1', null], ['1.234', null], ['abc', null], ['1e3', null], ['１', null],
  ])('yuanInputToFen(%j) = %s', (input, want) => {
    expect(yuanInputToFen(input)).toBe(want)
  })

  it.each([[150, '1.50'], [0, '0.00'], [1, '0.01'], [10000, '100.00'], [29, '0.29']])('fenToYuanInput(%d) = %s', (fen, want) => {
    expect(fenToYuanInput(fen)).toBe(want)
    expect(yuanInputToFen(want)).toBe(fen) // 互逆
  })

  it('fmtPrice 显示两位小数（元 / 百万）', () => {
    expect(fmtPrice(150)).toBe('¥1.50')
    expect(fmtPrice(0)).toBe('¥0.00')
    expect(fmtPrice(123456)).toBe('¥1,234.56')
  })
})

describe('nodeEnvSpec', () => {
  it('去掉平台保留变量（MODEL_ID / MODEL_SOURCE / SERVED_MODEL_NAME / MAX_MODEL_LEN / API_KEY / TASK）', () => {
    expect(nodeEnvSpec(vllmEnv).map((e) => e.name)).toEqual(['GPU_MEMORY_UTILIZATION', 'MAX_NUM_SEQS', 'QUANTIZATION'])
    expect(nodeEnvSpec(undefined)).toEqual([])
  })
})

describe('validateEnvValue', () => {
  const gpu = vllmEnv[6]
  const seqs = vllmEnv[7]
  const quant = vllmEnv[8]
  it.each([
    [gpu, '', null], [gpu, '0.85', null], [gpu, '0.3', null], [gpu, '0.2', '格式不正确'], [gpu, '1.0', '格式不正确'],
    [seqs, '64', null], [seqs, '0', '超出允许范围'], [seqs, '513', '超出允许范围'], [seqs, '6.4', '必须是整数'],
    [quant, 'fp8', null], [quant, 'int4', '不在可选项内'],
    [env({ name: 'X', type: 'bool' }), 'yes', '只能是 true 或 false'],
    [env({ name: 'X', type: 'string', required: true, label: '必填项' }), '', '请填写 必填项'],
    [env({ name: 'X', type: 'string' }), 'a\nb', '不能包含换行或 NUL'],
  ] as const)('%#', (e, v, want) => {
    expect(validateEnvValue(e, v)).toBe(want)
  })
})

describe('validateModelForm（镜像规格 §3.1）', () => {
  const spec = nodeEnvSpec(vllmEnv)

  it('spec 外的变量：未确认 → 不能保存并列出变量名；确认后从请求里去掉', () => {
    const f = goodForm({ node_env: { MAX_NUM_SEQS: '128', OTHER: '1', API_KEY: 'x', EMPTY: '' } })
    expect(extraEnvKeys(f, spec)).toEqual(['API_KEY', 'OTHER'])
    const blocked = validateModelForm(f, { creating: true, envSpec: spec })
    expect(blocked.ok).toBe(false)
    if (!blocked.ok) expect(blocked.errors.node_env_extra).toContain('API_KEY、OTHER')
    const out = validateModelForm({ ...f, drop_extra_env: true }, { creating: true, envSpec: spec })
    expect(out.ok && out.input.node_env).toEqual({ MAX_NUM_SEQS: '128' })
  })

  it('镜像不在所选版本兼容列表（或已停用）→ 拦下；不传列表时不在前端校验', () => {
    const blocked = validateModelForm(goodForm({ image_id: 6 }), { creating: true, envSpec: spec, allowedImageIds: [5] })
    expect(blocked.ok).toBe(false)
    if (!blocked.ok) expect(blocked.errors.image_id).toContain('不兼容所选 vllm-node 版本')
    expect(validateModelForm(goodForm({ image_id: 5 }), { creating: true, envSpec: spec, allowedImageIds: [5] }).ok).toBe(true)
    expect(validateModelForm(goodForm({ image_id: 6 }), { creating: true, envSpec: spec }).ok).toBe(true)
  })

  it('合法表单 → 请求体：价格换算成分，node_env 去掉空值', () => {
    const out = validateModelForm(goodForm({ node_env: { MAX_NUM_SEQS: '128', GPU_MEMORY_UTILIZATION: '' } }), { creating: true, envSpec: spec })
    expect(out.ok).toBe(true)
    if (!out.ok) return
    expect(out.input).toMatchObject({ slug: 'qwen3-32b', input_price: 150, output_price: 600, node_env: { MAX_NUM_SEQS: '128' }, app_version_id: null })
    expect(out.input.name).toBe('Qwen3 32B')
  })

  it('编辑时 slug 不校验', () => {
    expect(validateModelForm(goodForm({ slug: '' }), { creating: false, envSpec: spec }).ok).toBe(true)
  })

  it.each([
    ['slug', { slug: 'Qwen3' }], ['slug', { slug: 'a' }], ['slug', { slug: '-abc' }], ['slug', { slug: 'a'.repeat(65) }],
    ['name', { name: '  ' }], ['name', { name: '名'.repeat(65) }],
    ['description', { description: 'x'.repeat(2001) }],
    ['source_id', { source_id: 'Qwen3-32B' }], ['source_id', { source_id: 'Qwen/Qwen3 32B' }], ['source_id', { source_id: 'a/b/c' }],
    ['image_id', { image_id: undefined }],
    ['context_len', { context_len: 1023 }], ['context_len', { context_len: 1048577 }], ['context_len', { context_len: undefined }],
    ['max_output_tokens', { max_output_tokens: 0 }], ['max_output_tokens', { max_output_tokens: 262145 }],
    ['default_max_tokens', { default_max_tokens: 0 }], ['default_max_tokens', { max_output_tokens: 1000, default_max_tokens: 2000 }],
    ['input_price', { input_price: '-1' }], ['input_price', { input_price: '1.234' }], ['output_price', { output_price: '' }],
    ['replicas', { replicas: 201 }], ['replicas', { replicas: -1 }],
    ['max_concurrency', { max_concurrency: 0 }], ['max_concurrency', { max_concurrency: 513 }],
    ['features', { features: ['tools', 'magic'] }],
    ['node_env.MAX_NUM_SEQS', { node_env: { MAX_NUM_SEQS: '9999' } }],
    ['node_env.GPU_MEMORY_UTILIZATION', { node_env: { GPU_MEMORY_UTILIZATION: '0.99x' } }],
  ] as const)('%s 不合法：%j', (field, over) => {
    const out = validateModelForm(goodForm(over as Partial<ModelForm>), { creating: true, envSpec: spec })
    expect(out.ok).toBe(false)
    if (out.ok) return
    expect(Object.keys(out.errors)).toContain(field)
  })

  it('边界值通过', () => {
    const out = validateModelForm(goodForm({ slug: 'a.b-1', context_len: 1024, max_output_tokens: 262144, default_max_tokens: 262144, replicas: 200, max_concurrency: 512, input_price: '0', output_price: '0' }), { creating: true, envSpec: spec })
    expect(out.ok).toBe(true)
    expect(validateModelForm(goodForm({ context_len: 1048576, replicas: 0, max_concurrency: 1 }), { creating: true, envSpec: spec }).ok).toBe(true)
  })
})

describe('modelToForm', () => {
  it('价格回显为元；node_env 只保留 spec 里的变量并为其余补空串', () => {
    const f = modelToForm(model(), nodeEnvSpec(vllmEnv))
    expect(f.input_price).toBe('1.50')
    expect(f.output_price).toBe('6.00')
    expect(f.node_env).toEqual({ GPU_MEMORY_UTILIZATION: '', MAX_NUM_SEQS: '64', QUANTIZATION: '' })
    expect(f.features).toEqual(['tools'])
    expect(f.drop_extra_env).toBe(false)
  })
  it('已存的、不在所选版本 spec 里的变量保留在表单里（不静默丢弃）', () => {
    const f = modelToForm(model({ node_env: { MAX_NUM_SEQS: '64', OLD_FLAG: 'on' } }), nodeEnvSpec(vllmEnv))
    expect(f.node_env.OLD_FLAG).toBe('on')
    expect(extraEnvKeys(f, nodeEnvSpec(vllmEnv))).toEqual(['OLD_FLAG'])
    expect(validateModelForm(f, { creating: false, envSpec: nodeEnvSpec(vllmEnv) }).ok).toBe(false)
  })
  it('往返：未改动的表单校验后与原模型参数一致', () => {
    const m = model()
    const out = validateModelForm(modelToForm(m, nodeEnvSpec(vllmEnv)), { creating: false, envSpec: nodeEnvSpec(vllmEnv) })
    expect(out.ok).toBe(true)
    if (out.ok) expect(nodeParamsChanged(m, out.input)).toBe(false)
  })
})

describe('nodeParamsChanged / staleNodesWarning', () => {
  const m = model()
  const input = () => {
    const out = validateModelForm(modelToForm(m, nodeEnvSpec(vllmEnv)), { creating: false, envSpec: nodeEnvSpec(vllmEnv) })
    if (!out.ok) throw new Error('bad form')
    return out.input
  }
  it.each([
    ['source', { source: 'hf-mirror' }], ['source_id', { source_id: 'Qwen/Qwen3-14B' }], ['image_id', { image_id: 6 }],
    ['app_version_id', { app_version_id: 9 }], ['context_len', { context_len: 65536 }], ['max_output_tokens', { max_output_tokens: 4096 }],
    ['node_env', { node_env: { MAX_NUM_SEQS: '128' } }], ['node_env 增加', { node_env: { MAX_NUM_SEQS: '64', QUANTIZATION: 'fp8' } }],
  ] as const)('%s 变化 → 提示', (_, over) => {
    const after = { ...input(), ...over }
    expect(nodeParamsChanged(m, after)).toBe(true)
    expect(staleNodesWarning(m, after)).toBe('已有 2 个节点仍按旧参数运行，可逐个重建')
  })
  it.each([
    ['name', { name: '新名字' }], ['价格', { input_price: 1, output_price: 2 }], ['replicas', { replicas: 3 }], ['max_concurrency', { max_concurrency: 8 }],
    ['default_max_tokens', { default_max_tokens: 100 }], ['features', { features: [] as string[] }], ['sort', { sort: 9 }],
  ] as const)('只改 %s 不提示', (_, over) => {
    const after = { ...input(), ...over }
    expect(nodeParamsChanged(m, after)).toBe(false)
    expect(staleNodesWarning(m, after)).toBeNull()
  })
  it('没有节点时不提示；新建不提示', () => {
    const after = { ...input(), context_len: 65536 }
    expect(staleNodesWarning(model({ nodes: { ready: 0, total: 0 } }), after)).toBeNull()
    expect(staleNodesWarning(null, after)).toBeNull()
  })
})

describe('vllm-node 版本与镜像', () => {
  const ver = (id: number, status: AdminAppVersion['status'], published_at: string | null, image_ids: number[] = [5]): AdminAppVersion => ({
    id, app_id: 1, version: `1.0.${id}`, status, image_ids, spec: { ports: [{ port: 8000 }], env: [] }, spec_hash: '', changelog: '',
    published_at, created_at: '', updated_at: '', coverage: [],
  })
  it('latestPublished：已发布中 published_at 最新（同时刻取 id 大）', () => {
    const vs = [ver(4, 'draft', null), ver(3, 'published', '2026-10-01T00:00:00Z'), ver(2, 'published', '2026-10-02T00:00:00Z'), ver(1, 'retired', '2026-10-03T00:00:00Z')]
    expect(latestPublished(vs)?.id).toBe(2)
    expect(latestPublished([ver(5, 'published', '2026-10-02T00:00:00Z'), ver(6, 'published', '2026-10-02T00:00:00Z')])?.id).toBe(6)
    expect(latestPublished([ver(4, 'draft', null)])).toBeUndefined()
  })
  it('compatibleImages：版本 image_ids 里启用的镜像；当前所选即使已停用也保留', () => {
    const img = (id: number, enabled: boolean): AdminImage => ({ id, name: `img${id}`, ref: '', description: '', enabled, sort: id, created_at: '', updated_at: '' })
    const images = [img(5, true), img(6, false), img(7, true)]
    expect(compatibleImages(ver(1, 'published', '', [5, 6]), images).map((i) => i.id)).toEqual([5])
    expect(compatibleImages(ver(1, 'published', '', [5, 6]), images, 6).map((i) => i.id)).toEqual([5, 6])
    expect(compatibleImages(undefined, images).map((i) => i.id)).toEqual([5, 7]) // 没有版本信息：列出全部启用镜像，由服务器校验
    expect(compatibleImages(ver(1, 'published', '', [5]), images, 7).map((i) => i.id)).toEqual([5, 7]) // 当前所选不兼容也保留
  })
  it('imageNote：不兼容所选版本 / 已停用 / 可选', () => {
    const img = (id: number, enabled: boolean): AdminImage => ({ id, name: `img${id}`, ref: '', description: '', enabled, sort: id, created_at: '', updated_at: '' })
    const v = ver(1, 'published', '', [5, 6])
    expect(imageNote(v, img(7, true))).toBe('不兼容所选版本')
    expect(imageNote(v, img(6, false))).toBe('已停用')
    expect(imageNote(v, img(5, true))).toBe('')
    expect(imageNote(undefined, img(7, true))).toBe('')
  })
})

describe('wantedReplicas', () => {
  it.each([['online', 3, 3], ['draft', 3, 0], ['offline', 3, 0]] as const)('%s replicas=%d → %d', (status, replicas, want) => {
    expect(wantedReplicas({ status, replicas })).toBe(want)
  })
})

describe('Tab 按角色显示', () => {
  it.each([
    ['super', ['overview', 'models', 'nodes', 'accounts', 'usage']],
    ['ops', ['overview', 'models', 'nodes', 'accounts']],
    ['finance', ['overview', 'models', 'accounts', 'usage']],
  ] as const)('%s', (role, want) => {
    expect(visibleTabs(role).map((t) => t.key)).toEqual(want)
  })
  it.each([
    ['ops', 'nodes', 'nodes'], ['ops', 'usage', 'overview'], ['finance', 'nodes', 'overview'], ['finance', undefined, 'overview'],
    ['super', 'bogus', 'overview'], ['super', ['models', 'usage'], 'models'],
  ] as const)('%s ?tab=%j → %s', (role, q, want) => {
    expect(tabFromQuery(q as string | string[] | undefined, role)).toBe(want)
  })
})

describe('节点与机器池', () => {
  it('行操作按状态（与后端 drain / rebuild / remove 允许的状态一致）', () => {
    const states: LLMNodeState[] = ['provisioning', 'ready', 'unhealthy', 'draining', 'removing', 'removed', 'failed']
    const ops = Object.fromEntries(states.map((s) => [s, [canDrainNode(s) && 'drain', canRebuildNode(s) && 'rebuild', canRemoveNode(s) && 'remove'].filter(Boolean)]))
    expect(ops).toEqual({
      provisioning: ['drain', 'remove'],
      ready: ['drain', 'rebuild', 'remove'],
      unhealthy: ['drain', 'rebuild', 'remove'],
      draining: ['remove'],
      removing: [],
      removed: [],
      failed: ['rebuild', 'remove'],
    })
  })
  it('移除原因：必填、去空白、≤ 200 字', () => {
    expect(validateNodeRemoveReason('  ')).toBe('请填写移除原因')
    expect(validateNodeRemoveReason(' 机器下架 ')).toBeNull()
    expect(validateNodeRemoveReason('字'.repeat(200))).toBeNull()
    expect(validateNodeRemoveReason('字'.repeat(201))).toBe('原因最多 200 个字符')
  })
  it('部署进度文本', () => {
    expect(nodeDeployText({ deployment: null })).toBe('—')
    expect(nodeDeployText({ deployment: { state: 'installing', health: 'unknown', phase: 'download', progress: 42 } })).toBe('下载模型 42%')
    expect(nodeDeployText({ deployment: { state: 'installing', health: 'unknown', phase: '', progress: 5 } })).toBe('5%')
    expect(nodeDeployText({ deployment: { state: 'queued', health: 'unknown', phase: 'busy', progress: 0 } })).toBe('等待上一次部署结束')
    expect(nodeDeployText({ deployment: { state: 'running', health: 'ok', phase: '', progress: 100 } })).toBe('运行中')
  })
  it('租约到期：没有节点时「等待租约结束」', () => {
    const at = '2026-11-01T02:00:00Z'
    expect(poolLeaseText({})).toBe('—')
    expect(poolLeaseText({ lease_until: at })).toBe(`等待租约结束（${fmtTime(at)}）`)
    expect(poolLeaseText({ lease_until: at, node: { id: 'n1', model_slug: 'm', state: 'ready' } })).toBe(fmtTime(at))
  })
  it('池机组识别：详情 group 带 purpose 优先，否则查 GET /groups', () => {
    expect(isPoolGroup(null, null, [{ id: 1, purpose: 'llm' }])).toBe(false)
    expect(isPoolGroup(1, { id: 1, purpose: 'llm' }, [])).toBe(true)
    expect(isPoolGroup(1, { id: 1, purpose: 'rental' }, [{ id: 1, purpose: 'llm' }])).toBe(false)
    expect(isPoolGroup(1, { id: 1 }, [{ id: 1, purpose: 'llm' }])).toBe(true)
    expect(isPoolGroup(2, null, [{ id: 1, purpose: 'llm' }, { id: 2 }])).toBe(false)
  })
  it('候选机器查询：未分组 / 某机组 + 机柜', () => {
    expect(poolCandidateQuery('none', ' ', 1)).toEqual({ group_id: 'none', rack: undefined, page: 1, page_size: 50 })
    expect(poolCandidateQuery(3, 'A01', 2)).toEqual({ group_id: 3, rack: 'A01', page: 2, page_size: 50 })
  })
})
