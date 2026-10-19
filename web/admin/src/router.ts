import { brand, type Role } from '@spark/shared'
import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'
import { ALL_ROLES, FIN_ROLES, OPS_ROLES, SUPER_ONLY } from './menu'
import { bindRouter, hasRole, probeSession, session } from './store/session'

declare module 'vue-router' {
  interface RouteMeta {
    title: string
    roles?: readonly Role[]
  }
}

/** redirect 参数只接受站内相对路径：以 / 开头、不以 // 开头、不含反斜杠（浏览器把 /\evil.com 当作 //evil.com）。 */
export function safeRedirect(v: unknown): string | null {
  return typeof v === 'string' && v.startsWith('/') && !v.startsWith('//') && !v.includes('\\') ? v : null
}

export const routes: RouteRecordRaw[] = [
  { path: '/login', component: () => import('./views/LoginView.vue'), meta: { title: '登录' } },
  { path: '/', component: () => import('./views/DashboardView.vue'), meta: { title: '仪表盘', roles: ALL_ROLES } },
  { path: '/machines', component: () => import('./views/MachinesView.vue'), meta: { title: '机器', roles: OPS_ROLES } },
  { path: '/machines/:id', component: () => import('./views/MachineDetailView.vue'), meta: { title: '机器详情', roles: OPS_ROLES } },
  { path: '/pdus', component: () => import('./views/PdusView.vue'), meta: { title: 'PDU', roles: OPS_ROLES } },
  { path: '/groups', component: () => import('./views/GroupsView.vue'), meta: { title: '机组', roles: OPS_ROLES } },
  { path: '/alerts', component: () => import('./views/AlertsView.vue'), meta: { title: '告警', roles: ALL_ROLES } },
  { path: '/instances', component: () => import('./views/InstancesView.vue'), meta: { title: '实例', roles: ALL_ROLES } },
  { path: '/instances/:id', component: () => import('./views/InstanceDetailView.vue'), meta: { title: '实例详情', roles: ALL_ROLES } },
  { path: '/bookings', component: () => import('./views/BookingsView.vue'), meta: { title: '预订', roles: ALL_ROLES } },
  { path: '/users', component: () => import('./views/UsersView.vue'), meta: { title: '用户', roles: ALL_ROLES } },
  { path: '/users/:id', component: () => import('./views/UserDetailView.vue'), meta: { title: '用户详情', roles: ALL_ROLES } },
  { path: '/kyc', component: () => import('./views/KycView.vue'), meta: { title: '企业认证审核', roles: OPS_ROLES } },
  { path: '/orders', component: () => import('./views/OrdersView.vue'), meta: { title: '订单', roles: FIN_ROLES } },
  { path: '/ledger', component: () => import('./views/LedgerView.vue'), meta: { title: '流水', roles: FIN_ROLES } },
  { path: '/recharges', component: () => import('./views/RechargesView.vue'), meta: { title: '充值单', roles: FIN_ROLES } },
  { path: '/catalog', component: () => import('./views/CatalogView.vue'), meta: { title: '价格与镜像', roles: SUPER_ONLY } },
  { path: '/admins', component: () => import('./views/AdminsView.vue'), meta: { title: '管理员', roles: SUPER_ONLY } },
  { path: '/audit', component: () => import('./views/AuditView.vue'), meta: { title: '审计日志', roles: ALL_ROLES } },
  { path: '/settings', component: () => import('./views/SettingsView.vue'), meta: { title: '个人设置', roles: ALL_ROLES } },
  { path: '/403', component: () => import('./views/ForbiddenView.vue'), meta: { title: '无权访问' } },
  { path: '/:pathMatch(.*)*', component: () => import('./views/NotFoundView.vue'), meta: { title: '页面不存在' } },
]

export const router = createRouter({ history: createWebHistory('/admin/'), routes })

router.beforeEach(async (to) => {
  await probeSession()
  if (to.path === '/login') return session.status === 'authed' ? (safeRedirect(to.query.redirect) ?? '/') : true
  if (session.status !== 'authed') return { path: '/login', query: { redirect: to.fullPath } }
  if (to.meta.roles && !hasRole(to.meta.roles)) return { path: '/403', query: { from: to.fullPath } }
  return true
})

router.afterEach((to) => {
  document.title = `${to.meta.title} - ${brand.name} 管理后台`
})

bindRouter(router)
