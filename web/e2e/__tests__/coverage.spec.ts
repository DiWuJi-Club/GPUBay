import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { describe, expect, it } from 'vitest'
import { check } from '../coverage.mjs'

const table = JSON.parse(readFileSync(join(__dirname, '..', 'coverage.json'), 'utf8')) as { console: string[]; admin: string[] }

describe('coverage.json', () => {
  it('与设计 §7.5 的数量一致且无重复', () => {
    // + 计划 3 的 C-DET-11～16、C-KEY-01～05、C-ING-01～06；+ 计划 6 的 C-BKG-01～11、C-DET-17～25、C-INS-05～07；
    // + 计划 7 的 C-MKT-09～12、C-APP-01～10、C-ING-07；+ 计划 8 的 C-REF-01～08、C-ACC-03、C-WAL-05；+ 计划 11 的 C-WF-01～12、C-MKT-13；
    // + 计划 9 的 C-TOK-01～10、C-WAL-06、C-ACC-04
    expect(table.console).toHaveLength(143)
    // 计划 5 的 50 个 + 计划 2 追加 A-MCH-07～09 + 计划 3 追加 A-INS-10～12、A-USR-07；+ 计划 6 的 A-GRP-01～05、A-BKG-01～06、A-INS-13；
    // + 计划 7 的 A-APP-01～08、A-INS-14、A-MCH-10；+ 计划 8 的 A-REF-01～07、A-FIN-05；+ 计划 11 的 A-WF-01～07、A-INS-15；
    // + 计划 9 的 A-LLM-01～09、A-INS-16、A-USR-08
    expect(table.admin).toHaveLength(106)
    expect(new Set([...table.console, ...table.admin]).size).toBe(249)
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
  it('注释与字符串里的 ID / 禁用写法不计入', () => {
    const text = [
      "// test('C-NAV-01 注释里的用例', () => {})",
      "/* test('C-NAV-02 块注释', () => {})",
      "   test.only('C-NAV-02 x', () => {}) */",
      "const s = \"test.skip('C-NAV-02 字符串里')\"",
      "const t = `test.describe.only('g', () => {})`",
    ].join('\n')
    const errors = check([{ file: 'console/c.spec.ts', text }], ['C-NAV-01', 'C-NAV-02'])
    expect(errors).toEqual(['缺少用例：C-NAV-01', '缺少用例：C-NAV-02'])
  })
  it('真实调用即使与注释同行也计入', () => {
    const text = "test('C-NAV-01 a', async () => {}) // test('C-NAV-02 b')"
    expect(check([{ file: 'console/d.spec.ts', text }], ['C-NAV-01', 'C-NAV-02'])).toEqual(['缺少用例：C-NAV-02'])
  })
  it('ID 必须位于对应目录：C- 在 console/，A- 在 admin/', () => {
    const errors = check(
      [
        { file: 'admin/wrong.spec.ts', text: "test('C-NAV-01 放错目录', () => {})" },
        { file: 'console/wrong.spec.ts', text: "test('A-AUTH-01 放错目录', () => {})" },
        { file: 'console/sub/nested.spec.ts', text: "test('C-NAV-02 子目录', () => {})" },
      ],
      ['C-NAV-01', 'C-NAV-02', 'A-AUTH-01'],
    )
    expect(errors).toContain('admin/wrong.spec.ts: ID C-NAV-01 不属于 admin/（C- 只能在 console/，A- 只能在 admin/）')
    expect(errors).toContain('console/wrong.spec.ts: ID A-AUTH-01 不属于 console/（C- 只能在 console/，A- 只能在 admin/）')
    expect(errors).toHaveLength(2)
  })
  it('禁止 skip / fixme / only / fail / slow 的所有变体', () => {
    const text = [
      "test.fail('C-NAV-01 a', () => {})",
      "test('C-NAV-02 b', () => { test.slow() })",
      "test.describe.serial.only('g1', () => {})",
      "test.describe.fixme('g2', () => {})",
      "test.describe.parallel.skip('g3', () => {})",
      "describe.skip('g4', () => {})",
      "test.fixme('C-NAV-03 c', () => {})",
    ].join('\n')
    const errors = check([{ file: 'console/e.spec.ts', text }], ['C-NAV-01', 'C-NAV-02', 'C-NAV-03'])
    for (const want of [
      'console/e.spec.ts: 禁止 test.fail',
      'console/e.spec.ts: 禁止 test.slow',
      'console/e.spec.ts: 禁止 describe.only',
      'console/e.spec.ts: 禁止 describe.fixme',
      'console/e.spec.ts: 禁止 describe.skip',
      'console/e.spec.ts: 禁止 test.fixme',
    ])
      expect(errors).toContain(want)
    expect(errors.filter((e) => e.includes('禁止 describe.skip'))).toHaveLength(2)
  })
  it('标题不是字符串字面量时报错', () => {
    const errors = check([{ file: 'console/f.spec.ts', text: "const id = 'C-NAV-01'\ntest(id + ' x', () => {})" }], ['C-NAV-01'])
    expect(errors.some((e) => e.includes('标题必须是字符串字面量'))).toBe(true)
  })
})
