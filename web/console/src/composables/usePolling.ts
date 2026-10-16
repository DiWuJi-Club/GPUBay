import { getCurrentScope, onMounted, onScopeDispose } from 'vue'

export interface PollingOptions {
  /** 返回 false 时本轮跳过（例如没有开通中 / 洗机中的实例） */
  enabled?: () => boolean
  /** 页面从不可见恢复为可见时立即执行一次（默认 true） */
  immediateOnVisible?: boolean
}

/** 可见时按固定间隔轮询；不可见时暂停；组件卸载自动停止。 */
export function usePolling(fn: () => void | Promise<void>, intervalMs: number, opts: PollingOptions = {}) {
  let timer: ReturnType<typeof setInterval> | undefined
  const visible = () => document.visibilityState === 'visible'
  const tick = () => {
    if (visible() && (opts.enabled?.() ?? true)) void fn()
  }
  const onVisibility = () => {
    if (visible() && (opts.immediateOnVisible ?? true) && (opts.enabled?.() ?? true)) void fn()
  }
  function start() {
    stop()
    timer = setInterval(tick, intervalMs)
  }
  function stop() {
    if (timer !== undefined) {
      clearInterval(timer)
      timer = undefined
    }
  }
  onMounted(() => {
    document.addEventListener('visibilitychange', onVisibility)
    start()
  })
  if (getCurrentScope()) {
    onScopeDispose(() => {
      document.removeEventListener('visibilitychange', onVisibility)
      stop()
    })
  }
  return { start, stop }
}
