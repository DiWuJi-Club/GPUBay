<script setup lang="ts">
import {
  durationLabel, fmtTime, fmtYuan, labelOf, ledgerKindLabel, messageOf, orderKindLabel, orderStatusLabel, rechargeChannelLabel, rechargeStatusLabel, useMobile,
  type LedgerRow, type Listed, type Order, type Paged, type Recharge,
} from '@spark/shared'
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import GuideDialog from '../components/GuideDialog.vue'
import { useGuide } from '../composables/useGuide'
import { notifyError } from '../logic/notify'
import { session } from '../store/session'

const route = useRoute()
const router = useRouter()
const guide = useGuide()
const mobile = useMobile()
const tabs = ['ledger', 'orders', 'recharges'] as const
type Tab = (typeof tabs)[number]
const tab = computed<Tab>({
  get: () => (tabs.includes(route.query.tab as Tab) ? (route.query.tab as Tab) : 'ledger'),
  set: (v) => void router.replace({ query: { ...route.query, tab: v } }),
})

// ---- 余额 ----
const balance = ref<number | null>(null)
const balanceError = ref('')
async function loadBalance() {
  try {
    balance.value = (await api.get<{ balance: number }>('/wallet')).balance
    session.balance = balance.value
    balanceError.value = ''
  } catch (e) {
    balanceError.value = messageOf(e)
    notifyError(e) // 401 时静默：handleUnauthorized 已提示「请先登录」并跳转（C-AUTH-06 要求只有一条提示）
  }
}
function goRecharge() {
  if (session.user?.kyc_status !== 'verified') return guide.kycRequired() // 后端创建充值单要求已实名
  void router.push('/wallet/recharge')
}

// ---- 流水：游标分页 + 滚动到底自动加载 ----
/** 邀请返利绿色、返利扣回橙色 */
const kindClass = (kind: string) => ({ 'kind-rebate': kind === 'referral_rebate', 'kind-clawback': kind === 'referral_clawback' })
const PAGE = 20
const ledger = ref<LedgerRow[]>([])
const ledgerHasMore = ref(true)
const ledgerLoading = ref(false)
const ledgerError = ref('')
const sentinel = ref<HTMLElement | null>(null)
// 每次重置加一代：刷新时还在路上的旧请求回来后直接丢弃，不会把旧数据拼进新列表
let ledgerGen = 0
async function loadLedger(reset = false) {
  if (reset) {
    ledgerGen++
    ledger.value = []
    ledgerHasMore.value = true
    ledgerError.value = ''
    ledgerLoading.value = false
  }
  // 出错后停止自动加载，直到用户点「重试」，避免哨兵反复触发请求和提示
  if (ledgerLoading.value || !ledgerHasMore.value || ledgerError.value) return
  const gen = ledgerGen
  ledgerLoading.value = true
  try {
    const before = ledger.value.length ? ledger.value[ledger.value.length - 1].id : undefined
    const { items } = await api.get<Listed<LedgerRow>>('/wallet/ledger', { before_id: before, limit: PAGE })
    if (gen !== ledgerGen) return
    ledger.value.push(...items)
    ledgerHasMore.value = items.length === PAGE
  } catch (e) {
    if (gen !== ledgerGen) return
    ledgerError.value = messageOf(e)
    notifyError(e)
    return
  } finally {
    if (gen === ledgerGen) ledgerLoading.value = false
  }
  // 只在成功后：屏幕很高时第一页加载完哨兵可能仍在视口内，IntersectionObserver 不会再触发，重新观察一次让它按当前位置再判断
  await nextTick()
  if (sentinel.value && io) {
    io.unobserve(sentinel.value)
    io.observe(sentinel.value)
  }
}
function retryLedger() {
  ledgerError.value = ''
  void loadLedger()
}
let io: IntersectionObserver | undefined
onMounted(() => {
  io = new IntersectionObserver((entries) => {
    if (entries.some((en) => en.isIntersecting)) void loadLedger()
  })
})
watch(sentinel, (el, old) => {
  if (old) io?.unobserve(old)
  if (el) io?.observe(el)
})
onUnmounted(() => io?.disconnect())

// ---- 订单 ----
const orders = ref<Order[]>([])
async function loadOrders() {
  try {
    orders.value = (await api.get<Listed<Order>>('/orders')).items
  } catch (e) {
    notifyError(e)
  }
}

// ---- 充值记录 ----
const recharges = ref<Recharge[]>([])
const rechargeTotal = ref(0)
const rechargePage = ref(1)
async function loadRecharges() {
  try {
    const out = await api.get<Paged<Recharge>>('/recharges', { page: rechargePage.value, page_size: 10 })
    recharges.value = out.items
    rechargeTotal.value = out.total
  } catch (e) {
    notifyError(e)
  }
}
watch(rechargePage, loadRecharges)

const loaded = new Set<Tab>()
watch(
  tab,
  (t) => {
    if (loaded.has(t)) return
    loaded.add(t)
    void loadTab(t)
  },
  { immediate: true },
)

function loadTab(t: Tab): Promise<void> {
  if (t === 'ledger') return loadLedger(true)
  if (t === 'orders') return loadOrders()
  return loadRecharges()
}
// 余额与当前标签页并发刷新；会话失效时两个请求都会 401，由 handleUnauthorized 保证只提示一次、只跳一次（C-AUTH-06）
async function refresh() {
  loaded.clear()
  loaded.add(tab.value)
  await Promise.all([loadBalance(), loadTab(tab.value)])
}
onMounted(loadBalance)

const signed = (n: number) => (n >= 0 ? `+${fmtYuan(n)}` : fmtYuan(n))
</script>

<template>
  <div class="wallet">
    <el-card class="balance-card">
      <div class="balance-row">
        <div>
          <div class="muted">账户余额</div>
          <div class="balance" data-testid="wallet-balance">{{ balance == null ? '—' : fmtYuan(balance) }}</div>
          <el-alert v-if="balanceError" type="error" :title="balanceError" :closable="false" show-icon data-testid="wallet-error" />
        </div>
        <div class="balance-actions">
          <el-button data-testid="wallet-refresh" @click="refresh">刷新</el-button>
          <el-button type="primary" data-testid="wallet-recharge-btn" @click="goRecharge">充值</el-button>
        </div>
      </div>
    </el-card>

    <el-card>
      <el-tabs v-model="tab">
        <el-tab-pane label="流水" name="ledger">
          <template v-if="mobile">
            <div v-for="r in ledger" :key="r.id" class="row-card" data-testid="ledger-row">
              <div class="row-head">
                <span :class="kindClass(r.kind)" data-testid="ledger-kind">{{ labelOf(ledgerKindLabel, r.kind) }}</span>
                <b :class="r.amount >= 0 ? 'money-plus' : 'money-minus'" data-testid="ledger-amount">{{ signed(r.amount) }}</b>
              </div>
              <div class="muted">余额 <span data-testid="ledger-after">{{ fmtYuan(r.balance_after) }}</span> · <span data-testid="ledger-time">{{ fmtTime(r.created_at) }}</span></div>
              <div v-if="r.remark" class="muted" data-testid="ledger-remark">{{ r.remark }}</div>
            </div>
          </template>
          <el-table v-else :data="ledger" row-key="id">
            <el-table-column label="类型" width="100"><template #default="{ row }"><span :class="kindClass(row.kind)" data-testid="ledger-kind">{{ labelOf(ledgerKindLabel, row.kind) }}</span></template></el-table-column>
            <el-table-column label="金额" width="140"><template #default="{ row }"><b :class="row.amount >= 0 ? 'money-plus' : 'money-minus'" data-testid="ledger-amount">{{ signed(row.amount) }}</b></template></el-table-column>
            <el-table-column label="变动后余额" width="140"><template #default="{ row }"><span data-testid="ledger-after">{{ fmtYuan(row.balance_after) }}</span></template></el-table-column>
            <el-table-column label="备注" min-width="160"><template #default="{ row }"><span data-testid="ledger-remark">{{ row.remark }}</span></template></el-table-column>
            <el-table-column label="时间" width="160"><template #default="{ row }"><span data-testid="ledger-time">{{ fmtTime(row.created_at) }}</span></template></el-table-column>
          </el-table>
          <el-alert v-if="ledgerError" type="error" :title="ledgerError" :closable="false" show-icon data-testid="wallet-ledger-error">
            <el-button size="small" data-testid="wallet-ledger-retry" @click="retryLedger">重试</el-button>
          </el-alert>
          <div v-else-if="ledgerHasMore" ref="sentinel" class="sentinel" data-testid="ledger-sentinel">{{ ledgerLoading ? '加载中…' : '' }}</div>
          <div v-else-if="ledger.length" class="muted end" data-testid="ledger-end">没有更多了</div>
          <el-empty v-else description="暂无流水" />
        </el-tab-pane>

        <el-tab-pane label="订单" name="orders">
          <div v-for="o in orders" :key="o.id" class="row-card" data-testid="order-row">
            <div class="row-head">
              <span><span data-testid="order-kind">{{ labelOf(orderKindLabel, o.kind) }}</span> · <span data-testid="order-duration">{{ labelOf(durationLabel, o.duration) }}</span></span>
              <b data-testid="order-amount">{{ fmtYuan(o.amount) }}</b>
            </div>
            <div class="muted">
              <router-link :to="`/instances/${o.instance_id}`" class="mono" data-testid="order-instance">{{ o.instance_id }}</router-link>
              · <span data-testid="order-status">{{ labelOf(orderStatusLabel, o.status) }}</span> · <span data-testid="order-time">{{ fmtTime(o.created_at) }}</span>
            </div>
          </div>
          <el-empty v-if="orders.length === 0" description="暂无订单" />
        </el-tab-pane>

        <el-tab-pane label="充值记录" name="recharges">
          <div v-for="r in recharges" :key="r.id" class="row-card" data-testid="recharge-row">
            <div class="row-head">
              <span><span data-testid="recharge-channel">{{ labelOf(rechargeChannelLabel, r.channel) }}</span> · <span data-testid="recharge-status">{{ labelOf(rechargeStatusLabel, r.status) }}</span></span>
              <b data-testid="recharge-amount">{{ fmtYuan(r.amount) }}</b>
            </div>
            <div class="muted">
              <span data-testid="recharge-time">{{ fmtTime(r.created_at) }}</span>
              <router-link v-if="r.status === 'pending'" :to="`/wallet/recharge/${r.id}`" data-testid="recharge-continue">继续支付</router-link>
            </div>
          </div>
          <el-empty v-if="recharges.length === 0" description="暂无充值记录" />
          <el-pagination v-if="rechargeTotal > 10" v-model:current-page="rechargePage" :page-size="10" :total="rechargeTotal" layout="prev, pager, next" data-testid="wallet-recharges-pager" />
        </el-tab-pane>
      </el-tabs>
    </el-card>
    <GuideDialog :guide="guide" />
  </div>
</template>

<style scoped>
.wallet { display: grid; gap: 16px; max-width: 900px; }
.balance-row { display: flex; justify-content: space-between; align-items: center; gap: 12px; flex-wrap: wrap; }
.balance { font-size: 28px; font-weight: 600; }
.balance-actions { display: flex; gap: 8px; }
.muted { color: #909399; font-size: 13px; }
.row-card { padding: 12px 0; border-bottom: 1px solid #ebeef5; }
.row-head { display: flex; justify-content: space-between; align-items: center; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.kind-rebate { color: var(--el-color-success); }
.kind-clawback { color: var(--el-color-warning); }
.sentinel { height: 24px; text-align: center; color: #909399; font-size: 12px; }
.end { text-align: center; padding: 12px; }
</style>
