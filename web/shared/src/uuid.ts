/**
 * 随机 UUID（RFC 4122 第 4 版）。crypto.randomUUID 只在安全上下文（https、localhost / 127.0.0.1）里存在：
 * 开发与验收用的 http://<域名或局域网 IP> 上没有它，此时用 crypto.getRandomValues（任何上下文都可用）自己拼。
 */
export function uuid(c: Pick<Crypto, 'getRandomValues'> & { randomUUID?: () => string } = globalThis.crypto): string {
  if (typeof c.randomUUID === 'function') return c.randomUUID()
  const b = c.getRandomValues(new Uint8Array(16))
  b[6] = (b[6] & 0x0f) | 0x40 // 版本 4
  b[8] = (b[8] & 0x3f) | 0x80 // 变体 10xx（RFC 4122）
  const h = Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('')
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`
}
