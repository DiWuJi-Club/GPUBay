import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import ElementPlus from 'element-plus'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'

const mocks = vi.hoisted(() => ({ get: vi.fn() }))
vi.mock('../../api', () => ({ api: { get: mocks.get }, onUnauthorized: vi.fn() }))

import { butler, resetButler } from '../../store/butler'
import { session } from '../../store/session'
import ConsoleLayout from '../ConsoleLayout.vue'

let mobile = false
let wrapper: VueWrapper | null = null
let router: Router

async function render(path: string) {
  router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/:p(.*)*', component: { template: '<div />' }, meta: { title: '页面' } },
    ],
  })
  await router.push(path)
  wrapper = mount(ConsoleLayout, { attachTo: document.body, global: { plugins: [ElementPlus, router] } })
  await flushPromises()
}
const $ = (id: string) => document.querySelector<HTMLElement>(`[data-testid="${id}"]`)

beforeEach(() => {
  mobile = false
  vi.stubGlobal('matchMedia', (q: string) => ({ matches: mobile, media: q, addEventListener: () => {}, removeEventListener: () => {} }))
  mocks.get.mockReset()
  mocks.get.mockImplementation(() => new Promise(() => {})) // 聊天区的请求挂起即可
  resetButler()
  session.status = 'authed'
})
afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  document.body.innerHTML = ''
  vi.unstubAllGlobals()
  session.status = 'unknown'
  resetButler()
})

describe('ConsoleLayout AI 管家入口', () => {
  it('桌面：侧栏有 menu-butler，浮动按钮打开抽屉', async () => {
    await render('/market')
    expect($('menu-butler')!.textContent!.trim()).toBe('AI 管家')
    $('butler-launcher')!.click()
    await flushPromises()
    expect(butler.open).toBe(true)
    expect($('butler-drawer')).not.toBeNull()
  })

  it('/butler 全页：不显示浮动按钮与抽屉', async () => {
    butler.open = true
    await render('/butler')
    expect($('butler-launcher')).toBeNull()
    expect($('butler-drawer')).toBeNull()
  })

  it('未登录点浮动按钮 → 登录页并带 redirect', async () => {
    session.status = 'anon'
    await render('/instances/ins-abc?tab=apps')
    $('butler-launcher')!.click()
    await flushPromises()
    expect(router.currentRoute.value.path).toBe('/login')
    expect(router.currentRoute.value.query.redirect).toBe('/instances/ins-abc?tab=apps')
    expect(butler.open).toBe(false)
  })

  it('手机：底部 tabbar 有「管家」，没有浮动按钮', async () => {
    mobile = true
    await render('/market')
    expect($('tabbar-butler')!.textContent!.trim()).toBe('管家')
    expect($('tabbar-butler')!.getAttribute('href')).toBe('/butler')
    expect($('butler-launcher')).toBeNull()
  })
})
