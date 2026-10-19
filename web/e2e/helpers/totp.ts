import { createHmac } from 'node:crypto'

const ALPHABET = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567'

export function base32Decode(s: string): Buffer {
  const clean = s.toUpperCase().replace(/\s+/g, '').replace(/=+$/, '')
  let bits = 0
  let value = 0
  const out: number[] = []
  for (const ch of clean) {
    const idx = ALPHABET.indexOf(ch)
    if (idx < 0) throw new Error(`invalid base32 character: ${ch}`)
    value = (value << 5) | idx
    bits += 5
    if (bits >= 8) {
      out.push((value >>> (bits - 8)) & 0xff)
      bits -= 8
    }
  }
  return Buffer.from(out)
}

/** RFC 4226 HOTP（HMAC-SHA1）。 */
export function hotp(key: Buffer, counter: bigint, digits = 6): string {
  const msg = Buffer.alloc(8)
  msg.writeBigUInt64BE(counter)
  const h = createHmac('sha1', key).update(msg).digest()
  const off = h[h.length - 1] & 0x0f
  const code = ((h[off] & 0x7f) << 24) | (h[off + 1] << 16) | (h[off + 2] << 8) | h[off + 3]
  return String(code % 10 ** digits).padStart(digits, '0')
}

/** RFC 6238 TOTP：30 秒一步、6 位；时间取服务器假时钟（GET /hooks/clock）。 */
export function totp(secretBase32: string, unixSeconds: number, period = 30, digits = 6): string {
  return hotp(base32Decode(secretBase32), BigInt(Math.floor(unixSeconds / period)), digits)
}
