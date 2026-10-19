import { isApiError, messageOf } from '@spark/shared'
import { ElMessage } from 'element-plus'

/**
 * 页面请求失败的统一提示：unauthorized 已由 handleUnauthorized 提示一次并跳登录，cancelled 是用户主动取消，两者静默；
 * 其余按 messageOf 显示（5xx 带请求号）。控制台视图的 catch 分支一律用它。
 */
export function notifyError(e: unknown): void {
  if (isApiError(e) && (e.code === 'unauthorized' || e.code === 'cancelled')) return
  ElMessage.error(messageOf(e))
}
