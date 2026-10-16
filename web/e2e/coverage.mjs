#!/usr/bin/env node
// 核对覆盖表：coverage.json 里的每个 ID 在 e2e/console、e2e/admin 的 spec 中恰好出现一次；
// 没有未登记的 ID、没有不带 ID 的用例、没有 test.skip / fixme / only、describe.skip / only。
import { readFileSync, readdirSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const ID_PREFIX = /^([CA]-[A-Z]+-\d{2})\b/

/** @param {{file: string, text: string}[]} specs @param {string[]} expected @returns {string[]} */
export function check(specs, expected) {
  const errors = []
  const seen = new Map()
  for (const { file, text } of specs) {
    for (const m of text.matchAll(/\b(?:test|it)\.(skip|fixme|only)\s*\(/g)) errors.push(`${file}: 禁止 test.${m[1]}`)
    for (const m of text.matchAll(/\bdescribe\.(skip|only)\s*\(/g)) errors.push(`${file}: 禁止 describe.${m[1]}`)
    for (const m of text.matchAll(/\btest\s*\(\s*(['"`])([^'"`\n]*)\1/g)) {
      const title = m[2]
      const id = ID_PREFIX.exec(title)?.[1]
      if (!id) {
        errors.push(`${file}: 用例标题未以覆盖表 ID 开头：「${title}」`)
        continue
      }
      if (!expected.includes(id)) errors.push(`${file}: 未登记的 ID ${id}`)
      seen.set(id, (seen.get(id) ?? 0) + 1)
    }
  }
  for (const id of expected) if (!seen.has(id)) errors.push(`缺少用例：${id}`)
  for (const [id, n] of seen) if (n > 1) errors.push(`ID ${id} 出现了 ${n} 次（每行覆盖表只对应一个用例）`)
  return errors
}

function main() {
  const root = dirname(fileURLToPath(import.meta.url))
  const table = JSON.parse(readFileSync(join(root, 'coverage.json'), 'utf8'))
  const expected = [...table.console, ...table.admin]
  if (new Set(expected).size !== expected.length) {
    console.error('coverage.json 含重复 ID')
    process.exit(1)
  }
  const specs = []
  for (const dir of ['console', 'admin']) {
    let names = []
    try {
      names = readdirSync(join(root, dir)).filter((n) => n.endsWith('.spec.ts'))
    } catch {
      /* 目录尚未创建 */
    }
    for (const n of names) specs.push({ file: `${dir}/${n}`, text: readFileSync(join(root, dir, n), 'utf8') })
  }
  const errors = check(specs, expected)
  if (errors.length) {
    for (const e of errors) console.error('  ' + e)
    console.error(`覆盖表核对失败：${errors.length} 个问题`)
    process.exit(1)
  }
  console.log(`覆盖表核对通过：${expected.length} 个用例 ID（控制台 ${table.console.length}，后台 ${table.admin.length}）`)
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) main()
