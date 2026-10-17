<script setup lang="ts">
import { digitsOnly, type Paged } from '@spark/shared'
import { onMounted, reactive, ref } from 'vue'
import { useRoute } from 'vue-router'
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
const filter = reactive({ user_id: digitsOnly(String(route.query.user_id ?? '')), kind: '', status: '', range: { from: '', to: '' } })
// 空值由 buildQuery 省略；日期按 YYYY-MM-DD 原样发出（后端按北京时间解释，to 含当天）
const query = () => ({ user_id: filter.user_id, [props.kindParam]: filter.kind, status: filter.status, from: filter.range.from, to: filter.range.to })
const list = usePaged<Record<string, unknown>>((page, size) => api.get<Paged<Record<string, unknown>>>(`/${props.endpoint}`, { ...query(), page, page_size: size }))
onMounted(list.load)
function resetFilter() {
  Object.assign(filter, { user_id: '', kind: '', status: '', range: { from: '', to: '' } })
  list.reset()
}

const exporting = ref(false)
async function exportCSV() {
  if (exporting.value) return // 同步守卫：连点两次只导出一次
  exporting.value = true
  try {
    const { blob, filename } = await api.download(`/${props.endpoint}/export`, query())
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
    <el-form inline class="filters" @submit.prevent="list.reset()">
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
