import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { describe, expect, it } from 'vitest'
import { check } from '../coverage.mjs'

const table = JSON.parse(readFileSync(join(__dirname, '..', 'coverage.json'), 'utf8')) as { console: string[]; admin: string[] }

describe('coverage.json', () => {
  it('与设计 §7.5 的数量一致且无重复', () => {
    expect(table.console).toHaveLength(52)
    expect(table.admin).toHaveLength(50)
    expect(new Set([...table.console, ...table.admin]).size).toBe(102)
  })
})

describe('check()', () => {
  const ok = { file: 'console/a.spec.ts', text: "test('C-NAV-01 响应式布局', async () => {})\ntest('C-NAV-02 弹窗', async () => {})" }
  it('全部出现且无违规', () => {
    expect(check([ok], ['C-NAV-01', 'C-NAV-02'])).toEqual([])
  })
  it('缺少、未登记、重复', () => {
    const errors = check([ok, { file: 'console/b.spec.ts', text: "test('C-NAV-02 again', () => {})\ntest('C-XXX-99 x', () => {})" }], ['C-NAV-01', 'C-NAV-02', 'C-NAV-03'])
    expect(errors).toContain('缺少用例：C-NAV-03')
    expect(errors).toContain('console/b.spec.ts: 未登记的 ID C-XXX-99')
    expect(errors.some((e) => e.includes('C-NAV-02 出现了 2 次'))).toBe(true)
  })
  it('skip / fixme / only 与无 ID 标题', () => {
    const errors = check([{ file: 'admin/x.spec.ts', text: "test.skip('A-AUTH-01 a', () => {})\ntest.only('A-AUTH-02 b', () => {})\ntest.describe.only('g', () => {})\ntest('no id here', () => {})" }], ['A-AUTH-01', 'A-AUTH-02'])
    expect(errors).toContain('admin/x.spec.ts: 禁止 test.skip')
    expect(errors).toContain('admin/x.spec.ts: 禁止 test.only')
    expect(errors).toContain('admin/x.spec.ts: 禁止 describe.only')
    expect(errors.some((e) => e.includes('未以覆盖表 ID 开头'))).toBe(true)
  })
})
