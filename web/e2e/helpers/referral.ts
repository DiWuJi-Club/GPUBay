import type { APIRequestContext } from '@playwright/test'
import { placeOrder, seedUser, userApi } from './console'
import { hooks } from './hooks'
import { uniquePhone } from './phone'

export const DAY = 86_400

/** 迁移写入的初始费率版本（10%、单人 ¥500、30 天 ¥5,000、7 天到账、365 天）。 */
export const DEFAULT_RATE = { rate_bp: 1000, max_per_invitee: 50_000, max_per_inviter_30d: 500_000, hold_days: 7, period_days: 365 }

/** 改过费率的用例在 finally 里调用：费率是全局的，不恢复会影响之后所有返利用例。 */
export async function restoreRate() {
  await hooks.referralConfig(DEFAULT_RATE)
}

export interface RewardItem { order_id: string; invitee_masked: string; reward: number; status: string; clawed_amount: number; refunded_amount: number }

/** 脱敏手机号（与后端 referral.MaskPhone 同格式）。 */
export const masked = (phone: string) => `${phone.slice(0, 3)}****${phone.slice(7)}`

/** testid 元素里只有 ID：按 ID 精确匹配（避免 3 匹配到 13）。 */
export const exactId = (id: number | string) => new RegExp(`^${id}$`)

/** 造一个邀请人（缺省已实名、余额 ¥1,000）并取其邀请码与 API 会话。 */
export async function inviterWithCode(opts: { verified?: boolean; balance?: number } = {}) {
  const u = await seedUser({ verified: opts.verified ?? true, balance: opts.balance ?? 100_000 })
  const api = await userApi(u.phone)
  const r = await api.get('/api/v1/referral/me')
  if (!r.ok()) throw new Error(`referral/me ${r.status()} ${await r.text()}`)
  const me = (await r.json()) as { code: string; link: string }
  return { ...u, code: me.code, link: me.link, api }
}

/** 用邀请码走真实注册路径造一个被邀请人（缺省已实名、余额 ¥1,000、随机新设备）。 */
export async function inviteeOf(code: string, opts: { verified?: boolean; balance?: number; device_id?: string; ip?: string } = {}) {
  const phone = uniquePhone()
  const u = await hooks.users({ phone, verified: opts.verified ?? true, balance: opts.balance ?? 100_000, invite_code: code, device_id: opts.device_id ?? crypto.randomUUID(), ip: opts.ip })
  return { id: u.id, phone, invite: u.invite ?? undefined }
}

export async function rewardsOf(api: APIRequestContext): Promise<RewardItem[]> {
  const r = await api.get('/api/v1/referral/rewards')
  if (!r.ok()) throw new Error(`rewards ${r.status()} ${await r.text()}`)
  return ((await r.json()) as { items: RewardItem[] }).items
}

/**
 * 造一笔返利：新邀请人（余额 0）+ 带码注册的被邀请人在新登记的机器上下 1 天单（¥69.00 → 返利 ¥6.90）并开通，清扫计提为待发放；
 * settle 时再推进 7 天并清扫，返利发放到邀请人余额。返回的实例仍在运行（1 天后到期），不占用其它用例的机器。
 */
export async function seedReward(opts: { settle?: boolean } = {}) {
  const inv = await inviterWithCode({ balance: 0 })
  const b = await inviteeOf(inv.code, { balance: 100_000 })
  await hooks.machines(1)
  const instance = await placeOrder(await userApi(b.phone))
  await hooks.agent() // 实例进入运行中；一直停在开通中会在超时后整单退款，返利随之作废
  await hooks.sweep()
  if (opts.settle) {
    await hooks.advance(7 * DAY)
    await hooks.sweep()
  }
  return { inv, b, instance }
}
