import { watch } from 'vue'
import { uuid } from './uuid'

export interface IdempotencyKey {
  current(): string
  rotate(): void
}

/** 打开确认框时生成一个键；同一次确认的重复点击、网络错误后的再次点击都复用它；成功后调用 rotate()。 */
export function createIdempotencyKey(gen: () => string = () => uuid()): IdempotencyKey {
  let key = gen()
  return {
    current: () => key,
    rotate() {
      key = gen()
    },
  }
}

/** deps 返回的参数（租期、镜像、实例）变化时自动换键。 */
export function useIdempotencyKey(deps: () => unknown): IdempotencyKey {
  const k = createIdempotencyKey()
  watch(deps, () => k.rotate(), { deep: true })
  return k
}
