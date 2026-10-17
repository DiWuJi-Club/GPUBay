import { mount, RouterLinkStub } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import IdLink from '../IdLink.vue'

type Props = { kind: 'machine' | 'instance' | 'user'; id: string | number | null | undefined; disabled?: boolean }
const render = (props: Props) => mount(IdLink, { props, attrs: { 'data-testid': 'x' }, global: { stubs: { RouterLink: RouterLinkStub } } })

describe('IdLink', () => {
  it.each([null, undefined, ''])('id 为 %s 显示 —，不是链接', (id) => {
    const w = render({ kind: 'machine', id })
    expect(w.text()).toBe('—')
    expect(w.findComponent(RouterLinkStub).exists()).toBe(false)
    expect(w.attributes('data-testid')).toBe('x')
  })

  it.each<[Props['kind'], string | number, string]>([
    ['machine', 7, '/machines/7'],
    ['instance', 'i-abc', '/instances/i-abc'],
    ['user', 0, '/users/0'],
  ])('%s 链接到详情', (kind, id, to) => {
    const w = render({ kind, id })
    const link = w.findComponent(RouterLinkStub)
    expect(link.props('to')).toBe(to)
    expect(link.text()).toBe(String(id))
  })

  it('disabled 只显示 ID', () => {
    const w = render({ kind: 'machine', id: 9, disabled: true })
    expect(w.text()).toBe('9')
    expect(w.findComponent(RouterLinkStub).exists()).toBe(false)
    expect(w.attributes('data-testid')).toBe('x')
  })
})
