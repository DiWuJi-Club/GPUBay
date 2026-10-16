import { isApiError } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { reactive } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { notifyError } from '../logic/notify'
import { handleUnauthorized } from '../store/session'

export interface GuideState {
  visible: boolean
  title: string
  message: string
  action: string
  to: { path: string; query: Record<string, string> } | null
}

/** 控制台统一的「需要引导」错误处理：实名、充值、封禁、无空闲机器。 */
export function useGuide() {
  const route = useRoute()
  const router = useRouter()
  const state = reactive<GuideState>({ visible: false, title: '', message: '', action: '', to: null })

  function show(title: string, message: string, action: string, path: string) {
    Object.assign(state, { visible: true, title, message, action, to: { path, query: { redirect: route.fullPath } } })
  }
  function kycRequired() {
    show('请先完成实名认证', '按监管要求，下单与充值前需要完成实名认证。', '去实名', '/kyc')
  }
  function insufficientBalance() {
    show('余额不足', '当前余额不足以支付本次费用，请先充值。', '去充值', '/wallet/recharge')
  }
  async function go() {
    state.visible = false
    if (state.to) await router.push(state.to)
  }
  /** cancelled / unauthorized 由 notifyError 静默；其余未识别的错误按 messageOf 提示。 */
  function handle(err: unknown, opts: { onNoCapacity?: () => void } = {}) {
    if (!isApiError(err)) return notifyError(err)
    switch (err.code) {
      case 'kyc_required':
        return kycRequired()
      case 'insufficient_balance':
        return insufficientBalance()
      case 'banned':
        return handleUnauthorized('账号已被封禁')
      case 'no_capacity':
        ElMessage.warning('暂无空闲机器')
        return opts.onNoCapacity?.()
      default:
        return notifyError(err)
    }
  }
  return { state, kycRequired, insufficientBalance, handle, go }
}

export type Guide = ReturnType<typeof useGuide>
