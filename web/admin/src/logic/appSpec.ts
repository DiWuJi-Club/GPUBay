import type { AppCoverage, AppEnvSpec, AppEnvType, AppSpec } from '@spark/shared'

// 版本 spec 的结构化表单（规格 §3.2、§12）：表格行 ↔ AppSpec。
// 表单里数值区间与选项用字符串保存（可留空、可输入负数），提交前由 formToSpec 转换；不合法的整数由 formIssues 先拦下，
// 路径写法与后端 app_version_invalid 相同，便于同一套高亮。

export interface PortRow {
  port: number | undefined
  label: string
  path: string
  health_path: string
}
export interface EnvRow {
  name: string
  label: string
  type: AppEnvType
  required: boolean
  default: string
  /** enum 的选项，一行一个 */
  options: string
  pattern: string
  max_len: string
  min: string
  max: string
  help: string
}
export interface LinkRow {
  label: string
  path: string
}
export interface SpecForm {
  ports: PortRow[]
  timeout_minutes: number | undefined
  min_data_free_gib: number | undefined
  env: EnvRow[]
  links: LinkRow[]
}

export const ENV_TYPES: readonly AppEnvType[] = ['string', 'int', 'bool', 'enum', 'secret']

export function emptySpec(): AppSpec {
  return { ports: [{ port: 8000 }], timeout_minutes: 60, min_data_free_gib: 0, env: [], links: [] }
}
export const emptyPortRow = (): PortRow => ({ port: undefined, label: '', path: '', health_path: '' })
export const emptyEnvRow = (): EnvRow => ({ name: '', label: '', type: 'string', required: false, default: '', options: '', pattern: '', max_len: '', min: '', max: '', help: '' })
export const emptyLinkRow = (): LinkRow => ({ label: '', path: '' })

/** 列是否对该类型有意义（表格据此禁用 / 隐藏，formToSpec 据此丢弃）。 */
export const envHas = {
  options: (t: AppEnvType) => t === 'enum',
  range: (t: AppEnvType) => t === 'int',
  pattern: (t: AppEnvType) => t === 'string' || t === 'secret',
  default: (t: AppEnvType) => t !== 'secret',
}

const numText = (v: number | undefined) => (v === undefined || v === null ? '' : String(v))

export function specToForm(s: AppSpec): SpecForm {
  return {
    ports: s.ports.map((p) => ({ port: p.port, label: p.label ?? '', path: p.path ?? '', health_path: p.health_path ?? '' })),
    timeout_minutes: s.timeout_minutes,
    min_data_free_gib: s.min_data_free_gib,
    env: (s.env ?? []).map((e) => ({
      name: e.name,
      label: e.label ?? '',
      type: e.type,
      required: !!e.required,
      default: e.default ?? '',
      options: (e.options ?? []).join('\n'),
      pattern: e.pattern ?? '',
      max_len: numText(e.max_len),
      min: numText(e.min),
      max: numText(e.max),
      help: e.help ?? '',
    })),
    links: (s.links ?? []).map((l) => ({ label: l.label, path: l.path })),
  }
}

/** 整数文本：空串 → undefined；不是整数 → NaN（formIssues 报错）。 */
function intOf(text: string): number | undefined {
  const t = text.trim()
  if (t === '') return undefined
  return /^-?\d+$/.test(t) ? Number(t) : NaN
}

/** 只放非空值：与后端 omitempty 输出一致，往返不产生多余键。 */
function put<T extends object, K extends keyof T>(o: T, k: K, v: T[K] | undefined | '' | false) {
  if (v === undefined || v === '' || v === false || (typeof v === 'number' && Number.isNaN(v))) return
  o[k] = v as T[K]
}

export function formToSpec(f: SpecForm): AppSpec {
  const spec: AppSpec = {
    ports: f.ports.map((r, i) => {
      const p: AppSpec['ports'][number] = { port: r.port ?? 0 }
      put(p, 'label', r.label.trim())
      put(p, 'path', r.path.trim())
      if (i === 0) put(p, 'health_path', r.health_path.trim()) // 只有主端口做 HTTP 健康检查
      return p
    }),
    env: f.env.map((r) => {
      const e: AppEnvSpec = { name: r.name.trim(), type: r.type }
      put(e, 'label', r.label.trim())
      put(e, 'required', r.required)
      if (envHas.options(r.type)) {
        const options = r.options.split('\n').map((o) => o.trim()).filter(Boolean)
        if (options.length) e.options = options
      }
      if (envHas.default(r.type)) put(e, 'default', r.default)
      if (envHas.pattern(r.type)) put(e, 'pattern', r.pattern)
      put(e, 'max_len', intOf(r.max_len)) // 后端对所有类型都补 max_len（enum 选项也受它约束），原样保留
      if (envHas.range(r.type)) {
        put(e, 'min', intOf(r.min))
        put(e, 'max', intOf(r.max))
      }
      put(e, 'help', r.help.trim())
      return e
    }),
    links: f.links.map((l) => ({ label: l.label.trim(), path: l.path.trim() })),
  }
  if (f.timeout_minutes !== undefined && f.timeout_minutes !== null) spec.timeout_minutes = f.timeout_minutes
  if (f.min_data_free_gib !== undefined && f.min_data_free_gib !== null) spec.min_data_free_gib = f.min_data_free_gib
  return spec
}

export interface FieldIssue {
  path: string
  reason: string
}

/** 表单本地能发现的问题（数值列不是整数）；其余规则以后端校验为准。 */
export function formIssues(f: SpecForm): FieldIssue[] {
  const out: FieldIssue[] = []
  f.env.forEach((r, i) => {
    const cols: (keyof EnvRow)[] = envHas.range(r.type) ? ['min', 'max', 'max_len'] : ['max_len']
    for (const c of cols) if (Number.isNaN(intOf(r[c] as string))) out.push({ path: `env[${i}].${c}`, reason: '需为整数' })
  })
  return out // 按行，行内按 min / max / max_len
}

export interface RowRef {
  table: 'ports' | 'env' | 'links'
  index: number
  field: string
}

/** 把后端 app_version_invalid 的字段路径（如 env[2].default）定位到表格行；不是表格行的路径返回 null。 */
export function fieldPathToRow(path: string): RowRef | null {
  const m = /^(ports|env|links)\[(\d+)\](?:\.([a-z_]+))?$/.exec(path)
  if (!m) return null
  return { table: m[1] as RowRef['table'], index: Number(m[2]), field: m[3] ?? '' }
}

/** 拆出 app_version_invalid 消息的「路径：原因」（全角冒号）；不是这种格式返回 null。 */
export function splitFieldError(message: string): FieldIssue | null {
  const m = /^([a-z_]+(?:\[\d+\])?(?:\.[a-z_]+)?)：([\s\S]*)$/.exec(message)
  return m ? { path: m[1], reason: m[2] } : null
}

/** 脚本语法错误的行号：后端消息形如 `script：第 12 行: <原因>`，这里接收拆出的原因部分。 */
export function scriptErrorLine(reason: string): number | null {
  const m = /^第 (\d+) 行/.exec(reason)
  return m ? Number(m[1]) : null
}

export const coverageText = (c: AppCoverage) => `已缓存 ${c.cached} / 在线 ${c.online}（未知 ${c.unknown}）`
/** 有在线机器没缓存该镜像：用户部署时可能要现场拉取镜像。 */
export const coverageLow = (c: AppCoverage) => c.cached < c.online
