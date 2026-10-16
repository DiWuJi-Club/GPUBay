import { getCurrentScope, onScopeDispose, ref, type Ref } from 'vue'

/** 每秒更新的当前时间戳，驱动页面上所有倒计时（按真实流逝时间计算，不累计 tick）。 */
export function useTicker(): Ref<number> {
  const now = ref(Date.now())
  const timer = setInterval(() => {
    now.value = Date.now()
  }, 1000)
  if (getCurrentScope()) onScopeDispose(() => clearInterval(timer))
  return now
}
