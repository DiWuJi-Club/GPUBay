import { useMobile } from '@spark/shared'
import { useRoute, useRouter } from 'vue-router'
import { butler, openButler } from '../store/butler'
import { session } from '../store/session'

/**
 * 页面上的「问问管家」入口（实例详情、市场页）：
 * 未登录 → 登录页（redirect 回当前页）；桌面 → 打开抽屉；手机或已在 /butler → 进入 /butler 全页（抽屉只在桌面端渲染）。
 */
export function useAskButler() {
  const route = useRoute()
  const router = useRouter()
  const mobile = useMobile()
  return (opts: { instanceId?: string; draft?: string } = {}) => {
    if (session.status !== 'authed') {
      void router.push({ path: '/login', query: { redirect: route.fullPath } })
      return
    }
    openButler(opts)
    if (mobile.value || route.path === '/butler') {
      butler.open = false
      if (route.path !== '/butler') void router.push('/butler')
    }
  }
}
