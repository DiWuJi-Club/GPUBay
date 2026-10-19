import { describe, expect, it } from 'vitest'
import { safeRedirect } from '../router'

describe('safeRedirect', () => {
  it('只接受站内相对路径', () => {
    expect(safeRedirect('/machines')).toBe('/machines')
    expect(safeRedirect('/users/3?tab=orders')).toBe('/users/3?tab=orders')
    expect(safeRedirect('//evil.com')).toBeNull()
    expect(safeRedirect('https://evil.com')).toBeNull()
    expect(safeRedirect('/\\evil.com')).toBeNull()
    expect(safeRedirect(['/a'])).toBeNull()
    expect(safeRedirect(undefined)).toBeNull()
  })
})
