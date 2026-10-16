export type LabelMap = Readonly<Record<string, string>>

/** 枚举值 → 中文；未知值原样返回（后端新增枚举时不至于显示空白）。 */
export function labelOf(map: LabelMap, value: string | null | undefined): string {
  if (value == null) return ''
  return map[value] ?? value
}

export const instanceStateLabel: LabelMap = {
  creating: '开通中', running: '运行中', stopped: '已到期停机', wiping: '洗机中', released: '已释放', failed: '开通失败',
}
/** el-tag 的 type：开通中蓝、运行中绿、停机橙、洗机中灰、已释放灰、失败红。 */
export const instanceStateType: LabelMap = {
  creating: 'primary', running: 'success', stopped: 'warning', wiping: 'info', released: 'info', failed: 'danger',
}
export const orderKindLabel: LabelMap = { new: '新购', renew: '续费' }
export const orderStatusLabel: LabelMap = { paid: '已支付', refunded: '已退款', partially_refunded: '部分退款' }
export const ledgerKindLabel: LabelMap = { recharge: '充值', order_pay: '扣款', refund: '退款', admin_adjust: '调账' }
export const rechargeStatusLabel: LabelMap = { pending: '待支付', paid: '已支付', closed: '已关闭' }
export const rechargeChannelLabel: LabelMap = { wechat: '微信支付', alipay: '支付宝', manual: '手动' }
export const machineStateLabel: LabelMap = { idle: '空闲', allocated: '已分配', maintenance: '维护中', faulty: '故障' }
export const alertLevelLabel: LabelMap = { warning: '警告', critical: '严重' }
export const alertKindLabel: LabelMap = {
  machine_offline: '机器失联', create_failed: '开通失败', start_failed: '启动失败', stop_failed: '停机失败',
  wipe_failed: '洗机失败', recharge_amount_mismatch: '支付金额不符', recharge_paid_after_close: '关单后到账',
}
export const roleLabel: LabelMap = { super: '超级管理员', ops: '运维', finance: '财务' }
export const adminStatusLabel: LabelMap = { active: '正常', disabled: '已停用' }
export const userStatusLabel: LabelMap = { active: '正常', banned: '已封禁' }
export const userKindLabel: LabelMap = { personal: '个人', enterprise: '企业' }
export const kycStatusLabel: LabelMap = { none: '未认证', pending: '审核中', verified: '已认证', rejected: '未通过' }
export const kycResultLabel: LabelMap = { pending: '审核中', verified: '已通过', rejected: '未通过', error: '核验出错' }
export const durationLabel: LabelMap = { '1d': '1 天', '1w': '1 周', '1m': '1 月' }
export const commandTypeLabel: LabelMap = {
  create_instance: '开通实例', start_instance: '启动实例', stop_instance: '停止实例', wipe_instance: '洗机',
}

/** 后台审计动作（与后端写入值一致，顺序按模块）。 */
export const auditActions: readonly string[] = [
  'admin.create', 'admin.update', 'admin.reset_totp', 'admin.login', 'admin.login_recovery_code', 'admin.totp_bind', 'admin.password_change',
  'machine.register', 'machine.import', 'machine.maintenance', 'machine.power_cycle',
  'pdu.create', 'pdu.update',
  'alert.resolve',
  'instance.extend', 'instance.migrate', 'instance.rewipe', 'instance.refund_release',
  'user.ban', 'user.unban',
  'kyc.review',
  'wallet.adjust', 'wallet.manual_recharge',
  'price.update',
  'image.create', 'image.update',
]
