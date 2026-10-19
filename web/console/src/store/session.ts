import { ElMessage } from 'element-plus'
import { reactive } from 'vue'
import type { Router } from 'vue-router'
import type { User } from '@spark/shared'
import { api, onUnauthorized } from '../api'

export type SessionStatus = 'unknown' | 'anon' | 'authed'

export const session = reactive<{ status: SessionStatus; user: User | null; balance: number | null }>({
  status: 'unknown',
  user: null,
  balance: null,
})

let probing: Promise<void> | null = null

/** 应用启动只探测一次 GET /me（quiet：匿名访客的 401 不提示、不跳转）。 */
export function probeSession(): Promise<void> {
  if (session.status !== 'unknown') return Promise.resolve()
  probing ??= api
    .get<User>('/me', undefined, { quiet: true })
    .then(
      (u) => setUser(u),
      () => {
        session.status = 'anon'
        session.user = null
      },
    )
    .finally(() => {
      probing = null
    })
  return probing
}

export function setUser(u: User) {
  session.user = u
  session.status = 'authed'
}

export function clearSession() {
  session.user = null
  session.balance = null
  session.status = 'anon'
}

export async function refreshBalance() {
  if (session.status !== 'authed') return
  const { balance } = await api.get<{ balance: number }>('/wallet')
  session.balance = balance
}

let router: Router | null = null
let redirecting = false

export function bindRouter(r: Router) {
  router = r
  r.afterEach(() => {
    redirecting = false
  })
}

/**
 * 401：清会话、提示一次、跳登录页并带 redirect。
 * 并发多个请求同时 401（钱包页三个请求）只提示一次、只跳一次；匿名访客在公开页不提示不跳转。
 */
export function handleUnauthorized(message = '请先登录') {
  const wasAuthed = session.status === 'authed'
  clearSession()
  if (!router || redirecting) return
  const current = router.currentRoute.value
  if (current.path === '/login') return
  if (!wasAuthed && !current.meta.auth) return
  redirecting = true
  ElMessage.warning(message)
  void router.replace({ path: '/login', query: { redirect: current.fullPath } })
}

onUnauthorized(() => handleUnauthorized())
