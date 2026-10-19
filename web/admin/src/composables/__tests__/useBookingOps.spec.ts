import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const { error, success, warning } = vi.hoisted(() => ({ error: vi.fn(), success: vi.fn(), warning: vi.fn() }))
vi.mock('element-plus', () => ({ ElMessage: { error, success, warning } }))

import type { AdminInstance } from '@spark/shared'
import { cancelTotp, submitTotp, totpState } from '../../store/totp'
import { useBookingOps } from '../useBookingOps'

type Call = { url: string; body: Record<string, unknown> }
let calls: Call[] = []
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
const flush = () => new Promise((r) => setTimeout(r, 0))
const booking = { id: 'i-1', machine_id: 3, state: 'reserved' } as AdminInstance

beforeEach(() => {
  calls = []
  error.mockReset()
  success.mockReset()
  warning.mockReset()
  // 假后端：cancel-booking 不带 totp → 403 totp_required；reassign 到 99 → 409 machine_unavailable
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init: RequestInit) => {
      const body = JSON.parse(String(init.body ?? '{}')) as Record<string, unknown>
      calls.push({ url, body })
      if (url.endsWith('/cancel-booking')) {
        if (body.totp !== '123456') return json(403, { code: 'totp_required', message: '该操作需要输入动态码', request_id: 'r' })
        return json(200, { refunded: 6900 })
      }
      if (body.machine_id === 99) return json(409, { code: 'machine_unavailable', message: '该机器在所选时段不可用', request_id: 'r' })
      return json(200, { ...booking, machine_id: body.machine_id ?? 7, reassign_count: 1 })
    }),
  )
})
afterEach(() => {
  cancelTotp()
  vi.unstubAllGlobals()
})

describe('useBookingOps', () => {
  it('改派：原因必填；机器 ID 只留数字，留空不发 machine_id；成功关闭弹窗并刷新', async () => {
    const done = vi.fn(async () => {})
    const ops = useBookingOps(done)
    ops.openReassign(booking)
    await ops.submitReassign()
    expect(warning).toHaveBeenCalledWith('请填写改派原因')
    expect(calls).toHaveLength(0)

    ops.reassign.reason = ' 换机柜 '
    await ops.submitReassign()
    expect(calls[0]).toEqual({ url: '/admin/api/v1/instances/i-1/reassign', body: { reason: '换机柜' } })
    expect(success).toHaveBeenCalledWith('已改派到机器 7')
    expect(ops.reassign.visible).toBe(false)
    expect(done).toHaveBeenCalledTimes(1)

    ops.openReassign(booking)
    ops.setReassignMachine('1a2')
    expect(ops.reassign.machineId).toBe('12')
    ops.reassign.reason = 'x'
    await ops.submitReassign()
    expect(calls[1].body).toEqual({ reason: 'x', machine_id: 12 })
  })

  it('改派失败：原文提示，弹窗保留', async () => {
    const done = vi.fn(async () => {})
    const ops = useBookingOps(done)
    ops.openReassign(booking)
    Object.assign(ops.reassign, { machineId: '99', reason: 'x' })
    await ops.submitReassign()
    expect(error).toHaveBeenCalledWith('该机器在所选时段不可用')
    expect(ops.reassign.visible).toBe(true)
    expect(done).not.toHaveBeenCalled()
  })

  it('取消预订：需要动态码，请求层弹框后带 totp 重发', async () => {
    const done = vi.fn(async () => {})
    const ops = useBookingOps(done)
    ops.openCancel(booking)
    ops.cancel.reason = '机房断电'
    const p = ops.submitCancel()
    await flush()
    expect(totpState.visible).toBe(true)
    totpState.code = '123456'
    submitTotp()
    await p
    expect(calls.map((c) => c.body)).toEqual([{ reason: '机房断电' }, { reason: '机房断电', totp: '123456' }])
    expect(success).toHaveBeenCalledWith('已取消预订，退款 ¥69.00')
    expect(ops.cancel.visible).toBe(false)
    expect(done).toHaveBeenCalledTimes(1)
  })

  it('取消预订：动态码框点取消静默，弹窗保留', async () => {
    const done = vi.fn(async () => {})
    const ops = useBookingOps(done)
    ops.openCancel(booking)
    ops.cancel.reason = 'x'
    const p = ops.submitCancel()
    await flush()
    cancelTotp()
    await p
    expect(error).not.toHaveBeenCalled()
    expect(ops.cancel.visible).toBe(true)
    expect(done).not.toHaveBeenCalled()
  })
})
