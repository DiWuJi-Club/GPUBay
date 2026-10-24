import {
  centsToYuanInput, fmtDate, fmtYuan, formatTokens, labelOf, llmKeyStatusLabel, llmUsageStatusLabel, yuanToCents,
  type LLMKey, type LLMKeyInput, type LLMModelPublic, type LLMSettlement, type LLMUsageStatus,
} from '@spark/shared'

// 计划 9：Token 商城（/tokens）的纯逻辑。价格单位「分 / 百万 token」，费用 cost_micro「微分」（1 分 = 10^6 微分）。

/** 单价（分 / 百万 token）→ 元字符串，两位小数：50 → '0.50'。 */
export function pricePerMillionYuan(fen: number): string {
  return centsToYuanInput(fen)
}

/** 费用（微分）→ 元字符串，6 位小数（四舍五入到 100 微分）：12500 → '0.000125'。页面显示用 formatMicroFen。 */
export function costYuan(costMicro: number): string {
  const sign = costMicro < 0 ? '-' : ''
  const units = String(Math.round(Math.abs(Math.trunc(costMicro)) / 100)).padStart(7, '0') // 10^-6 元
  return `${sign}${units.slice(0, -6)}.${units.slice(-6)}`
}

/** 市场页横幅「¥X 起」：在线模型的最低输入价（分 / 百万 token）；没有模型返回 null（横幅不显示）。 */
export function lowestInputPrice(models: readonly LLMModelPublic[]): number | null {
  if (models.length === 0) return null
  return Math.min(...models.map((m) => m.input_price))
}

/** 上下文长度：1024 的整数倍按「32K」「128K」显示（模型惯例），其余按 formatTokens。 */
export function contextLabel(n: number): string {
  if (n >= 1024 && n % 1024 === 0) return `${n / 1024}K`
  return formatTokens(n)
}

/** 示例里的 Key 占位符（真实 Key 在「API Keys」创建，明文只显示一次）。 */
export const KEY_PLACEHOLDER = 'sk-spark-xxxx'

/** 示例用的模型：第一个对话模型（示例调的是 /chat/completions），没有就用第一个模型，再没有用占位符。 */
export function exampleModel(models: readonly LLMModelPublic[]): string {
  return (models.find((m) => m.kind === 'chat') ?? models[0])?.slug ?? '<model>'
}

const trimSlash = (u: string) => u.replace(/\/+$/, '')

/** curl 非流式对话示例。 */
export function curlExample(baseUrl: string, model: string): string {
  const body = JSON.stringify({ model, messages: [{ role: 'user', content: '用一句话介绍 DGX Spark' }], max_tokens: 256 })
  return [
    `curl ${trimSlash(baseUrl)}/chat/completions \\`,
    `  -H "Content-Type: application/json" \\`,
    `  -H "Authorization: Bearer ${KEY_PLACEHOLDER}" \\`,
    `  -d '${body}'`,
  ].join('\n')
}

/** Python（openai SDK）流式对话示例：最后一个块 choices 为空、带 usage。 */
export function pythonExample(baseUrl: string, model: string): string {
  return [
    'from openai import OpenAI',
    '',
    `client = OpenAI(base_url=${JSON.stringify(trimSlash(baseUrl))}, api_key="${KEY_PLACEHOLDER}")`,
    '',
    'stream = client.chat.completions.create(',
    `    model=${JSON.stringify(model)},`,
    '    messages=[{"role": "user", "content": "用一句话介绍 DGX Spark"}],',
    '    stream=True,',
    ')',
    'for chunk in stream:',
    '    if chunk.choices and chunk.choices[0].delta.content:',
    '        print(chunk.choices[0].delta.content, end="", flush=True)',
    '    if chunk.usage:  # 平台总会在最后附带 usage 块',
    '        print("\\n用量:", chunk.usage)',
  ].join('\n')
}

/** Key 提示：后端给「sk-spark-…」+ 末 4 位，原样显示；只给了末尾几位或完整串时统一成「sk-spark-…末 4 位」。 */
export function keyHintMask(hint: string): string {
  if (!hint) return '—'
  if (hint.includes('…')) return hint
  return `sk-spark-…${hint.slice(-4)}`
}

/** 默认限额（按用户；后台可单独调整）。 */
export const DEFAULT_LLM_LIMITS = { rpm: 60, tpm: 200_000, concurrency: 8 } as const

/** 网关（/v1）错误码 → HTTP 状态码（规格 §6.1）。 */
export const llmErrorStatus: Record<string, number> = {
  invalid_request_error: 400,
  invalid_api_key: 401,
  insufficient_quota: 402,
  account_banned: 403,
  kyc_required: 403,
  account_blocked: 403,
  model_not_allowed: 403,
  model_not_found: 404,
  not_found: 404,
  request_too_large: 413,
  rate_limit_exceeded: 429,
  monthly_cap_exceeded: 429,
  upstream_error: 502,
  model_unavailable: 503,
  server_overloaded: 503,
}

/** 网关（/v1）错误码 → 中文说明（快速开始的错误码表）；错误体为 {"error":{"message","type","code"}}。 */
export const llmErrorHelp: Record<string, string> = {
  invalid_request_error: '请求格式错误：Content-Type 不是 application/json、缺少 model，或端点与模型类型不符（如对话模型调 embeddings）',
  invalid_api_key: 'API Key 不存在、格式错误、已停用、已撤销或已过期',
  insufficient_quota: '余额不足以支付本次预估费用，或账户有欠费；充值后即可继续调用',
  account_banned: '账号已被封禁',
  kyc_required: '账号未完成实名认证',
  account_blocked: 'Token 服务已被平台停用（消息中带原因）',
  model_not_allowed: '该 Key 不允许调用这个模型（创建 Key 时限定了模型）',
  model_not_found: '模型不存在或未上线',
  not_found: '接口不存在（只支持 chat/completions、completions、embeddings、models）',
  request_too_large: '请求体超过 2 MiB',
  rate_limit_exceeded: '超过每分钟请求数、每分钟 token 数或并发上限，按响应头 Retry-After 稍后重试',
  monthly_cap_exceeded: '该 Key 本月消费已达设定的月上限',
  upstream_error: '模型节点出错，本次不收费，可直接重试',
  model_unavailable: '模型暂无可用节点，请稍后再试',
  server_overloaded: '模型节点繁忙，按响应头 Retry-After 稍后重试',
}

// ---- API Keys（Task 18）----

/** Key 名称最多 32 个字符（按 Unicode 码点计，与后端 utf8.RuneCountInString 一致）。 */
export const KEY_NAME_MAX = 32
/** 月上限最大 100 万元（分）。 */
export const MONTHLY_CAP_MAX_FEN = 100_000_000

/** 创建 / 编辑 Key 对话框的表单。monthlyCap 是元字符串（空 = 不限）；expiresOn 是北京时间日期 YYYY-MM-DD（空 = 永不过期）。 */
export interface KeyForm {
  name: string
  modelSlugs: string[]
  monthlyCap: string
  expiresOn: string
}
export type KeyFormErrors = Partial<Record<keyof KeyForm, string>>

export const emptyKeyForm = (): KeyForm => ({ name: '', modelSlugs: [], monthlyCap: '', expiresOn: '' })

/** 编辑表单回填：月上限分 → 元字符串（不做除法）。到期日不可编辑，留空。 */
export function keyFormOf(k: LLMKey): KeyForm {
  return { name: k.name, modelSlugs: [...k.model_slugs], monthlyCap: k.monthly_cap == null ? '' : centsToYuanInput(k.monthly_cap), expiresOn: '' }
}

/** 月上限（元字符串）→ 分：空 = 不限（null）；按十进制字符串换算（yuanToCents），不做浮点乘法。 */
export function monthlyCapYuanToFen(input: string): { fen: number | null } | { error: string } {
  if (input.trim() === '') return { fen: null }
  const fen = yuanToCents(input)
  if (fen == null || fen <= 0) return { error: '月上限须大于 0，最多两位小数' }
  if (fen > MONTHLY_CAP_MAX_FEN) return { error: `月上限不能超过 ${fmtYuan(MONTHLY_CAP_MAX_FEN)}` }
  return { fen }
}

const ymdRe = /^\d{4}-\d{2}-\d{2}$/

/** 北京时间的「今天」YYYY-MM-DD（日期选择器禁用今天之前的日期、校验到期日用）。 */
export const beijingToday = (now: Date): string => fmtDate(now)

/**
 * 校验 Key 表单并生成请求体。
 * create：POST /llm/keys 的 LLMKeyInput——月上限、到期日为空时不带；到期日取北京时间当天 23:59:59（今天可选）。
 * edit：PUT /llm/keys/{id} 的字段（名称 / 模型 / 月上限）；清空月上限发 monthly_cap: null；到期日不可改，不带。
 */
export function validateKeyInput(f: KeyForm, now: Date, mode: 'create' | 'edit' = 'create'): { body: LLMKeyInput } | { errors: KeyFormErrors } {
  const errors: KeyFormErrors = {}
  const name = f.name.trim()
  if (name === '') errors.name = '请填写名称'
  else if ([...name].length > KEY_NAME_MAX) errors.name = `名称最多 ${KEY_NAME_MAX} 个字符`
  const cap = monthlyCapYuanToFen(f.monthlyCap)
  if ('error' in cap) errors.monthlyCap = cap.error
  const expiresOn = f.expiresOn.trim()
  if (mode === 'create' && expiresOn !== '') {
    if (!ymdRe.test(expiresOn)) errors.expiresOn = '日期格式不正确'
    else if (expiresOn < beijingToday(now)) errors.expiresOn = '到期日期不能早于今天'
  }
  if (Object.keys(errors).length > 0 || 'error' in cap) return { errors }

  const body: LLMKeyInput = { name, model_slugs: [...new Set(f.modelSlugs)] }
  if (mode === 'edit') body.monthly_cap = cap.fen
  else {
    if (cap.fen != null) body.monthly_cap = cap.fen
    if (expiresOn !== '') body.expires_at = `${expiresOn}T23:59:59+08:00`
  }
  return { body }
}

/** 表格「状态」列：撤销 / 停用按状态；启用但已过 expires_at 显示「已过期」。 */
export function keyStatusTag(k: LLMKey, now: Date): { label: string; type: 'success' | 'warning' | 'info' | 'danger' } {
  if (k.status === 'revoked') return { label: labelOf(llmKeyStatusLabel, 'revoked'), type: 'info' }
  if (k.status === 'disabled') return { label: labelOf(llmKeyStatusLabel, 'disabled'), type: 'warning' }
  if (k.expires_at && new Date(k.expires_at).getTime() <= now.getTime()) return { label: '已过期', type: 'danger' }
  return { label: labelOf(llmKeyStatusLabel, k.status), type: 'success' }
}

// ---- 用量 / 请求日志 / 结算（Task 19）----

/** token 数（千分位整数）；估算值（客户端中断按块数估算等）前缀「≈」。 */
export function tokenCount(n: number, estimated = false): string {
  const v = String(Math.max(0, Math.trunc(n))).replace(/\B(?=(\d{3})+(?!\d))/g, ',')
  return estimated ? `≈${v}` : v
}

/** 延迟 / 首字节时间：< 1 秒「850 ms」，其余「1.23 s」；null（非流式没有 TTFT）→「—」。 */
export function latencyLabel(ms: number | null | undefined): string {
  if (ms == null) return '—'
  if (ms < 1000) return `${Math.max(0, Math.round(ms))} ms`
  return `${(Math.round(ms / 10) / 100).toFixed(2)} s` // 先按 10 ms 取整，避免 2.345.toFixed(2) 的浮点误差
}

/** 请求状态标签：成功绿、客户端中断橙、其余（上游错误 / 超时 / 无可用节点）红。 */
export function usageStatusTag(status: LLMUsageStatus | string): { label: string; type: 'success' | 'warning' | 'danger' } {
  const label = labelOf(llmUsageStatusLabel, status)
  if (status === 'ok') return { label, type: 'success' }
  if (status === 'client_abort') return { label, type: 'warning' }
  return { label, type: 'danger' }
}

/** 请求日志查询窗口（与后端一致：默认最近 7 天，最长 90 天，含首尾两天，北京时间日期）。 */
export const USAGE_DEFAULT_DAYS = 7
export const USAGE_MAX_DAYS = 90

const ymdUTC = (ymd: string) => {
  const [y, m, d] = ymd.split('-').map(Number)
  return Date.UTC(y, m - 1, d)
}
/** YYYY-MM-DD 加减天数（纯日历运算，不受本地时区影响）。 */
export function addDays(ymd: string, n: number): string {
  return new Date(ymdUTC(ymd) + n * 86_400_000).toISOString().slice(0, 10)
}
/** 含首尾两天的天数：同一天 = 1。 */
export function daySpan(from: string, to: string): number {
  return Math.round((ymdUTC(to) - ymdUTC(from)) / 86_400_000) + 1
}
/** 默认范围：北京时间今天及之前 6 天。 */
export function defaultUsageRange(now: Date): { from: string; to: string } {
  const to = beijingToday(now)
  return { from: addDays(to, -(USAGE_DEFAULT_DAYS - 1)), to }
}
/** 校验日期范围；合法返回 null，否则返回提示文案（不发请求）。 */
export function validateUsageRange(from: string, to: string): string | null {
  if (!ymdRe.test(from) || !ymdRe.test(to)) return '请选择开始和结束日期'
  if (from > to) return '开始日期不能晚于结束日期'
  if (daySpan(from, to) > USAGE_MAX_DAYS) return `查询范围最长 ${USAGE_MAX_DAYS} 天`
  return null
}

/** 结算金额明细：用量折出的金额、并入的历史欠费、扣不到转为欠费的部分。 */
export function settlementDetail(s: LLMSettlement): string[] {
  const out: string[] = []
  if (s.usage_amount > 0 || s.debt_amount === 0) out.push(`用量 ${fmtYuan(s.usage_amount)}`)
  if (s.debt_amount > 0) out.push(`补扣欠费 ${fmtYuan(s.debt_amount)}`)
  const short = s.usage_amount + s.debt_amount - s.charged
  if (short > 0) out.push(`余额不足，${fmtYuan(short)} 计入欠费`)
  return out
}

/** 结算对应的钱包流水链接（charged = 0 时没有流水）。 */
export function settlementLedgerLink(s: LLMSettlement): { path: string; query: { ledger: string } } | null {
  return s.wallet_ledger_id == null ? null : { path: '/wallet', query: { ledger: String(s.wallet_ledger_id) } }
}
