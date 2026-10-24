import { ApiError, type User } from '@spark/shared'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import ElementPlus from 'element-plus'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter } from 'vue-router'

const mocks = vi.hoisted(() => ({
  get: vi.fn(),
  post: vi.fn(),
  msg: { call: vi.fn(), success: vi.fn(), warning: vi.fn(), error: vi.fn() },
}))
vi.mock('../../api', () => ({ api: { get: mocks.get, post: mocks.post }, onUnauthorized: vi.fn() }))
vi.mock('element-plus', async (orig) => {
  const m = await orig<typeof import('element-plus')>()
  const ElMessage = Object.assign((o: unknown) => mocks.msg.call(o), mocks.msg)
  return { ...m, ElMessage }
})

import { INVITE_KEY, storeInvite } from '../../logic/invite'
import { clearSession } from '../../store/session'
import LoginView from '../LoginView.vue'

const user = (over: Partial<User> = {}): User => ({ id: 1, phone: '13900001111', kind: 'personal', kyc_status: 'none', status: 'active', created_at: '2026-10-20T00:00:00Z', ...over })

let wrapper: VueWrapper | null = null
async function render() {
  const router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/login', component: LoginView }, { path: '/market', component: { template: '<div />' } }] })
  await router.push('/login')
  wrapper = mount(LoginView, { attachTo: document.body, global: { plugins: [ElementPlus, router] } })
  await flushPromises()
}
const $ = (id: string) => document.querySelector<HTMLElement>(`[data-testid="${id}"]`)
async function fill(id: string, v: string) {
  const el = $(id)!
  const input = (el.tagName === 'INPUT' ? el : el.querySelector('input')) as HTMLInputElement
  input.value = v
  input.dispatchEvent(new Event('input'))
  await flushPromises()
}
async function submit() {
  await fill('login-phone', '13900001111')
  await fill('login-code', '000000')
  await wrapper!.find('form').trigger('submit')
  await flushPromises()
}
const loginBody = () => mocks.post.mock.calls.find((c) => c[0] === '/auth/login')![1] as Record<string, unknown>

beforeEach(() => {
  localStorage.clear()
  mocks.get.mockReset()
  mocks.post.mockReset()
  Object.values(mocks.msg).forEach((f) => f.mockReset())
})
afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  document.body.innerHTML = ''
  clearSession()
})

describe('LoginView 邀请提示', () => {
  it('码有效：显示脱敏邀请人，登录带 invite_code，绑定成功提示并清存储', async () => {
    storeInvite('ABCDEFGH')
    mocks.get.mockResolvedValue({ valid: true, inviter: '138****1234' })
    mocks.post.mockResolvedValue(user({ invite: { bound: true, reason: 'ok', inviter: '138****1234' } }))
    await render()
    expect(mocks.get).toHaveBeenCalledWith('/referral/codes/ABCDEFGH')
    expect($('login-invite')!.textContent).toContain('通过 138****1234 的邀请注册')
    await submit()
    expect(loginBody()).toEqual({ phone: '13900001111', code: '000000', invite_code: 'ABCDEFGH' })
    expect(mocks.msg.success).toHaveBeenCalledWith('已绑定邀请关系')
    expect(localStorage.getItem(INVITE_KEY)).toBeNull()
  })

  it('查询临时失败（429 / 网络）：不显示提示条，但登录仍带码交给后端判定', async () => {
    storeInvite('ABCDEFGH')
    mocks.get.mockRejectedValue(new ApiError(429, 'rate_limited', '请求过于频繁', ''))
    mocks.post.mockResolvedValue(user({ invite: { bound: false, reason: 'invalid_code' } }))
    await render()
    expect($('login-invite')).toBeNull()
    expect(localStorage.getItem(INVITE_KEY)).not.toBeNull() // 未丢弃
    await submit()
    expect(loginBody().invite_code).toBe('ABCDEFGH')
    expect(mocks.msg.warning).toHaveBeenCalledWith('邀请码无效')
    expect(localStorage.getItem(INVITE_KEY)).toBeNull() // 登录已用掉
  })

  it('查询明确无效：静默丢弃，登录不带码', async () => {
    storeInvite('ABCDEFGH')
    mocks.get.mockResolvedValue({ valid: false, inviter: '' })
    mocks.post.mockResolvedValue(user())
    await render()
    expect($('login-invite')).toBeNull()
    expect(localStorage.getItem(INVITE_KEY)).toBeNull()
    await submit()
    expect(loginBody()).not.toHaveProperty('invite_code')
    expect(mocks.msg.warning).not.toHaveBeenCalled()
  })

  it('点 × 去掉邀请：隐藏提示条、清存储、登录不带码；老用户不提示', async () => {
    storeInvite('ABCDEFGH')
    mocks.get.mockResolvedValue({ valid: true, inviter: '138****1234' })
    mocks.post.mockResolvedValue(user({ invite: { bound: false, reason: 'existing_user' } }))
    await render()
    $('login-invite-clear')!.click()
    await flushPromises()
    expect($('login-invite')).toBeNull()
    expect(localStorage.getItem(INVITE_KEY)).toBeNull()
    await submit()
    expect(loginBody()).not.toHaveProperty('invite_code')
    expect(mocks.msg.success).not.toHaveBeenCalled()
    expect(mocks.msg.warning).not.toHaveBeenCalled()
  })

  it('登录失败（验证码错误）时不丢弃邀请码', async () => {
    storeInvite('ABCDEFGH')
    mocks.get.mockResolvedValue({ valid: true, inviter: '138****1234' })
    mocks.post.mockRejectedValue(new ApiError(400, 'invalid_code', '验证码错误', ''))
    await render()
    await submit()
    expect(loginBody().invite_code).toBe('ABCDEFGH')
    expect(localStorage.getItem(INVITE_KEY)).not.toBeNull()
    expect($('login-invite')).not.toBeNull()
  })

  it('没有邀请码：不查询、不显示', async () => {
    await render()
    expect(mocks.get).not.toHaveBeenCalled()
    expect($('login-invite')).toBeNull()
  })
})
