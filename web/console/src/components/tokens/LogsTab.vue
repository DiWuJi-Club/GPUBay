<script setup lang="ts">
import { fmtTimeSec, formatMicroFen, labelOf, llmEndpointLabel, messageOf, useMobile, type LLMKey, type LLMModelPublic, type LLMUsageRow, type Paged } from '@spark/shared'
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { api } from '../../api'
import { copyText } from '../../logic/clipboard'
import { notifyError } from '../../logic/notify'
import { addDays, beijingToday, defaultUsageRange, keyHintMask, latencyLabel, logModelOptions, tokenCount, usageStatusTag, USAGE_MAX_DAYS, validateUsageRange } from '../../logic/tokens'

// 请求日志 Tab（规格 §9.2 第 4 项）：筛选（模型、Key、日期，最长 90 天，默认最近 7 天）+ GET /llm/usage 分页。
const props = defineProps<{ models: LLMModelPublic[] }>()
const mobile = useMobile()

// 「今天」在页面停留跨过零点后会变：每次加载前、日期框获得焦点时重算（日期框的 min / max 随之更新）
const today = ref(beijingToday(new Date()))
const earliest = computed(() => addDays(today.value, -(USAGE_MAX_DAYS - 1))) // 明细只保留 90 天
const refreshToday = () => {
  today.value = beijingToday(new Date())
}
const filter = reactive({ model: '', keyId: null as number | null, ...defaultUsageRange(new Date()) })
const rangeError = computed(() => validateUsageRange(filter.from, filter.to))

// Key 下拉：GET /llm/keys（含 30 天内撤销的，旧日志仍能按它筛）
const keys = ref<LLMKey[]>([])
async function loadKeys() {
  try {
    keys.value = (await api.get<{ items: LLMKey[] }>('/llm/keys')).items
  } catch (e) {
    notifyError(e) // 只影响下拉，日志照常显示
  }
}

const PAGE_SIZE = 20
const items = ref<LLMUsageRow[]>([])
const total = ref(0)
const page = ref(1)
const loading = ref(true)
const loadError = ref('')
// 筛选连续变化时，只认最后一次请求的结果
let gen = 0
async function load() {
  refreshToday()
  if (rangeError.value) {
    // 范围不合法：清空旧结果（不再显示上一个范围的数据），并作废进行中的请求
    gen++
    items.value = []
    total.value = 0
    loading.value = false
    loadError.value = ''
    return
  }
  const my = ++gen
  loading.value = true
  loadError.value = ''
  try {
    const r = await api.get<Paged<LLMUsageRow>>('/llm/usage', {
      from: filter.from, to: filter.to, page: page.value, page_size: PAGE_SIZE,
      model: filter.model || undefined, key_id: filter.keyId ?? undefined,
    })
    if (my !== gen) return
    items.value = r.items
    total.value = r.total
  } catch (e) {
    if (my !== gen) return
    notifyError(e)
    loadError.value = messageOf(e)
  } finally {
    if (my === gen) loading.value = false
  }
}
watch(
  () => [filter.model, filter.keyId, filter.from, filter.to],
  () => {
    if (page.value !== 1) page.value = 1 // 由 page 的 watch 触发加载
    else void load()
  },
)
watch(page, load)
onMounted(() => {
  void loadKeys()
  void load()
})
function resetRange() {
  refreshToday()
  Object.assign(filter, defaultUsageRange(new Date()))
}

// 模型下拉：在线模型 ∪ 已加载日志里的模型（含已下线的）∪ 当前选中的
const modelOptions = computed(() => logModelOptions(props.models.map((m) => m.slug), items.value.map((u) => u.model), filter.model))

/** Key 显示：名称 + 提示（有提示时），区分同名 Key */
const keyLabel = (name: string, hint?: string) => (hint ? `${name}（${keyHintMask(hint)}）` : name)
const rows = computed(() =>
  items.value.map((u) => ({
    u,
    key: keyLabel(u.key_name, u.key_hint),
    httpStatus: u.http_status && u.http_status !== 200 ? String(u.http_status) : '',
    time: fmtTimeSec(u.created_at),
    endpoint: labelOf(llmEndpointLabel, u.endpoint),
    tokens: `${tokenCount(u.prompt_tokens, u.estimated)} / ${tokenCount(u.completion_tokens, u.estimated)}`,
    cost: formatMicroFen(u.cost_micro),
    status: usageStatusTag(u.status),
    latency: `${latencyLabel(u.latency_ms)} / ${latencyLabel(u.ttft_ms)}`,
  })),
)
</script>

<template>
  <div class="logs">
    <div class="filters">
      <el-select v-model="filter.model" clearable placeholder="全部模型" class="sel" data-testid="tokens-logs-model">
        <el-option v-for="m in modelOptions" :key="m" :label="m" :value="m" :data-testid="`tokens-logs-model-option-${m}`" />
      </el-select>
      <el-select v-model="filter.keyId" clearable placeholder="全部 Key" class="sel" data-testid="tokens-logs-key">
        <el-option v-for="k in keys" :key="k.id" :label="keyLabel(k.name, k.key_hint)" :value="k.id" :data-testid="`tokens-logs-key-option-${k.id}`" />
      </el-select>
      <span class="range">
        <input v-model="filter.from" type="date" :min="earliest" :max="today" class="el-input__inner native-date" aria-label="开始日期" @focus="refreshToday" data-testid="tokens-logs-from" />
        <span class="muted">至</span>
        <input v-model="filter.to" type="date" :min="earliest" :max="today" class="el-input__inner native-date" aria-label="结束日期" @focus="refreshToday" data-testid="tokens-logs-to" />
      </span>
      <el-button size="small" data-testid="tokens-logs-reset" @click="resetRange">最近 7 天</el-button>
    </div>
    <p class="muted">北京时间，含首尾两天，最长 {{ USAGE_MAX_DAYS }} 天（明细保留 90 天）。带「≈」的 token 数为估算（如客户端中途断开）。</p>
    <p v-if="rangeError" class="danger" data-testid="tokens-logs-range-error">{{ rangeError }}</p>

    <el-alert v-if="loadError" type="error" :title="loadError" show-icon :closable="false" data-testid="tokens-logs-error">
      <el-button size="small" data-testid="tokens-logs-retry" @click="load">重试</el-button>
    </el-alert>
    <el-skeleton v-else-if="loading && items.length === 0" :rows="4" animated />
    <el-empty v-else-if="rows.length === 0" description="该范围内没有请求" :image-size="60" data-testid="tokens-logs-empty" />

    <!-- 手机：卡片 -->
    <div v-else-if="mobile" class="cards" data-testid="tokens-logs-cards">
      <el-card v-for="r in rows" :key="r.u.request_id" shadow="never" :data-testid="`tokens-log-row-${r.u.request_id}`">
        <div class="card-head">
          <b>{{ r.u.model }}</b>
          <span>
            <el-tag :type="r.status.type" size="small" data-testid="tokens-log-status">{{ r.status.label }}</el-tag>
            <span v-if="r.httpStatus" class="muted" data-testid="tokens-log-http"> {{ r.httpStatus }}</span>
          </span>
        </div>
        <ul class="meta">
          <li><span class="label">时间</span>{{ r.time }}</li>
          <li><span class="label">Key</span><span data-testid="tokens-log-key">{{ r.key }}</span></li>
          <li><span class="label">端点</span>{{ r.endpoint }}{{ r.u.stream ? ' · 流式' : '' }}</li>
          <li><span class="label">输入/输出</span><span data-testid="tokens-log-tokens">{{ r.tokens }}</span></li>
          <li><span class="label">费用</span><span data-testid="tokens-log-cost">{{ r.cost }}</span></li>
          <li><span class="label">延迟/TTFT</span><span data-testid="tokens-log-latency">{{ r.latency }}</span></li>
          <li>
            <span class="label">请求 ID</span>
            <code class="mono" data-testid="tokens-log-request-id">{{ r.u.request_id }}</code>
            <el-button link type="primary" size="small" data-testid="tokens-log-copy" @click="copyText(r.u.request_id)">复制</el-button>
          </li>
        </ul>
      </el-card>
    </div>

    <!-- 电脑：表格 -->
    <el-table v-else :data="rows" size="small" row-key="u.request_id" data-testid="tokens-logs-table">
      <el-table-column label="时间" min-width="150">
        <template #default="{ row }"><span :data-testid="`tokens-log-row-${row.u.request_id}`">{{ row.time }}</span></template>
      </el-table-column>
      <el-table-column label="模型" min-width="110">
        <template #default="{ row }">{{ row.u.model }}</template>
      </el-table-column>
      <el-table-column label="Key" min-width="100">
        <template #default="{ row }"><span data-testid="tokens-log-key">{{ row.key }}</span></template>
      </el-table-column>
      <el-table-column label="端点" min-width="60">
        <template #default="{ row }"><span data-testid="tokens-log-endpoint">{{ row.endpoint }}</span></template>
      </el-table-column>
      <el-table-column label="流式" min-width="50">
        <template #default="{ row }"><span data-testid="tokens-log-stream">{{ row.u.stream ? '是' : '否' }}</span></template>
      </el-table-column>
      <el-table-column label="输入 / 输出 token" min-width="140">
        <template #default="{ row }"><span data-testid="tokens-log-tokens">{{ row.tokens }}</span></template>
      </el-table-column>
      <el-table-column label="费用" min-width="90">
        <template #default="{ row }"><span data-testid="tokens-log-cost">{{ row.cost }}</span></template>
      </el-table-column>
      <el-table-column label="状态" min-width="100">
        <template #default="{ row }">
          <el-tag :type="row.status.type" size="small" data-testid="tokens-log-status">{{ row.status.label }}</el-tag>
          <span v-if="row.httpStatus" class="muted" data-testid="tokens-log-http"> {{ row.httpStatus }}</span>
        </template>
      </el-table-column>
      <el-table-column label="延迟 / TTFT" min-width="130">
        <template #default="{ row }"><span data-testid="tokens-log-latency">{{ row.latency }}</span></template>
      </el-table-column>
      <el-table-column label="请求 ID" min-width="200">
        <template #default="{ row }">
          <code class="mono" data-testid="tokens-log-request-id">{{ row.u.request_id }}</code>
          <el-button link type="primary" size="small" data-testid="tokens-log-copy" @click="copyText(row.u.request_id)">复制</el-button>
        </template>
      </el-table-column>
    </el-table>

    <div v-if="total > 0" class="pager">
      <span class="muted" data-testid="tokens-logs-total">共 {{ total }} 条</span>
      <el-pagination
        v-if="total > PAGE_SIZE"
        v-model:current-page="page"
        :page-size="PAGE_SIZE"
        :total="total"
        layout="prev, pager, next"
        size="small"
        data-testid="tokens-logs-pager"
      />
    </div>
  </div>
</template>

<style scoped>
.logs { display: grid; grid-template-columns: minmax(0, 1fr); gap: 12px; }
.filters { display: flex; flex-wrap: wrap; align-items: center; gap: 8px; }
.sel { width: 170px; }
.range { display: inline-flex; align-items: center; gap: 6px; flex-wrap: wrap; }
/* el-input__inner 自带 width: 100% / flex-grow: 1，在可换行的 flex 里会各占一整行：固定宽度 */
.native-date { width: 150px; flex: none; height: 32px; border: 1px solid #dcdfe6; border-radius: 4px; padding: 0 8px; font-size: 14px; background: #fff; }
.muted { color: #909399; font-size: 12px; }
p { margin: 0; }
.danger { color: var(--el-color-danger); font-size: 13px; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 12px; word-break: break-all; }
.cards { display: grid; gap: 12px; }
.card-head { display: flex; justify-content: space-between; align-items: center; gap: 8px; margin-bottom: 6px; }
.meta { list-style: none; padding: 0; margin: 0; display: grid; gap: 4px; font-size: 13px; }
.meta .label { display: inline-block; width: 76px; color: #909399; }
.pager { display: flex; align-items: center; justify-content: space-between; flex-wrap: wrap; gap: 8px; }
</style>
