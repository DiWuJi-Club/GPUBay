<script setup lang="ts">
import { durationLabel, fmtYuan, labelOf, messageOf, type AppCatalogItem, type Duration, type Image, type Listed, type Product } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { onMounted, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import AppPicker from '../components/AppPicker.vue'
import GuideDialog from '../components/GuideDialog.vue'
import RentDialog, { type PlacedResult } from '../components/RentDialog.vue'
import { useAskButler } from '../composables/useAskButler'
import { useGuide } from '../composables/useGuide'
import { usePolling } from '../composables/usePolling'
import { compatibleImages } from '../logic/apps'
import { notifyError } from '../logic/notify'
import { refreshBalance, session } from '../store/session'

const route = useRoute()
const router = useRouter()
const guide = useGuide()
const products = ref<Product[]>([])
const images = ref<Image[]>([])
const apps = ref<AppCatalogItem[]>([]) // 一键部署：GET /apps（匿名可访问）
const loading = ref(true)
const loadError = ref('')
let loaded = false
let failing = false
// 每个商品卡片内的选择：租期默认第一档、镜像默认第一个
const sel = reactive<Record<number, { duration: Duration; imageId: number | null }>>({})

/** 应用目录失败不阻塞商品列表：只是不显示「一键部署」区，下次轮询再试。 */
async function loadApps() {
  try {
    apps.value = (await api.get<{ items: AppCatalogItem[] }>('/apps')).items
  } catch {
    // 静默：商品卡片照常可租
  }
}

async function load() {
  void loadApps()
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

// ---- 租用 / 预订对话框（RentDialog）----
const dialog = reactive<{ visible: boolean; product: Product | null; mode: 'immediate' | 'booking'; appId: number | null }>({ visible: false, product: null, mode: 'immediate', appId: null })

async function open(p: Product, mode: 'immediate' | 'booking', appId: number | null = null) {
  if (p.prices.length === 0) return // 全部档位已下架：按钮已禁用，这里再兜一层，不会发出没有价格的订单
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
  dialog.product = p
  dialog.mode = mode
  dialog.appId = appId
  dialog.visible = true
}
const rent = (p: Product, appId: number | null = null) => open(p, 'immediate', appId)
/** 一键部署卡片的灰显原因：与「立即租用」按钮同口径，另加「没有兼容镜像」。 */
function appBlocked(p: Product, a: AppCatalogItem): string | null {
  if (p.prices.length === 0) return '暂未开放'
  if (p.idle_count === 0) return '暂无空闲'
  if (compatibleImages(a, images.value).length === 0) return '暂无可用镜像'
  return null
}
const book = (p: Product) => open(p, 'booking')
const askButler = useAskButler()

async function onPlaced(r: PlacedResult) {
  if (r.batchId && r.instances.length > 1) {
    ElMessage.success(`已下单 ${r.instances.length} 台`)
    await router.push({ path: '/instances', query: { batch: r.batchId } })
    return
  }
  ElMessage.success(r.immediate ? '下单成功，正在开通' : '预订成功') // 按实际提交的开始方式，不按打开按钮
  await router.push(`/instances/${r.instances[0].id}`)
}
</script>

<template>
  <div class="market">
    <p class="ask">
      不知道选哪个？<el-button link type="primary" data-testid="market-ask-butler" @click="askButler({ draft: '帮我选一台机器' })">问 AI 管家</el-button>
    </p>
    <el-alert v-if="loadError" type="error" :title="loadError" show-icon :closable="false" data-testid="market-error">
      <el-button size="small" data-testid="market-retry" @click="retry">重试</el-button>
    </el-alert>
    <el-skeleton v-else-if="loading" :rows="5" animated />
    <div v-else class="cards">
      <el-card v-for="p in products" :key="p.id" class="card" data-testid="market-card" :data-product-id="p.id">
        <template #header>
          <div class="head">
            <span class="name" data-testid="market-name">{{ p.name }}</span>
            <span class="idle">现在可租 <b data-testid="market-idle">{{ p.idle_count }}</b> 台</span>
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
          <p v-if="p.prices.length === 0" class="muted" data-testid="market-no-price">该商品暂无可租的租期，暂不能下单</p>
          <el-button type="primary" class="rent" data-testid="market-rent-btn" :disabled="p.prices.length === 0 || p.idle_count === 0" @click="rent(p)">
            {{ p.prices.length === 0 ? '暂未开放' : p.idle_count === 0 ? '暂无空闲' : '立即租用' }}
          </el-button>
          <el-button class="rent" data-testid="market-book-btn" :disabled="p.prices.length === 0" @click="book(p)">预订档期</el-button>
        </el-form>
        <div v-if="apps.length" class="one-click" data-testid="market-apps">
          <div class="one-click-title">一键部署</div>
          <AppPicker :apps="apps" :model-value="null" testid="market-app" :disabled="(a) => appBlocked(p, a)" @update:model-value="(id) => id != null && rent(p, id)" />
        </div>
      </el-card>
    </div>

    <RentDialog v-if="dialog.product" v-model="dialog.visible" :product="dialog.product" :images="images" :mode="dialog.mode" :duration="sel[dialog.product.id].duration" :image-id="sel[dialog.product.id].imageId" :apps="apps" :app-id="dialog.appId" @placed="onPlaced" @no-capacity="load" />
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
.rent + .rent { margin: 8px 0 0; }
.warn { color: var(--el-color-warning); }
.muted { color: #909399; font-size: 13px; margin: 0 0 8px; }
.ask { margin: 0 0 12px; font-size: 14px; color: #606266; display: flex; align-items: center; }
.one-click { margin-top: 16px; padding-top: 12px; border-top: 1px solid #ebeef5; }
.one-click-title { font-weight: 600; margin-bottom: 8px; }
</style>
