<script setup lang="ts">
import {
  commandStatusLabel, commandTypeLabel, durationLabel, fmtDuration, fmtTime, fmtYuan, instanceStateLabel, instanceStateType, labelOf, messageOf,
  orderKindLabel, orderStatusLabel, type AdminImage, type AdminInstance, type AdminInstanceDetail, type AdminProduct, type Listed, type RefundQuote,
} from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, onMounted, reactive, ref } from 'vue'
import { onBeforeRouteUpdate, useRoute } from 'vue-router'
import { api } from '../api'
import IdLink from '../components/IdLink.vue'
import { extendSeconds, isZeroRefund, showExtend, showMigrate, showRefund, showRewipe, validateExtend, validateRefund } from '../logic/instanceOps'
import { notifyError } from '../logic/notify'
import { session } from '../store/session'

const route = useRoute()
const id = computed(() => String(route.params.id))
const data = ref<AdminInstanceDetail | null>(null)
const loading = ref(false)
const error = ref('')
const images = ref<AdminImage[]>([])
const products = ref<AdminProduct[]>([])
const role = computed(() => session.admin?.role ?? 'finance')
const inst = computed(() => data.value?.instance)
const imageName = computed(() => images.value.find((i) => i.id === inst.value?.image_id)?.name ?? inst.value?.image_id)
const productName = computed(() => products.value.find((p) => p.id === inst.value?.product_id)?.name ?? inst.value?.product_id)
const isFinance = computed(() => role.value === 'finance')

let seq = 0
/**
 * 首次加载失败：页内错误 + 重试按钮（不自动重试）；操作后的刷新失败只提示，保留已显示的数据。
 * 只采用最后一次请求的结果：从一个实例切到另一个（换机后点新实例）时，先发后到的旧响应被丢弃。
 */
async function load(target = id.value) {
  const mine = ++seq
  loading.value = true
  try {
    const d = await api.get<AdminInstanceDetail>(`/instances/${target}`)
    if (mine !== seq) return
    data.value = d
    error.value = ''
  } catch (e) {
    if (mine !== seq) return
    notifyError(e)
    if (!data.value) error.value = messageOf(e)
  } finally {
    if (mine === seq) loading.value = false
  }
}
/** 镜像 / 商品只用于显示名称：失败提示一次，详情照常显示（名称退回 ID）。 */
async function loadOptions() {
  try {
    const [i, p] = await Promise.all([api.get<Listed<AdminImage>>('/images'), api.get<Listed<AdminProduct>>('/products')])
    images.value = i.items
    products.value = p.items
  } catch (e) {
    notifyError(e)
  }
}
onMounted(() => {
  void load()
  void loadOptions()
})
// 同一组件换了实例（换机结果里点新实例）：清掉旧实例的数据与弹窗，按新 ID 重新加载
onBeforeRouteUpdate((to) => {
  if (to.params.id === route.params.id) return
  data.value = null
  error.value = ''
  extend.visible = false
  migrate.visible = false
  migrate.result = null
  rewipe.visible = false
  refund.visible = false
  void load(String(to.params.id))
})

// ---- 免费延长（ops）----
const extend = reactive({ visible: false, amount: 1 as number | null, unit: 'day' as 'hour' | 'day', reason: '', error: '', busy: false })
function openExtend() {
  Object.assign(extend, { visible: true, amount: 1, unit: 'day', reason: '', error: '' })
}
async function submitExtend() {
  if (extend.busy) return // 同步守卫：连点只发一次
  const seconds = extendSeconds(extend.amount, extend.unit)
  extend.error = validateExtend(seconds) ?? (extend.reason.trim() ? '' : '请填写补偿原因')
  if (extend.error) return
  extend.busy = true
  try {
    await api.post<AdminInstance>(`/instances/${id.value}/extend`, { seconds, reason: extend.reason.trim() })
    ElMessage.success(`已延长 ${fmtDuration(seconds)}`)
    extend.visible = false
    await load()
  } catch (e) {
    notifyError(e) // 弹窗保留，输入不丢
  } finally {
    extend.busy = false
  }
}

// ---- 换机（ops）----
const migrate = reactive({ visible: false, reason: '', busy: false, result: null as AdminInstance | null })
function openMigrate() {
  Object.assign(migrate, { visible: true, reason: '' })
}
async function submitMigrate() {
  if (migrate.busy) return // 同步守卫：换机不是幂等操作
  if (!migrate.reason.trim()) return void ElMessage.warning('请填写换机原因')
  migrate.busy = true
  try {
    const out = await api.post<{ instance: AdminInstance }>(`/instances/${id.value}/migrate`, { reason: migrate.reason.trim() })
    migrate.visible = false
    migrate.result = out.instance // 换机后 SSH 密码变化，提示运营通知用户
    await load()
  } catch (e) {
    notifyError(e) // no_capacity →「暂无空闲机器，请稍后再试」；弹窗保留
  } finally {
    migrate.busy = false
  }
}

// ---- 重新洗机（ops）----
const rewipe = reactive({ visible: false, busy: false })
async function submitRewipe() {
  if (rewipe.busy) return // 同步守卫：连点只下发一次
  rewipe.busy = true
  try {
    await api.post(`/instances/${id.value}/rewipe`, {})
    ElMessage.success('已重新下发洗机指令')
    rewipe.visible = false
    await load()
  } catch (e) {
    notifyError(e)
  } finally {
    rewipe.busy = false
  }
}

// ---- 退款并释放（finance；动态码由请求层弹出，取消静默）----
const refund = reactive({ visible: false, quoting: false, quote: null as RefundQuote | null, amount: '', reason: '', error: '', busy: false })
async function openRefund() {
  if (refund.quoting) return
  refund.quoting = true
  try {
    const q = await api.get<RefundQuote>(`/instances/${id.value}/refund-quote`)
    Object.assign(refund, { quote: q, amount: (q.suggested / 100).toFixed(2), reason: '', error: '', visible: true })
  } catch (e) {
    notifyError(e)
  } finally {
    refund.quoting = false
  }
}
async function submitRefund() {
  if (refund.busy || !refund.quote) return // 同步守卫：退款不是幂等操作
  const v = validateRefund(refund.amount, refund.quote.max)
  if ('error' in v) {
    refund.error = v.error
    return
  }
  if (!refund.reason.trim()) {
    refund.error = '请填写退款原因'
    return
  }
  refund.error = ''
  refund.busy = true
  try {
    const out = await api.post<{ refunded: number }>(`/instances/${id.value}/refund-release`, { amount: v.cents, reason: refund.reason.trim() })
    ElMessage.success(out.refunded > 0 ? `已退款 ${fmtYuan(out.refunded)}，实例进入洗机` : '已释放（未退款），实例进入洗机')
    refund.visible = false
    await load()
  } catch (e) {
    notifyError(e) // 动态码取消静默；其余原文，弹窗保留
  } finally {
    refund.busy = false
  }
}

const eventText = (e: { from_state: string; to_state: string; reason: string; at: string }) => `${e.from_state || '—'} → ${e.to_state} · ${e.reason} · ${fmtTime(e.at)}`
</script>

<template>
  <div>
    <el-alert v-if="error && !data" type="error" :title="error" show-icon :closable="false" data-testid="adetail-error">
      <el-button size="small" :loading="loading" data-testid="adetail-retry" @click="load()">重试</el-button>
    </el-alert>
    <div v-if="data && inst" class="detail">
      <el-card>
        <template #header>
          <div class="head"><span class="mono" data-testid="adetail-id">{{ inst.id }}</span>
            <span><el-tag :type="labelOf(instanceStateType, inst.state) as 'primary'" data-testid="adetail-state">{{ labelOf(instanceStateLabel, inst.state) }}</el-tag>
              <el-tag v-if="inst.suspended" type="warning" data-testid="adetail-suspended">计时已暂停</el-tag></span>
          </div>
        </template>
        <el-descriptions :column="3" border>
          <el-descriptions-item label="用户"><span data-testid="adetail-user"><IdLink kind="user" :id="inst.user_id" /></span></el-descriptions-item>
          <el-descriptions-item label="机器"><span data-testid="adetail-machine"><IdLink kind="machine" :id="inst.machine_id" :disabled="isFinance" /></span></el-descriptions-item>
          <el-descriptions-item label="商品"><span data-testid="adetail-product">{{ productName }}</span></el-descriptions-item>
          <el-descriptions-item label="镜像"><span data-testid="adetail-image">{{ imageName }}</span></el-descriptions-item>
          <el-descriptions-item label="剩余时长"><span data-testid="adetail-remaining" :data-remaining="inst.remaining_seconds">{{ inst.state === 'running' ? fmtDuration(inst.remaining_seconds) : '—' }}</span></el-descriptions-item>
          <el-descriptions-item label="待开始"><span data-testid="adetail-pending">{{ inst.pending_seconds > 0 ? fmtDuration(inst.pending_seconds) : '—' }}</span></el-descriptions-item>
          <el-descriptions-item label="到期时间"><span data-testid="adetail-expires">{{ fmtTime(inst.expires_at) || '—' }}</span></el-descriptions-item>
          <el-descriptions-item label="停机时间">{{ fmtTime(inst.stopped_at) || '—' }}</el-descriptions-item>
          <el-descriptions-item label="创建时间">{{ fmtTime(inst.created_at) }}</el-descriptions-item>
        </el-descriptions>
        <div class="actions">
          <el-button v-if="showExtend(role, inst.state)" data-testid="adetail-extend-btn" @click="openExtend">免费延长</el-button>
          <el-button v-if="showMigrate(role, inst.state)" data-testid="adetail-migrate-btn" @click="openMigrate">换机</el-button>
          <el-button v-if="showRewipe(role, inst.state)" data-testid="adetail-rewipe-btn" @click="rewipe.visible = true">重新洗机</el-button>
          <el-button v-if="showRefund(role, inst.state)" type="danger" plain :loading="refund.quoting" data-testid="adetail-refund-btn" @click="openRefund">退款并释放</el-button>
        </div>
      </el-card>

      <div class="two">
        <el-card header="状态时间线">
          <el-timeline>
            <el-timeline-item v-for="e in data.events" :key="e.id">
              <span data-testid="adetail-event">{{ eventText(e) }}</span>
            </el-timeline-item>
          </el-timeline>
        </el-card>
        <el-card header="订单">
          <table class="grid">
            <thead><tr><th>订单号</th><th>类型</th><th>租期</th><th>金额</th><th>已退</th><th>状态</th><th>时间</th></tr></thead>
            <tbody>
              <tr v-for="o in data.orders" :key="o.id" data-testid="adetail-order-row">
                <td class="mono" data-testid="adetail-order-id">{{ o.id }}</td>
                <td data-testid="adetail-order-kind">{{ labelOf(orderKindLabel, o.kind) }}</td>
                <td>{{ labelOf(durationLabel, o.duration) }}</td>
                <td data-testid="adetail-order-amount">{{ fmtYuan(o.amount) }}</td>
                <td data-testid="adetail-order-refunded">{{ fmtYuan(o.refunded_amount) }}</td>
                <td data-testid="adetail-order-status">{{ labelOf(orderStatusLabel, o.status) }}</td>
                <td>{{ fmtTime(o.created_at) }}</td>
              </tr>
              <tr v-if="!data.orders.length"><td colspan="7" class="muted empty">无订单</td></tr>
            </tbody>
          </table>
        </el-card>
      </div>
      <el-card header="指令">
        <table class="grid">
          <thead><tr><th>类型</th><th>状态</th><th>尝试</th><th>结果</th><th>时间</th></tr></thead>
          <tbody>
            <tr v-for="c in data.commands" :key="c.id" data-testid="adetail-cmd-row">
              <td data-testid="adetail-cmd-type">{{ labelOf(commandTypeLabel, c.type) }}</td>
              <td data-testid="adetail-cmd-status">{{ labelOf(commandStatusLabel, c.status) }}</td>
              <td data-testid="adetail-cmd-attempts">{{ c.attempts }}</td>
              <td data-testid="adetail-cmd-result">{{ c.result }}</td>
              <td>{{ fmtTime(c.created_at) }}</td>
            </tr>
            <tr v-if="!data.commands.length"><td colspan="5" class="muted empty">无指令</td></tr>
          </tbody>
        </table>
      </el-card>

      <el-dialog v-model="extend.visible" title="免费延长" width="460" align-center data-testid="extend-dialog">
        <!-- novalidate：数字框的原生 step 校验会让 1.5 小时这类值根本提交不了，范围由 validateExtend 统一校验 -->
        <el-form label-position="top" novalidate @submit.prevent="submitExtend">
          <div class="two">
            <el-form-item label="时长"><el-input-number v-model="extend.amount" :min="0" :controls="false" data-testid="extend-amount" /></el-form-item>
            <el-form-item label="单位">
              <el-select v-model="extend.unit" data-testid="extend-unit"><el-option label="小时" value="hour" /><el-option label="天" value="day" /></el-select>
            </el-form-item>
          </div>
          <p class="muted">范围 1 分钟～30 天</p>
          <el-form-item label="原因（必填）"><el-input v-model="extend.reason" data-testid="extend-reason" /></el-form-item>
          <p v-if="extend.error" class="error" data-testid="extend-error">{{ extend.error }}</p>
          <el-button type="primary" native-type="submit" :loading="extend.busy" data-testid="extend-submit">确定延长</el-button>
        </el-form>
      </el-dialog>

      <el-dialog v-model="migrate.visible" title="换机" width="460" align-center data-testid="migrate-dialog">
        <p>将在另一台空闲机器上用相同镜像重建实例，剩余时长转移；原实例进入洗机。</p>
        <el-form label-position="top" @submit.prevent="submitMigrate">
          <el-form-item label="原因（必填）"><el-input v-model="migrate.reason" data-testid="migrate-reason" /></el-form-item>
          <el-button type="primary" native-type="submit" :loading="migrate.busy" data-testid="migrate-submit">确定换机</el-button>
        </el-form>
      </el-dialog>
      <el-dialog :model-value="!!migrate.result" title="换机成功" width="460" align-center @close="migrate.result = null">
        <p v-if="migrate.result" data-testid="migrate-result">
          已在机器 <span data-testid="adetail-migrate-machine"><IdLink kind="machine" :id="migrate.result.machine_id" :disabled="isFinance" /></span>
          上创建新实例 <span data-testid="adetail-migrate-new"><IdLink kind="instance" :id="migrate.result.id" /></span>，SSH 密码已变化，请通知用户。
        </p>
      </el-dialog>

      <el-dialog v-model="rewipe.visible" title="重新洗机" width="420" align-center data-testid="rewipe-dialog">
        <p>将取消未完成的指令并重新向机器下发洗机指令，确定执行？</p>
        <template #footer>
          <el-button data-testid="rewipe-cancel" @click="rewipe.visible = false">取消</el-button>
          <el-button type="primary" :loading="rewipe.busy" data-testid="rewipe-ok" @click="submitRewipe">确定</el-button>
        </template>
      </el-dialog>

      <el-dialog v-model="refund.visible" title="退款并释放" width="480" align-center data-testid="refund-dialog">
        <el-form v-if="refund.quote" label-position="top" @submit.prevent="submitRefund">
          <el-form-item label="退款金额（元）"><el-input v-model="refund.amount" inputmode="decimal" data-testid="refund-amount" /></el-form-item>
          <p class="muted" data-testid="refund-hint">建议 {{ fmtYuan(refund.quote.suggested) }}（按剩余时长 {{ fmtDuration(refund.quote.remaining_seconds) }} 折算），最多可退 {{ fmtYuan(refund.quote.max) }}</p>
          <el-form-item label="原因（必填）"><el-input v-model="refund.reason" data-testid="refund-reason" /></el-form-item>
          <p v-if="isZeroRefund(refund.amount)" class="zero-hint" data-testid="refund-zero-hint">金额为 0 表示只释放、不退款</p>
          <p v-if="refund.error" class="error" data-testid="refund-error">{{ refund.error }}</p>
          <el-button type="danger" native-type="submit" :loading="refund.busy" data-testid="refund-submit">退款并释放</el-button>
        </el-form>
      </el-dialog>
    </div>
  </div>
</template>

<style scoped>
.detail { display: grid; gap: 16px; }
.head { display: flex; justify-content: space-between; align-items: center; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.muted { color: #909399; font-size: 13px; }
.error { color: var(--el-color-danger); font-size: 13px; }
.zero-hint { color: var(--el-color-warning); font-size: 13px; }
.actions { display: flex; gap: 12px; margin-top: 16px; }
.two { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; }
.grid { width: 100%; border-collapse: collapse; font-size: 13px; }
.grid th, .grid td { padding: 8px; text-align: left; border-bottom: 1px solid var(--el-border-color-lighter); }
.grid th { color: #909399; font-weight: 500; }
.empty { text-align: center; }
</style>
