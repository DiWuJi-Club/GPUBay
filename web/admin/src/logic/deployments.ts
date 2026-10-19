import { deploymentPhaseLabel, deploymentStateLabel, labelOf, type Deployment, type DeploymentState, type Role } from '@spark/shared'
import type { LocationQuery } from 'vue-router'

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

export const digits = (v: string) => v.replace(/\D/g, '')
export const firstOf = (v: LocationQuery[string] | undefined) => String((Array.isArray(v) ? v[0] : v) ?? '')

/** 部署列表的 ?state=&app_id=&user_id=&instance_id= 预填；ID 只留数字（实例 ID 去首尾空白），未知状态忽略。 */
export function filterFromQuery(q: LocationQuery) {
  const state = firstOf(q.state)
  return {
    state: state in deploymentStateLabel ? state : '',
    app_id: digits(firstOf(q.app_id)),
    user_id: digits(firstOf(q.user_id)),
    instance_id: firstOf(q.instance_id).trim(),
  }
}

/**
 * 两个查询串除 dep（详情抽屉）外完全相同：只是打开 / 关闭抽屉，列表不该重新加载、也不该回到第一页。
 * 比较原始查询串而不是 filterFromQuery 的结果，page 等将来加入的参数也算筛选变化。
 */
export function onlyDepChanged(from: LocationQuery, to: LocationQuery): boolean {
  const rest = (q: LocationQuery) =>
    JSON.stringify(
      Object.keys(q)
        .filter((k) => k !== 'dep' && q[k] !== undefined)
        .sort()
        .map((k) => [k, q[k]]),
    )
  return rest(from) === rest(to)
}
