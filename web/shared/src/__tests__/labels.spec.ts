import { describe, expect, it } from 'vitest'
import {
  alertKindLabel, auditActions, bindingStatusLabel, butlerToolLabel, clusterAppStatusLabel, clusterAppStatusType, clusterNetworkLabel, clusterRoleLabel,
  clusterStatusLabel, clusterStatusType, commandStatusLabel, commandTypeLabel, fabricIsolationLabel, fabricKindLabel, instanceStateLabel, instanceStateType,
  labelOf, ledgerKindLabel, llmFeatureLabel, llmKeyStatusLabel, llmModelStatusLabel, llmNodeStateLabel, llmNodeStateType, llmUsageStatusLabel, rewardStatusLabel,
} from '../labels'
import type { ClusterAppStatus, ClusterNetwork, ClusterRole, ClusterStatus, FabricIsolation, FabricKind, LLMNodeState, LLMUsageStatus } from '../types'

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
    expect(labelOf(alertKindLabel, 'referral_scan_stuck')).toBe('邀请返利扫描卡住')
  })
  it('指令状态中文', () => {
    expect(commandStatusLabel).toEqual({ pending: '待执行', sent: '已下发', done: '已完成', failed: '失败' })
  })
  it('邀请绑定 / 返利状态中文', () => {
    expect(bindingStatusLabel).toEqual({ active: '有效', held: '审核中', revoked: '已失效' })
    expect(rewardStatusLabel).toEqual({ pending: '待发放', settled: '已发放', void: '已作废' })
  })
  it('未知值原样显示', () => {
    expect(labelOf(ledgerKindLabel, 'mystery')).toBe('mystery')
    expect(labelOf(ledgerKindLabel, 'referral_rebate')).toBe('邀请返利')
    expect(labelOf(ledgerKindLabel, 'referral_clawback')).toBe('返利扣回')
    expect(labelOf(alertKindLabel, 'new_kind')).toBe('new_kind')
    expect(labelOf(alertKindLabel, 'container_down')).toBe('容器未运行')
  })
  it('审计动作列表与后端写入值一致', () => {
    expect(auditActions).toHaveLength(105) // 计划 9 加 12 个 llm.*、计划 12 加 butler.tool_execute / butler.settings、计划 13 加 12 个 fabric.* / cluster.*
    expect(new Set(auditActions).size).toBe(auditActions.length)
    expect(auditActions).toContain('instance.port_force_private')
    expect(auditActions).toContain('ssh_key.delete')
    expect(auditActions).toContain('instance.refund_release')
    expect(auditActions).toContain('wallet.manual_recharge')
    expect(auditActions).toContain('booking.cancel')
    expect(auditActions).toContain('referral.rescan')
    expect(auditActions).toContain('referral.kyc_check_failed')
    expect(auditActions).toContain('referral.auto_hold')
    expect(auditActions).toContain('butler.tool_execute')
    expect(auditActions).toContain('butler.settings')
  })
  it('AI 管家工具中文名覆盖 23 个工具、告警种类', () => {
    const read = ['list_products', 'check_availability', 'list_apps', 'get_app', 'list_instances', 'get_instance', 'get_instance_events',
      'list_instance_apps', 'get_deployment_log', 'get_deployment_events', 'get_wallet', 'list_orders', 'get_cancel_quote', 'get_help', 'generate_secret']
    const write = ['place_order', 'renew_instance', 'release_instance', 'cancel_booking', 'deploy_app', 'redeploy_app', 'set_app_state', 'remove_app']
    expect(Object.keys(butlerToolLabel).sort()).toEqual([...read, ...write].sort())
    for (const name of [...read, ...write]) expect(butlerToolLabel[name]).toMatch(/\S/)
    expect(labelOf(butlerToolLabel, 'list_instances')).toBe('查询实例列表')
    expect(labelOf(butlerToolLabel, 'release_instance')).toBe('释放实例')
    expect(labelOf(butlerToolLabel, 'set_app_state')).toBe('停止 / 启动应用')
    expect(labelOf(alertKindLabel, 'butler_llm_unavailable')).toBe('AI 管家模型不可用')
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

describe('计划 13：多机集群文案', () => {
  it('集群状态 / 网络 / fabric / 角色', () => {
    const statuses: ClusterStatus[] = ['forming', 'ready', 'degraded', 'released', 'failed']
    expect(statuses.map((s) => clusterStatusLabel[s])).toEqual(['组建中', '就绪', '降级', '已释放', '开通失败'])
    for (const s of statuses) expect(clusterStatusType[s]).toBeTruthy()
    expect(Object.keys(clusterStatusLabel).sort()).toEqual([...statuses].sort())
    expect(labelOf(clusterStatusType, 'ready')).toBe('success')
    expect(labelOf(clusterStatusType, 'failed')).toBe('danger')
    const networks: ClusterNetwork[] = ['rdma', 'ethernet', 'rdma_degraded']
    expect(networks.map((n) => clusterNetworkLabel[n])).toEqual(['RDMA 高速互联', '普通网络', 'RDMA 不可用'])
    const kinds: FabricKind[] = ['lan', 'cx7']
    expect(kinds.map((k) => fabricKindLabel[k])).toEqual(['普通以太网', 'ConnectX-7'])
    const isolations: FabricIsolation[] = ['routed', 'direct', 'vlan']
    expect(isolations.map((i) => fabricIsolationLabel[i])).toEqual(['三层路由', '直连', 'VLAN 隔离'])
    const roles: ClusterRole[] = ['head', 'worker']
    expect(roles.map((r) => clusterRoleLabel[r])).toEqual(['主节点', '工作节点'])
  })
  it('集群应用聚合状态', () => {
    const statuses: ClusterAppStatus[] = ['installing', 'running', 'failed', 'stopped', 'stale', 'removing', 'removed']
    expect(Object.keys(clusterAppStatusLabel).sort()).toEqual([...statuses].sort())
    for (const s of statuses) {
      expect(clusterAppStatusLabel[s]).toMatch(/\S/)
      expect(clusterAppStatusType[s]).toBeTruthy()
    }
    expect(clusterAppStatusLabel.stale).toBe('需要重新部署')
  })
  it('告警种类、审计动作与指令类型', () => {
    expect(labelOf(alertKindLabel, 'cluster_failed')).toBe('集群开通失败')
    expect(labelOf(alertKindLabel, 'cluster_rdma_degraded')).toBe('集群 RDMA 不可用')
    expect(labelOf(alertKindLabel, 'cluster_net_error')).toBe('集群网络异常')
    for (const a of [
      'fabric.create', 'fabric.update', 'fabric.delete', 'cluster.extend', 'cluster.resync', 'cluster.release', 'cluster.auto_renew',
      'cluster.app_deploy', 'cluster.app_redeploy', 'cluster.app_stop', 'cluster.app_start', 'cluster.app_remove',
    ]) expect(auditActions).toContain(a)
    expect(labelOf(commandTypeLabel, 'configure_cluster')).toBe('配置集群网络')
  })
})
