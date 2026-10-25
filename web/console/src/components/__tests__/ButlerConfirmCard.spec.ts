import type { PendingToolCall } from '@spark/shared'
import { mount, type VueWrapper } from '@vue/test-utils'
import ElementPlus from 'element-plus'
import { afterEach, beforeEach, describe, expect, it, vi, type MockInstance } from 'vitest'
import { nextTick } from 'vue'

import ButlerConfirmCard from '../ButlerConfirmCard.vue'

const card: PendingToolCall = {
  id: 'tc-1',
  tool: 'renew_instance',
  label: '续费实例',
  summary: '续费实例 ins-abc\n时长：1 天',
  quote_amount: 9900,
  args: { instance_id: 'ins-abc' },
  expires_at: '2000-01-01T00:00:00Z',
  expires_in: 600,
  danger: false,
}

let wrapper: VueWrapper | null = null
let drift: MockInstance<() => number>
const $ = (id: string) => wrapper!.find(`[data-testid="${id}"]`)

function render(c: PendingToolCall) {
  wrapper = mount(ButlerConfirmCard, { props: { card: c, state: 'pending' }, global: { plugins: [ElementPlus] } })
  return wrapper
}

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['Date', 'setInterval', 'clearInterval'] })
  vi.setSystemTime(new Date('2026-10-25T08:00:00Z'))
  // 每次读钟都前进 1ms：模拟负载下两次 Date.now() 之间跨过毫秒边界；倒计时只能读一次钟
  const fakeNow = Date.now.bind(Date)
  let reads = 0
  drift = vi.spyOn(Date, 'now').mockImplementation(() => fakeNow() + reads++)
})
afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  drift.mockRestore()
  vi.useRealTimers()
})

describe('ButlerConfirmCard 倒计时（时钟每次读取都前进 1ms）', () => {
  it('expires_in = 600 → 正好 10:00，走 1 秒后 09:59', async () => {
    render(card)
    expect($('butler-confirm-countdown').text()).toBe('10:00 内有效')
    await vi.advanceTimersByTimeAsync(1_000)
    await nextTick()
    expect($('butler-confirm-countdown').text()).toBe('09:59 内有效')
  })

  it('expires_in = 0 → 挂载即已失效并 emit expire', () => {
    const w = render({ ...card, expires_in: 0 })
    expect($('butler-confirm-countdown').exists()).toBe(false)
    expect($('butler-confirm-result').text()).toBe('已失效')
    expect(w.emitted('expire')).toHaveLength(1)
  })

  it('换一张卡（ticker 已过期若干毫秒）→ 按新卡的 expires_in 正好重新计时', async () => {
    const w = render(card)
    await vi.advanceTimersByTimeAsync(400) // ticker 的 now 停在挂载时，真实时钟已前进
    await w.setProps({ card: { ...card, id: 'tc-2', expires_in: 300 } })
    expect($('butler-confirm-countdown').text()).toBe('05:00 内有效')
    await w.setProps({ card: { ...card, id: 'tc-3', expires_in: 0 } })
    expect($('butler-confirm-result').text()).toBe('已失效')
    expect(w.emitted('expire')).toHaveLength(1)
  })
})
