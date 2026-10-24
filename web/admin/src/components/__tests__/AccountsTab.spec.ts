import type { Admin, LLMAccountAdmin, LLMKeyAdmin, Role } from '@spark/shared'
import { flushPromises, mount } from '@vue/test-utils'
import ElementPlus from 'element-plus'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'

const { get, post, put, del } = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn(), put: vi.fn(), del: vi.fn() }))
vi.mock('../../api', () => ({ api: { get, post, put, del }, onUnauthorized: vi.fn(), onTotpSetupRequired: vi.fn() }))

import { currentYM } from '../../logic/llm'
import { session } from '../../store/session'
import AccountsTab from '../llm/AccountsTab.vue'

const account = (over: Partial<LLMAccountAdmin> = {}): LLMAccountAdmin => ({
  user_id: 42, unsettled_micro: 1_500_000, carry_micro: 0, debt: 0, rpm_limit: null, tpm_limit: 500_000, concurrency_limit: null, blocked: false, blocked_reason: '',
  created_at: '', updated_at: '', phone: '13800001234', key_count: 2, month_cost: 1234, unsettled: 2, limits: { rpm: 60, tpm: 500_000, concurrency: 8 },
  ...over,
})
const key = (over: Partial<LLMKeyAdmin> = {}): LLMKeyAdmin => ({
  id: 7, user_id: 42, name: '生产', key_hint: 'sk-…abcd', status: 'active', model_slugs: [], monthly_cap: null, month_ym: currentYM(), month_cost_micro: 1_250_000,
  expires_at: null, last_used_at: null, created_at: '', revoked_at: null, ...over,
})

let accounts: LLMAccountAdmin[]
let wrapper: ReturnType<typeof mount> | undefined
let router: Router
function login(role: Role) {
  session.status = 'authed'
  session.admin = { id: 1, username: 'a', role, status: 'active', totp_confirmed: true, last_login_at: null, last_login_ip: '', created_at: '' } as Admin
}
async function mountTab(url = '/llm?tab=accounts') {
  router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/:p(.*)*', component: { template: '<div />' } }] })
  await router.push(url)
  wrapper = mount(AccountsTab, { global: { plugins: [router, ElementPlus] }, attachTo: document.body })
  await flushPromises()
}
const byId = (id: string) => wrapper!.find(`[data-testid="${id}"]`)
// el-table 会把列插槽用空行另渲染一份到 .hidden-columns：只取表体里的
const allById = (id: string) => wrapper!.findAll(`.el-table__body [data-testid="${id}"]`)
const inBody = (id: string) => document.body.querySelector<HTMLElement>(`[data-testid="${id}"]`)
const click = async (el: HTMLElement | null) => {
  expect(el).not.toBeNull()
  el!.click()
  await flushPromises()
}
function inputInBody(id: string): HTMLInputElement {
  const host = inBody(id)
  const el = host?.tagName === 'INPUT' ? host : (host?.querySelector('input') ?? null)
  expect(el).not.toBeNull()
  return el as HTMLInputElement
}
async function typeInBody(id: string, v: string) {
  const el = inputInBody(id)
  ;(el as HTMLInputElement).value = v
  el!.dispatchEvent(new Event('input'))
  await flushPromises()
}
const accountCalls = () => get.mock.calls.filter((c) => c[0] === '/llm/accounts')

beforeEach(() => {
  login('ops')
  accounts = [account(), account({ user_id: 43, phone: '13900005678', debt: 300, blocked: true, blocked_reason: '欠费', rpm_limit: 10, key_count: 0 })]
  for (const f of [get, post, put, del]) f.mockReset()
  get.mockImplementation(async (path: string) => {
    if (path === '/llm/accounts') return { items: accounts, total: accounts.length }
    if (path === '/llm/keys') return { items: [key(), key({ id: 8, name: '旧', status: 'disabled' })] }
    throw new Error(`unexpected GET ${path}`)
  })
  put.mockResolvedValue({})
  post.mockResolvedValue({})
})
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  document.body.innerHTML = ''
  vi.restoreAllMocks()
})

describe('AccountsTab 列表与筛选', () => {
  it('表格：手机号链接到用户详情、金额、限额（默认标注）、停用', async () => {
    await mountTab()
    expect(accountCalls()[0][1]).toEqual({ q: undefined, user_id: undefined, blocked: undefined, debt: undefined, page: 1, page_size: 20 })
    const rows = allById('llm-acc-row')
    expect(rows).toHaveLength(2)
    expect(rows[0].text()).toBe('13800001234')
    expect(rows[0].attributes('href')).toBe('/users/42')
    expect(allById('llm-acc-keys-count')[0].text()).toBe('2')
    expect(allById('llm-acc-month')[0].text()).toBe('¥12.34')
    expect(allById('llm-acc-unsettled')[0].text()).toBe('¥0.02')
    expect(allById('llm-acc-debt-amount')[1].text()).toBe('¥3.00')
    expect(allById('llm-acc-limit-rpm')[0].text()).toBe('RPM 60（默认）')
    expect(allById('llm-acc-limit-tpm')[0].text()).toBe('TPM 500,000')
    expect(allById('llm-acc-limit-rpm')[1].text()).toBe('RPM 10')
    expect(allById('llm-acc-blocked-tag').map((t) => t.text())).toEqual(['正常', '已停用'])
  })

  it('搜索 + 欠费 / 停用筛选：点查询才生效', async () => {
    await mountTab()
    await typeInBody('llm-acc-q', '１３８ 00')
    await wrapper!.findComponent({ name: 'ElCheckbox' }).find('input').setValue(true)
    expect(accountCalls()).toHaveLength(1)
    await wrapper!.find('form').trigger('submit')
    await flushPromises()
    expect(accountCalls().at(-1)![1]).toMatchObject({ q: '13800', debt: 'true', page: 1 })
  })

  it('?user_id= 按精确用户 ID 筛选并自动展开该用户的 Key；关闭标签回到全部', async () => {
    await mountTab('/llm?tab=accounts&user_id=42')
    expect(accountCalls()[0][1]).toMatchObject({ q: undefined, user_id: '42' })
    expect(byId('llm-acc-user-tag').text()).toContain('42')
    expect(get.mock.calls.filter((c) => c[0] === '/llm/keys')[0][1]).toEqual({ user_id: 42 })
    expect(inBody('llm-acc-keys')?.dataset.user).toBe('42')
    expect(allById('llm-key-cost')[0].text()).toBe('¥0.0125')
    await byId('llm-acc-user-tag').find('.el-tag__close').trigger('click')
    await flushPromises()
    expect(accountCalls().at(-1)![1]).toMatchObject({ user_id: undefined })
  })

  it('Key 本月消费：month_ym 不是当前月显示 ¥0.00；有 month_cost 时优先', async () => {
    get.mockImplementation(async (path: string) => {
      if (path === '/llm/accounts') return { items: accounts, total: accounts.length }
      if (path === '/llm/keys') return { items: [key({ month_ym: '2000-01' }), key({ id: 8, month_cost: 250 })] }
      throw new Error(`unexpected GET ${path}`)
    })
    await mountTab('/llm?tab=accounts&user_id=42')
    expect(allById('llm-key-cost').map((x) => x.text())).toEqual(['¥0.00', '¥2.50'])
  })

  it('finance：只读（无编辑 / 停用），可展开查看 Key 但不能停用，有用量链接', async () => {
    login('finance')
    await mountTab()
    expect(byId('llm-acc-edit-limits').exists()).toBe(false)
    expect(byId('llm-acc-block').exists()).toBe(false)
    await wrapper!.find('.el-table__expand-icon').trigger('click')
    await flushPromises()
    expect(get.mock.calls.filter((c) => c[0] === '/llm/keys')).toHaveLength(1)
    expect(allById('llm-key-hint')).toHaveLength(2)
    expect(allById('llm-key-disable')).toHaveLength(0)
    expect(byId('llm-acc-usage-link').attributes('href')).toBe('/llm?tab=usage&user_id=42')
  })
})

describe('AccountsTab 操作（ops）', () => {
  it('编辑限额：清空 = null（恢复默认），未改的字段不发', async () => {
    await mountTab()
    await allById('llm-acc-edit-limits')[0].trigger('click')
    await flushPromises()
    await typeInBody('llm-limit-tpm', '')
    expect(inputInBody('llm-limit-tpm').placeholder).toBe('留空 = 默认 200,000') // 清空后提示平台默认值
    await typeInBody('llm-limit-rpm', '120')
    await click(inBody('llm-limits-submit'))
    expect(put).toHaveBeenCalledTimes(1)
    expect(put.mock.calls[0][0]).toBe('/llm/accounts/42')
    expect(put.mock.calls[0][1]).toEqual({ rpm_limit: 120, tpm_limit: null })
    expect(accountCalls()).toHaveLength(2) // 成功后刷新
  })

  it('编辑限额：没有改动不发请求；越界显示错误', async () => {
    await mountTab()
    await allById('llm-acc-edit-limits')[0].trigger('click')
    await flushPromises()
    await click(inBody('llm-limits-submit'))
    expect(put).not.toHaveBeenCalled()
    await allById('llm-acc-edit-limits')[0].trigger('click')
    await flushPromises()
    await typeInBody('llm-limit-concurrency', '513')
    await click(inBody('llm-limits-submit'))
    expect(put).not.toHaveBeenCalled()
    expect(inBody('llm-limit-concurrency-error')?.textContent).toContain('1～512 的整数')
  })

  it('停用：原因必填；解除停用发 blocked=false', async () => {
    await mountTab()
    await allById('llm-acc-block')[0].trigger('click')
    await flushPromises()
    await click(inBody('llm-block-submit'))
    expect(inBody('llm-block-error')?.textContent).toBe('请填写停用原因')
    expect(put).not.toHaveBeenCalled()
    await typeInBody('llm-block-reason', ' 恶意调用 ')
    await click(inBody('llm-block-submit'))
    expect(put).toHaveBeenLastCalledWith('/llm/accounts/42', { blocked: true, blocked_reason: '恶意调用' })

    await byId('llm-acc-unblock').trigger('click')
    await flushPromises()
    await click(inBody('llm-unblock-ok'))
    expect(put).toHaveBeenLastCalledWith('/llm/accounts/43', { blocked: false })
  })

  it('展开行：Key 列表 + 停用（原因必填），提示用户可自行重新启用', async () => {
    await mountTab()
    await wrapper!.find('.el-table__expand-icon').trigger('click')
    await flushPromises()
    expect(get.mock.calls.filter((c) => c[0] === '/llm/keys')).toHaveLength(1)
    expect(inBody('llm-acc-keys')?.textContent).toContain('用户可自行重新启用')
    expect(allById('llm-key-hint').map((k) => k.text())).toEqual(['sk-…abcd', 'sk-…abcd'])
    expect(allById('llm-key-disable')).toHaveLength(1) // 已停用的 Key 没有按钮
    await allById('llm-key-disable')[0].trigger('click')
    await flushPromises()
    await click(inBody('llm-key-disable-submit'))
    expect(post).not.toHaveBeenCalled()
    await typeInBody('llm-key-disable-reason', '泄露')
    await click(inBody('llm-key-disable-submit'))
    expect(post).toHaveBeenCalledWith('/llm/keys/7/disable', { reason: '泄露' })
    expect(get.mock.calls.filter((c) => c[0] === '/llm/keys')).toHaveLength(2) // 停用后刷新 Key 列表
  })
})
