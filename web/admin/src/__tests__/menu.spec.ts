import { describe, expect, it } from 'vitest'
import { menu, menuFor } from '../menu'
import { routes } from '../router'

describe('menuFor (U-12)', () => {
  it.each([
    ['super', 13, []],
    ['ops', 8, ['orders', 'ledger', 'recharges', 'catalog', 'admins']],
    ['finance', 8, ['machines', 'pdus', 'kyc', 'catalog', 'admins']],
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
})
