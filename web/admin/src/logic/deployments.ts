import { deploymentPhaseLabel, labelOf, type Deployment, type DeploymentState, type Role } from '@spark/shared'

/** 与后端 adminapi.maxForceRemoveReason 一致（去首尾空白后按字符计，不按 UTF-16 单元）。 */
export const FORCE_REMOVE_REASON_MAX = 200

export const runeCount = (s: string) => [...s].length

export function validateForceRemoveReason(reason: string): string | null {
  const r = reason.trim()
  if (!r) return '请填写原因'
  if (runeCount(r) > FORCE_REMOVE_REASON_MAX) return `原因最多 ${FORCE_REMOVE_REASON_MAX} 个字符`
  return null
}

/** 强制卸载：运维与超管；后端允许任何非终态（removing 再点只是原样返回，已在卸载中就不再显示按钮）。 */
export const showForceRemove = (r: Role, s: DeploymentState) =>
  (r === 'ops' || r === 'super') && s !== 'removing' && s !== 'removed' && s !== 'lost'

export const deploymentHealthLabel: Readonly<Record<string, string>> = { unknown: '未知', ok: '正常', unhealthy: '异常' }
export const deploymentDesiredLabel: Readonly<Record<string, string>> = { running: '运行', stopped: '停止', removed: '卸载' }

/** 进度列：部署中显示「阶段 百分比」；排队且上一次部署未结束（phase = busy）显示阶段；其余为空。 */
export function progressText(d: Pick<Deployment, 'state' | 'phase' | 'progress'>): string {
  if (d.state === 'installing') return `${d.phase ? labelOf(deploymentPhaseLabel, d.phase) + ' ' : ''}${d.progress}%`
  if (d.state === 'queued' && d.phase) return labelOf(deploymentPhaseLabel, d.phase)
  return ''
}

export type EnvValueKind = 'value' | 'empty' | 'secret_set' | 'secret_unset'
export interface EnvRow { name: string; label: string; value: string; kind: EnvValueKind }

/**
 * 变量表：按版本 spec.env 的顺序列出，再补上 spec 里没有的（旧版本留下的）变量。
 * 敏感变量只有「已设置」与否：后端只给 secret_env = {NAME: true}，这里也只显示 *** / 未设置，从不显示值。
 */
export function envRows(d: Pick<Deployment, 'env' | 'secret_env' | 'spec'>): EnvRow[] {
  const env = d.env ?? {}
  const secrets = d.secret_env ?? {}
  const rows: EnvRow[] = []
  const seen = new Set<string>()
  const secretRow = (name: string, label: string): EnvRow =>
    secrets[name] ? { name, label, value: '***', kind: 'secret_set' } : { name, label, value: '未设置', kind: 'secret_unset' }
  const plainRow = (name: string, label: string): EnvRow => {
    const v = env[name]
    return v ? { name, label, value: v, kind: 'value' } : { name, label, value: '（空）', kind: 'empty' }
  }
  for (const e of d.spec?.env ?? []) {
    seen.add(e.name)
    rows.push(e.type === 'secret' ? secretRow(e.name, e.label ?? '') : plainRow(e.name, e.label ?? ''))
  }
  for (const name of Object.keys(secrets).sort()) if (!seen.has(name)) { seen.add(name); rows.push(secretRow(name, '')) }
  for (const name of Object.keys(env).sort()) if (!seen.has(name)) { seen.add(name); rows.push(plainRow(name, '')) }
  return rows
}
