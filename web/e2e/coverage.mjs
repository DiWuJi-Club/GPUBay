#!/usr/bin/env node
// 核对覆盖表：coverage.json 里的每个 ID 在 e2e/console、e2e/admin（含子目录）的 spec 中恰好出现一次；
// C- 只能出现在 console/、A- 只能出现在 admin/；没有未登记的 ID、没有不带 ID 的用例；
// test / describe 的任何变体都不得带 skip / fixme / only / fail / slow。
// 用 TypeScript AST 识别真实调用：注释与字符串里的 test(...) 不计入。
import { readFileSync, readdirSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import ts from 'typescript'

const ID_PREFIX = /^([CA]-[A-Z]+-\d{2})\b/
const FORBIDDEN = new Set(['skip', 'fixme', 'only', 'fail', 'slow'])
const DIR_OF = { C: 'console', A: 'admin' }

/** 把 test.describe.serial.only 这样的被调用表达式拆成 ['test', 'describe', 'serial', 'only']；不是纯标识符链则返回 null。 */
function calleeChain(expr) {
  const names = []
  while (ts.isPropertyAccessExpression(expr)) {
    names.unshift(expr.name.text)
    expr = expr.expression
  }
  if (!ts.isIdentifier(expr)) return null
  names.unshift(expr.text)
  return names
}

/** 字符串字面量 / 无插值模板返回全文；带插值模板返回开头的固定部分；其它返回 null。 */
function literalTitle(node) {
  if (!node) return null
  if (ts.isStringLiteral(node) || ts.isNoSubstitutionTemplateLiteral(node)) return node.text
  if (ts.isTemplateExpression(node)) return node.head.text
  return null
}

/** @param {{file: string, text: string}[]} specs @param {string[]} expected @returns {string[]} */
export function check(specs, expected) {
  const errors = []
  const seen = new Map()
  for (const { file, text } of specs) {
    const dir = file.split('/')[0]
    const sf = ts.createSourceFile(file, text, ts.ScriptTarget.Latest, false, ts.ScriptKind.TS)
    const visit = (node) => {
      if (ts.isCallExpression(node)) {
        const chain = calleeChain(node.expression)
        if (chain && ['test', 'it', 'describe'].includes(chain[0])) {
          const isDescribe = chain.includes('describe')
          for (const name of chain.slice(1)) if (FORBIDDEN.has(name)) errors.push(`${file}: 禁止 ${isDescribe ? 'describe' : 'test'}.${name}`)
          if (chain.length === 1 && chain[0] === 'test') {
            const title = literalTitle(node.arguments[0])
            const id = title === null ? undefined : ID_PREFIX.exec(title)?.[1]
            if (title === null) errors.push(`${file}: 用例标题必须是字符串字面量（第 ${sf.getLineAndCharacterOfPosition(node.getStart(sf)).line + 1} 行）`)
            else if (!id) errors.push(`${file}: 用例标题未以覆盖表 ID 开头：「${title}」`)
            else {
              if (!expected.includes(id)) errors.push(`${file}: 未登记的 ID ${id}`)
              if (DIR_OF[id[0]] !== dir) errors.push(`${file}: ID ${id} 不属于 ${dir}/（C- 只能在 console/，A- 只能在 admin/）`)
              seen.set(id, (seen.get(id) ?? 0) + 1)
            }
          }
        }
      }
      ts.forEachChild(node, visit)
    }
    visit(sf)
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
    /** @type {string[]} */
    let names = []
    try {
      // 递归扫描子目录；统一用 / 分隔，便于按首段判断所属目录
      names = readdirSync(join(root, dir), { recursive: true, encoding: 'utf8' })
        .map((n) => n.split('\\').join('/'))
        .filter((n) => n.endsWith('.spec.ts'))
        .sort()
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
