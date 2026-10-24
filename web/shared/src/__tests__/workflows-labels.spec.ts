import { describe, expect, it } from 'vitest'
import {
  auditActions, formatGB, labelOf, runStateLabel, workflowCategoryLabel, workflowErrorLabel, workflowStateLabel, workflowStateType,
} from '../labels'
import type { QuickRunState, WorkflowCategory, WorkflowInstallState } from '../types'

const categories: WorkflowCategory[] = ['txt2img', 'img2img', 'video', 'upscale', 'other']
const states: WorkflowInstallState[] = ['pending', 'installed', 'failed']
const runStates: QuickRunState[] = ['queued', 'running', 'done', 'error']
const errorCodes = [
  'workflow_not_found', 'workflow_unavailable', 'workflow_exists', 'workflow_limit', 'workflow_app_unsupported', 'workflow_version_invalid',
  'workflow_version_frozen', 'workflow_busy', 'workflow_prompt_rejected', 'workflow_input_invalid', 'upstream_unreachable',
]

describe('工作流市场文案', () => {
  it('5 个分类都有中文', () => {
    expect(Object.keys(workflowCategoryLabel).sort()).toEqual([...categories].sort())
    expect(workflowCategoryLabel.video).toBe('视频')
    expect(workflowCategoryLabel.txt2img).toBe('文生图')
    expect(workflowCategoryLabel.img2img).toBe('图生图')
    expect(workflowCategoryLabel.upscale).toBe('放大修复')
    expect(workflowCategoryLabel.other).toBe('其它')
  })
  it('安装状态：待安装 / 已安装 / 安装失败，各有颜色', () => {
    expect(Object.keys(workflowStateLabel).sort()).toEqual([...states].sort())
    expect(workflowStateLabel).toEqual({ pending: '待安装', installed: '已安装', failed: '安装失败' })
    for (const s of states) expect(workflowStateType[s]).toBeTruthy()
    expect(labelOf(workflowStateType, 'failed')).toBe('danger')
    expect(labelOf(workflowStateType, 'installed')).toBe('success')
  })
  it('快速出图运行状态', () => {
    expect(Object.keys(runStateLabel).sort()).toEqual([...runStates].sort())
    expect(runStateLabel).toEqual({ queued: '排队中', running: '生成中', done: '完成', error: '失败' })
  })
  it('错误码都有中文', () => {
    expect(Object.keys(workflowErrorLabel).sort()).toEqual([...errorCodes].sort())
    for (const c of errorCodes) expect(workflowErrorLabel[c]).toBeTruthy()
    expect(labelOf(workflowErrorLabel, 'workflow_limit')).toBe('每个 ComfyUI 最多安装 8 个工作流')
  })
  it('下载量格式化', () => {
    expect(formatGB(0.5)).toBe('0.5 GB')
    expect(formatGB(17)).toBe('17 GB')
    expect(formatGB(6.9)).toBe('6.9 GB')
    expect(formatGB('6.90')).toBe('6.9 GB')
    expect(formatGB(6.97)).toBe('6.97 GB')
    expect(formatGB(30.12)).toBe('30.1 GB')
    expect(formatGB(0)).toBe('0 GB')
    expect(formatGB(Number.NaN)).toBe('0 GB')
    expect(formatGB(-3)).toBe('0 GB')
  })
  it('审计动作新增 9 项', () => {
    for (const a of [
      'workflow.create', 'workflow.update', 'workflow.delete', 'workflow.version.create', 'workflow.version.update', 'workflow.version.publish',
      'instance.workflow_add', 'instance.workflow_update', 'instance.workflow_remove',
    ]) expect(auditActions).toContain(a)
  })
})
