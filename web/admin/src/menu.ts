import type { Role } from '@spark/shared'

export const ALL_ROLES: readonly Role[] = ['super', 'ops', 'finance']
export const OPS_ROLES: readonly Role[] = ['super', 'ops']
export const FIN_ROLES: readonly Role[] = ['super', 'finance']
export const SUPER_ONLY: readonly Role[] = ['super']

export interface MenuItem {
  key: string
  path: string
  label: string
  roles: readonly Role[]
}

/** 侧边菜单（与路由 meta.roles 一致，由 menu.spec.ts 表驱动核对）。 */
export const menu: MenuItem[] = [
  { key: 'dashboard', path: '/', label: '仪表盘', roles: ALL_ROLES },
  { key: 'machines', path: '/machines', label: '机器', roles: OPS_ROLES },
  { key: 'pdus', path: '/pdus', label: 'PDU', roles: OPS_ROLES },
  { key: 'groups', path: '/groups', label: '机组', roles: OPS_ROLES },
  { key: 'alerts', path: '/alerts', label: '告警', roles: ALL_ROLES },
  { key: 'instances', path: '/instances', label: '实例', roles: ALL_ROLES },
  { key: 'bookings', path: '/bookings', label: '预订', roles: ALL_ROLES },
  { key: 'users', path: '/users', label: '用户', roles: ALL_ROLES },
  { key: 'kyc', path: '/kyc', label: '企业认证', roles: OPS_ROLES },
  { key: 'orders', path: '/orders', label: '订单', roles: FIN_ROLES },
  { key: 'ledger', path: '/ledger', label: '流水', roles: FIN_ROLES },
  { key: 'recharges', path: '/recharges', label: '充值单', roles: FIN_ROLES },
  { key: 'referral', path: '/referral', label: '邀请返利', roles: FIN_ROLES },
  { key: 'catalog', path: '/catalog', label: '价格与镜像', roles: SUPER_ONLY },
  { key: 'admins', path: '/admins', label: '管理员', roles: SUPER_ONLY },
  { key: 'audit', path: '/audit', label: '审计日志', roles: ALL_ROLES },
]

export function menuFor(role: Role): MenuItem[] {
  return menu.filter((m) => m.roles.includes(role))
}
