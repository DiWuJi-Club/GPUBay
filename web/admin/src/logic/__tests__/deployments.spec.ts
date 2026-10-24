import { deploymentErrorLabel, type AppSpec, type DeploymentState, type Role } from '@spark/shared'
import { readdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { describe, expect, it } from 'vitest'
import { envRows, filterFromQuery, FORCE_REMOVE_REASON_MAX, onlyDepChanged, progressText, runeCount, showForceRemove, validateForceRemoveReason } from '../deployments'

describe('强制卸载原因', () => {
  it('必填（去首尾空白）', () => {
    expect(validateForceRemoveReason('')).toBe('请填写原因')
    expect(validateForceRemoveReason('  \n ')).toBe('请填写原因')
    expect(validateForceRemoveReason(' 用户投诉占用 ')).toBeNull()
  })
  it('最多 200 个字符：按字符计，不按 UTF-16 单元；首尾空白不计', () => {
    expect(FORCE_REMOVE_REASON_MAX).toBe(200)
    expect(validateForceRemoveReason('长'.repeat(200))).toBeNull()
    expect(validateForceRemoveReason('长'.repeat(201))).toBe('原因最多 200 个字符')
    expect(validateForceRemoveReason(`  ${'a'.repeat(200)}  `)).toBeNull()
    // emoji 是 1 个字符、2 个 UTF-16 单元：200 个仍然合法（与后端 utf8.RuneCountInString 一致）
    expect('😀'.repeat(200).length).toBe(400)
    expect(runeCount('😀'.repeat(200))).toBe(200)
    expect(validateForceRemoveReason('😀'.repeat(200))).toBeNull()
    expect(validateForceRemoveReason('😀'.repeat(201))).toBe('原因最多 200 个字符')
  })
})

describe('强制卸载按钮', () => {
  const live: DeploymentState[] = ['queued', 'installing', 'running', 'stopped', 'failed']
  const notLive: DeploymentState[] = ['removing', 'removed', 'lost']
  it.each(['super', 'ops'] as Role[])('%s：非终态且不在卸载中才显示', (r) => {
    for (const s of live) expect(showForceRemove(r, s), s).toBe(true)
    for (const s of notLive) expect(showForceRemove(r, s), s).toBe(false)
  })
  it('财务只读', () => {
    for (const s of [...live, ...notLive]) expect(showForceRemove('finance', s), s).toBe(false)
  })
})

describe('进度文案', () => {
  it('部署中：阶段 + 百分比；排队等待上一次部署：阶段；其余为空', () => {
    expect(progressText({ state: 'installing', phase: 'download', progress: 45 })).toBe('下载模型 45%')
    expect(progressText({ state: 'installing', phase: '', progress: 0 })).toBe('0%')
    expect(progressText({ state: 'queued', phase: '', progress: 0 })).toBe('')
    expect(progressText({ state: 'running', phase: 'health', progress: 100 })).toBe('')
    expect(progressText({ state: 'failed', phase: 'install', progress: 30 })).toBe('')
  })
})

describe('变量表', () => {
  const spec: AppSpec = {
    ports: [{ port: 8000 }],
    env: [
      { name: 'MODEL_ID', label: '模型', type: 'enum', options: ['a', 'b'] },
      { name: 'API_KEY', label: '接口密钥', type: 'secret' },
      { name: 'HF_TOKEN', label: 'HF 令牌', type: 'secret' },
      { name: 'NOTE', type: 'string' },
    ],
  }
  it('按 spec 顺序；secret 只显示 *** / 未设置，从不显示值；空值显示（空）', () => {
    const rows = envRows({ env: { MODEL_ID: 'a' }, secret_env: { API_KEY: true }, spec })
    expect(rows).toEqual([
      { name: 'MODEL_ID', label: '模型', value: 'a', kind: 'value' },
      { name: 'API_KEY', label: '接口密钥', value: '***', kind: 'secret_set' },
      { name: 'HF_TOKEN', label: 'HF 令牌', value: '未设置', kind: 'secret_unset' },
      { name: 'NOTE', label: '', value: '（空）', kind: 'empty' },
    ])
  })
  it('即使 env 里误带了 secret 的值也不显示', () => {
    const rows = envRows({ env: { API_KEY: 'sk-should-not-show' }, secret_env: {}, spec })
    expect(rows.find((r) => r.name === 'API_KEY')).toEqual({ name: 'API_KEY', label: '接口密钥', value: '未设置', kind: 'secret_unset' })
    expect(JSON.stringify(rows)).not.toContain('sk-should-not-show')
  })
  it('spec 里没有的变量（旧版本留下的）补在后面；secret 仍只显示 ***', () => {
    const rows = envRows({ env: { OLD: 'x' }, secret_env: { OLD_SECRET: true }, spec: { ports: [] } })
    expect(rows).toEqual([
      { name: 'OLD_SECRET', label: '', value: '***', kind: 'secret_set' },
      { name: 'OLD', label: '', value: 'x', kind: 'value' },
    ])
  })
  it('缺字段（旧数据）不报错', () => {
    expect(envRows({ env: undefined as never, secret_env: undefined as never, spec: undefined as never })).toEqual([])
  })
})

describe('错误码文案与后端一致', () => {
  // 后端所有部署错误码（internal/appdeploy 里的 ErrCode* 常量）都要有中文，含 Runner 的 deploy_busy
  const dir = join(__dirname, '../../../../../internal/appdeploy')
  const codes = readdirSync(dir)
    .filter((f) => f.endsWith('.go') && !f.endsWith('_test.go'))
    .flatMap((f) => [...readFileSync(join(dir, f), 'utf8').matchAll(/\bErrCode\w+\s*=\s*"([a-z_]+)"/g)].map((m) => m[1]))
  it('从 Go 源码读到错误码', () => {
    expect(codes).toContain('script_failed')
    expect(codes).toContain('deploy_busy')
  })
  it('每个错误码都有中文', () => {
    for (const c of codes) expect(deploymentErrorLabel[c], c).toBeTruthy()
  })
})

describe('部署列表查询串', () => {
  it('预填：未知状态忽略、ID 只留数字、实例 ID 去空白、数组取第一个', () => {
    expect(filterFromQuery({ state: 'failed', app_id: '3x', user_id: ['7', '8'], instance_id: ' ins-a ', dep: 'dep-1' }))
      .toEqual({ state: 'failed', app_id: '3', user_id: '7', instance_id: 'ins-a' })
    expect(filterFromQuery({ state: 'bogus' })).toEqual({ state: '', app_id: '', user_id: '', instance_id: '' })
  })
  it('只有 dep 变化（开 / 关抽屉）不算筛选变化', () => {
    expect(onlyDepChanged({ instance_id: 'ins-a', dep: 'dep-1' }, { instance_id: 'ins-a' })).toBe(true)
    expect(onlyDepChanged({ state: 'failed' }, { dep: 'dep-2', state: 'failed' })).toBe(true)
    expect(onlyDepChanged({}, {})).toBe(true)
  })
  it('其它参数变化都算筛选变化', () => {
    expect(onlyDepChanged({ instance_id: 'ins-a', dep: 'dep-1' }, { instance_id: 'ins-b' })).toBe(false)
    expect(onlyDepChanged({ dep: 'dep-1' }, { state: 'running' })).toBe(false)
    expect(onlyDepChanged({ state: 'running' }, {})).toBe(false)
    expect(onlyDepChanged({ user_id: '1' }, { user_id: ['1', '2'] })).toBe(false)
  })
})
