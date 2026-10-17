import { messageOf, type Paged } from '@spark/shared'
import { ref, watch, type Ref } from 'vue'
import { notifyError } from '../logic/notify'

/**
 * 后台列表通用分页：page / page_size 变化自动加载（改 page_size 回到第一页）。
 * 失败用 notifyError 提示（forbidden → 无权执行该操作；401 / 取消静默）；每次加载都是用户操作触发，不自动重试。
 * 任何一次加载失败都清空表格并给出 error（页面显示占位与重试按钮）：失败往往发生在改筛选 / 翻页之后，
 * 留着上一次的行会让表格内容与当前筛选条件、页码对不上；total 保留，分页器停在用户请求的那一页，重试即重新请求这一页。
 * 只采用最后一次请求的结果：快速翻页 / 改筛选时先发后到的旧响应（含旧请求的失败）被丢弃。
 */
export function usePaged<T>(fetcher: (page: number, pageSize: number) => Promise<Paged<T>>, pageSize = 20) {
  const items = ref<T[]>([]) as Ref<T[]>
  const total = ref(0)
  const page = ref(1)
  const size = ref(pageSize)
  const loading = ref(false)
  const error = ref('')
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
    } catch (e) {
      if (mine !== seq) return
      notifyError(e)
      items.value = []
      error.value = messageOf(e)
    } finally {
      if (mine === seq) loading.value = false
    }
  }
  watch(page, () => void load())
  // 改每页条数：回到第一页再加载（旧页码可能已超出范围）；不在第一页时由翻页触发，只发一次请求
  watch(size, () => {
    if (page.value === 1) void load()
    else page.value = 1
  })
  /** 筛选条件变化：回到第一页并重新加载 */
  function reset() {
    if (page.value === 1) void load()
    else page.value = 1
  }
  return { items, total, page, size, loading, error, load, reset }
}
