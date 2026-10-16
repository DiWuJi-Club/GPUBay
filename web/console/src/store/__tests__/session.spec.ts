import { ref } from 'vue'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const { warning } = vi.hoisted(() => ({ warning: vi.fn() })) // vi.mock 会被提升到文件顶部，工厂里引用的变量必须用 vi.hoisted 声明
vi.mock('element-plus', () => ({ ElMessage: { warning, error: vi.fn(), success: vi.fn() } }))
vi.mock('../../api', () => ({ api: { get: vi.fn() }, onUnauthorized: vi.fn() }))

import { bindRouter, clearSession, handleUnauthorized, session, setUser } from '../session'

function fakeRouter(path: string, fullPath = path, auth = true) {
  let after: () => void = () => {}
  const router = {
    currentRoute: ref({ path, fullPath, meta: { auth } }),
    replace: vi.fn(async () => {}),
    afterEach: (fn: () => void) => {
      after = fn
    },
  }
  return { router, fireAfterEach: () => after() }
}

describe('handleUnauthorized (Review Focus 2)', () => {
  beforeEach(() => {
    warning.mockClear()
    setUser({ id: 1, phone: '13800001234', kind: 'personal', kyc_status: 'verified', status: 'active', created_at: '2026-01-01T00:00:00Z' })
  })

  it('连续三次 401 只提示一次、只跳转一次，并带 redirect', () => {
    const { router, fireAfterEach } = fakeRouter('/wallet', '/wallet?tab=orders')
    bindRouter(router as never)
    handleUnauthorized()
    handleUnauthorized()
    handleUnauthorized()
    expect(session.status).toBe('anon')
    expect(session.user).toBeNull()
    expect(warning).toHaveBeenCalledTimes(1)
    expect(warning).toHaveBeenCalledWith('请先登录')
    expect(router.replace).toHaveBeenCalledTimes(1)
    expect(router.replace).toHaveBeenCalledWith({ path: '/login', query: { redirect: '/wallet?tab=orders' } })
    fireAfterEach()
    router.currentRoute.value = { path: '/login', fullPath: '/login', meta: { auth: false } }
    handleUnauthorized()
    expect(router.replace).toHaveBeenCalledTimes(1)
  })

  it('匿名访客在公开页收到 401 不提示不跳转', () => {
    clearSession()
    const { router } = fakeRouter('/market', '/market', false)
    bindRouter(router as never)
    handleUnauthorized()
    expect(warning).not.toHaveBeenCalled()
    expect(router.replace).not.toHaveBeenCalled()
  })
})
