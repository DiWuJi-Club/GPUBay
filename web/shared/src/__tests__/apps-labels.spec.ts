import { describe, expect, it } from 'vitest'
import {
  alertKindLabel, appCategoryLabel, appStatusLabel, auditActions, deploymentErrorLabel, deploymentPhaseLabel, deploymentStateLabel,
  deploymentStateType, labelOf,
} from '../labels'
import type { AppCategory, DeploymentState } from '../types'

const states: DeploymentState[] = ['queued', 'installing', 'running', 'stopped', 'failed', 'removing', 'removed', 'lost']
const errorCodes = ['script_failed', 'timeout', 'health_failed', 'upload_failed', 'unreachable', 'port_conflict', 'port_limit', 'instance_gone', 'crashed', 'disk']
const phases = ['prepare', 'download', 'install', 'start', 'health']
const categories: AppCategory[] = ['llm', 'image', 'notebook', 'tool']

describe('应用部署文案', () => {
  it('8 个部署状态都有中文与颜色', () => {
    expect(Object.keys(deploymentStateLabel).sort()).toEqual([...states].sort())
    for (const s of states) {
      expect(deploymentStateLabel[s]).toBeTruthy()
      expect(deploymentStateType[s]).toBeTruthy()
    }
    expect(labelOf(deploymentStateLabel, 'installing')).toBe('部署中')
    expect(labelOf(deploymentStateType, 'failed')).toBe('danger')
  })
  it('10 个错误码都有中文', () => {
    expect(Object.keys(deploymentErrorLabel).sort()).toEqual([...errorCodes].sort())
    for (const c of errorCodes) expect(deploymentErrorLabel[c]).toBeTruthy()
  })
  it('5 个阶段都有中文', () => {
    expect(Object.keys(deploymentPhaseLabel).sort()).toEqual([...phases].sort())
    for (const p of phases) expect(deploymentPhaseLabel[p]).toBeTruthy()
  })
  it('分类、上架状态、告警、审计动作', () => {
    for (const c of categories) expect(appCategoryLabel[c]).toBeTruthy()
    expect(appStatusLabel).toEqual({ draft: '草稿', published: '已发布', retired: '已退役' })
    expect(labelOf(alertKindLabel, 'app_deploy_unreachable')).toBe('应用部署无法连接容器')
    for (const a of [
      'app.create', 'app.update', 'app.delete', 'app.version.create', 'app.version.update', 'app.version.publish', 'app.version.prepull',
      'app.deployment.force_remove', 'instance.app_deploy', 'instance.app_redeploy', 'instance.app_stop', 'instance.app_start', 'instance.app_remove',
    ]) expect(auditActions).toContain(a)
  })
})
