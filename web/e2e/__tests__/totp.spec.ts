import { describe, expect, it } from 'vitest'
import { base32Decode, hotp, totp } from '../helpers/totp'

const SECRET_B32 = 'GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ' // "12345678901234567890"

describe('totp helper (U-13)', () => {
  it('base32 解码', () => {
    expect(base32Decode(SECRET_B32).toString('ascii')).toBe('12345678901234567890')
    expect(base32Decode('gezd gnbv').toString('ascii')).toBe('12345')
  })
  it.each([
    [59, '94287082'],
    [1111111109, '07081804'],
    [1111111111, '14050471'],
    [1234567890, '89005924'],
    [2000000000, '69279037'],
    [20000000000, '65353130'],
  ])('T=%d -> %s', (t, want) => {
    expect(totp(SECRET_B32, t, 30, 8)).toBe(want)
  })
  it('默认 6 位、30 秒', () => {
    expect(totp(SECRET_B32, 59)).toBe('287082')
    expect(hotp(base32Decode(SECRET_B32), 1n)).toBe('287082')
  })
})
