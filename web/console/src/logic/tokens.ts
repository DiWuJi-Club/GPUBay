import { centsToYuanInput, formatTokens, type LLMModelPublic } from '@spark/shared'

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
