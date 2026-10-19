import { describe, expect, it } from 'vitest'
import { fmtBytes, fmtPercent, fmtTemp, rootUsage } from '../metrics'

const m = { gpu_util: 12, gpu_temp_c: 55.4, gpu_power_w: 30, load1: 0.5, mem_total: 128 * 2 ** 30, mem_used: 8 * 2 ** 30, root_disk_total: 4000 * 2 ** 30, root_disk_used: 1600 * 2 ** 30, data_disk_used: 0 }

describe('metrics', () => {
  it('温度取整，无指标为 —', () => {
    expect(fmtTemp(m)).toBe('55°C')
    expect(fmtTemp(null)).toBe('—')
  })
  it('百分比与字节', () => {
    expect(fmtPercent(1600, 4000)).toBe('40%')
    expect(fmtPercent(1, 0)).toBe('—')
    expect(rootUsage(m)).toBe('40%')
    expect(fmtBytes(128 * 2 ** 30)).toBe('128.0 GiB')
    expect(fmtBytes(1.5 * 2 ** 40)).toBe('1.5 TiB')
    expect(fmtBytes(0)).toBe('0 B')
  })
})
