import type { Paged } from '@spark/shared'
import { ref } from 'vue'
import { api } from '../api'

/** 未处理告警数（侧边菜单角标）：布局每 60 秒刷新，「标记已处理」后立即刷新。 */
export const unresolvedAlerts = ref(0)

let seq = 0

/** 取 GET /alerts?resolved=false&page_size=1 的 total；失败保留上次的数、不提示（401 已由会话处理统一提示）。 */
export async function refreshUnresolved(): Promise<void> {
  const mine = ++seq
  try {
    const total = (await api.get<Paged<unknown>>('/alerts', { resolved: 'false', page_size: 1 })).total
    if (mine === seq) unresolvedAlerts.value = total // 定时刷新与处理后的刷新并发时，只采用最后发出的那次
  } catch {
    /* 角标失败不打扰用户 */
  }
}
