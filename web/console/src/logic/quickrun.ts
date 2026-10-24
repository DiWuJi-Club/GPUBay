import { isApiError, labelOf, messageOf, workflowErrorLabel, type QuickRun, type QuickRunFile, type WorkflowInput } from '@spark/shared'

/**
 * 快速出图（计划 11 §6）的纯逻辑：表单默认值、前端预校验（镜像 internal/comfywf BuildPrompt / coerceValue，服务器仍是准绳）、
 * 请求体、运行状态文案、轮询节奏。
 */

/** 轮询间隔与上限（D10：单次运行等待 30 分钟后前端停止轮询，结果仍可在 ComfyUI 历史里看）。 */
export const pollDelayMs = 2000
export const pollDeadlineMs = 30 * 60 * 1000
export const pollExpired = (startedAt: number, now: number) => now - startedAt >= pollDeadlineMs

/** 种子上限：2^53 - 1（后端 maxSeed = 2^53，取值 [0, 2^53)）。 */
export const MAX_SEED = Number.MAX_SAFE_INTEGER
/** text / textarea 的 max_len 缺省值（comfywf defaultMaxLen）。 */
const DEFAULT_MAX_LEN = 1000
/** 上传接口返回的图片名（comfywf quickImageRe）。 */
const QUICK_IMAGE_RE = /^spark-quickrun\/[a-z0-9]{12}\.(png|jpg|webp)$/
const SEED_REASON = '种子应为 -1（随机）或 0～9007199254740991 的整数'

/** 表单值：数字框为 number | null（清空）；seed 为字符串（空 = 随机）；image 为上传返回的名字（空 = 未上传）。 */
export type QuickValue = string | number | boolean | null
export type QuickValues = Record<string, QuickValue>

/** 表单初值：有默认值用默认值；seed 无默认 / -1 显示为空（= 随机）；无默认的 bool 为 false、数字为 null、其余为空串。 */
export function defaultValues(inputs: WorkflowInput[]): QuickValues {
  const out: QuickValues = {}
  for (const i of inputs) {
    const d = i.default
    switch (i.type) {
      case 'seed':
        out[i.key] = d == null || Number(d) === -1 ? '' : String(d)
        break
      case 'int':
      case 'float':
        out[i.key] = d == null || d === '' ? null : Number(d)
        break
      case 'bool':
        out[i.key] = d == null ? false : d === true || d === 'true'
        break
      default:
        out[i.key] = d == null ? '' : String(d)
    }
  }
  return out
}

const hasDefault = (i: WorkflowInput) => i.default !== undefined && i.default !== null
const isEmpty = (v: unknown) => v === undefined || v === null || v === ''

function rangeReason(i: WorkflowInput, n: number): string | null {
  const low = i.min != null && n < i.min
  const high = i.max != null && n > i.max
  if (!low && !high) return null
  if (i.min != null && i.max != null) return `应在 ${i.min}～${i.max}`
  return low ? `不能小于 ${i.min}` : `不能大于 ${i.max}`
}

function seedReason(v: unknown): string | null {
  if (v === undefined || v === null) return null
  if (typeof v === 'string') {
    const s = v.trim()
    if (s === '') return null
    if (!/^[+-]?[0-9]+$/.test(s)) return SEED_REASON
    const n = BigInt(s)
    return n < -1n || n > BigInt(MAX_SEED) ? SEED_REASON : null
  }
  if (typeof v !== 'number' || !Number.isInteger(v) || v < -1 || v > MAX_SEED) return SEED_REASON
  return null
}

/**
 * 单项预校验，返回中文原因（null = 通过）。空值：seed 为随机；有默认值时由服务器补默认；否则「必填」。
 * 文案与后端 coerceValue 一致（区间只给出一端时说「不能小于 / 大于」）。
 */
export function validateValue(i: WorkflowInput, v: unknown): string | null {
  if (i.type === 'seed') return seedReason(v)
  if (isEmpty(v)) return hasDefault(i) ? null : '必填'
  switch (i.type) {
    case 'text':
    case 'textarea': {
      if (typeof v !== 'string') return '必须是文本'
      const maxLen = i.max_len || DEFAULT_MAX_LEN
      if ([...v].length > maxLen) return `不能超过 ${maxLen} 字`
      if (v.includes('\0')) return '含非法字符'
      return null
    }
    case 'int': {
      if (typeof v !== 'number' || !Number.isInteger(v)) return '必须是整数'
      const r = rangeReason(i, v)
      if (r) return r
      if (i.step != null && i.step > 0) {
        const base = i.min ?? 0
        if ((v - base) % i.step !== 0) return `必须是 ${base} 加 ${i.step} 的整数倍`
      }
      return null
    }
    case 'float':
      if (typeof v !== 'number' || !Number.isFinite(v)) return '必须是数字'
      return rangeReason(i, v)
    case 'bool':
      return typeof v === 'boolean' ? null : '必须是布尔值'
    case 'enum':
      return typeof v === 'string' && (i.options ?? []).includes(v) ? null : '不是可选值'
    case 'image':
      return typeof v === 'string' && QUICK_IMAGE_RE.test(v) ? null : '请先上传图片'
  }
  return '类型未知'
}

/** 全部输入的预校验。 */
export function validateValues(inputs: WorkflowInput[], values: Record<string, unknown>): { ok: boolean; errors: Record<string, string> } {
  const errors: Record<string, string> = {}
  for (const i of inputs) {
    const r = validateValue(i, values[i.key])
    if (r) errors[i.key] = r
  }
  return { ok: Object.keys(errors).length === 0, errors }
}

/**
 * POST …/runs 的 inputs：只发表单里的 key（后端对未知参数报错）；数字 / 枚举 / 图片留空不发送（服务器补默认）；
 * seed 留空不发送（= 随机），否则发数字（-1 也照发，服务器同样当随机）；文本照发（用户清空的反向提示词就是空串）。
 */
export function buildRunInputs(inputs: WorkflowInput[], values: Record<string, unknown>): Record<string, string | number | boolean> {
  const out: Record<string, string | number | boolean> = {}
  for (const i of inputs) {
    const v = values[i.key]
    if (i.type === 'seed') {
      const s = typeof v === 'string' ? v.trim() : v
      if (!isEmpty(s)) out[i.key] = Number(s)
      continue
    }
    if (i.type === 'text' || i.type === 'textarea') {
      if (typeof v === 'string') out[i.key] = v
      continue
    }
    if (!isEmpty(v)) out[i.key] = v as string | number | boolean
  }
  return out
}

/** 耗时：`12 秒` / `2 分 5 秒`。 */
function durationText(ms: number): string {
  const s = Math.max(0, Math.round(ms / 1000))
  return s < 60 ? `${s} 秒` : `${Math.floor(s / 60)} 分 ${s % 60} 秒`
}

/** 运行状态文案：排队中 / 生成中 / 完成（12 秒）/ 失败：<后端 message>。 */
export function runStatusText(run: Pick<QuickRun, 'state' | 'message' | 'created_at' | 'finished_at'>): string {
  switch (run.state) {
    case 'queued':
      return '排队中'
    case 'running':
      return '生成中'
    case 'done': {
      const ms = run.finished_at ? Date.parse(run.finished_at) - Date.parse(run.created_at) : Number.NaN
      return Number.isFinite(ms) ? `完成（${durationText(ms)}）` : '完成'
    }
    case 'error':
      return run.message ? `失败：${run.message}` : '失败'
  }
  return ''
}

/** 运行已结束（停止轮询）。 */
export const runFinished = (run: Pick<QuickRun, 'state'>) => run.state === 'done' || run.state === 'error'

const VIDEO_EXT = /\.(mp4|webm|mov|mkv)$/i

/** 输出文件按图片 / 视频渲染：以后端 kind 为准，缺省时按扩展名。 */
export function fileKind(f: Partial<Pick<QuickRunFile, 'kind'>> & Pick<QuickRunFile, 'filename'>): 'image' | 'video' {
  if (f.kind === 'image' || f.kind === 'video') return f.kind
  return VIDEO_EXT.test(f.filename) ? 'video' : 'image'
}

/** 运行 / 上传失败的提示：workflow_busy 用固定文案（稍后再试），其余用后端 message（node_errors、参数错误等）。 */
export function quickRunErrorText(e: unknown): string {
  if (isApiError(e) && e.code === 'workflow_busy') return labelOf(workflowErrorLabel, e.code)
  return messageOf(e)
}
