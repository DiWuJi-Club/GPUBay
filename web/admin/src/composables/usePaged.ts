import { messageOf, type Paged } from '@spark/shared'
import { ref, watch, type Ref } from 'vue'
import { notifyError } from '../logic/notify'

/**
 * 后台列表通用分页：page / page_size 变化自动加载。
 * 失败用 notifyError 提示（forbidden → 无权执行该操作；401 / 取消静默）；每次加载都是用户操作触发，不自动重试。
 * 还没成功加载过时失败，error 给出原文，页面据此显示占位与重试按钮；已有数据时失败只提示、保留原数据。
 * 只采用最后一次请求的结果：快速翻页 / 改筛选时先发后到的旧响应（含旧请求的失败）被丢弃。
 */
export function usePaged<T>(fetcher: (page: number, pageSize: number) => Promise<Paged<T>>, pageSize = 20) {
  const items = ref<T[]>([]) as Ref<T[]>
  const total = ref(0)
  const page = ref(1)
  const size = ref(pageSize)
  const loading = ref(false)
  const error = ref('')
  let loaded = false
  let seq = 0

  async function load() {
    const mine = ++seq
    loading.value = true
    try {
      const out = await fetcher(page.value, size.value)
      if (mine !== seq) return
      items.value = out.items
      total.value = out.total
      error.value = ''
      loaded = true
    } catch (e) {
      if (mine !== seq) return
      notifyError(e)
      if (!loaded) error.value = messageOf(e)
    } finally {
      if (mine === seq) loading.value = false
    }
  }
  watch([page, size], () => void load())
  /** 筛选条件变化：回到第一页并重新加载 */
  function reset() {
    if (page.value === 1) void load()
    else page.value = 1
  }
  return { items, total, page, size, loading, error, load, reset }
}
