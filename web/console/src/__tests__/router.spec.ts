import { describe, expect, it } from 'vitest'
import { safeRedirect } from '../router'

describe('safeRedirect', () => {
  it('只接受站内相对路径', () => {
    expect(safeRedirect('/instances')).toBe('/instances')
    expect(safeRedirect('/kyc?redirect=/market')).toBe('/kyc?redirect=/market')
    expect(safeRedirect('//evil.com')).toBeNull()
    expect(safeRedirect('https://evil.com')).toBeNull()
    expect(safeRedirect(['/a'])).toBeNull()
    expect(safeRedirect(undefined)).toBeNull()
  })
})
