import { generateKeyPairSync } from 'node:crypto'
import type { Page } from '@playwright/test'

/** 在 Node 里新生成一把 ed25519 公钥的 authorized_keys 行（SPKI DER 的最后 32 字节即公钥 → OpenSSH wire 格式）。 */
export function freshPublicKey(comment?: string): string {
  const { publicKey } = generateKeyPairSync('ed25519')
  const der = publicKey.export({ type: 'spki', format: 'der' })
  const raw = der.subarray(der.length - 32)
  const type = Buffer.from('ssh-ed25519')
  const len = (b: Buffer) => {
    const l = Buffer.alloc(4)
    l.writeUInt32BE(b.length)
    return l
  }
  const line = `ssh-ed25519 ${Buffer.concat([len(type), type, len(raw), raw]).toString('base64')}`
  return comment ? `${line} ${comment}` : line
}

/** 页面上应当原样显示为文本的 HTML 注入载荷（≤ 32 字符，放得进端口备注）：若被当作 HTML 渲染，onerror 会把 window.__xss 置 1。 */
export const XSS = '<img src=x onerror=__xss=1>'

/** 当前页面是否执行过 XSS 载荷。 */
export const xssFired = (page: Page) => page.evaluate(() => (window as unknown as { __xss?: number }).__xss === 1)
