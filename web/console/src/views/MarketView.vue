<script setup lang="ts">
import {
  durationLabel, fmtYuan, isApiError, labelOf, messageOf, useIdempotencyKey, type Duration, type Image, type Instance, type Listed, type Order, type Product,
} from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, onMounted, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import AppDialog from '../components/AppDialog.vue'
import GuideDialog from '../components/GuideDialog.vue'
import { useGuide } from '../composables/useGuide'
import { usePolling } from '../composables/usePolling'
import { notifyError } from '../logic/notify'
import { refreshBalance, session } from '../store/session'

const route = useRoute()
const router = useRouter()
const guide = useGuide()
const products = ref<Product[]>([])
const images = ref<Image[]>([])
const loading = ref(true)
const loadError = ref('')
let loaded = false
let failing = false
// 每个商品卡片内的选择：租期默认第一档、镜像默认第一个
const sel = reactive<Record<number, { duration: Duration; imageId: number | null }>>({})

async function load() {
  try {
    const [p, i] = await Promise.all([api.get<Listed<Product>>('/products'), api.get<Listed<Image>>('/images')])
    products.value = p.items
    images.value = i.items
    for (const prod of p.items) {
      const cur = sel[prod.id]
      if (!cur) sel[prod.id] = { duration: prod.prices[0]?.duration ?? '1d', imageId: i.items[0]?.id ?? null }
      else if (!prod.prices.some((x) => x.duration === cur.duration)) cur.duration = prod.prices[0]?.duration ?? '1d' // 档位被下架
    }
    loaded = true
    loadError.value = ''
    failing = false
  } catch (e) {
    if (!failing) notifyError(e) // 持续故障只提示一次，成功后复位
    failing = true
    if (!loaded) loadError.value = messageOf(e) // 首次加载失败才占位；轮询失败保留已有卡片
  } finally {
    loading.value = false
  }
}
async function retry() {
  loading.value = true
  loadError.value = ''
  await load()
}
onMounted(load)
usePolling(load, 30_000) // 30 秒刷新一次空闲台数（页面可见时）

const specItems = (p: Product) => [
  { key: 'gpu', label: 'GPU', value: p.spec.gpu },
  { key: 'memory', label: '内存', value: `${p.spec.memory_gb} GB` },
  { key: 'disk', label: '硬盘', value: `${p.spec.disk_tb} TB` },
  { key: 'cpu', label: 'CPU', value: p.spec.cpu },
]

// ---- 确认框 ----
const confirm = reactive<{ visible: boolean; product: Product | null }>({ visible: false, product: null })
const confirmSel = computed(() => (confirm.product ? sel[confirm.product.id] : null))
const price = computed(() => confirm.product?.prices.find((p) => p.duration === confirmSel.value?.duration)?.amount ?? null)
const imageName = computed(() => images.value.find((i) => i.id === confirmSel.value?.imageId)?.name ?? '')
const balance = computed(() => session.balance ?? 0)
const after = computed(() => (price.value == null ? null : balance.value - price.value))
const insufficient = computed(() => price.value != null && balance.value < price.value)
const submitting = ref(false)
const key = useIdempotencyKey(() => [confirm.product?.id, confirmSel.value?.duration, confirmSel.value?.imageId])

async function rent(p: Product) {
  if (session.status !== 'authed') {
    await router.push({ path: '/login', query: { redirect: route.fullPath } })
    return
  }
  if (session.user?.kyc_status !== 'verified') {
    guide.kycRequired() // 不发下单请求
    return
  }
  try {
    await refreshBalance()
  } catch (e) {
    guide.handle(e)
    return
  }
  confirm.product = p
  confirm.visible = true
}

async function submit() {
  if (!confirm.product || !confirmSel.value || submitting.value) return // 双击保护：请求进行中第二次点击不发请求
  if (insufficient.value) {
    confirm.visible = false
    await router.push({ path: '/wallet/recharge', query: { redirect: route.fullPath } })
    return
  }
  submitting.value = true
  try {
    const out = await api.post<{ order: Order; instance: Instance }>(
      '/orders',
      { product_id: confirm.product.id, duration: confirmSel.value.duration, image_id: confirmSel.value.imageId },
      { idempotencyKey: key.current() },
    )
    key.rotate()
    confirm.visible = false
    ElMessage.success('下单成功，正在开通')
    await router.push(`/instances/${out.instance.id}`)
  } catch (e) {
    // 需要引导的错误关闭确认框；网络错误等保持打开，用户再次点击时复用同一个幂等键
    if (isApiError(e) && ['kyc_required', 'insufficient_balance', 'no_capacity', 'banned'].includes(e.code)) confirm.visible = false
    guide.handle(e, { onNoCapacity: load })
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <div class="market">
    <el-alert v-if="loadError" type="error" :title="loadError" show-icon :closable="false" data-testid="market-error">
      <el-button size="small" data-testid="market-retry" @click="retry">重试</el-button>
    </el-alert>
    <el-skeleton v-else-if="loading" :rows="5" animated />
    <div v-else class="cards">
      <el-card v-for="p in products" :key="p.id" class="card" data-testid="market-card" :data-product-id="p.id">
        <template #header>
          <div class="head">
            <span class="name" data-testid="market-name">{{ p.name }}</span>
            <span class="idle">空闲 <b data-testid="market-idle">{{ p.idle_count }}</b> 台</span>
          </div>
        </template>
        <ul class="spec">
          <li v-for="s in specItems(p)" :key="s.key"><span class="label">{{ s.label }}</span><span :data-testid="`market-spec-${s.key}`">{{ s.value }}</span></li>
        </ul>
        <div class="prices">
          <span v-for="pr in p.prices" :key="pr.duration" :data-testid="`market-price-${pr.duration}`">{{ labelOf(durationLabel, pr.duration) }} {{ fmtYuan(pr.amount) }}</span>
        </div>
        <el-form label-position="top" class="form">
          <el-form-item label="租期">
            <el-radio-group v-model="sel[p.id].duration">
              <el-radio-button v-for="pr in p.prices" :key="pr.duration" :value="pr.duration" :data-testid="`market-duration-${pr.duration}`">{{ labelOf(durationLabel, pr.duration) }}</el-radio-button>
            </el-radio-group>
          </el-form-item>
          <el-form-item label="镜像">
            <el-select v-model="sel[p.id].imageId" data-testid="market-image" placeholder="选择镜像">
              <el-option v-for="img in images" :key="img.id" :label="img.name" :value="img.id" />
            </el-select>
          </el-form-item>
          <el-button type="primary" class="rent" data-testid="market-rent-btn" :disabled="p.idle_count === 0" @click="rent(p)">
            {{ p.idle_count === 0 ? '暂无空闲' : '立即租用' }}
          </el-button>
        </el-form>
      </el-card>
    </div>

    <AppDialog v-model="confirm.visible" kind="confirm" title="确认下单" data-testid="market-confirm-dialog">
      <el-descriptions v-if="confirm.product && confirmSel" :column="1" border>
        <el-descriptions-item label="商品"><span data-testid="market-confirm-product">{{ confirm.product.name }}</span></el-descriptions-item>
        <el-descriptions-item label="租期"><span data-testid="market-confirm-duration">{{ labelOf(durationLabel, confirmSel.duration) }}</span></el-descriptions-item>
        <el-descriptions-item label="镜像"><span data-testid="market-confirm-image">{{ imageName }}</span></el-descriptions-item>
        <el-descriptions-item label="价格"><span data-testid="market-confirm-price">{{ price == null ? '—' : fmtYuan(price) }}</span></el-descriptions-item>
        <el-descriptions-item label="当前余额"><span data-testid="market-confirm-balance">{{ fmtYuan(balance) }}</span></el-descriptions-item>
        <el-descriptions-item label="扣款后余额">
          <span data-testid="market-confirm-after" :class="{ warn: insufficient }">{{ after == null ? '—' : fmtYuan(after) }}<template v-if="insufficient">（余额不足）</template></span>
        </el-descriptions-item>
      </el-descriptions>
      <template #footer>
        <el-button data-testid="market-confirm-cancel" @click="confirm.visible = false">取消</el-button>
        <el-button type="primary" :loading="submitting" data-testid="market-confirm-ok" @click="submit">{{ insufficient ? '去充值' : '确认下单' }}</el-button>
      </template>
    </AppDialog>
    <GuideDialog :guide="guide" />
  </div>
</template>

<style scoped>
.cards { display: grid; gap: 16px; grid-template-columns: repeat(auto-fill, minmax(320px, 1fr)); }
.head { display: flex; justify-content: space-between; align-items: center; }
.name { font-weight: 600; }
.idle { color: #909399; font-size: 13px; }
.spec { list-style: none; padding: 0; margin: 0 0 12px; display: grid; gap: 4px; }
.spec .label { display: inline-block; width: 48px; color: #909399; }
.prices { display: flex; flex-wrap: wrap; gap: 12px; margin-bottom: 12px; font-weight: 600; }
.rent { width: 100%; }
.warn { color: var(--el-color-warning); }
</style>
