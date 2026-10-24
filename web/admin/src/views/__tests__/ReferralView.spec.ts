import { ApiError, type Admin, type AdminBinding } from '@spark/shared'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import ElementPlus from 'element-plus'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter } from 'vue-router'

const mocks = vi.hoisted(() => ({
  get: vi.fn(),
  post: vi.fn(),
  download: vi.fn(),
  msg: { call: vi.fn(), success: vi.fn(), warning: vi.fn(), error: vi.fn() },
}))
vi.mock('../../api', () => ({
  api: { get: mocks.get, post: mocks.post, download: mocks.download },
  onUnauthorized: vi.fn(),
  onTotpSetupRequired: vi.fn(),
}))
vi.mock('element-plus', async (orig) => {
  const m = await orig<typeof import('element-plus')>()
  const ElMessage = Object.assign((o: unknown) => mocks.msg.call(o), mocks.msg)
  return { ...m, ElMessage }
})

import { session } from '../../store/session'
import ReferralView from '../ReferralView.vue'

const binding = (over: Partial<AdminBinding> = {}): AdminBinding => ({
  id: 5, inviter_id: 10, invitee_id: 11, inviter_phone: '13800001234', invitee_phone: '13900005678', code: 'ABC234', status: 'held',
  risk_flags: ['same_device'], hold_reason: 'same_device', rate_version_id: 1, bound_at: '2026-10-20T00:00:00Z', rebate_until: '2027-10-20T00:00:00Z',
  ip: '1.2.3.4', device_id: 'dev-secret', user_agent: 'ua', reviewed_by: null, reviewed_at: null, review_note: '', pending: 690, contributed: 690,
  kyc_check_unresolved: false, ...over,
})
const rate = { id: 2, rate_bp: 1500, max_per_invitee: 50000, max_per_inviter_30d: 500000, hold_days: 7, period_days: 365, effective_from: '2026-10-01T00:00:00Z', note: '', created_by: 1, created_at: '2026-10-01T00:00:00Z' }

const inviterRow = { user_id: 10, code: 'ABC234', status: 'active', frozen_reason: '', debt: 0, created_at: '', phone: '13800001234', kyc_status: 'verified', user_status: 'active', invitees: 120, held_count: 0, pending: 0, settled_net: 0, last_bound_at: null }
let detail: Record<string, unknown> = {}
let heldItems: AdminBinding[] = []
function routeGet(path: string) {
  if (path === '/referral/summary') return Promise.resolve({ pending: 690, month_settled: 0, settled_net: 100, debt: 0, held_count: heldItems.length })
  if (path === '/referral/bindings') return Promise.resolve({ items: heldItems, total: heldItems.length })
  if (path === '/referral/config') return Promise.resolve({ current: rate, versions: [rate] })
  if (path === '/referral/inviters') return Promise.resolve({ items: [inviterRow], total: 1 })
  if (path === '/referral/inviters/10') return Promise.resolve(detail)
  return Promise.resolve({ items: [], total: 0 })
}

let wrapper: VueWrapper | null = null
async function render(role: Admin['role'], url: string) {
  session.admin = { id: 1, username: 'a', role, status: 'active', totp_confirmed: true, last_login_at: null, last_login_ip: '', created_at: '' }
  session.status = 'authed'
  const router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/referral', component: ReferralView }, { path: '/users/:id', component: { template: '<div />' } }] })
  await router.push(url)
  wrapper = mount(ReferralView, { attachTo: document.body, global: { plugins: [ElementPlus, router] } })
  await flushPromises()
}
const $ = (id: string) => document.querySelector<HTMLElement>(`[data-testid="${id}"]`)
const $$ = (id: string) => document.querySelectorAll<HTMLElement>(`[data-testid="${id}"]`)
async function click(id: string) {
  $(id)!.click()
  await flushPromises()
}
async function fill(id: string, v: string) {
  const el = $(id)!
  const input = (el.tagName === 'INPUT' ? el : el.querySelector('input')) as HTMLInputElement
  input.value = v
  input.dispatchEvent(new Event('input'))
  await flushPromises()
}

beforeEach(() => {
  heldItems = [binding()]
  detail = { account: inviterRow, phone: '13800001234', bindings: [binding({ status: 'active' })], bindings_total: 1, rewards: [], rewards_total: 0, events: [] }
  mocks.get.mockImplementation(routeGet)
  mocks.post.mockReset()
  Object.values(mocks.msg).forEach((f) => f.mockReset())
})
afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  document.body.innerHTML = ''
})

describe('ReferralView 角色', () => {
  it('finance：没有配置 Tab 与重扫；?tab=config 回到概览', async () => {
    await render('finance', '/referral?tab=config')
    expect($('ref-tab-config')).toBeNull()
    expect($('cfg-rescan')).toBeNull()
    expect($('ref-sum-pending')!.textContent).toBe('¥6.90')
    expect(mocks.get).not.toHaveBeenCalledWith('/referral/config')
  })
  it('super：配置 Tab 显示当前版本', async () => {
    await render('super', '/referral?tab=config')
    expect($('cfg-current-rate')!.textContent).toBe('15%')
    expect($('cfg-current-invitee')!.textContent).toBe('¥500.00')
    expect($$('cfg-version-row')).toHaveLength(1)
    // 表单以当前版本预填
    expect(($('cfg-rate') as HTMLInputElement).value).toBe('15')
    expect(($('cfg-invitee') as HTMLInputElement).value).toBe('500.00')
  })
})

describe('待审核', () => {
  it('只显示脱敏手机号，不显示设备 / IP', async () => {
    await render('finance', '/referral?tab=held')
    const row = $('held-row')!
    expect(row.textContent).toContain('138****1234')
    expect(row.textContent).toContain('139****5678')
    expect(row.textContent).not.toContain('13800001234')
    expect(row.textContent).not.toContain('dev-secret')
    expect($('held-flags')!.textContent).toContain('同设备')
    // ID 与手机号分开：按 ID 过滤行时只匹配 ID
    expect($('held-invitee')!.textContent).toBe('11')
    expect($('held-invitee-phone')!.textContent).toBe('139****5678')
    expect($('held-kyc-unresolved')).toBeNull()
  })

  it('实名核查未解决：显示警示，必须勾选已人工核查才能通过', async () => {
    heldItems = [binding({ kyc_check_unresolved: true })]
    mocks.post.mockResolvedValue({ ok: true })
    await render('finance', '/referral?tab=held')
    expect($('held-kyc-unresolved')!.textContent).toBe('实名核查未解决')
    await click('held-approve')
    expect($('held-approve-kyc-warning')).not.toBeNull()
    expect(($('held-approve-ok') as HTMLButtonElement).disabled).toBe(true)
    $('held-approve-ok')!.click()
    await flushPromises()
    expect(mocks.post).not.toHaveBeenCalled()
    $('held-approve-kyc-ack')!.querySelector('input')!.click()
    await flushPromises()
    expect(($('held-approve-ok') as HTMLButtonElement).disabled).toBe(false)
    await fill('held-approve-note', '家人')
    await click('held-approve-ok')
    expect(mocks.post).toHaveBeenCalledWith('/referral/bindings/5/approve', { note: '家人' })
    expect(mocks.msg.success).toHaveBeenCalledWith('已通过')
  })

  it('无核查问题时直接可通过', async () => {
    mocks.post.mockResolvedValue({ ok: true })
    await render('finance', '/referral?tab=held')
    await click('held-approve')
    expect($('held-approve-kyc-ack')).toBeNull()
    await click('held-approve-ok')
    expect(mocks.post).toHaveBeenCalledWith('/referral/bindings/5/approve', { note: '' })
  })

  it('撤销：原因必填；成功提示作废金额', async () => {
    mocks.post.mockResolvedValue({ voided_amount: 690 })
    await render('finance', '/referral?tab=held')
    await click('held-revoke')
    await click('held-revoke-ok')
    expect($('held-revoke-error')!.textContent).toBe('请填写撤销原因')
    expect(mocks.post).not.toHaveBeenCalled()
    await fill('held-revoke-reason', ' 刷单 ')
    await click('held-revoke-ok')
    expect(mocks.post).toHaveBeenCalledWith('/referral/bindings/5/revoke', { reason: '刷单' })
    expect(mocks.msg.success).toHaveBeenCalledWith('已撤销，作废待发放 ¥6.90')
  })

  it('动态码弹框取消：静默，不报错', async () => {
    mocks.post.mockRejectedValue(new ApiError(0, 'cancelled', '已取消'))
    await render('finance', '/referral?tab=held')
    await click('held-revoke')
    await fill('held-revoke-reason', '刷单')
    await click('held-revoke-ok')
    expect(mocks.msg.error).not.toHaveBeenCalled()
  })
})

describe('配置与重扫（super）', () => {
  it('新增版本：% → bp、元 → 分', async () => {
    mocks.post.mockResolvedValue(rate)
    await render('super', '/referral?tab=config')
    await fill('cfg-rate', '12.5')
    await fill('cfg-invitee', '300')
    await fill('cfg-note', 'n')
    await click('cfg-submit')
    expect(mocks.post).toHaveBeenCalledWith('/referral/config', { rate_bp: 1250, max_per_invitee: 30000, max_per_inviter_30d: 500000, hold_days: 7, period_days: 365, note: 'n' })
    expect(mocks.msg.success).toHaveBeenCalledWith('已新增版本')
  })

  it('referral_config_invalid 原文提示', async () => {
    mocks.post.mockRejectedValue(new ApiError(400, 'referral_config_invalid', '生效时间不能早于当前时间'))
    await render('super', '/referral?tab=config')
    await click('cfg-submit')
    expect(mocks.msg.error).toHaveBeenCalledWith('生效时间不能早于当前时间')
  })

  it('生效时间输入框带 cfg-effective', async () => {
    await render('super', '/referral?tab=config')
    expect($('cfg-effective')!.tagName).toBe('INPUT')
  })

  const rescanOut = (over: Record<string, unknown> = {}) => ({ watermark: '2026-10-17T00:00:00Z', processed: 5, failed: 0, released: 2, incomplete: false, ...over })
  it.each([
    [rescanOut(), { type: 'success', message: '已重扫：处理 5 行，发放 2 笔' }],
    [rescanOut({ failed: 4 }), { type: 'warning', message: '已重扫，4 行仍失败（处理 5 行，发放 2 笔）' }],
    [rescanOut({ incomplete: true }), { type: 'warning', message: '已重扫，但本轮未全部完成（处理 5 行，发放 2 笔），之后的清扫会继续' }],
  ])('重扫 200 %o → %o', async (out, msg) => {
    mocks.post.mockResolvedValue(out)
    await render('super', '/referral?tab=config')
    await click('cfg-rescan')
    expect(mocks.post).toHaveBeenCalledWith('/referral/rescan', { days: 3 })
    expect(mocks.msg.call).toHaveBeenCalledWith(msg)
  })

  it('重扫返回错误（什么都没发生）：按原文报错，不当成已重扫', async () => {
    mocks.post.mockRejectedValue(new ApiError(500, 'internal', '服务器内部错误', 'req-1'))
    await render('super', '/referral?tab=config')
    await click('cfg-rescan')
    expect(mocks.msg.error).toHaveBeenCalledWith('服务器内部错误（请求号 req-1）')
    expect(mocks.msg.call).not.toHaveBeenCalled()
    expect(mocks.msg.warning).not.toHaveBeenCalled()
  })

  it('重扫天数越界不发请求', async () => {
    await render('super', '/referral?tab=config')
    await fill('cfg-rescan-days', '31')
    await click('cfg-rescan')
    expect($('cfg-rescan-error')!.textContent).toBe('天数需为 1～30 的整数')
    expect(mocks.post).not.toHaveBeenCalled()
  })
})

describe('邀请人', () => {
  it('行可用键盘打开抽屉（tabindex + role=button + Enter）', async () => {
    await render('finance', '/referral?tab=inviters')
    const row = $('inv-row')!
    expect(row.getAttribute('tabindex')).toBe('0')
    expect(row.getAttribute('role')).toBe('button')
    row.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter' }))
    await flushPromises()
    expect(mocks.get).toHaveBeenCalledWith('/referral/inviters/10')
    expect($$('inv-binding-row')).toHaveLength(1)
    expect($('inv-d-bindings-more')).toBeNull()
    expect($('inv-d-rewards-more')).toBeNull()
  })

  it('全量多于抽屉显示的条数：提示总数，返利跳到按邀请人筛选的明细，绑定可导出全部', async () => {
    detail = { ...detail, bindings_total: 120, rewards: [], rewards_total: 300 }
    mocks.download.mockResolvedValue({ blob: new Blob(['x']), filename: 'b.csv' })
    URL.createObjectURL = vi.fn(() => 'blob:x')
    URL.revokeObjectURL = vi.fn()
    await render('finance', '/referral?tab=inviters')
    await click('inv-row')
    expect($('inv-d-bindings-more')!.textContent).toContain('共 120 个，仅显示最近 1 个')
    expect($('inv-d-rewards-more')!.textContent).toContain('共 300 条，仅显示最近 0 条')
    await click('inv-d-bindings-export')
    expect(mocks.download).toHaveBeenCalledWith('/referral/bindings/export', { inviter_id: 10 })
    await click('inv-d-rewards-all')
    expect(mocks.get).toHaveBeenCalledWith('/referral/rewards', expect.objectContaining({ inviter_id: '10', page: 1 }))
    expect(($('rw-inviter') as HTMLInputElement).value).toBe('10')
  })
})

describe('概览', () => {
  it('待审核数可用键盘切到待审核 Tab', async () => {
    await render('finance', '/referral')
    const card = $('ref-sum-held-link')!
    expect(card.getAttribute('tabindex')).toBe('0')
    expect(card.getAttribute('role')).toBe('button')
    card.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter' }))
    await flushPromises()
    expect(mocks.get).toHaveBeenCalledWith('/referral/bindings', expect.objectContaining({ status: 'held' }))
  })
})
