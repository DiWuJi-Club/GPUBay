import { describe, expect, it } from 'vitest'
import { menu, menuFor } from '../menu'
import { routes } from '../router'

describe('menuFor (U-12)', () => {
  it.each([
    ['super', 18, []],
    ['ops', 13, ['orders', 'ledger', 'recharges', 'catalog', 'admins']],
    ['finance', 12, ['machines', 'pdus', 'kyc', 'catalog', 'admins', 'groups']],
  ] as const)('%s 有 %d 项', (role, count, missing) => {
    const items = menuFor(role)
    expect(items).toHaveLength(count)
    for (const key of missing) expect(items.find((m) => m.key === key)).toBeUndefined()
  })

  it('「应用部署」紧跟「应用」', () => {
    const keys = menu.map((m) => m.key)
    expect(keys.indexOf('deployments')).toBe(keys.indexOf('apps') + 1)
  })

  it('菜单与路由 roles 一致（表驱动）', () => {
    for (const item of menu) {
      const route = routes.find((r) => r.path === item.path)
      expect(route, item.path).toBeDefined()
      expect([...(route!.meta?.roles ?? [])].sort()).toEqual([...item.roles].sort())
    }
  })

  it.each([
    ['/apps', ['super', 'ops', 'finance']],
    ['/apps/:id', ['super', 'ops', 'finance']],
    ['/apps/:id/versions/:vid', ['super', 'ops', 'finance']],
    ['/deployments', ['super', 'ops', 'finance']],
  ] as const)('应用页 %s 对所有角色开放（写按钮按角色显示）', (path, roles) => {
    const route = routes.find((r) => r.path === path)
    expect(route, path).toBeDefined()
    expect([...(route!.meta?.roles ?? [])].sort()).toEqual([...roles].sort())
  })

  it('「Token 商城」对所有角色开放（Tab 与写操作按角色显示）', () => {
    const item = menu.find((m) => m.key === 'llm')
    expect(item).toMatchObject({ path: '/llm', label: 'Token 商城' })
    expect([...item!.roles].sort()).toEqual(['finance', 'ops', 'super'])
    for (const role of ['super', 'ops', 'finance'] as const) expect(menuFor(role).some((m) => m.key === 'llm')).toBe(true)
  })
})
