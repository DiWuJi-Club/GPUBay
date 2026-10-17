import { ref } from 'vue'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { Admin } from '@spark/shared'

const { warning } = vi.hoisted(() => ({ warning: vi.fn() })) // vi.mock 会被提升到文件顶部，工厂里引用的变量必须用 vi.hoisted 声明
vi.mock('element-plus', () => ({ ElMessage: { warning, error: vi.fn(), success: vi.fn() } }))
vi.mock('../../api', () => ({ api: { get: vi.fn() }, onUnauthorized: vi.fn(), onTotpSetupRequired: vi.fn() }))

import { bindRouter, clearSession, handleUnauthorized, hasRole, session, setAdmin } from '../session'

const admin = (role: Admin['role']): Admin => ({
  id: 1,
  username: 'ops1',
  role,
  status: 'active',
  totp_confirmed: true,
  last_login_at: null,
  last_login_ip: '',
  created_at: '2026-01-01T00:00:00Z',
})

function fakeRouter(path: string, fullPath = path) {
  let after: () => void = () => {}
  const router = {
    currentRoute: ref({ path, fullPath, meta: {} }),
    replace: vi.fn(async () => {}),
    afterEach: (fn: () => void) => {
      after = fn
    },
  }
  return { router, fireAfterEach: () => after() }
}

describe('handleUnauthorized', () => {
  beforeEach(() => {
    warning.mockClear()
    setAdmin(admin('ops'))
  })

  it('并发三次 401 只提示一次「登录已失效」、只跳转一次，并带 redirect', () => {
    const { router, fireAfterEach } = fakeRouter('/instances', '/instances?state=running')
    bindRouter(router as never)
    handleUnauthorized()
    handleUnauthorized()
    handleUnauthorized()
    expect(session.status).toBe('anon')
    expect(session.admin).toBeNull()
    expect(warning).toHaveBeenCalledTimes(1)
    expect(warning).toHaveBeenCalledWith('登录已失效，请重新登录')
    expect(router.replace).toHaveBeenCalledTimes(1)
    expect(router.replace).toHaveBeenCalledWith({ path: '/login', query: { redirect: '/instances?state=running' } })
    fireAfterEach()
    router.currentRoute.value = { path: '/login', fullPath: '/login', meta: {} }
    handleUnauthorized()
    expect(router.replace).toHaveBeenCalledTimes(1)
  })

  it('登录页上的 401（临时会话超时）由页面处理：不提示、不跳转', () => {
    const { router } = fakeRouter('/login', '/login?redirect=/machines')
    bindRouter(router as never)
    handleUnauthorized()
    expect(warning).not.toHaveBeenCalled()
    expect(router.replace).not.toHaveBeenCalled()
  })

  it('本来就未登录时不提示「登录已失效」，仍回登录页', () => {
    clearSession()
    const { router } = fakeRouter('/users')
    bindRouter(router as never)
    handleUnauthorized()
    expect(warning).not.toHaveBeenCalled()
    expect(router.replace).toHaveBeenCalledWith({ path: '/login', query: { redirect: '/users' } })
  })
})

describe('hasRole', () => {
  it('按当前管理员角色判断；未指定 roles 视为放行；未登录一律拒绝', () => {
    setAdmin(admin('finance'))
    expect(hasRole(['super', 'finance'])).toBe(true)
    expect(hasRole(['super', 'ops'])).toBe(false)
    expect(hasRole(undefined)).toBe(true)
    clearSession()
    expect(hasRole(['super', 'ops', 'finance'])).toBe(false)
  })
})
