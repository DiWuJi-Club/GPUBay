import { describe, expect, it } from 'vitest'
import { menu, menuFor } from '../menu'
import { routes } from '../router'

describe('menuFor (U-12)', () => {
  it.each([
    ['super', 16, []],
    ['ops', 11, ['orders', 'ledger', 'recharges', 'catalog', 'admins']],
    ['finance', 10, ['machines', 'pdus', 'kyc', 'catalog', 'admins', 'groups']],
  ] as const)('%s 有 %d 项', (role, count, missing) => {
    const items = menuFor(role)
    expect(items).toHaveLength(count)
    for (const key of missing) expect(items.find((m) => m.key === key)).toBeUndefined()
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
  ] as const)('应用页 %s 对所有角色开放（写按钮按角色显示）', (path, roles) => {
    const route = routes.find((r) => r.path === path)
    expect(route, path).toBeDefined()
    expect([...(route!.meta?.roles ?? [])].sort()).toEqual([...roles].sort())
  })
})
