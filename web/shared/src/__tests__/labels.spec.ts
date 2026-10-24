import { describe, expect, it } from 'vitest'
import {
  alertKindLabel, auditActions, commandStatusLabel, instanceStateLabel, instanceStateType, labelOf, ledgerKindLabel, llmFeatureLabel, llmKeyStatusLabel, llmModelStatusLabel,
  llmNodeStateLabel, llmNodeStateType, llmUsageStatusLabel,
} from '../labels'
import type { LLMNodeState, LLMUsageStatus } from '../types'

describe('labels', () => {
  it('实例状态中文与颜色', () => {
    expect(labelOf(instanceStateLabel, 'creating')).toBe('开通中')
    expect(labelOf(instanceStateLabel, 'stopped')).toBe('已到期停机')
    expect(labelOf(instanceStateType, 'running')).toBe('success')
    expect(labelOf(instanceStateType, 'failed')).toBe('danger')
    expect(labelOf(instanceStateLabel, 'reserved')).toBe('已预订')
    expect(labelOf(instanceStateLabel, 'cancelled')).toBe('已取消')
    expect(labelOf(instanceStateType, 'reserved')).toBe('primary')
    expect(labelOf(alertKindLabel, 'booking_at_risk')).toBe('预订机器不可用')
  })
  it('指令状态中文', () => {
    expect(commandStatusLabel).toEqual({ pending: '待执行', sent: '已下发', done: '已完成', failed: '失败' })
  })
  it('未知值原样显示', () => {
    expect(labelOf(ledgerKindLabel, 'mystery')).toBe('mystery')
    expect(labelOf(alertKindLabel, 'new_kind')).toBe('new_kind')
    expect(labelOf(alertKindLabel, 'container_down')).toBe('容器未运行')
  })
  it('审计动作列表与后端写入值一致', () => {
    expect(auditActions).toHaveLength(72)
    expect(new Set(auditActions).size).toBe(auditActions.length)
    expect(auditActions).toContain('instance.port_force_private')
    expect(auditActions).toContain('ssh_key.delete')
    expect(auditActions).toContain('instance.refund_release')
    expect(auditActions).toContain('wallet.manual_recharge')
    expect(auditActions).toContain('booking.cancel')
  })
  it('Token 商城文案', () => {
    expect(labelOf(ledgerKindLabel, 'llm_pay')).toBe('Token 消费')
    expect(llmModelStatusLabel).toEqual({ draft: '草稿', online: '已上线', offline: '已下线' })
    const states: LLMNodeState[] = ['provisioning', 'ready', 'unhealthy', 'draining', 'removing', 'removed', 'failed']
    expect(Object.keys(llmNodeStateLabel).sort()).toEqual([...states].sort())
    for (const st of states) expect(llmNodeStateType[st]).toBeTruthy()
    expect(labelOf(llmNodeStateType, 'ready')).toBe('success')
    expect(labelOf(llmNodeStateType, 'failed')).toBe('danger')
    const usage: LLMUsageStatus[] = ['ok', 'client_abort', 'upstream_error', 'upstream_timeout', 'no_node']
    expect(usage.map((u) => llmUsageStatusLabel[u])).toEqual(['成功', '客户端中断', '上游错误', '上游超时', '无可用节点'])
    expect(Object.keys(llmKeyStatusLabel).sort()).toEqual(['active', 'disabled', 'revoked'])
    for (const k of ['llm_pool_short', 'llm_node_unhealthy', 'llm_node_failed', 'llm_usage_record_failed']) {
      expect(labelOf(alertKindLabel, k)).not.toBe(k)
    }
    for (const a of [
      'llm.model.create', 'llm.model.update', 'llm.model.status', 'llm.model.delete', 'llm.node.drain', 'llm.node.rebuild', 'llm.node.remove',
      'llm.account.update', 'llm.key.create', 'llm.key.update', 'llm.key.revoke', 'llm.key.admin_disable',
    ]) expect(auditActions).toContain(a)
  })
})

describe('llmFeatureLabel', () => {
  it('已知 feature 中文，未知原样', () => {
    expect(labelOf(llmFeatureLabel, 'tools')).toBe('工具调用')
    expect(labelOf(llmFeatureLabel, 'reasoning')).toBe('深度思考')
    expect(labelOf(llmFeatureLabel, 'vision')).toBe('视觉')
    expect(labelOf(llmFeatureLabel, 'long-context')).toBe('长上下文')
    expect(labelOf(llmFeatureLabel, 'json-mode')).toBe('json-mode')
  })
})
