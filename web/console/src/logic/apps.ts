import {
  deploymentErrorLabel, deploymentPhaseLabel, deploymentStateLabel, deploymentStateType, labelOf,
  type AppCatalogItem, type AppEnvSpec, type Deployment, type Image, type InstanceState,
} from '@spark/shared'

// 与 internal/appdeploy 保持一致：MinSecretLen、max_len 缺省值
const MIN_SECRET_LEN = 8
const DEFAULT_MAX_LEN = 256
const INT64_MIN = -(2n ** 63n)
const INT64_MAX = 2n ** 63n - 1n
const SECRET_CHARS = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789'
const SECRET_LEN = 32
// 判断 pattern 是否允许生成的密钥：含大小写与数字的 32 位探针
const SECRET_PROBE = 'Aa0Bb1Cc2Dd3Ee4Ff5Gg6Hh7Ii8Jj9Kk'

/** 应用兼容且当前可选的镜像，按 app.images 的顺序（第一个为默认）。 */
export function compatibleImages(app: AppCatalogItem, images: Image[]): Image[] {
  return app.images.map((a) => images.find((i) => i.id === a.id)).filter((i): i is Image => i != null)
}

/** 变量表单初值：default；bool 无默认为 'false'，其余为空串。 */
export function envDefaults(spec: AppEnvSpec[]): Record<string, string> {
  return Object.fromEntries(spec.map((e) => [e.name, e.default ?? (e.type === 'bool' ? 'false' : '')]))
}

/** 提交前去掉空串：空值交给服务器补默认值（与 ValidateEnv 对空值的处理一致）。 */
export function stripEmpty(values: Record<string, string>): Record<string, string> {
  return Object.fromEntries(Object.entries(values).filter(([, v]) => v !== ''))
}

const envLabel = (e: AppEnvSpec) => e.label || e.name
const byteLen = (v: string) => new TextEncoder().encode(v).length

/** pattern 编译为全匹配（Go：`^(?:pattern)$`）；JS 无法编译的 RE2 语法返回 null，由服务器校验。 */
function fullMatch(pattern: string): RegExp | null {
  for (const flags of ['u', '']) {
    try {
      return new RegExp(`^(?:${pattern})$`, flags)
    } catch {
      // 换不带 u 再试
    }
  }
  return null
}

/**
 * 前端预校验单个值（镜像 internal/appdeploy/spec.go 的 ValidateEnv / checkValue，服务器仍是准绳）。
 * 返回中文原因；null 表示通过。空值：有默认值或非必填即通过（服务器补默认 / 不设置）。
 */
export function validateEnvValue(e: AppEnvSpec, v: string): string | null {
  if (v === '') return e.required && !e.default ? `请填写 ${envLabel(e)}` : null
  if (/[\n\r\0]/.test(v)) return '不能包含换行或 NUL'
  const maxLen = e.max_len || DEFAULT_MAX_LEN
  if (byteLen(v) > maxLen) return `最多 ${maxLen} 字节`
  switch (e.type) {
    case 'int': {
      if (!/^[+-]?[0-9]+$/.test(v)) return '必须是整数'
      const n = BigInt(v)
      if (n < INT64_MIN || n > INT64_MAX) return '必须是整数'
      if ((e.min != null && n < BigInt(e.min)) || (e.max != null && n > BigInt(e.max))) return '超出允许范围'
      return null
    }
    case 'bool':
      return v === 'true' || v === 'false' ? null : '只能是 true 或 false'
    case 'enum':
      return e.options?.includes(v) ? null : '不在可选项内'
    case 'secret':
      if (byteLen(v) < MIN_SECRET_LEN) return `至少 ${MIN_SECRET_LEN} 个字符`
      return checkPattern(e, v)
    case 'string':
      return checkPattern(e, v)
  }
  return null
}

function checkPattern(e: AppEnvSpec, v: string): string | null {
  if (!e.pattern) return null
  const re = fullMatch(e.pattern)
  return re && !re.test(v) ? '格式不正确' : null
}

/** secret 且规则允许 32 位字母数字时显示「生成」按钮。 */
export function canGenerate(e: AppEnvSpec): boolean {
  return e.type === 'secret' && validateEnvValue(e, SECRET_PROBE) === null
}

/** 生成 32 位 [A-Za-z0-9]（crypto.getRandomValues，拒绝采样无偏）；规则不允许时返回 ''。 */
export function generateSecret(e: AppEnvSpec, c: Pick<Crypto, 'getRandomValues'> = globalThis.crypto): string {
  if (!canGenerate(e)) return ''
  for (let attempt = 0; attempt < 10; attempt++) {
    let out = ''
    while (out.length < SECRET_LEN) {
      for (const b of c.getRandomValues(new Uint8Array(64))) {
        if (b < 248 && out.length < SECRET_LEN) out += SECRET_CHARS[b % 62] // 248 = 62 × 4
      }
    }
    if (validateEnvValue(e, out) === null) return out
  }
  return ''
}

/** 「模型」变量：spec.env 里第一个 enum。 */
const modelEnv = (app: AppCatalogItem) => app.version.env.find((e) => e.type === 'enum')

/** 预装应用下拉的选项：`名称 · 默认模型`（没有 enum 变量时只有名称）。 */
export function appOptionLabel(app: AppCatalogItem): string {
  const m = modelEnv(app)?.default
  return m ? `${app.name} · ${m}` : app.name
}

/** 确认页「预装应用」：`名称（所选模型）`。 */
export function appConfirmText(app: AppCatalogItem, values: Record<string, string>): string {
  const e = modelEnv(app)
  const m = e ? values[e.name] || e.default : ''
  return m ? `${app.name}（${m}）` : app.name
}

/** 部署状态文案与标签颜色（实例详情「应用」卡片与列表摘要共用）。 */
export function deploymentStatusText(d: Deployment, instanceState: InstanceState): { text: string; type: string } {
  if (instanceState === 'stopped') return { text: '实例已停机，续费后自动恢复', type: 'info' }
  if ((instanceState === 'reserved' || instanceState === 'creating') && d.state === 'queued') return { text: '将在开通后自动部署', type: 'info' }
  if (d.state === 'installing') {
    const phase = labelOf(deploymentPhaseLabel, d.phase)
    return { text: `部署中 ${d.progress}%${phase ? ` · ${phase}` : ''}`, type: 'primary' }
  }
  if (d.state === 'failed') return { text: d.error_code ? `失败：${labelOf(deploymentErrorLabel, d.error_code)}` : '失败', type: 'danger' }
  return { text: labelOf(deploymentStateLabel, d.state), type: labelOf(deploymentStateType, d.state) }
}
