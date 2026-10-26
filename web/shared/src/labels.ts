export type LabelMap = Readonly<Record<string, string>>

/** 枚举值 → 中文；未知值原样返回（后端新增枚举时不至于显示空白）。 */
export function labelOf(map: LabelMap, value: string | null | undefined): string {
  if (value == null) return ''
  return map[value] ?? value
}

export const instanceStateLabel: LabelMap = {
  reserved: '已预订', creating: '开通中', running: '运行中', stopped: '已到期停机', wiping: '洗机中', released: '已释放', failed: '开通失败', cancelled: '已取消',
}
/** el-tag 的 type：已预订 / 开通中蓝、运行中绿、停机橙、洗机中 / 已释放 / 已取消灰、失败红。 */
export const instanceStateType: LabelMap = {
  reserved: 'primary', creating: 'primary', running: 'success', stopped: 'warning', wiping: 'info', released: 'info', failed: 'danger', cancelled: 'info',
}
export const orderKindLabel: LabelMap = { new: '新购', renew: '续费' }
export const orderStatusLabel: LabelMap = { paid: '已支付', refunded: '已退款', partially_refunded: '部分退款' }
export const ledgerKindLabel: LabelMap = {
  recharge: '充值', order_pay: '扣款', refund: '退款', admin_adjust: '调账', referral_rebate: '邀请返利', referral_clawback: '返利扣回', llm_pay: 'Token 消费',
}
/** el-tag 的 type：入账（充值 / 退款 / 邀请返利）绿、扣款类（扣款 / 返利扣回 / Token 消费）橙、调账灰；未登记的种类用 info。 */
export const ledgerKindType: LabelMap = {
  recharge: 'success', refund: 'success', referral_rebate: 'success', order_pay: 'warning', referral_clawback: 'warning', llm_pay: 'warning', admin_adjust: 'info',
}
export const bindingStatusLabel: LabelMap = { active: '有效', held: '审核中', revoked: '已失效' }
export const rewardStatusLabel: LabelMap = { pending: '待发放', settled: '已发放', void: '已作废' }
export const rechargeStatusLabel: LabelMap = { pending: '待支付', paid: '已支付', closed: '已关闭' }
export const rechargeChannelLabel: LabelMap = { wechat: '微信支付', alipay: '支付宝', manual: '手动' }
export const machineStateLabel: LabelMap = { idle: '空闲', allocated: '已分配', maintenance: '维护中', faulty: '故障' }
export const alertLevelLabel: LabelMap = { warning: '警告', critical: '严重' }
export const alertKindLabel: LabelMap = {
  machine_offline: '机器失联', create_failed: '开通失败', start_failed: '启动失败', stop_failed: '停机失败',
  wipe_failed: '洗机失败', recharge_amount_mismatch: '支付金额不符', recharge_paid_after_close: '关单后到账',
  lan_ip_mismatch: '内网 IP 不一致', container_down: '容器未运行', container_missing: '容器不存在', data_disk_missing: '数据盘未挂载',
  orphan_container: '残留容器', orphan_wipe_failed: '残留容器清理失败', root_disk_high: '根分区空间不足', gpu_temp_high: 'GPU 温度过高',
  invalid_report: '上报数据异常',
  ssh_bruteforce: 'SSH 暴力破解', upstream_unreachable: '实例 SSH 上游不可达', tls_cert_reload_failed: 'TLS 证书加载失败', tls_cert_expiring: 'TLS 证书即将到期',
  booking_at_risk: '预订机器不可用', booking_unfulfilled: '预订无法履约', booking_cancel_failed: '预订取消失败',
  referral_scan_stuck: '邀请返利扫描卡住',
  app_deploy_unreachable: '应用部署无法连接容器',
  butler_llm_unavailable: 'AI 管家模型不可用',
  llm_pool_short: 'Token 服务池机器不足', llm_node_unhealthy: '模型节点健康检查失败', llm_node_failed: '模型节点多次重建失败', llm_usage_record_failed: 'Token 用量记账失败',
  cluster_failed: '集群开通失败', cluster_rdma_degraded: '集群 RDMA 不可用', cluster_net_error: '集群网络异常',
}
export const roleLabel: LabelMap = { super: '超级管理员', ops: '运维', finance: '财务' }
export const adminStatusLabel: LabelMap = { active: '正常', disabled: '已停用' }
export const userStatusLabel: LabelMap = { active: '正常', banned: '已封禁' }
export const userKindLabel: LabelMap = { personal: '个人', enterprise: '企业' }
export const kycStatusLabel: LabelMap = { none: '未认证', pending: '审核中', verified: '已认证', rejected: '未通过' }
export const kycResultLabel: LabelMap = { pending: '审核中', verified: '已通过', rejected: '未通过', error: '核验出错' }
export const durationLabel: LabelMap = { '1d': '1 天', '1w': '1 周', '1m': '1 月' }
export const commandTypeLabel: LabelMap = {
  create_instance: '开通实例', start_instance: '启动实例', stop_instance: '停止实例', wipe_instance: '洗机', pull_image: '预拉取镜像',
  configure_cluster: '配置集群网络',
}
export const portVisibilityLabel: LabelMap = { private: '私有', public: '公开' }
export const commandStatusLabel: LabelMap = { pending: '待执行', sent: '已下发', done: '已完成', failed: '失败' }

/** 后台审计动作（与后端写入值一致，顺序按模块）。 */
export const auditActions: readonly string[] = [
  'admin.create', 'admin.update', 'admin.reset_totp', 'admin.login', 'admin.login_recovery_code', 'admin.totp_bind', 'admin.password_change',
  'machine.register', 'machine.import', 'machine.maintenance', 'machine.power_cycle', 'machine.enroll_token', 'machine.enrolled', 'machine.revoke_enrollment',
  'pdu.create', 'pdu.update',
  'alert.resolve',
  'instance.extend', 'instance.migrate', 'instance.rewipe', 'instance.refund_release',
  'instance.password_reset', 'instance.password_auth', 'instance.port_add', 'instance.port_update', 'instance.port_delete', 'instance.port_force_private',
  'ssh_key.add', 'ssh_key.delete',
  'user.ban', 'user.unban',
  'kyc.review',
  'wallet.adjust', 'wallet.manual_recharge',
  'price.update',
  'image.create', 'image.update',
  'group.create', 'group.update', 'group.delete', 'group.assign', 'group.unassign', 'machine.update',
  'booking.cancel', 'booking.admin_cancel', 'booking.reassign', 'instance.auto_renew',
  'referral.bind', 'referral.auto_revoke', 'referral.auto_hold', 'referral.kyc_check_failed', 'referral.config', 'referral.freeze', 'referral.unfreeze', 'referral.approve', 'referral.revoke', 'referral.rescan',
  'app.create', 'app.update', 'app.delete', 'app.version.create', 'app.version.update', 'app.version.publish', 'app.version.prepull',
  'app.deployment.force_remove',
  'instance.app_deploy', 'instance.app_redeploy', 'instance.app_stop', 'instance.app_start', 'instance.app_remove',
  'llm.model.create', 'llm.model.update', 'llm.model.status', 'llm.model.delete',
  'llm.node.drain', 'llm.node.rebuild', 'llm.node.remove',
  'llm.account.update',
  'llm.key.create', 'llm.key.update', 'llm.key.revoke', 'llm.key.admin_disable',
  'workflow.create', 'workflow.update', 'workflow.delete', 'workflow.version.create', 'workflow.version.update', 'workflow.version.publish',
  'instance.workflow_add', 'instance.workflow_update', 'instance.workflow_remove',
  'butler.tool_execute', 'butler.settings',
  'fabric.create', 'fabric.update', 'fabric.delete', 'cluster.extend', 'cluster.resync',
  'cluster.release', 'cluster.auto_renew', 'cluster.app_deploy', 'cluster.app_redeploy', 'cluster.app_stop', 'cluster.app_start', 'cluster.app_remove',
]

export const deploymentStateLabel: LabelMap = {
  queued: '等待部署', installing: '部署中', running: '运行中', stopped: '已停止', failed: '失败', removing: '卸载中', removed: '已卸载', lost: '已失效',
}
export const deploymentStateType: LabelMap = {
  queued: 'info', installing: 'primary', running: 'success', stopped: 'info', failed: 'danger', removing: 'warning', removed: 'info', lost: 'info',
}
// busy：queued 期间上一次部署仍在容器里进行（run.sh 部署锁），Runner 30 秒后自动重试
export const deploymentPhaseLabel: LabelMap = { prepare: '准备', download: '下载模型', install: '安装', start: '启动', health: '健康检查', busy: '等待上一次部署结束' }
export const deploymentErrorLabel: LabelMap = {
  script_failed: '脚本执行失败', timeout: '部署超时', health_failed: '健康检查未通过', upload_failed: '部署包上传失败', unreachable: '无法连接容器',
  port_conflict: '端口冲突', port_limit: '端口数量超限', instance_gone: '实例已不存在', crashed: '服务进程已退出', disk: '数据盘空间不足',
  deploy_busy: '上一次部署长时间未结束',
}
/**
 * 部署失败原因：错误码中文 + 「：message」；message 与错误码中文相同（如 crashed「服务进程已退出」）时只显示一次。
 * 没有错误码时用 fallback。
 */
export function deploymentErrorText(code: string, message: string, fallback = ''): string {
  const label = code ? labelOf(deploymentErrorLabel, code) : fallback
  if (!message || message === label) return label
  return label ? `${label}：${message}` : message
}
export const appCategoryLabel: LabelMap = { llm: '大模型', image: '图像生成', notebook: 'Notebook', tool: '工具' }
export const appStatusLabel: LabelMap = { draft: '草稿', published: '已发布', retired: '已退役' }

// 计划 9：Token 商城
export const llmModelStatusLabel: LabelMap = { draft: '草稿', online: '已上线', offline: '已下线' }
export const llmModelStatusType: LabelMap = { draft: 'info', online: 'success', offline: 'warning' }
export const llmModelKindLabel: LabelMap = { chat: '对话', embedding: '向量' }
export const llmNodeStateLabel: LabelMap = {
  provisioning: '部署中', ready: '就绪', unhealthy: '不健康', draining: '排空中', removing: '移除中', removed: '已移除', failed: '失败',
}
/** el-tag 的 type：部署中蓝、就绪绿、不健康 / 排空中橙、移除中 / 已移除灰、失败红。 */
export const llmNodeStateType: LabelMap = {
  provisioning: 'primary', ready: 'success', unhealthy: 'warning', draining: 'warning', removing: 'info', removed: 'info', failed: 'danger',
}
export const llmUsageStatusLabel: LabelMap = {
  ok: '成功', client_abort: '客户端中断', upstream_error: '上游错误', upstream_timeout: '上游超时', no_node: '无可用节点',
}
export const llmKeyStatusLabel: LabelMap = { active: '启用', disabled: '已停用', revoked: '已撤销' }
/** 模型 features 标签；未登记的原样显示（labelOf 回落）。 */
export const llmFeatureLabel: LabelMap = { tools: '工具调用', reasoning: '深度思考', vision: '视觉', 'long-context': '长上下文' }
export const llmEndpointLabel: LabelMap = { chat: '对话', completions: '补全', embeddings: '向量' }
// 计划 11：ComfyUI 工作流市场
export const workflowCategoryLabel: LabelMap = { txt2img: '文生图', img2img: '图生图', video: '视频', upscale: '放大修复', other: '其它' }
export const workflowStateLabel: LabelMap = { pending: '待安装', installed: '已安装', failed: '安装失败' }
export const workflowStateType: LabelMap = { pending: 'info', installed: 'success', failed: 'danger' }
export const runStateLabel: LabelMap = { queued: '排队中', running: '生成中', done: '完成', error: '失败' }
/** 工作流相关错误码的中文（与后端 apperr 一致；带参数的错误以后端 message 为准）。 */
export const workflowErrorLabel: LabelMap = {
  workflow_not_found: '工作流不存在或未上架', workflow_unavailable: '该工作流当前没有可用版本', workflow_exists: '该工作流已安装在此 ComfyUI 上',
  workflow_limit: '每个 ComfyUI 最多安装 8 个工作流', workflow_app_unsupported: '当前 ComfyUI 应用版本不支持工作流市场',
  workflow_version_invalid: '工作流版本校验未通过', workflow_version_frozen: '已发布的工作流版本不能修改',
  workflow_busy: 'ComfyUI 正在处理其它任务，请稍后再试', workflow_prompt_rejected: 'ComfyUI 拒绝了该任务', workflow_input_invalid: '参数不正确',
  upstream_unreachable: '无法连接实例内的 ComfyUI',
}
/** 下载量（GB）→ `6.9 GB` / `17 GB`：小于 10 最多两位小数，否则最多一位，去掉尾零；非法或负数为 0。 */
export function formatGB(n: number | string): string {
  const v = typeof n === 'string' ? Number(n) : n
  if (!Number.isFinite(v) || v <= 0) return '0 GB'
  return `${Number(v.toFixed(v < 10 ? 2 : 1))} GB`
}

/** AI 管家 23 个工具的中文名（与 Go internal/butler/tools.Labels 同表；前 15 个读、后 8 个写）。 */
export const butlerToolLabel: LabelMap = {
  list_products: '查询套餐', check_availability: '查询可租时段', list_apps: '查询应用目录', get_app: '查看应用详情',
  list_instances: '查询实例列表', get_instance: '查看实例详情', get_instance_events: '查看实例动态', list_instance_apps: '查询实例上的应用',
  get_deployment_log: '读取部署日志', get_deployment_events: '查看部署事件', get_wallet: '查询钱包', list_orders: '查询订单',
  get_cancel_quote: '查询取消退款', get_help: '查阅帮助', generate_secret: '生成随机密钥',
  place_order: '租用实例', renew_instance: '续费实例', release_instance: '释放实例', cancel_booking: '取消预订',
  deploy_app: '部署应用', redeploy_app: '重新部署应用', set_app_state: '停止 / 启动应用', remove_app: '卸载应用',
}

// 计划 13：多机集群
export const clusterStatusLabel: LabelMap = { forming: '组建中', ready: '就绪', degraded: '降级', released: '已释放', failed: '开通失败' }
/** el-tag 的 type：组建中蓝、就绪绿、降级橙、已释放灰、开通失败红。 */
export const clusterStatusType: LabelMap = { forming: 'primary', ready: 'success', degraded: 'warning', released: 'info', failed: 'danger' }
export const clusterNetworkLabel: LabelMap = { rdma: 'RDMA 高速互联', ethernet: '普通网络', rdma_degraded: 'RDMA 不可用' }
export const fabricKindLabel: LabelMap = { lan: '普通以太网', cx7: 'ConnectX-7' }
export const fabricIsolationLabel: LabelMap = { routed: '三层路由', direct: '直连', vlan: 'VLAN 隔离' }
export const clusterAppStatusLabel: LabelMap = {
  installing: '部署中', running: '运行中', failed: '失败', stopped: '已停止', stale: '需要重新部署', removing: '卸载中', removed: '已卸载',
}
export const clusterAppStatusType: LabelMap = {
  installing: 'primary', running: 'success', failed: 'danger', stopped: 'info', stale: 'warning', removing: 'warning', removed: 'info',
}
export const clusterRoleLabel: LabelMap = { head: '主节点', worker: '工作节点' }
