<script setup lang="ts">
import {
  digitsOnly, fmtTime, fmtTimeSec, fmtYuan, formatMicroFen, labelOf, llmEndpointLabel, llmUsageStatusLabel, messageOf,
  type LLMDailyAdminRow, type LLMModelAdmin, type LLMSettlementAdmin, type LLMUsageAdminRow, type Listed, type Paged,
} from '@spark/shared'
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { api } from '../../api'
import { usePaged } from '../../composables/usePaged'
import { downloadBlob } from '../../logic/csv'
import { firstOf } from '../../logic/deployments'
import {
  dailyGroupLabel, dailyQuery, emptyUsageFilter, fmtCount, fmtPrice, settlementsQuery, sumDaily, summarizeDaily, usageQuery, validateUsageRange,
  type DailyGroup, type DailySummaryRow, type UsageFilter,
} from '../../logic/llm'
import { notifyError } from '../../logic/notify'
import DateRange from '../DateRange.vue'
import IdLink from '../IdLink.vue'

// 用量 Tab（规格 §11，finance / super）：筛选 + 明细表 + CSV（与明细同一筛选）；汇总按天 / 模型 / 用户（只按日期范围）；结算记录（按用户 + 日期）。
// 日期按北京时间 YYYY-MM-DD 发出，to 含当天，最多 90 天；默认最近 7 天。?user_id= 预填用户。
type View = 'detail' | 'daily' | 'settlements'
const view = ref<View>('detail')
const group = ref<DailyGroup>('day')

const route = useRoute()
const filter = reactive<UsageFilter>(emptyUsageFilter(route.query))
// 已生效的筛选：翻页、导出、切换视图都用它，与屏幕上的表格一致
const applied = ref<UsageFilter>(clone(filter))
function clone(f: UsageFilter): UsageFilter {
  return { ...f, range: { ...f.range } }
}
const rangeError = ref('')

const models = ref<LLMModelAdmin[]>([])
async function loadModels() {
  try {
    models.value = (await api.get<Listed<LLMModelAdmin>>('/llm/models')).items
  } catch (e) {
    notifyError(e) // 只影响模型下拉
  }
}

const detail = usePaged<LLMUsageAdminRow>((page, size) => api.get<Paged<LLMUsageAdminRow>>('/llm/usage', { ...usageQuery(applied.value), page, page_size: size }))
const settlements = usePaged<LLMSettlementAdmin>((page, size) => api.get<Paged<LLMSettlementAdmin>>('/llm/settlements', { ...settlementsQuery(applied.value), page, page_size: size }))

// 汇总：每次按 (日期范围, group_by) 取一次；按天与按模型共用 group_by=model 的数据
const daily = reactive({ loading: false, error: '', rows: [] as LLMDailyAdminRow[], key: '' })
let dailySeq = 0
async function loadDaily(force = false) {
  const q = dailyQuery(applied.value, group.value)
  const key = JSON.stringify(q)
  if (!force && key === daily.key && !daily.error) return
  const mine = ++dailySeq
  daily.loading = true
  try {
    const out = await api.get<Listed<LLMDailyAdminRow>>('/llm/usage/daily', q)
    if (mine !== dailySeq) return
    daily.rows = out.items
    daily.key = key
    daily.error = ''
  } catch (e) {
    if (mine !== dailySeq) return
    notifyError(e)
    daily.rows = []
    daily.key = ''
    daily.error = messageOf(e)
  } finally {
    if (mine === dailySeq) daily.loading = false
  }
}
const summary = computed<DailySummaryRow[]>(() => summarizeDaily(daily.rows, group.value))
const total = computed(() => sumDaily(summary.value))

/** 当前视图按已生效筛选从第 1 页加载（查询时 / 查询后第一次切到该视图） */
function loadView() {
  if (view.value === 'daily') return void loadDaily(true)
  ;(view.value === 'detail' ? detail : settlements).reset()
}
const loadedViews = new Set<View>()
watch(view, (v) => {
  if (loadedViews.has(v)) return // 已加载过：筛选没变就不重复请求（查询时会清空这个集合）
  loadedViews.add(v)
  if (v === 'daily') void loadDaily()
  else loadView() // 查询后第一次打开该视图：回到第 1 页（旧页码属于上一组筛选）
})
watch(group, () => void loadDaily())

onMounted(() => {
  void loadModels()
  loadedViews.add('detail')
  void detail.load()
})
function search() {
  rangeError.value = validateUsageRange(filter.range) ?? ''
  if (rangeError.value) return
  applied.value = clone(filter)
  loadedViews.clear()
  loadedViews.add(view.value)
  loadView()
}
function resetFilter() {
  Object.assign(filter, emptyUsageFilter())
  search()
}
// ?user_id=（账户 Tab 的「用量」链接）：只在当前是用量 Tab 时响应
watch(
  () => [route.query.tab, route.query.user_id] as const,
  ([tab, v]) => {
    const id = digitsOnly(firstOf(v))
    if (firstOf(tab) !== 'usage' || id === applied.value.user_id) return
    Object.assign(filter, emptyUsageFilter(route.query))
    search()
  },
)

const exporting = ref(false)
async function exportCSV() {
  if (exporting.value) return
  exporting.value = true
  try {
    const { blob, filename } = await api.download('/llm/usage/export', usageQuery(applied.value))
    downloadBlob(filename, blob)
  } catch (e) {
    notifyError(e) // export_too_large 等按原文提示
  } finally {
    exporting.value = false
  }
}

const statusOptions = Object.entries(llmUsageStatusLabel).map(([value, label]) => ({ value, label }))
const detailOnly = computed(() => view.value !== 'detail') // 模型 / Key / 状态只作用于明细与导出
const userDisabled = computed(() => view.value === 'daily') // 汇总只按日期范围
const tokens = (r: { prompt_tokens: number; completion_tokens: number }) => `${fmtCount(r.prompt_tokens)} / ${fmtCount(r.completion_tokens)}`
const ms = (n: number | null | undefined) => (n == null ? '—' : `${fmtCount(n)} ms`)
</script>

<template>
  <div data-testid="llm-usage">
    <el-form inline class="filters" @submit.prevent="search">
      <el-form-item label="用户 ID">
        <el-input :model-value="filter.user_id" clearable :disabled="userDisabled" style="width: 110px" data-testid="llm-usage-user" @update:model-value="(v: string) => (filter.user_id = digitsOnly(v))" />
      </el-form-item>
      <el-form-item label="模型">
        <el-select v-model="filter.model_id" clearable placeholder="全部" :disabled="detailOnly" style="width: 160px" data-testid="llm-usage-model">
          <el-option v-for="m in models" :key="m.id" :label="m.slug" :value="m.id" />
        </el-select>
      </el-form-item>
      <el-form-item label="Key ID">
        <el-input :model-value="filter.key_id" clearable :disabled="detailOnly" style="width: 100px" data-testid="llm-usage-key" @update:model-value="(v: string) => (filter.key_id = digitsOnly(v))" />
      </el-form-item>
      <el-form-item label="状态">
        <el-select v-model="filter.status" clearable placeholder="全部" :disabled="detailOnly" style="width: 130px" data-testid="llm-usage-status">
          <el-option v-for="o in statusOptions" :key="o.value" :label="o.label" :value="o.value" />
        </el-select>
      </el-form-item>
      <el-form-item label="日期"><DateRange v-model="filter.range" prefix="llm-usage" /></el-form-item>
      <el-button type="primary" native-type="submit" data-testid="llm-usage-search">查询</el-button>
      <el-button data-testid="llm-usage-reset" @click="resetFilter">重置</el-button>
      <span class="spacer" />
      <el-button v-if="view === 'detail'" :loading="exporting" data-testid="llm-usage-export" @click="exportCSV">导出 CSV</el-button>
    </el-form>
    <p v-if="rangeError" class="err" data-testid="llm-usage-range-error">{{ rangeError }}</p>

    <div class="bar">
      <el-radio-group v-model="view" size="small" data-testid="llm-usage-view">
        <el-radio-button value="detail" data-testid="llm-usage-view-detail">明细</el-radio-button>
        <el-radio-button value="daily" data-testid="llm-usage-view-daily">汇总</el-radio-button>
        <el-radio-button value="settlements" data-testid="llm-usage-view-settlements">结算记录</el-radio-button>
      </el-radio-group>
      <el-radio-group v-if="view === 'daily'" v-model="group" size="small" data-testid="llm-usage-group">
        <el-radio-button v-for="(label, g) in dailyGroupLabel" :key="g" :value="g" :data-testid="`llm-usage-group-${g}`">{{ label }}</el-radio-button>
      </el-radio-group>
    </div>

    <!-- 明细 -->
    <template v-if="view === 'detail'">
      <el-alert v-if="detail.error.value" type="error" :title="detail.error.value" show-icon :closable="false" class="error" data-testid="llm-usage-error">
        <el-button size="small" :loading="detail.loading.value" data-testid="llm-usage-retry" @click="detail.load()">重试</el-button>
      </el-alert>
      <el-table :data="detail.items.value" v-loading="detail.loading.value" row-key="request_id" size="small" data-testid="llm-usage-table">
        <el-table-column label="时间" width="160"><template #default="{ row }"><span data-testid="llm-usage-time">{{ fmtTimeSec(row.created_at) }}</span></template></el-table-column>
        <el-table-column label="请求 ID" min-width="190"><template #default="{ row }"><span class="mono" data-testid="llm-usage-rid">{{ row.request_id }}</span></template></el-table-column>
        <el-table-column label="用户" width="130">
          <template #default="{ row }"><router-link :to="`/users/${row.user_id}`" class="mono" data-testid="llm-usage-row-user">{{ row.user_phone || row.user_id }}</router-link></template>
        </el-table-column>
        <el-table-column label="模型" min-width="130"><template #default="{ row }"><span class="mono" data-testid="llm-usage-row-model">{{ row.model }}</span></template></el-table-column>
        <el-table-column label="Key" min-width="140"><template #default="{ row }">{{ row.key_name }} <span class="mono muted">{{ row.key_hint }}</span></template></el-table-column>
        <el-table-column label="端点" width="90"><template #default="{ row }">{{ labelOf(llmEndpointLabel, row.endpoint) }}{{ row.stream ? ' · 流式' : '' }}</template></el-table-column>
        <el-table-column label="输入 / 输出 token" width="150">
          <template #default="{ row }"><span data-testid="llm-usage-tokens">{{ tokens(row as LLMUsageAdminRow) }}</span><el-tag v-if="row.estimated" size="small" type="warning" class="tag">估算</el-tag></template>
        </el-table-column>
        <el-table-column label="单价（元 / 百万）" width="140"><template #default="{ row }">{{ fmtPrice(row.input_price) }} / {{ fmtPrice(row.output_price) }}</template></el-table-column>
        <el-table-column label="费用" width="110"><template #default="{ row }"><span data-testid="llm-usage-cost">{{ formatMicroFen(row.cost_micro) }}</span></template></el-table-column>
        <el-table-column label="状态" width="120">
          <template #default="{ row }"><span data-testid="llm-usage-row-status" :class="{ bad: row.status !== 'ok' }">{{ labelOf(llmUsageStatusLabel, row.status) }}</span> <span class="muted">{{ row.http_status }}</span></template>
        </el-table-column>
        <el-table-column label="延迟 / 首字" width="140"><template #default="{ row }">{{ ms(row.latency_ms) }} / {{ ms(row.ttft_ms) }}</template></el-table-column>
        <el-table-column label="节点" width="110"><template #default="{ row }"><span class="mono">{{ row.node_id || '—' }}</span></template></el-table-column>
        <el-table-column label="来源 IP" width="130"><template #default="{ row }"><span class="mono">{{ row.client_ip || '—' }}</span></template></el-table-column>
      </el-table>
      <div class="pager-row">
        <span class="muted" data-testid="llm-usage-total">共 {{ detail.total.value }} 条</span>
        <el-pagination v-model:current-page="detail.page.value" v-model:page-size="detail.size.value" :total="detail.total.value" layout="prev, pager, next" data-testid="llm-usage-pager" />
      </div>
    </template>

    <!-- 汇总 -->
    <template v-else-if="view === 'daily'">
      <p class="muted">汇总含未结算的用量，只按日期范围筛选。</p>
      <el-alert v-if="daily.error" type="error" :title="daily.error" show-icon :closable="false" class="error" data-testid="llm-daily-error">
        <el-button size="small" :loading="daily.loading" data-testid="llm-daily-retry" @click="loadDaily(true)">重试</el-button>
      </el-alert>
      <el-table :data="summary" v-loading="daily.loading" row-key="key" size="small" empty-text="这段时间没有用量" data-testid="llm-daily-table">
        <el-table-column v-if="group === 'day'" label="日期" width="130"><template #default="{ row }"><span data-testid="llm-daily-key">{{ row.day }}</span></template></el-table-column>
        <el-table-column v-else-if="group === 'model'" label="模型" min-width="160"><template #default="{ row }"><span class="mono" data-testid="llm-daily-key">{{ row.model }}</span></template></el-table-column>
        <el-table-column v-else label="用户" min-width="160">
          <template #default="{ row }"><span data-testid="llm-daily-key"><router-link :to="`/users/${row.user_id}`" class="mono">{{ row.phone || row.user_id }}</router-link></span></template>
        </el-table-column>
        <el-table-column label="请求数" width="110"><template #default="{ row }"><span data-testid="llm-daily-requests">{{ fmtCount(row.requests) }}</span></template></el-table-column>
        <el-table-column label="输入 token" width="140"><template #default="{ row }">{{ fmtCount(row.prompt_tokens) }}</template></el-table-column>
        <el-table-column label="输出 token" width="140"><template #default="{ row }">{{ fmtCount(row.completion_tokens) }}</template></el-table-column>
        <el-table-column label="费用" min-width="120"><template #default="{ row }"><span data-testid="llm-daily-cost">{{ formatMicroFen(row.cost_micro) }}</span></template></el-table-column>
      </el-table>
      <div v-if="summary.length" class="sum" data-testid="llm-daily-total">
        合计：{{ fmtCount(total.requests) }} 次请求 · 输入 {{ fmtCount(total.prompt_tokens) }} · 输出 {{ fmtCount(total.completion_tokens) }} token · {{ formatMicroFen(total.cost_micro) }}
      </div>
    </template>

    <!-- 结算记录 -->
    <template v-else>
      <p class="muted">按用户与结算时间筛选；「并入欠费」为本次一并结算的历史欠费，「实扣」为钱包实际扣款，不足部分记为欠费。</p>
      <el-alert v-if="settlements.error.value" type="error" :title="settlements.error.value" show-icon :closable="false" class="error" data-testid="llm-settle-error">
        <el-button size="small" :loading="settlements.loading.value" data-testid="llm-settle-retry" @click="settlements.load()">重试</el-button>
      </el-alert>
      <el-table :data="settlements.items.value" v-loading="settlements.loading.value" row-key="id" size="small" data-testid="llm-settle-table">
        <el-table-column label="ID" width="80"><template #default="{ row }"><span class="mono" data-testid="llm-settle-id">{{ row.id }}</span></template></el-table-column>
        <el-table-column label="结算时间" width="150"><template #default="{ row }">{{ fmtTime(row.created_at) }}</template></el-table-column>
        <el-table-column label="用户" width="90"><template #default="{ row }"><IdLink kind="user" :id="row.user_id" /></template></el-table-column>
        <el-table-column label="用量区间" min-width="220"><template #default="{ row }">{{ fmtTime(row.period_from) || '—' }} ～ {{ fmtTime(row.period_to) || '—' }}</template></el-table-column>
        <el-table-column label="请求数" width="90"><template #default="{ row }">{{ fmtCount(row.requests) }}</template></el-table-column>
        <el-table-column label="输入 / 输出 token" width="160"><template #default="{ row }">{{ tokens(row as LLMSettlementAdmin) }}</template></el-table-column>
        <el-table-column label="用量金额" width="110"><template #default="{ row }"><span data-testid="llm-settle-usage">{{ fmtYuan(row.usage_amount) }}</span></template></el-table-column>
        <el-table-column label="并入欠费" width="100"><template #default="{ row }">{{ fmtYuan(row.debt_amount) }}</template></el-table-column>
        <el-table-column label="实扣" width="110">
          <template #default="{ row }"><span data-testid="llm-settle-charged" :class="{ bad: row.charged < row.usage_amount + row.debt_amount }">{{ fmtYuan(row.charged) }}</span></template>
        </el-table-column>
        <el-table-column label="流水" width="90"><template #default="{ row }"><span class="mono">{{ row.wallet_ledger_id ?? '—' }}</span></template></el-table-column>
      </el-table>
      <div class="pager-row">
        <span class="muted" data-testid="llm-settle-total">共 {{ settlements.total.value }} 条</span>
        <el-pagination v-model:current-page="settlements.page.value" v-model:page-size="settlements.size.value" :total="settlements.total.value" layout="prev, pager, next" data-testid="llm-settle-pager" />
      </div>
    </template>
  </div>
</template>

<style scoped>
.filters { display: flex; flex-wrap: wrap; gap: 8px 16px; align-items: center; margin-bottom: 4px; }
.spacer { flex: 1; }
.bar { display: flex; gap: 12px; align-items: center; margin: 8px 0 12px; }
.error { margin-bottom: 12px; }
.pager-row { display: flex; justify-content: space-between; align-items: center; margin-top: 12px; }
.muted { color: #909399; font-size: 13px; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.bad { color: var(--el-color-danger); }
.err { color: var(--el-color-danger); font-size: 13px; margin: 0 0 8px; }
.tag { margin-left: 4px; }
.sum { margin-top: 8px; font-size: 13px; }
</style>
