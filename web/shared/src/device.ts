import { getCurrentScope, onScopeDispose, ref, type Ref } from 'vue'

/** 只用于充值渠道的选择（手机只给支付宝）；布局用 useMobile。 */
export function isMobileUA(ua: string = navigator.userAgent): boolean {
  return /Android|iPhone|iPad|iPod|Mobile/i.test(ua)
}

export const MOBILE_QUERY = '(max-width: 767px)'

/** 布局断点 768px：< 768 为手机布局。 */
export function useMobile(): Ref<boolean> {
  const q = window.matchMedia(MOBILE_QUERY)
  const mobile = ref(q.matches)
  const onChange = (e: MediaQueryListEvent) => {
    mobile.value = e.matches
  }
  q.addEventListener('change', onChange)
  if (getCurrentScope()) onScopeDispose(() => q.removeEventListener('change', onChange))
  return mobile
}
