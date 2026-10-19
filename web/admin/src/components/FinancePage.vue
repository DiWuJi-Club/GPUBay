<script setup lang="ts">
import { digitsOnly, type Paged } from '@spark/shared'
import { onMounted, reactive, ref } from 'vue'
import { onBeforeRouteUpdate, useRoute, type LocationQuery } from 'vue-router'
import { api } from '../api'
import { usePaged } from '../composables/usePaged'
import { downloadBlob } from '../logic/csv'
import { notifyError } from '../logic/notify'
import DateRange from './DateRange.vue'

/** 订单 / 流水 / 充值单共用：筛选（用户、类型或渠道、状态、日期）+ 表格（调用方插槽）+ 分页 + 按同一筛选导出 CSV。 */
const props = defineProps<{
  endpoint: 'orders' | 'ledger' | 'recharges'
  kindParam: 'kind' | 'channel'
  kindLabel: string
  kindOptions: { label: string; value: string }[]
  statusOptions: { label: string; value: string }[]
}>()

const route = useRoute()
// 批次筛选只有订单页有（?batch_id= 预填，来自预订 / 实例页的批次链接）
const isOrders = props.endpoint === 'orders'
const emptyFilter = (q: LocationQuery = {}) => ({
  user_id: digitsOnly(String(q.user_id ?? '')),
  batch_id: isOrders ? String(q.batch_id ?? '') : '',
  kind: '',
  status: '',
  range: { from: '', to: '' },
})
const filter = reactive(emptyFilter(route.query))
// 空值由 buildQuery 省略；日期按 YYYY-MM-DD 原样发出（后端按北京时间解释，to 含当天）
const query = () => ({
  user_id: filter.user_id,
  [props.kindParam]: filter.kind,
  status: filter.status,
  from: filter.range.from,
  to: filter.range.to,
  ...(isOrders ? { batch_id: filter.batch_id.trim() } : {}),
})
// 已生效的筛选：只在查询 / 重置 / 打开页面（含 ?user_id= 预填）时取表单快照。
// 翻页与导出都用它：表单改了但没点「查询」时，第 2 页和导出的 CSV 仍与屏幕上的表格一致。
let applied = query()
const list = usePaged<Record<string, unknown>>((page, size) => api.get<Paged<Record<string, unknown>>>(`/${props.endpoint}`, { ...applied, page, page_size: size }))
onMounted(list.load)
function search() {
  applied = query()
  list.reset()
}
function resetFilter() {
  Object.assign(filter, emptyFilter())
  search()
}
// 同一路由只换查询串（/orders?user_id=X → /orders 或 ?user_id=Y / ?batch_id=，含浏览器前进后退）：组件复用，按新地址重新预填并加载
const same = (a: LocationQuery, b: LocationQuery, k: string) => String(a[k] ?? '') === String(b[k] ?? '')
onBeforeRouteUpdate((to) => {
  if (same(to.query, route.query, 'user_id') && same(to.query, route.query, 'batch_id')) return
  Object.assign(filter, emptyFilter(to.query))
  search()
})

const exporting = ref(false)
async function exportCSV() {
  if (exporting.value) return // 同步守卫：连点两次只导出一次
  exporting.value = true
  try {
    const { blob, filename } = await api.download(`/${props.endpoint}/export`, applied)
    downloadBlob(filename, blob)
  } catch (e) {
    notifyError(e) // export_too_large 等按原文提示
  } finally {
    exporting.value = false
  }
}
</script>

<template>
  <el-card>
    <el-form inline class="filters" @submit.prevent="search">
      <el-form-item label="用户 ID">
        <el-input :model-value="filter.user_id" clearable data-testid="fin-filter-user" style="width: 120px" @update:model-value="(v: string) => (filter.user_id = digitsOnly(v))" />
      </el-form-item>
      <el-form-item :label="kindLabel">
        <el-select v-model="filter.kind" clearable placeholder="全部" data-testid="fin-filter-kind" style="width: 130px">
          <el-option v-for="o in kindOptions" :key="o.value" :label="o.label" :value="o.value" />
        </el-select>
      </el-form-item>
      <el-form-item v-if="statusOptions.length" label="状态">
        <el-select v-model="filter.status" clearable placeholder="全部" data-testid="fin-filter-status" style="width: 130px">
          <el-option v-for="o in statusOptions" :key="o.value" :label="o.label" :value="o.value" />
        </el-select>
      </el-form-item>
      <el-form-item v-if="endpoint === 'orders'" label="批次"><el-input v-model="filter.batch_id" clearable data-testid="fin-filter-batch" style="width: 170px" /></el-form-item>
      <el-form-item label="日期"><DateRange v-model="filter.range" prefix="fin" /></el-form-item>
      <el-button type="primary" native-type="submit" data-testid="fin-search">查询</el-button>
      <el-button data-testid="fin-reset" @click="resetFilter">重置</el-button>
      <span class="spacer" />
      <el-button :loading="exporting" data-testid="fin-export" @click="exportCSV">导出 CSV</el-button>
    </el-form>
    <el-alert v-if="list.error.value" type="error" :title="list.error.value" show-icon :closable="false" class="error" data-testid="fin-error">
      <el-button size="small" :loading="list.loading.value" data-testid="fin-retry" @click="list.load()">重试</el-button>
    </el-alert>
    <div v-loading="list.loading.value">
      <slot name="table" :items="list.items.value" />
    </div>
    <div class="pager-row">
      <span class="muted" data-testid="fin-total">共 {{ list.total.value }} 条</span>
      <el-pagination v-model:current-page="list.page.value" v-model:page-size="list.size.value" :total="list.total.value" layout="prev, pager, next" data-testid="fin-pager" />
    </div>
  </el-card>
</template>

<style scoped>
.filters { display: flex; flex-wrap: wrap; gap: 8px 16px; align-items: center; margin-bottom: 12px; }
.spacer { flex: 1; }
.error { margin-bottom: 12px; }
.pager-row { display: flex; justify-content: space-between; align-items: center; margin-top: 12px; }
.muted { color: #909399; }
</style>
