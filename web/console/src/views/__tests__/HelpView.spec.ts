import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'

/** 帮助页「AI 管家」一节与 get_help(butler) 的语料 internal/butler/help/butler.md 同文（两边手工维护，这里防漂移）。 */
const read = (rel: string) => readFileSync(fileURLToPath(new URL(rel, import.meta.url)), 'utf8')

function mdItems(md: string): string[] {
  return md
    .split('\n')
    .filter((l) => l.startsWith('- '))
    .map((l) => l.slice(2).replace(/\*\*/g, '').trim())
}

function vueItems(vue: string): string[] {
  const at = vue.indexOf('data-testid="help-butler"')
  const section = vue.slice(at, vue.indexOf('</el-card>', at))
  return [...section.matchAll(/<li>([\s\S]*?)<\/li>/g)].map((m) => m[1].replace(/<[^>]+>/g, '').trim())
}

describe('HelpView AI 管家章节', () => {
  it('与 internal/butler/help/butler.md 逐条一致', () => {
    const md = mdItems(read('../../../../../internal/butler/help/butler.md'))
    const vue = vueItems(read('../HelpView.vue'))
    expect(md.length).toBeGreaterThanOrEqual(5)
    expect(vue).toEqual(md)
    expect(read('../HelpView.vue')).toContain('<h2 id="butler" data-testid="help-butler">AI 管家</h2>')
  })
})
