import type { AppSpec } from '@spark/shared'
import { describe, expect, it } from 'vitest'
import {
  coverageLow, coverageText, emptyEnvRow, emptySpec, fieldPathToRow, formIssues, formToSpec, scriptErrorLine, specToForm, splitFieldError,
} from '../appSpec'

// 后端规范化之后的 spec（label 补齐、max_len 补 256、timeout 补 60）：往返必须原样不变，否则已发布版本的整版回传会被判为改动。
const normalized: AppSpec = {
  ports: [
    { port: 8000, label: 'OpenAI 兼容接口', path: '/v1', health_path: '/health' },
    { port: 8001, label: '管理' },
  ],
  timeout_minutes: 60,
  min_data_free_gib: 40,
  env: [
    { name: 'MODEL_ID', label: '模型', type: 'enum', required: true, options: ['Qwen/Qwen3-8B', 'Qwen/Qwen3-4B'], default: 'Qwen/Qwen3-8B', max_len: 256, help: 'ModelScope 模型 ID' },
    { name: 'MAX_MODEL_LEN', label: '最大上下文', type: 'int', min: 1024, max: 131072, default: '32768', max_len: 256 },
    { name: 'OFFSET', label: 'OFFSET', type: 'int', min: -5, max_len: 256 },
    { name: 'API_KEY', label: '接口密钥', type: 'secret', pattern: '^[A-Za-z0-9_-]{8,128}$', max_len: 128, help: '留空则不鉴权' },
    { name: 'DEBUG', label: '调试', type: 'bool', default: 'false', max_len: 256 },
  ],
  links: [{ label: '接口文档', path: '/docs' }],
}

describe('specToForm / formToSpec', () => {
  it('规范化的 spec 往返不变', () => {
    expect(formToSpec(specToForm(normalized))).toEqual(normalized)
  })

  it('emptySpec 往返不变，且有一个主端口与默认超时', () => {
    const s = emptySpec()
    expect(s.ports).toHaveLength(1)
    expect(s.timeout_minutes).toBe(60)
    expect(formToSpec(specToForm(s))).toEqual(s)
  })

  it('表格行：int 的 min / max 与 enum 的 options（一行一个）', () => {
    const f = specToForm(normalized)
    expect(f.env[0].options).toBe('Qwen/Qwen3-8B\nQwen/Qwen3-4B')
    expect(f.env[1]).toMatchObject({ min: '1024', max: '131072', default: '32768' })
    expect(f.env[2]).toMatchObject({ min: '-5', max: '' })
    expect(f.env[3]).toMatchObject({ pattern: '^[A-Za-z0-9_-]{8,128}$', max_len: '128', required: false })
    expect(f.ports[1].health_path).toBe('')
    expect(f.links).toEqual([{ label: '接口文档', path: '/docs' }])
  })

  it('options 去掉空行与首尾空白；min / max 去空白后转整数，空串省略', () => {
    const f = specToForm(emptySpec())
    f.env.push({ ...emptyEnvRow(), name: ' SIZE ', type: 'enum', options: ' a \n\n b\r\n  ', default: 'a' })
    f.env.push({ ...emptyEnvRow(), name: 'N', type: 'int', min: ' 3 ', max: '' })
    const s = formToSpec(f)
    expect(s.env![0]).toEqual({ name: 'SIZE', type: 'enum', options: ['a', 'b'], default: 'a' })
    expect(s.env![1]).toEqual({ name: 'N', type: 'int', min: 3 })
  })

  it('与类型无关的列不发送：非 enum 无 options、非 int 无 min / max、secret 无默认值、int 无 pattern（max_len 后端对所有类型都补，原样保留）', () => {
    const f = specToForm(emptySpec())
    f.env.push({ ...emptyEnvRow(), name: 'S', type: 'secret', options: 'x', min: '1', max: '2', default: 'leak', pattern: '^.{8,}$' })
    f.env.push({ ...emptyEnvRow(), name: 'I', type: 'int', options: 'x', pattern: '^1$', max_len: '9', min: '1' })
    const [s, i] = formToSpec(f).env!
    expect(s).toEqual({ name: 'S', type: 'secret', pattern: '^.{8,}$' })
    expect(i).toEqual({ name: 'I', type: 'int', min: 1, max_len: 9 })
  })

  it('只有主端口带 health_path；空字符串字段省略；端口未填为 0（交给后端报 ports[i].port）', () => {
    const f = specToForm(emptySpec())
    f.ports.push({ port: undefined, label: ' ', path: '', health_path: '/h' })
    f.timeout_minutes = undefined
    f.min_data_free_gib = undefined
    const s = formToSpec(f)
    expect(s.ports[1]).toEqual({ port: 0 })
    expect(s.timeout_minutes).toBeUndefined()
    expect(s.min_data_free_gib).toBeUndefined()
  })
})

describe('formIssues', () => {
  it('min / max / max_len 不是整数时按后端同款路径报错', () => {
    const f = specToForm(emptySpec())
    f.env.push({ ...emptyEnvRow(), name: 'A', type: 'int', min: '1.5', max: 'abc' })
    f.env.push({ ...emptyEnvRow(), name: 'B', type: 'string', max_len: '12x' })
    f.env.push({ ...emptyEnvRow(), name: 'C', type: 'enum', min: 'zzz' }) // 与类型无关的列不检查
    expect(formIssues(f)).toEqual([
      { path: 'env[0].min', reason: '需为整数' },
      { path: 'env[0].max', reason: '需为整数' },
      { path: 'env[1].max_len', reason: '需为整数' },
    ])
  })

  it('合法表单没有问题', () => {
    expect(formIssues(specToForm(normalized))).toEqual([])
  })
})

describe('fieldPathToRow', () => {
  it.each([
    ['env[2].default', { table: 'env', index: 2, field: 'default' }],
    ['ports[0].port', { table: 'ports', index: 0, field: 'port' }],
    ['ports[1].health_path', { table: 'ports', index: 1, field: 'health_path' }],
    ['links[3].label', { table: 'links', index: 3, field: 'label' }],
    ['env[10]', { table: 'env', index: 10, field: '' }],
  ] as const)('%s', (path, want) => {
    expect(fieldPathToRow(path)).toEqual(want)
  })

  it.each(['env', 'ports', 'timeout_minutes', 'script', 'image_ids', 'version', 'foo[1].x', 'env[x].name', ''])('%s → null', (path) => {
    expect(fieldPathToRow(path)).toBeNull()
  })
})

describe('splitFieldError / scriptErrorLine', () => {
  it('拆出字段路径与原因（全角冒号）', () => {
    expect(splitFieldError('env[2].default：默认值不符合自身规则：不在选项里')).toEqual({ path: 'env[2].default', reason: '默认值不符合自身规则：不在选项里' })
    expect(splitFieldError('script：第 12 行: reached EOF without matching if with fi')).toEqual({ path: 'script', reason: '第 12 行: reached EOF without matching if with fi' })
    expect(splitFieldError('timeout_minutes：需在 1～180 之间')).toEqual({ path: 'timeout_minutes', reason: '需在 1～180 之间' })
    expect(splitFieldError('已发布的版本不能修改')).toBeNull()
    expect(splitFieldError('选项 "a：b" 不合法')).toBeNull()
  })

  it('脚本错误行号', () => {
    expect(scriptErrorLine('第 12 行: reached EOF')).toBe(12)
    expect(scriptErrorLine('第 1 行: x')).toBe(1)
    expect(scriptErrorLine('脚本不能为空')).toBeNull()
  })
})

describe('覆盖率', () => {
  it('文案与偏低判断', () => {
    const c = { image_id: 1, image_name: 'vllm', cached: 3, online: 5, unknown: 1 }
    expect(coverageText(c)).toBe('已缓存 3 / 在线 5（未知 1）')
    expect(coverageLow(c)).toBe(true)
    expect(coverageLow({ ...c, cached: 5 })).toBe(false)
    expect(coverageLow({ ...c, cached: 0, online: 0 })).toBe(false)
  })
})
