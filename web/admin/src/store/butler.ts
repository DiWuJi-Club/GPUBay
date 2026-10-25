import { isApiError } from '@spark/shared'
import { ref } from 'vue'
import { api } from '../api'

/**
 * 平台是否关闭了 AI 管家（SPARK_BUTLER=off → 后台 /butler/* 路由不注册，GET /butler/settings 404）：
 * 布局进入时探测一次，404 才隐藏「AI 管家」菜单；探测前与其它错误都照常显示（页面里另有「平台未启用」提示），
 * 所以菜单的静态配置（menu.ts / menu.spec.ts）不变。只要得到过 200 或 404 就不再探测。
 */
export const butlerOff = ref(false)

let settled = false
let inflight: Promise<void> | null = null

export function probeButlerOff(): Promise<void> {
  if (settled) return Promise.resolve()
  inflight ??= api
    .get<unknown>('/butler/settings', undefined, { quiet: true })
    .then(
      () => {
        butlerOff.value = false
        settled = true
      },
      (e: unknown) => {
        if (isApiError(e) && e.status === 404) {
          butlerOff.value = true
          settled = true
        }
      },
    )
    .finally(() => {
      inflight = null
    })
  return inflight
}
