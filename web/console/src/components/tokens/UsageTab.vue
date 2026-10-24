<script setup lang="ts">
import { fmtTime, fmtYuan, formatMicroFen, formatTokens, messageOf, type LLMAccount, type LLMDailyRow, type LLMSettlement, type Paged } from '@spark/shared'
import { computed, onMounted, ref, watch } from 'vue'
import { api } from '../../api'
import { notifyError } from '../../logic/notify'
import { settlementDetail, settlementLedgerLink, tokenCount } from '../../logic/tokens'

// 用量 Tab（规格 §9.2 第 3 项）：统计卡（来自 /llm/account，由 TokensView 加载后传入）、最近 30 天按天 × 模型、结算记录（分页）。
const props = defineProps<{ account: LLMAccount | null; accountError?: string }>()
const emit = defineEmits<{ retryAccount: [] }>()

const DAYS = 30
const daily = ref<LLMDailyRow[]>([])
const dailyLoading = ref(true)
const dailyError = ref('')
async function loadDaily() {
  dailyLoading.value = true
  dailyError.value = ''
  try {
    daily.value = (await api.get<{ items: LLMDailyRow[] }>('/llm/usage/daily', { days: DAYS })).items
  } catch (e) {
    notifyError(e)
    dailyError.value = messageOf(e)
  } finally {
    dailyLoading.value = false
  }
}
// 新的日期在前，同一天按模型排序
const dailyRows = computed(() =>
  [...daily.value]
    .sort((a, b) => (a.day === b.day ? a.model.localeCompare(b.model) : a.day < b.day ? 1 : -1))
    .map((r) => ({ ...r, cost: formatMicroFen(r.cost_micro) })),
)

const PAGE_SIZE = 10
const settlements = ref<LLMSettlement[]>([])
const settleTotal = ref(0)
const settlePage = ref(1)
const settleLoading = ref(true)
const settleError = ref('')
async function loadSettlements() {
  settleLoading.value = true
  settleError.value = ''
  try {
    const r = await api.get<Paged<LLMSettlement>>('/llm/settlements', { page: settlePage.value, page_size: PAGE_SIZE })
    settlements.value = r.items
    settleTotal.value = r.total
  } catch (e) {
    notifyError(e)
    settleError.value = messageOf(e)
  } finally {
    settleLoading.value = false
  }
}
watch(settlePage, loadSettlements)
const settleRows = computed(() =>
  settlements.value.map((s) => ({ s, detail: settlementDetail(s), link: settlementLedgerLink(s), short: s.charged < s.usage_amount + s.debt_amount })),
)

onMounted(() => {
  void loadDaily()
  void loadSettlements()
})

const stats = computed(() => {
  const a = props.account
  const v = (f: (a: LLMAccount) => string) => (a ? f(a) : '—')
  return [
    { id: 'today', label: '今日消费', value: v((a) => fmtYuan(a.today_cost)) },
    { id: 'month', label: '本月消费', value: v((a) => fmtYuan(a.month_cost)) },
    { id: 'tokens', label: '本月 token', value: v((a) => formatTokens(a.month_tokens)) },
    { id: 'unsettled', label: '未结算', value: v((a) => fmtYuan(a.unsettled)), hint: '累计到 ¥1 或满 10 分钟时结算' },
    { id: 'debt', label: '欠费', value: v((a) => fmtYuan(a.debt)), danger: !!a && a.debt > 0 },
  ]
})
</script>

<template>
  <div class="usage">
    <el-alert v-if="accountError" type="error" :title="accountError" show-icon :closable="false" data-testid="tokens-account-error">
      <el-button size="small" data-testid="tokens-account-retry" @click="emit('retryAccount')">重试</el-button>
    </el-alert>
    <div class="stats">
      <el-card v-for="s in stats" :key="s.id" shadow="never" class="stat" :class="{ danger: s.danger }">
        <div class="muted">{{ s.label }}</div>
        <div class="value" :data-testid="`tokens-stat-${s.id}`">{{ s.value }}</div>
        <div v-if="s.hint" class="muted hint">{{ s.hint }}</div>
      </el-card>
    </div>

    <section>
      <h3>最近 {{ DAYS }} 天</h3>
      <el-alert v-if="dailyError" type="error" :title="dailyError" show-icon :closable="false" data-testid="tokens-daily-error">
        <el-button size="small" data-testid="tokens-daily-retry" @click="loadDaily">重试</el-button>
      </el-alert>
      <el-skeleton v-else-if="dailyLoading" :rows="3" animated />
      <el-empty v-else-if="dailyRows.length === 0" description="最近 30 天没有调用" :image-size="60" data-testid="tokens-daily-empty" />
      <el-table v-else :data="dailyRows" size="small" data-testid="tokens-daily-table">
        <el-table-column label="日期" min-width="100">
          <template #default="{ row }"><span data-testid="tokens-daily-row">{{ row.day }}</span></template>
        </el-table-column>
        <el-table-column label="模型" prop="model" min-width="120" />
        <el-table-column label="请求数" min-width="80">
          <template #default="{ row }">{{ tokenCount(row.requests) }}</template>
        </el-table-column>
        <el-table-column label="输入 token" min-width="100">
          <template #default="{ row }">{{ tokenCount(row.prompt_tokens) }}</template>
        </el-table-column>
        <el-table-column label="输出 token" min-width="100">
          <template #default="{ row }">{{ tokenCount(row.completion_tokens) }}</template>
        </el-table-column>
        <el-table-column label="费用" min-width="100">
          <template #default="{ row }"><span data-testid="tokens-daily-cost">{{ row.cost }}</span></template>
        </el-table-column>
      </el-table>
    </section>

    <section>
      <h3>结算记录</h3>
      <p class="muted">用量累计到 ¥1 或最早一笔满 10 分钟时，从钱包余额扣款一次，每次一条「Token 消费」流水。</p>
      <el-alert v-if="settleError" type="error" :title="settleError" show-icon :closable="false" data-testid="tokens-settlements-error">
        <el-button size="small" data-testid="tokens-settlements-retry" @click="loadSettlements">重试</el-button>
      </el-alert>
      <el-skeleton v-else-if="settleLoading && settlements.length === 0" :rows="3" animated />
      <el-empty v-else-if="settleRows.length === 0" description="还没有结算记录" :image-size="60" data-testid="tokens-settlements-empty" />
      <el-table v-else :data="settleRows" size="small" data-testid="tokens-settlements-table">
        <el-table-column label="时间" min-width="140">
          <template #default="{ row }"><span :data-testid="`tokens-settlement-row-${row.s.id}`">{{ fmtTime(row.s.created_at) }}</span></template>
        </el-table-column>
        <el-table-column label="请求数" min-width="70">
          <template #default="{ row }">{{ tokenCount(row.s.requests) }}</template>
        </el-table-column>
        <el-table-column label="token（输入 / 输出）" min-width="140">
          <template #default="{ row }">{{ formatTokens(row.s.prompt_tokens) }} / {{ formatTokens(row.s.completion_tokens) }}</template>
        </el-table-column>
        <el-table-column label="金额" min-width="170">
          <template #default="{ row }">
            <b :data-testid="`tokens-settlement-charged-${row.s.id}`">{{ fmtYuan(row.s.charged) }}</b>
            <div class="muted" :class="{ warn: row.short }" :data-testid="`tokens-settlement-detail-${row.s.id}`">{{ row.detail.join('；') }}</div>
          </template>
        </el-table-column>
        <el-table-column label="流水" min-width="80">
          <template #default="{ row }">
            <router-link v-if="row.link" :to="row.link" :data-testid="`tokens-settlement-ledger-${row.s.id}`">查看</router-link>
            <span v-else class="muted">—</span>
          </template>
        </el-table-column>
      </el-table>
      <el-pagination
        v-if="settleTotal > PAGE_SIZE"
        v-model:current-page="settlePage"
        :page-size="PAGE_SIZE"
        :total="settleTotal"
        layout="prev, pager, next"
        size="small"
        data-testid="tokens-settlements-pager"
      />
    </section>
  </div>
</template>

<style scoped>
.usage { display: grid; grid-template-columns: minmax(0, 1fr); gap: 20px; }
.stats { display: grid; grid-template-columns: repeat(auto-fill, minmax(150px, 1fr)); gap: 12px; }
.stat .value { font-size: 22px; font-weight: 600; margin-top: 4px; }
.stat.danger .value { color: var(--el-color-danger); }
.hint { margin-top: 4px; }
section { min-width: 0; }
h3 { margin: 0 0 8px; font-size: 15px; }
.muted { color: #909399; font-size: 12px; }
p.muted { margin: 0 0 8px; }
.warn { color: var(--el-color-danger); }
</style>
