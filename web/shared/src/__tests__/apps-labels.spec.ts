import { describe, expect, it } from 'vitest'
import {
  alertKindLabel, appCategoryLabel, appStatusLabel, auditActions, deploymentErrorLabel, deploymentErrorText, deploymentPhaseLabel, deploymentStateLabel,
  deploymentStateType, labelOf,
} from '../labels'
import type { AppCategory, DeploymentState } from '../types'

const states: DeploymentState[] = ['queued', 'installing', 'running', 'stopped', 'failed', 'removing', 'removed', 'lost']
const errorCodes = ['script_failed', 'timeout', 'health_failed', 'upload_failed', 'unreachable', 'port_conflict', 'port_limit', 'instance_gone', 'crashed', 'disk', 'deploy_busy']
const phases = ['prepare', 'download', 'install', 'start', 'health', 'busy']
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
  it('11 个错误码都有中文', () => {
    expect(Object.keys(deploymentErrorLabel).sort()).toEqual([...errorCodes].sort())
    for (const c of errorCodes) expect(deploymentErrorLabel[c]).toBeTruthy()
    expect(labelOf(deploymentErrorLabel, 'deploy_busy')).toBe('上一次部署长时间未结束')
  })
  it('失败原因：message 与错误码中文相同时不重复（验收观察 O-7）', () => {
    expect(deploymentErrorText('crashed', '服务进程已退出')).toBe('服务进程已退出')
    expect(deploymentErrorText('health_failed', '服务进程已退出')).toBe('健康检查未通过：服务进程已退出')
    expect(deploymentErrorText('script_failed', '')).toBe('脚本执行失败')
    expect(deploymentErrorText('', 'boom', '部署失败')).toBe('部署失败：boom')
    expect(deploymentErrorText('', '', '部署失败')).toBe('部署失败')
    expect(deploymentErrorText('', 'boom')).toBe('boom')
    expect(deploymentErrorText('new_code', 'x')).toBe('new_code：x')
  })
  it('6 个阶段都有中文', () => {
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
