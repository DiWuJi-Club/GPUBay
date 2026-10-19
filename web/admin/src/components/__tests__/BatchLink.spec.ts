import { mount, RouterLinkStub } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import BatchLink from '../BatchLink.vue'

const render = (props: { id: string | null | undefined; disabled?: boolean }) => mount(BatchLink, { props, global: { stubs: { RouterLink: RouterLinkStub } } })

describe('BatchLink', () => {
  it.each([null, undefined, ''])('批次为 %s 显示 —', (id) => {
    const w = render({ id })
    expect(w.text()).toBe('—')
    expect(w.findComponent(RouterLinkStub).exists()).toBe(false)
  })
  it('链接到按批次筛选的订单页', () => {
    const link = render({ id: 'b-1' }).findComponent(RouterLinkStub)
    expect(link.props('to')).toEqual({ path: '/orders', query: { batch_id: 'b-1' } })
    expect(link.text()).toBe('b-1')
  })
  it('disabled（运维无订单页权限）只显示批次号', () => {
    const w = render({ id: 'b-1', disabled: true })
    expect(w.text()).toBe('b-1')
    expect(w.findComponent(RouterLinkStub).exists()).toBe(false)
  })
})
