import type { MachineMetrics } from '@spark/shared'

export function fmtTemp(m: MachineMetrics | null | undefined): string {
  return m ? `${Math.round(m.gpu_temp_c)}°C` : '—'
}

export function fmtPercent(used: number, total: number): string {
  if (!total) return '—'
  return `${Math.round((used / total) * 100)}%`
}

export function rootUsage(m: MachineMetrics | null | undefined): string {
  return m ? fmtPercent(m.root_disk_used, m.root_disk_total) : '—'
}

const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
export function fmtBytes(n: number): string {
  if (!n) return '0 B'
  let i = 0
  let v = n
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return i === 0 ? `${v} B` : `${v.toFixed(1)} ${units[i]}`
}
