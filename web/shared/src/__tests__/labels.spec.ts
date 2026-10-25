import { describe, expect, it } from 'vitest'
import { alertKindLabel, auditActions, bindingStatusLabel, butlerToolLabel, commandStatusLabel, instanceStateLabel, instanceStateType, labelOf, ledgerKindLabel, rewardStatusLabel } from '../labels'

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
    expect(auditActions).toHaveLength(81)
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
})
