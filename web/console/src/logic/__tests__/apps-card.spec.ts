import type { AppCatalogItem, Deployment, DeploymentEvent, UserEvent } from '@spark/shared'
import { describe, expect, it } from 'vitest'
import {
  addPickReason, appOpBlockReason, canAddApp, deploymentActions, deploymentBusy, instanceAppHint, mergeTimeline, redeployForm, summaryTag,
} from '../apps'

const dep = (over: Partial<Deployment> = {}): Deployment => ({
  id: 'dep-1', instance_id: 'ins-1', app_id: 1, app_slug: 'vllm', app_name: 'vLLM', app_icon: 'llm', version_id: 10, version: '1.0.0',
  latest_version_id: 10, latest_version: '1.0.0', update_available: false, state: 'running', desired: 'running', phase: '', progress: 0, message: '',
  health: 'ok', error_code: '', env: {}, secret_env: {}, spec: { ports: [] }, port: 8000, url: null, links: [], attempt: 1, cleanup_pending: false,
  started_at: null, finished_at: null, created_at: '2026-10-20T00:00:00Z', updated_at: '2026-10-20T00:00:00Z', ...over,
})

describe('应用卡片：实例状态', () => {
  it('非运行实例的每行提示', () => {
    expect(instanceAppHint('stopped')).toBe('实例已停机，续费后自动恢复')
    expect(instanceAppHint('reserved')).toBe('将在开通后自动部署')
    expect(instanceAppHint('creating')).toBe('将在开通后自动部署')
    expect(instanceAppHint('running')).toBeNull()
  })
  it('重新部署 / 停止 / 启动只在实例运行中可用，并给出原因', () => {
    expect(appOpBlockReason('running')).toBeNull()
    expect(appOpBlockReason('stopped')).toBe('实例已停机，续费后可操作')
    expect(appOpBlockReason('reserved')).toBe('实例开通后可操作')
    expect(appOpBlockReason('wiping')).toBe('实例未运行')
  })
  it('添加应用：预订 / 开通中 / 运行中 / 停机可添加', () => {
    for (const s of ['reserved', 'creating', 'running', 'stopped'] as const) expect(canAddApp(s)).toBe(true)
    for (const s of ['wiping', 'released', 'failed', 'cancelled'] as const) expect(canAddApp(s)).toBe(false)
  })
})

describe('deploymentActions（规格 §6.2）', () => {
  const a = (state: Deployment['state'], desired: Deployment['desired'] = 'running') => deploymentActions({ state, desired })
  it('按状态显示操作', () => {
    expect(a('running')).toEqual({ redeploy: true, stop: true, start: false, remove: true })
    expect(a('installing')).toEqual({ redeploy: false, stop: true, start: false, remove: true })
    expect(a('stopped', 'stopped')).toEqual({ redeploy: true, stop: false, start: true, remove: true })
    expect(a('failed')).toEqual({ redeploy: true, stop: false, start: false, remove: true })
    expect(a('queued')).toEqual({ redeploy: false, stop: false, start: false, remove: true })
    expect(a('removing', 'removed')).toEqual({ redeploy: false, stop: false, start: false, remove: false })
  })
  it('已请求停止的不再显示停止', () => {
    expect(a('running', 'stopped').stop).toBe(false)
  })
  it('deploymentBusy：排队 / 部署中 / 卸载中 / 停止中', () => {
    expect(deploymentBusy({ state: 'queued', desired: 'running' })).toBe(true)
    expect(deploymentBusy({ state: 'installing', desired: 'running' })).toBe(true)
    expect(deploymentBusy({ state: 'removing', desired: 'removed' })).toBe(true)
    expect(deploymentBusy({ state: 'running', desired: 'stopped' })).toBe(true)
    expect(deploymentBusy({ state: 'running', desired: 'running' })).toBe(false)
    expect(deploymentBusy({ state: 'failed', desired: 'running' })).toBe(false)
  })
})

describe('addPickReason', () => {
  const app = { id: 2, images: [{ id: 3, name: 'LLM Serving' }, { id: 1, name: 'PyTorch' }] } as AppCatalogItem
  it('已部署优先，其次镜像不兼容', () => {
    expect(addPickReason(app, 3, [{ app_id: 2 }])).toBe('已部署')
    expect(addPickReason(app, 9, [])).toBe('需要镜像：LLM Serving、PyTorch')
    expect(addPickReason(app, 1, [{ app_id: 5 }])).toBeNull()
  })
})

describe('summaryTag', () => {
  const s = (state: Deployment['state'], progress = 0) => summaryTag({ id: 'd', name: 'vLLM', state, progress, url: null })
  it('部署中带百分比（蓝），运行中绿，失败红，已停止灰', () => {
    expect(s('installing', 45)).toEqual({ text: '部署中 45%', type: 'primary' })
    expect(s('running')).toEqual({ text: '运行中', type: 'success' })
    expect(s('failed')).toEqual({ text: '失败', type: 'danger' })
    expect(s('stopped')).toEqual({ text: '已停止', type: 'info' })
    expect(s('queued')).toEqual({ text: '等待部署', type: 'info' })
  })
})

describe('redeployForm', () => {
  const spec = [
    { name: 'MODEL_ID', type: 'enum' as const, options: ['a', 'b'], default: 'a' },
    { name: 'MAX', type: 'int' as const, default: '32768' },
    { name: 'API_KEY', type: 'secret' as const },
    { name: 'TOKEN', type: 'secret' as const },
  ]
  it('普通变量预填当前值（缺省取默认），secret 从不回显；已设置的标记 secretSet', () => {
    const f = redeployForm(dep({ env: { MODEL_ID: 'b', OLD: 'x' }, secret_env: { API_KEY: true, GONE: true } }), spec)
    expect(f.values).toEqual({ MODEL_ID: 'b', MAX: '32768', API_KEY: '', TOKEN: '' })
    expect(f.secretSet).toEqual({ API_KEY: true })
  })
})

describe('mergeTimeline', () => {
  it('实例事件与部署事件按时间合并（新到旧），部署事件带应用名、kind = app', () => {
    const inst: UserEvent[] = [
      { to_state: 'running', reason_kind: 'renewed', at: '2026-10-20T03:00:00Z', detail: '续费' },
      { to_state: 'running', reason_kind: 'created', at: '2026-10-20T01:00:00Z', detail: '开通' },
    ]
    const ev = (id: number, at: string, detail: string): DeploymentEvent => ({ id, at, from_state: '', to_state: 'queued', detail })
    const out = mergeTimeline(inst, [{ name: 'vLLM', events: [ev(1, '2026-10-20T00:30:00Z', '创建部署'), ev(2, '2026-10-20T02:00:00Z', '运行中')] }])
    expect(out).toEqual([
      { at: '2026-10-20T03:00:00Z', kind: 'renewed', text: '续费' },
      { at: '2026-10-20T02:00:00Z', kind: 'app', text: 'vLLM：运行中' },
      { at: '2026-10-20T01:00:00Z', kind: 'created', text: '开通' },
      { at: '2026-10-20T00:30:00Z', kind: 'app', text: 'vLLM：创建部署' },
    ])
  })
})
