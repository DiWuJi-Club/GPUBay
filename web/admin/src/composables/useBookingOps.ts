import { fmtYuan, type AdminInstance } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { reactive } from 'vue'
import { api } from '../api'
import { notifyError } from '../logic/notify'

const digits = (v: string) => v.replace(/\D/g, '')

/**
 * 预订（reserved）的后台操作：改派、取消预订。预订列表与实例详情共用。
 * 取消预订需要动态码：后端返回 totp_required 时由请求层弹框并带 totp 重发（取消弹框则静默）。
 * 成功后调用 onDone 刷新；失败原文提示，弹窗保留、输入不丢。
 */
export function useBookingOps(onDone: () => Promise<unknown>) {
  const reassign = reactive({ visible: false, target: null as AdminInstance | null, machineId: '', reason: '', busy: false })
  function openReassign(b: AdminInstance) {
    Object.assign(reassign, { visible: true, target: b, machineId: '', reason: '' })
  }
  function setReassignMachine(v: string) {
    reassign.machineId = digits(v)
  }
  async function submitReassign() {
    if (reassign.busy || !reassign.target) return // 同步守卫：连点只发一次
    if (!reassign.reason.trim()) return void ElMessage.warning('请填写改派原因')
    reassign.busy = true
    try {
      const body: Record<string, unknown> = { reason: reassign.reason.trim() }
      if (reassign.machineId) body.machine_id = Number(reassign.machineId)
      const out = await api.post<AdminInstance>(`/instances/${reassign.target.id}/reassign`, body)
      ElMessage.success(`已改派到机器 ${out.machine_id}`)
      reassign.visible = false
      await onDone()
    } catch (e) {
      notifyError(e) // machine_unavailable 原文；弹窗保留
    } finally {
      reassign.busy = false
    }
  }

  const cancel = reactive({ visible: false, target: null as AdminInstance | null, reason: '', busy: false })
  function openCancel(b: AdminInstance) {
    Object.assign(cancel, { visible: true, target: b, reason: '' })
  }
  async function submitCancel() {
    if (cancel.busy || !cancel.target) return // 同步守卫：取消退款不是幂等操作
    if (!cancel.reason.trim()) return void ElMessage.warning('请填写取消原因')
    cancel.busy = true
    try {
      const out = await api.post<{ refunded: number }>(`/instances/${cancel.target.id}/cancel-booking`, { reason: cancel.reason.trim() })
      ElMessage.success(`已取消预订，退款 ${fmtYuan(out.refunded)}`)
      cancel.visible = false
      await onDone()
    } catch (e) {
      notifyError(e) // 动态码取消静默；cancel_not_allowed 等原文
    } finally {
      cancel.busy = false
    }
  }

  function closeAll() {
    reassign.visible = false
    cancel.visible = false
  }

  return { reassign, openReassign, setReassignMachine, submitReassign, cancel, openCancel, submitCancel, closeAll }
}
