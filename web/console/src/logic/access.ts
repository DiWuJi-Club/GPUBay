export const MAX_PORTS = 5
export const MAX_KEYS = 10

/** 端口：半角整数 1～65535，且不能是 22（容器 sshd）。 */
export function validatePort(input: string): { port: number } | { error: string } {
  const s = input.trim()
  if (!/^\d{1,5}$/.test(s)) return { error: '端口必须是 1～65535 之间的整数' }
  const port = Number(s)
  if (port < 1 || port > 65535) return { error: '端口必须是 1～65535 之间的整数' }
  if (port === 22) return { error: '22 端口由 SSH 使用，不能作为 Web 端口' }
  return { port }
}

const KEY_TYPES = ['ssh-ed25519', 'ecdsa-sha2-nistp256', 'ecdsa-sha2-nistp384', 'ecdsa-sha2-nistp521', 'ssh-rsa']

/** 前端粗校验（后端为准）：单行、类型受支持、至少有 base64 段。 */
export function looksLikePublicKey(s: string): boolean {
  const line = s.trim()
  if (!line || /[\r\n]/.test(line)) return false
  const parts = line.split(/\s+/)
  return parts.length >= 2 && KEY_TYPES.includes(parts[0]) && /^[A-Za-z0-9+/=]+$/.test(parts[1])
}

/**
 * redirect 参数只接受站内相对路径：以 / 开头、不以 // 开头、不含反斜杠（浏览器把 /\evil.com 当作 //evil.com）、
 * 不含控制字符（浏览器解析 URL 时剔除制表 / 换行，/\t/evil.com 会变成 //evil.com）。
 */
export function safeRedirect(v: unknown): string | null {
  return typeof v === 'string' && v.startsWith('/') && !v.startsWith('//') && !v.includes('\\') && !/[\x00-\x1f\x7f]/.test(v) ? v : null
}

/** 登录后的 redirect 若是实例 Web 访问的授权端点，必须整页跳转（它不是前端路由）。 */
export function isAuthorizeRedirect(v: string | null): boolean {
  return v !== null && (v === '/api/v1/ingress/authorize' || v.startsWith('/api/v1/ingress/authorize?'))
}

export type LoginTarget = { kind: 'assign'; url: string } | { kind: 'route'; path: string }

/**
 * 登录后（或已登录访问 /login 时）去哪：先过 safeRedirect（不安全的一律回落 fallback），
 * authorize 端点用 location.assign 整页跳转（router.replace 到不了后端路径），其余走前端路由。
 */
export function postLoginTarget(v: unknown, fallback = '/market'): LoginTarget {
  const target = safeRedirect(v)
  if (target && isAuthorizeRedirect(target)) return { kind: 'assign', url: target }
  return { kind: 'route', path: target ?? fallback }
}
