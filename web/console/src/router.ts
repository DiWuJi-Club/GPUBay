import { brand } from '@spark/shared'
import { createRouter, createWebHistory } from 'vue-router'
import { bindRouter, probeSession, session } from './store/session'

declare module 'vue-router' {
  interface RouteMeta {
    title: string
    auth?: boolean
  }
}

/** redirect 参数只接受站内相对路径（以 / 开头且不以 // 开头）。 */
export function safeRedirect(v: unknown): string | null {
  return typeof v === 'string' && v.startsWith('/') && !v.startsWith('//') ? v : null
}

export const router = createRouter({
  history: createWebHistory('/'),
  routes: [
    { path: '/', redirect: '/market' },
    { path: '/login', component: () => import('./views/LoginView.vue'), meta: { title: '登录' } },
    { path: '/market', component: () => import('./views/MarketView.vue'), meta: { title: '算力市场' } },
    { path: '/instances', component: () => import('./views/InstancesView.vue'), meta: { title: '我的实例', auth: true } },
    { path: '/instances/:id', component: () => import('./views/InstanceDetailView.vue'), meta: { title: '实例详情', auth: true } },
    { path: '/wallet', component: () => import('./views/WalletView.vue'), meta: { title: '钱包', auth: true } },
    { path: '/wallet/recharge', component: () => import('./views/RechargeView.vue'), meta: { title: '充值', auth: true } },
    { path: '/wallet/recharge/:id', component: () => import('./views/RechargeOrderView.vue'), meta: { title: '充值单', auth: true } },
    { path: '/kyc', component: () => import('./views/KycView.vue'), meta: { title: '实名认证', auth: true } },
    { path: '/account', component: () => import('./views/AccountView.vue'), meta: { title: '我的', auth: true } },
    { path: '/help', component: () => import('./views/HelpView.vue'), meta: { title: '使用帮助' } },
    { path: '/:pathMatch(.*)*', component: () => import('./views/NotFoundView.vue'), meta: { title: '页面不存在' } },
  ],
})

router.beforeEach(async (to) => {
  await probeSession()
  if (to.meta.auth && session.status !== 'authed') return { path: '/login', query: { redirect: to.fullPath } }
  if (to.path === '/login' && session.status === 'authed') return safeRedirect(to.query.redirect) ?? '/market'
  return true
})

router.afterEach((to) => {
  document.title = `${to.meta.title} - ${brand.name}`
})

bindRouter(router)
