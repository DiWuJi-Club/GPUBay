import { afterEach, describe, expect, it, vi } from 'vitest'
import { clearInvite, INVITE_KEY, inviteToast, normalizeInviteCode, parseInviteParam, readInvite, storeInvite } from '../invite'

class MemStorage implements Storage {
  private m = new Map<string, string>()
  get length() {
    return this.m.size
  }
  clear() {
    this.m.clear()
  }
  getItem(k: string) {
    return this.m.get(k) ?? null
  }
  key(i: number) {
    return [...this.m.keys()][i] ?? null
  }
  removeItem(k: string) {
    this.m.delete(k)
  }
  setItem(k: string, v: string) {
    this.m.set(k, v)
  }
}

/** 隐私模式 / 配额满：任何读写都抛错 */
class ThrowingStorage extends MemStorage {
  getItem(): string | null {
    throw new Error('SecurityError')
  }
  setItem() {
    throw new Error('QuotaExceededError')
  }
  removeItem() {
    throw new Error('SecurityError')
  }
}

describe('invite code (Review Focus 5)', () => {
  it('规范化：空白、小写、O/I/L 混写；长度与字母表', () => {
    expect(normalizeInviteCode(' o1lI0Zy9 ')).toBe('01110ZY9')
    expect(normalizeInviteCode('abcdefgh')).toBe('ABCDEFGH')
    expect(normalizeInviteCode('ABCDEFG')).toBeNull()
    expect(normalizeInviteCode('ABCDEFGU')).toBeNull()
    expect(normalizeInviteCode('ABCD-EFG')).toBeNull()
    expect(normalizeInviteCode('')).toBeNull()
  })

  it('与后端 referral.NormalizeCode 的用例表一致（code_test.go）', () => {
    const cases: Record<string, string | null> = {
      ' ab0lio-z ': null, // 含 '-' 非法
      abcdefgh: 'ABCDEFGH',
      ' o1lI0Zy9 ': '01110ZY9',
      ABCDEFG: null, // 7 位
      ABCDEFGHJ: null, // 9 位
      'ABCD EFGH': 'ABCDEFGH',
      abcdefgu: null, // U 不在字母表
      '': null,
    }
    for (const [input, want] of Object.entries(cases)) expect(normalizeInviteCode(input), JSON.stringify(input)).toBe(want)
  })

  // 期望值由 Go 的 referral.NormalizeCode 实测得到；非 ASCII 字符用码点构造，避免源码里出现不可见字符
  const c = (cp: number) => String.fromCodePoint(cp)

  it('空白集合与 Go strings.Fields（unicode.IsSpace）一致：全角空格等去掉，零宽字符 / BOM 不算空白', () => {
    expect(normalizeInviteCode(`${c(0x3000)}abcd${c(0x3000)}efgh${c(0x3000)}`)).toBe('ABCDEFGH') // 全角空格
    expect(normalizeInviteCode('abcd\tefgh\n')).toBe('ABCDEFGH')
    expect(normalizeInviteCode('ab\r\ncd\v\fefgh')).toBe('ABCDEFGH')
    expect(normalizeInviteCode(`abcd${c(0xa0)}efgh`)).toBe('ABCDEFGH') // NBSP
    expect(normalizeInviteCode(`abcd${c(0x85)}efgh`)).toBe('ABCDEFGH') // NEL：Go 算空白，JS 的 \s 不算
    expect(normalizeInviteCode(`abcd${c(0x2003)}efgh`)).toBe('ABCDEFGH') // EM SPACE
    expect(normalizeInviteCode(`abcd${c(0x2028)}efgh`)).toBe('ABCDEFGH') // LINE SEPARATOR
    expect(normalizeInviteCode(`abcd${c(0xfeff)}efgh`)).toBeNull() // BOM：JS 的 \s 算空白，Go 不算
    expect(normalizeInviteCode(`abcd${c(0x200b)}efgh`)).toBeNull() // 零宽空格
  })

  it('大写按单字符映射（与 Go strings.ToUpper 一致）：ß / ﬀ 不展开，ı / ſ 映射到 I / S', () => {
    expect(normalizeInviteCode(`abcdef${c(0xdf)}`)).toBeNull() // ß：JS toUpperCase 得 'SS'（凑满 8 位），Go 保持 ß
    expect(normalizeInviteCode(`${c(0xfb00)}bcdefg`)).toBeNull() // ﬀ：JS 得 'FF'，Go 保持
    expect(normalizeInviteCode(`${c(0x131)}bcdefgh`)).toBe('1BCDEFGH') // 无点 ı → I → 1
    expect(normalizeInviteCode(`${c(0x17f)}bcdefgh`)).toBe('SBCDEFGH') // 长 s ſ → S
    expect(normalizeInviteCode(`ABCDEFG${c(0xff28)}`)).toBeNull() // 全角 Ｈ
    expect(normalizeInviteCode(`abcdefg${c(0x212a)}`)).toBeNull() // 开尔文符号 K
    expect(normalizeInviteCode('abcdefgo')).toBe('ABCDEFG0')
    expect(normalizeInviteCode('abcdefgl')).toBe('ABCDEFG1')
  })

  it('parseInviteParam 只认合法的 ic', () => {
    expect(parseInviteParam('?ic=abcdefgh&x=1')).toBe('ABCDEFGH')
    expect(parseInviteParam('?ic=hello%20world')).toBeNull()
    expect(parseInviteParam('?other=1')).toBeNull()
    expect(parseInviteParam('')).toBeNull()
    expect(parseInviteParam('?ic=')).toBeNull()
    expect(parseInviteParam('?ic=abcd%E3%80%80efgh')).toBe('ABCDEFGH') // 全角空格
  })

  it('存储 30 天过期', () => {
    const s = new MemStorage()
    storeInvite('ABCDEFGH', 1_000, s)
    expect(JSON.parse(s.getItem(INVITE_KEY)!)).toEqual({ code: 'ABCDEFGH', at: 1_000 })
    expect(readInvite(1_000 + 29 * 86_400_000, s)).toBe('ABCDEFGH')
    expect(readInvite(1_000 + 30 * 86_400_000, s)).toBe('ABCDEFGH') // 恰好 30 天仍有效
    expect(readInvite(1_000 + 31 * 86_400_000, s)).toBeNull()
    expect(s.getItem('sc_invite')).toBeNull() // 过期即清
    storeInvite('ABCDEFGH', 1_000, s)
    clearInvite(s)
    expect(readInvite(2_000, s)).toBeNull()
    s.setItem('sc_invite', '{broken')
    expect(readInvite(2_000, s)).toBeNull()
    expect(s.getItem('sc_invite')).toBeNull()
    s.setItem('sc_invite', JSON.stringify({ code: 'ABCD-EFG', at: 1_000 }))
    expect(readInvite(2_000, s)).toBeNull() // 被改坏的码不返回
    s.setItem('sc_invite', 'null')
    expect(readInvite(2_000, s)).toBeNull()
  })

  it('存储不可用时不抛错', () => {
    const s = new ThrowingStorage()
    expect(() => storeInvite('ABCDEFGH', 1_000, s)).not.toThrow()
    expect(readInvite(1_000, s)).toBeNull()
    expect(() => clearInvite(s)).not.toThrow()
  })

  describe('缺省用 window.localStorage', () => {
    afterEach(() => {
      vi.restoreAllMocks()
      localStorage.clear()
    })

    it('不传 storage 时读写 localStorage', () => {
      storeInvite('ABCDEFGH', 1_000)
      expect(JSON.parse(localStorage.getItem(INVITE_KEY)!)).toEqual({ code: 'ABCDEFGH', at: 1_000 })
      expect(readInvite(2_000)).toBe('ABCDEFGH')
      clearInvite()
      expect(localStorage.getItem(INVITE_KEY)).toBeNull()
    })

    it('读取 window.localStorage 本身抛 SecurityError（站点数据被禁用）时不外泄', () => {
      vi.spyOn(window, 'localStorage', 'get').mockImplementation(() => {
        throw new DOMException('denied', 'SecurityError')
      })
      expect(() => storeInvite('ABCDEFGH', 1_000)).not.toThrow()
      expect(readInvite(1_000)).toBeNull()
      expect(() => clearInvite()).not.toThrow()
    })
  })

  it('提示文案', () => {
    expect(inviteToast({ bound: true, reason: 'ok', inviter: '138****1234' })).toEqual({ type: 'success', message: '已绑定邀请关系' })
    expect(inviteToast({ bound: false, reason: 'existing_user' })).toEqual({ type: null, message: '' })
    expect(inviteToast({ bound: false, reason: 'invalid_code' })).toEqual({ type: 'warning', message: '邀请码无效' })
    expect(inviteToast({ bound: false, reason: 'inviter_frozen' })).toEqual({ type: 'warning', message: '邀请码无效' })
  })
})
