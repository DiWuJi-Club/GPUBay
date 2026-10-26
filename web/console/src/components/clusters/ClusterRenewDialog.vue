<script setup lang="ts">
import { durationLabel, fmtTime, fmtYuan, isApiError, labelOf, useIdempotencyKey, type Duration, type Order, type Product } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../../api'
import { useGuide } from '../../composables/useGuide'
import { describeDuration } from '../../logic/booking'
import { clusterRenewMax, clusterTotalPrice, type ConsoleClusterView } from '../../logic/cluster'
import { refreshBalance, session } from '../../store/session'
import AppDialog from '../AppDialog.vue'
import GuideDialog from '../GuideDialog.vue'

/**
 * 整组续费：每个成员加同样的时长（N 张续费单，全成或全败）。总价 = 单价 × 台数 × 数量；
 * 必须带 Idempotency-Key（参数不变的重试复用同一个键）。
 */
const props = defineProps<{ view: ConsoleClusterView; product: Product | null }>()
const visible = defineModel<boolean>({ required: true })
const emit = defineEmits<{ done: [view: ConsoleClusterView] }>()
const route = useRoute()
const router = useRouter()
const guide = useGuide()

const duration = ref<Duration>('1d')
const quantity = ref(1)
const submitting = ref(false)
const key = useIdempotencyKey(() => [props.view.id, duration.value, quantity.value])

const prices = computed(() => props.product?.prices ?? [])
const maxOf = (d: Duration) => clusterRenewMax(props.view, d)
const max = computed(() => maxOf(duration.value))
const unit = computed(() => prices.value.find((p) => p.duration === duration.value)?.amount ?? null)
const total = computed(() => (unit.value == null ? null : clusterTotalPrice(unit.value, props.view.size, quantity.value)))
const balance = computed(() => session.balance ?? 0)
const after = computed(() => (total.value == null ? null : balance.value - total.value))
const insufficient = computed(() => total.value != null && balance.value < total.value)

watch([duration, max], () => {
  quantity.value = Math.min(Math.max(quantity.value || 1, 1), Math.max(max.value, 1))
})
watch(visible, (v) => {
  if (!v) return
  duration.value = prices.value.find((p) => maxOf(p.duration) > 0)?.duration ?? prices.value[0]?.duration ?? '1d'
  quantity.value = 1
  void refreshBalance().catch(() => {})
}, { immediate: true })

async function submit() {
  if (submitting.value) return
  if (insufficient.value) {
    visible.value = false
    await router.push({ path: '/wallet/recharge', query: { redirect: route.fullPath } })
    return
  }
  submitting.value = true
  try {
    const out = await api.post<{ orders: Order[]; cluster: ConsoleClusterView }>(
      `/clusters/${props.view.id}/renew`, { duration: duration.value, quantity: quantity.value }, { idempotencyKey: key.current() },
    )
    key.rotate()
    visible.value = false
    ElMessage.success(`续费成功（${out.orders.length} 台）`)
    void refreshBalance().catch(() => {})
    emit('done', out.cluster)
  } catch (e) {
    visible.value = false
    if (isApiError(e) && (e.code === 'renew_blocked' || e.code === 'invalid_state' || e.code === 'cluster_member')) ElMessage.warning(e.message)
    else guide.handle(e)
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <AppDialog v-model="visible" kind="form" :title="`续费集群（${view.size} 台）`" data-testid="cluster-renew-dialog">
    <el-form label-position="top">
      <p class="muted">每台成员加同样的时长，按 {{ view.size }} 台分别下单扣款，全部成功或全部不扣。</p>
      <p v-if="view.renewable_until" class="muted" data-testid="cluster-renew-until">可续至 {{ fmtTime(view.renewable_until) }}（有成员机器之后已有预订）</p>
      <el-form-item label="租期">
        <el-radio-group v-model="duration">
          <el-radio-button v-for="p in prices" :key="p.duration" :value="p.duration" :disabled="maxOf(p.duration) === 0" :data-testid="`cluster-renew-duration-${p.duration}`">
            {{ labelOf(durationLabel, p.duration) }} {{ fmtYuan(p.amount) }}/台
            <span v-if="maxOf(p.duration) === 0" class="small">超出可续时间</span>
          </el-radio-button>
        </el-radio-group>
        <p v-if="!prices.length" class="warn small" data-testid="cluster-renew-no-price">价格加载失败，请刷新页面重试</p>
      </el-form-item>
      <el-form-item label="数量">
        <div class="row">
          <el-input-number v-model="quantity" :min="1" :max="Math.max(max, 1)" :value-on-clear="1" :disabled="max === 0" data-testid="cluster-renew-quantity" />
          <span class="muted">{{ describeDuration(duration, quantity) }}</span>
        </div>
      </el-form-item>
      <el-descriptions :column="1" border>
        <el-descriptions-item label="单台价"><span data-testid="cluster-renew-unit">{{ unit == null ? '—' : fmtYuan(unit * quantity) }}</span></el-descriptions-item>
        <el-descriptions-item label="总价"><span data-testid="cluster-renew-price">{{ total == null ? '—' : `${fmtYuan(total)}（${view.size} 台）` }}</span></el-descriptions-item>
        <el-descriptions-item label="当前余额"><span data-testid="cluster-renew-balance">{{ fmtYuan(balance) }}</span></el-descriptions-item>
        <el-descriptions-item label="扣款后余额">
          <span data-testid="cluster-renew-after" :class="{ warn: insufficient }">{{ after == null ? '—' : fmtYuan(after) }}<template v-if="insufficient">（余额不足）</template></span>
        </el-descriptions-item>
      </el-descriptions>
    </el-form>
    <template #footer>
      <el-button data-testid="cluster-renew-cancel" @click="visible = false">取消</el-button>
      <el-button type="primary" :loading="submitting" :disabled="max === 0 || unit == null" data-testid="cluster-renew-ok" @click="submit">{{ insufficient ? '去充值' : '确认续费' }}</el-button>
    </template>
  </AppDialog>
  <GuideDialog :guide="guide" />
</template>

<style scoped>
.row { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.muted { color: #909399; font-size: 13px; }
.small { font-size: 12px; margin: 4px 0; }
.warn { color: var(--el-color-warning); }
</style>
