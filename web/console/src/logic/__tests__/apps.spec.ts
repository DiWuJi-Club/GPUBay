import type { AppCatalogItem, AppEnvSpec, Deployment, Image } from '@spark/shared'
import { describe, expect, it } from 'vitest'
import {
  appConfirmText, appOptionLabel, canGenerate, compatibleImages, deploymentStatusText, envDefaults, generateSecret, stripEmpty, validateEnvValue,
} from '../apps'

const vllmEnv: AppEnvSpec[] = [
  { name: 'MODEL_ID', label: '模型', type: 'enum', required: true, options: ['Qwen/Qwen3-8B', 'Qwen/Qwen3-4B'], default: 'Qwen/Qwen3-8B' },
  { name: 'MAX_MODEL_LEN', label: '最大上下文', type: 'int', min: 1024, max: 131072, default: '32768' },
  { name: 'GPU_MEMORY_UTILIZATION', label: '显存占用比例', type: 'string', pattern: '^0\\.[3-9][0-9]?$', default: '0.7' },
  { name: 'API_KEY', label: '接口密钥', type: 'secret', pattern: '^[A-Za-z0-9_-]{8,128}$' },
  { name: 'DEBUG', label: '调试', type: 'bool' },
]
const app = (over: Partial<AppCatalogItem> = {}): AppCatalogItem => ({
  id: 1, slug: 'vllm', name: 'vLLM 推理服务', summary: '推理接口', category: 'llm', icon: 'llm',
  version: { id: 10, version: '1.0.0', ports: [{ port: 8000 }], env: vllmEnv, links: [], timeout_minutes: 90 },
  images: [{ id: 3, name: 'LLM Serving' }, { id: 1, name: 'PyTorch' }],
  ...over,
})
const img = (id: number, name: string): Image => ({ id, name, description: '' })

describe('compatibleImages', () => {
  it('按 app.images 的顺序筛选，忽略不在可用列表里的镜像', () => {
    const images = [img(1, 'PyTorch'), img(2, 'Ubuntu'), img(3, 'LLM Serving')]
    expect(compatibleImages(app(), images).map((i) => i.id)).toEqual([3, 1])
    expect(compatibleImages(app({ images: [{ id: 9, name: 'X' }] }), images)).toEqual([])
  })
})

describe('envDefaults / stripEmpty', () => {
  it('default 填充；bool 无默认为 false；其余无默认为空串', () => {
    expect(envDefaults(vllmEnv)).toEqual({ MODEL_ID: 'Qwen/Qwen3-8B', MAX_MODEL_LEN: '32768', GPU_MEMORY_UTILIZATION: '0.7', API_KEY: '', DEBUG: 'false' })
    expect(envDefaults([{ name: 'B', type: 'bool', default: 'true' }])).toEqual({ B: 'true' })
  })
  it('去掉空串，保留其余', () => {
    expect(stripEmpty({ A: '', B: 'x', C: 'false' })).toEqual({ B: 'x', C: 'false' })
  })
})

describe('validateEnvValue（与 Go checkValue / ValidateEnv 一致）', () => {
  const s = (over: Partial<AppEnvSpec>): AppEnvSpec => ({ name: 'X', label: '变量', type: 'string', ...over })
  it.each<[string, AppEnvSpec, string, string | null]>([
    ['必填空', s({ required: true }), '', '请填写 变量'],
    ['必填空但有默认值', s({ required: true, default: 'a' }), '', null],
    ['非必填空', s({}), '', null],
    ['label 缺省用 name', { name: 'TOKEN', type: 'secret', required: true }, '', '请填写 TOKEN'],
    ['int 合法', s({ type: 'int', min: 1024, max: 131072 }), '32768', null],
    ['int 带符号', s({ type: 'int', min: -5 }), '-5', null],
    ['int 下界外', s({ type: 'int', min: 1024, max: 131072 }), '1023', '超出允许范围'],
    ['int 上界外', s({ type: 'int', min: 1024, max: 131072 }), '131073', '超出允许范围'],
    ['int 非整数', s({ type: 'int' }), '1.5', '必须是整数'],
    ['int 超出 int64', s({ type: 'int' }), '9223372036854775808', '必须是整数'],
    ['bool', s({ type: 'bool' }), 'yes', '只能是 true 或 false'],
    ['enum 不在选项', s({ type: 'enum', options: ['a', 'b'] }), 'c', '不在可选项内'],
    ['enum 合法', s({ type: 'enum', options: ['a', 'b'] }), 'b', null],
    ['pattern 全匹配', s({ pattern: '0\\.[3-9]' }), '0.7', null],
    ['pattern 不是前缀匹配', s({ pattern: '0\\.[3-9]' }), '0.75', '格式不正确'],
    ['非法 pattern 交给服务器', s({ pattern: '(?P<x>a)' }), 'zzz', null],
    // RE2 专有语法：u 模式下编译失败 → 跳过前端校验（不带 u 会编译成别的含义而误拦合法值）
    ['RE2 \\z 交给服务器', s({ pattern: '[a-z]+\\z' }), 'abc', null],
    ['RE2 \\A 交给服务器', s({ pattern: '\\A[a-z]+' }), 'abc', null],
    ['RE2 [[:alpha:]] 交给服务器', s({ pattern: '[[:alpha:]]+' }), 'abc', null],
    ['RE2 \\pL 交给服务器', s({ pattern: '\\pL+' }), 'abc', null],
    ['secret 少于 8', s({ type: 'secret' }), 'abc1234', '至少 8 个字符'],
    ['secret 8 位', s({ type: 'secret' }), 'abc12345', null],
    ['secret pattern', s({ type: 'secret', pattern: '^[A-Za-z0-9]{16,64}$' }), 'abcdefgh', '格式不正确'],
    ['max_len 默认 256', s({}), 'a'.repeat(257), '最多 256 字节'],
    ['max_len 按字节', s({ max_len: 6 }), '模型', null],
    ['max_len 按字节超出（6 个字符 7 字节）', s({ max_len: 6 }), '模型A', '最多 6 字节'],
    ['换行', s({}), 'a\nb', '不能包含换行或 NUL'],
  ])('%s', (_name, e, v, want) => {
    expect(validateEnvValue(e, v)).toBe(want)
  })
})

describe('generateSecret / canGenerate', () => {
  it('32 位字母数字', () => {
    const e: AppEnvSpec = { name: 'T', type: 'secret', pattern: '^[A-Za-z0-9]{16,64}$' }
    expect(canGenerate(e)).toBe(true)
    const a = generateSecret(e)
    expect(a).toMatch(/^[A-Za-z0-9]{32}$/)
    expect(generateSecret(e)).not.toBe(a)
    expect(generateSecret({ name: 'T', type: 'secret' })).toMatch(/^[A-Za-z0-9]{32}$/)
  })
  it('pattern 不允许 32 位字母数字时不生成', () => {
    for (const e of [
      { name: 'T', type: 'secret', pattern: '^[a-z]{8,64}$' },
      { name: 'T', type: 'secret', pattern: '^[A-Za-z0-9]{8,16}$' },
      { name: 'T', type: 'secret', max_len: 16 },
      { name: 'T', type: 'string' },
    ] as AppEnvSpec[]) {
      expect(canGenerate(e)).toBe(false)
      expect(generateSecret(e)).toBe('')
    }
  })
})

describe('选项与确认文案', () => {
  it('名称 · 默认模型；没有 enum 时只有名称', () => {
    expect(appOptionLabel(app())).toBe('vLLM 推理服务 · Qwen/Qwen3-8B')
    const jl = app({ name: 'JupyterLab', version: { ...app().version, env: [{ name: 'JUPYTER_TOKEN', type: 'secret', required: true }] } })
    expect(appOptionLabel(jl)).toBe('JupyterLab')
    expect(appConfirmText(jl, { JUPYTER_TOKEN: 'secretsecret' })).toBe('JupyterLab')
  })
  it('确认页按当前选择的模型', () => {
    expect(appConfirmText(app(), { MODEL_ID: 'Qwen/Qwen3-4B' })).toBe('vLLM 推理服务（Qwen/Qwen3-4B）')
    expect(appConfirmText(app(), {})).toBe('vLLM 推理服务（Qwen/Qwen3-8B）')
  })
})

describe('deploymentStatusText', () => {
  const d = (over: Partial<Deployment>): Deployment => ({
    id: 'dep-1', instance_id: 'ins-1', app_id: 1, app_slug: 'vllm', app_name: 'vLLM', app_icon: 'llm', version_id: 10, version: '1.0.0',
    latest_version_id: 10, latest_version: '1.0.0', update_available: false, state: 'running', desired: 'running', phase: '', progress: 0, message: '',
    health: 'ok', error_code: '', env: {}, secret_env: {}, spec: { ports: [] }, port: 8000, url: null, links: [], attempt: 1, cleanup_pending: false,
    started_at: null, finished_at: null, created_at: '2026-10-20T00:00:00Z', updated_at: '2026-10-20T00:00:00Z', ...over,
  })
  it('部署中：进度 + 阶段中文', () => {
    expect(deploymentStatusText(d({ state: 'installing', progress: 45, phase: 'download' }), 'running')).toEqual({ text: '部署中 45% · 下载模型', type: 'primary' })
    expect(deploymentStatusText(d({ state: 'installing', progress: 0, phase: '' }), 'running')).toEqual({ text: '部署中 0%', type: 'primary' })
  })
  it('实例停机：所有行提示续费后恢复', () => {
    expect(deploymentStatusText(d({ state: 'installing', progress: 45 }), 'stopped')).toEqual({ text: '实例已停机，续费后自动恢复', type: 'info' })
    expect(deploymentStatusText(d({ state: 'running' }), 'stopped')).toEqual({ text: '实例已停机，续费后自动恢复', type: 'info' })
  })
  it('预订 / 开通中：开通后自动部署', () => {
    expect(deploymentStatusText(d({ state: 'queued' }), 'reserved')).toEqual({ text: '将在开通后自动部署', type: 'info' })
    expect(deploymentStatusText(d({ state: 'queued' }), 'creating')).toEqual({ text: '将在开通后自动部署', type: 'info' })
  })
  it('失败：错误码中文', () => {
    expect(deploymentStatusText(d({ state: 'failed', error_code: 'timeout' }), 'running')).toEqual({ text: '失败：部署超时', type: 'danger' })
    expect(deploymentStatusText(d({ state: 'failed', error_code: '' }), 'running')).toEqual({ text: '失败', type: 'danger' })
  })
  it('其余按状态', () => {
    expect(deploymentStatusText(d({ state: 'running' }), 'running')).toEqual({ text: '运行中', type: 'success' })
    expect(deploymentStatusText(d({ state: 'queued' }), 'running')).toEqual({ text: '等待部署', type: 'info' })
  })
})
