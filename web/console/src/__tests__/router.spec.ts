import { afterEach, describe, expect, it, vi } from 'vitest'
import { postLoginTarget } from '../logic/access'
import { router, safeRedirect } from '../router'
import { session } from '../store/session'

describe('safeRedirect', () => {
  it('只接受站内相对路径', () => {
    expect(safeRedirect('/instances')).toBe('/instances')
    expect(safeRedirect('/kyc?redirect=/market')).toBe('/kyc?redirect=/market')
    expect(safeRedirect('//evil.com')).toBeNull()
    expect(safeRedirect('https://evil.com')).toBeNull()
    expect(safeRedirect(['/a'])).toBeNull()
    expect(safeRedirect('/\\evil.com')).toBeNull()
    expect(safeRedirect('/a\\b')).toBeNull()
    expect(safeRedirect(undefined)).toBeNull()
  })
})

describe('authorize redirect handling', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    session.status = 'unknown'
  })

  it('已登录访问 /login 且 redirect 是 authorize：路由守卫整页跳转，不做路由导航', async () => {
    const assign = vi.fn()
    vi.stubGlobal('location', { ...window.location, assign })
    session.status = 'authed'
    const target = '/api/v1/ingress/authorize?target=http%3A%2F%2Fins-a1b2c3d4-8888.spark.test%3A18080%2F'
    const before = router.currentRoute.value.fullPath
    await router.push({ path: '/login', query: { redirect: target } })
    expect(assign).toHaveBeenCalledWith(target)
    expect(router.currentRoute.value.fullPath).toBe(before)
  })

  it('已登录访问 /login 且 redirect 是外站：不整页跳转，回落 /market', async () => {
    const assign = vi.fn()
    vi.stubGlobal('location', { ...window.location, assign })
    session.status = 'authed'
    await router.push({ path: '/login', query: { redirect: '//evil.example/api/v1/ingress/authorize?target=x' } })
    expect(assign).not.toHaveBeenCalled()
    expect(router.currentRoute.value.path).toBe('/market')
    expect(postLoginTarget('//evil.example/api/v1/ingress/authorize')).toEqual({ kind: 'route', path: '/market' })
  })
})

describe('邀请返利路由 (计划 8)', () => {
  afterEach(() => {
    session.status = 'unknown'
  })

  it('/invite 需要登录：未登录跳登录页并带 redirect', async () => {
    session.status = 'anon'
    await router.push('/invite')
    expect(router.currentRoute.value.path).toBe('/login')
    expect(router.currentRoute.value.query.redirect).toBe('/invite')
    expect(router.resolve('/invite').meta).toEqual({ title: '邀请返利', auth: true })
  })
})

describe('AI 管家路由 (计划 12)', () => {
  afterEach(() => {
    session.status = 'unknown'
  })

  it('/butler 需要登录，标题「AI 管家」', async () => {
    session.status = 'anon'
    await router.push('/butler')
    expect(router.currentRoute.value.path).toBe('/login')
    expect(router.currentRoute.value.query.redirect).toBe('/butler')
    expect(router.resolve('/butler').meta).toEqual({ title: 'AI 管家', auth: true })
  })
})
