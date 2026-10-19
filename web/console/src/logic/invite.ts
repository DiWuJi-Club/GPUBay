import type { InviteResult } from '@spark/shared'

export const INVITE_KEY = 'sc_invite'
/** 落地参数在本地保留 30 天（R7） */
export const INVITE_TTL_MS = 30 * 86_400_000
const CODE_LEN = 8
const ALPHABET = '0123456789ABCDEFGHJKMNPQRSTVWXYZ' // Crockford Base32，无 I L O U

/**
 * Go unicode.IsSpace 的字符集（strings.Fields 按它切分）。不用 JS 的 \s：它多了 U+FEFF、少了 U+0085，
 * 前后端对同一输入的判定会不一致。
 */
const GO_SPACE = /[\t\n\v\f\r \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]/g

/**
 * 逐字符转大写，只接受单字符结果（对应 Go 的 unicode.ToUpper 简单映射）：
 * JS 的 toUpperCase 会把 ß、ﬀ 等展开成多个字母，可能凑出后端不认的「合法」码。
 */
function toUpperSimple(s: string): string {
  let out = ''
  for (const ch of s) {
    const u = ch.toUpperCase()
    out += [...u].length === 1 ? u : ch
  }
  return out
}

/** 与后端 referral.NormalizeCode 同规则：去空白、大写、O→0、I/L→1；8 位且全在字母表内才合法，否则返回 null。 */
export function normalizeInviteCode(raw: string): string | null {
  const s = toUpperSimple(raw.replace(GO_SPACE, '')).replace(/O/g, '0').replace(/[IL]/g, '1')
  if (s.length !== CODE_LEN) return null
  for (const ch of s) if (!ALPHABET.includes(ch)) return null
  return s
}

/** 从地址栏取 ?ic=，规范化；返回 null 表示没有或不合法 */
export function parseInviteParam(search: string): string | null {
  const v = new URLSearchParams(search).get('ic')
  return v ? normalizeInviteCode(v) : null
}

export function storeInvite(code: string, now = Date.now(), storage: Storage = localStorage): void {
  try {
    storage.setItem(INVITE_KEY, JSON.stringify({ code, at: now }))
  } catch {
    /* 隐私模式 / 配额满：不记住邀请码，注册照常 */
  }
}

/** 读取 30 天内存下的邀请码；过期或内容损坏则清除并返回 null。 */
export function readInvite(now = Date.now(), storage: Storage = localStorage): string | null {
  try {
    const raw = storage.getItem(INVITE_KEY)
    if (!raw) return null
    const v = JSON.parse(raw) as { code?: unknown; at?: unknown } | null
    const code = typeof v?.code === 'string' ? normalizeInviteCode(v.code) : null
    if (!code || typeof v?.at !== 'number' || now - v.at > INVITE_TTL_MS) {
      clearInvite(storage)
      return null
    }
    return code
  } catch {
    clearInvite(storage)
    return null
  }
}

export function clearInvite(storage: Storage = localStorage): void {
  try {
    storage.removeItem(INVITE_KEY)
  } catch {
    /* ignore */
  }
}

/** 登录响应里 invite 的提示：绑定成功提示；老用户不提示；其它原因（码无效 / 码主冻结）统一「邀请码无效」。 */
export function inviteToast(r: InviteResult): { type: 'success' | 'warning' | null; message: string } {
  if (r.bound) return { type: 'success', message: '已绑定邀请关系' }
  if (r.reason === 'existing_user') return { type: null, message: '' }
  return { type: 'warning', message: '邀请码无效' }
}
