import { isApiError, messageOf } from '@spark/shared'
import { ElMessage } from 'element-plus'

/**
 * 后台页面请求失败的统一提示：unauthorized 已由 handleUnauthorized 提示「登录已失效」并跳转，
 * cancelled 是用户在动态码弹框点了取消，两者静默；forbidden 固定提示「无权执行该操作」；
 * 其余按 messageOf 显示（5xx 带请求号）。
 */
export function notifyError(e: unknown): void {
  if (isApiError(e) && (e.code === 'unauthorized' || e.code === 'cancelled')) return
  ElMessage.error(isApiError(e) && e.code === 'forbidden' ? '无权执行该操作' : messageOf(e))
}
