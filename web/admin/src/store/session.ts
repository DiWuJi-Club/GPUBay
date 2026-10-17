import type { Admin, Role } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { reactive } from 'vue'
import type { Router } from 'vue-router'
import { api, onTotpSetupRequired, onUnauthorized } from '../api'

export type SessionStatus = 'unknown' | 'anon' | 'authed'

export const session = reactive<{ status: SessionStatus; admin: Admin | null }>({ status: 'unknown', admin: null })

let probing: Promise<void> | null = null

/** 应用启动只探测一次 GET /me（quiet：未登录的 401 不提示、不跳转，由路由守卫送去登录页）。 */
export function probeSession(): Promise<void> {
  if (session.status !== 'unknown') return Promise.resolve()
  probing ??= api
    .get<Admin>('/me', undefined, { quiet: true })
    .then(
      (a) => setAdmin(a),
      () => {
        session.status = 'anon'
        session.admin = null
      },
    )
    .finally(() => {
      probing = null
    })
  return probing
}

export function setAdmin(a: Admin) {
  session.admin = a
  session.status = 'authed'
}

export function clearSession() {
  session.admin = null
  session.status = 'anon'
}

/** 未指定 roles 视为所有已登录管理员可用。 */
export const hasRole = (roles?: readonly Role[]) => !roles || (session.admin != null && roles.includes(session.admin.role))

let router: Router | null = null
let redirecting = false

export function bindRouter(r: Router) {
  router = r
  r.afterEach(() => {
    redirecting = false
  })
}

/**
 * 401：清会话、提示一次、回登录页并带 redirect。
 * 同一页面并发多个请求同时 401 只提示一次、只跳一次；登录页自身的 401 由页面处理（临时会话超时回到步骤一）。
 */
export function handleUnauthorized() {
  const wasAuthed = session.status === 'authed'
  clearSession()
  if (!router || redirecting) return
  const current = router.currentRoute.value
  if (current.path === '/login') return
  redirecting = true
  if (wasAuthed) ElMessage.warning('登录已失效，请重新登录')
  void router.replace({ path: '/login', query: { redirect: current.fullPath } })
}

onUnauthorized(handleUnauthorized)
onTotpSetupRequired(() => {
  clearSession()
  ElMessage.warning('请先绑定动态码：重新输入密码后按提示绑定')
  void router?.replace('/login')
})
