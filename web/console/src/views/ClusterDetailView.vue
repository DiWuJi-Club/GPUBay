<script setup lang="ts">
import {
  clusterNetworkLabel, durationLabel, fabricKindLabel, fmtTime, labelOf, messageOf,
  type AppCatalogItem, type Duration, type Instance, type Listed, type Product,
} from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { api } from '../api'
import AppDialog from '../components/AppDialog.vue'
import ClusterAppCard from '../components/clusters/ClusterAppCard.vue'
import ClusterConnectInfo from '../components/clusters/ClusterConnectInfo.vue'
import ClusterMembers from '../components/clusters/ClusterMembers.vue'
import ClusterRenewDialog from '../components/clusters/ClusterRenewDialog.vue'
import GuideDialog from '../components/GuideDialog.vue'
import { useGuide } from '../composables/useGuide'
import { usePolling } from '../composables/usePolling'
import { clusterActions, clusterAppBusy, clusterNotices, clusterStatusBadge, isClusterActive, type ConsoleClusterView } from '../logic/cluster'
import { notifyError } from '../logic/notify'

/**
 * 集群详情 /clusters/:id：状态、网络类型、到期；成员表；连接信息；整组续费 / 自动续费 / 释放；集群应用卡片。
 * 组建中、成员开通 / 洗机中、集群应用部署 / 卸载中每 5 秒刷新，其余每 30 秒。
 */
const route = useRoute()
const guide = useGuide()
const id = computed(() => String(route.params.id))
const view = ref<ConsoleClusterView | null>(null)
const loadError = ref('')
let seq = 0
let failing = false

async function load() {
  const mine = ++seq
  try {
    const next = await api.get<ConsoleClusterView>(`/clusters/${id.value}`)
    if (mine !== seq) return
    view.value = next
    loadError.value = ''
    failing = false
    void loadMemberInstance()
  } catch (e) {
    if (mine !== seq) return
    if (!view.value) loadError.value = messageOf(e)
    else if (!failing) notifyError(e)
    failing = true
  }
}

// 成员实例（rank 最小的在位成员）：取商品（续费价格、自动续费档位）与镜像（集群应用兼容性）
const memberInst = ref<Instance | null>(null)
const product = ref<Product | null>(null)
async function loadMemberInstance() {
  const first = view.value?.members[0]?.instance_id
  if (!first || memberInst.value?.id === first) return
  try {
    const inst = await api.get<Instance>(`/instances/${first}`)
    memberInst.value = inst
    if (!product.value || product.value.id !== inst.product_id) {
      const { items } = await api.get<Listed<Product>>('/products')
      product.value = items.find((p) => p.id === inst.product_id) ?? null
    }
  } catch {
    // 静默：续费对话框里提示价格加载失败；应用兼容性交给服务器校验
  }
}

const catalog = ref<AppCatalogItem[]>([])
async function loadCatalog() {
  try {
    catalog.value = (await api.get<Listed<AppCatalogItem>>('/apps')).items
  } catch {
    catalog.value = []
  }
}

onMounted(() => {
  void load()
  void loadCatalog()
})
watch(id, () => {
  view.value = null
  memberInst.value = null
  loadError.value = ''
  failing = false
  void load()
})
const fast = () => {
  const v = view.value
  if (!v) return false
  // 活动集群的在位成员不足 size（释放 / 换机后旧成员在洗机，不再列在 members 里）：同样快刷，洗完即显示已释放
  return v.status === 'forming' || v.members.some((m) => m.state === 'creating' || m.state === 'wiping') || (isClusterActive(v) && v.members.length < v.size) || clusterAppBusy(v.app)
}
usePolling(load, 5_000, { enabled: fast })
usePolling(load, 30_000, { enabled: () => !!view.value && isClusterActive(view.value) && !fast() })

const badge = computed(() => (view.value ? clusterStatusBadge(view.value) : null))
const notices = computed(() => (view.value ? clusterNotices(view.value) : []))
const actions = computed(() => (view.value ? clusterActions(view.value) : null))

// ---- 续费 ----
const renewVisible = ref(false)
function onRenewed(v: ConsoleClusterView) {
  seq++
  view.value = v
}

// ---- 自动续费（全员同设）----
const togglingAutoRenew = ref(false)
const autoRenewDurations = computed<Duration[]>(() => {
  if (product.value) return product.value.prices.map((p) => p.duration)
  const d = view.value?.auto_renew_duration
  return d ? [d] : []
})
async function setAutoRenew(enabled: boolean, duration?: Duration) {
  const v = view.value
  if (!v || togglingAutoRenew.value) return
  togglingAutoRenew.value = true
  try {
    const next = await api.put<ConsoleClusterView>(`/clusters/${v.id}/auto-renew`, { enabled, duration: duration ?? (v.auto_renew_duration || autoRenewDurations.value[0] || '1d') })
    seq++
    view.value = next
    ElMessage.success(enabled ? '已为全部成员开启自动续费' : '已关闭自动续费')
  } catch (e) {
    notifyError(e)
  } finally {
    togglingAutoRenew.value = false
  }
}
const autoRenewFailure = computed(() => {
  const r = view.value?.auto_renew_failed_reason
  if (r === 'insufficient_balance') return '自动续费失败：余额不足，请充值后系统会自动重试'
  if (r === 'renew_blocked') return `自动续费失败：有成员机器已有后续预订${view.value?.renewable_until ? `，可续至 ${fmtTime(view.value.renewable_until)}` : ''}`
  return r ? '自动续费失败' : ''
})

// ---- 释放（二次确认）----
const releaseVisible = ref(false)
const releasing = ref(false)
async function confirmRelease() {
  const v = view.value
  if (!v || releasing.value) return
  releasing.value = true
  try {
    await api.post(`/clusters/${v.id}/release`, {})
    releaseVisible.value = false
    ElMessage.success('已提交释放，全部成员正在清除数据')
    await load()
  } catch (e) {
    releaseVisible.value = false
    guide.handle(e)
    await load()
  } finally {
    releasing.value = false
  }
}
</script>

<template>
  <div class="detail">
    <el-alert v-if="loadError" type="error" :title="loadError" show-icon :closable="false" data-testid="cluster-error">
      <router-link to="/instances">返回我的实例</router-link>
    </el-alert>
    <el-skeleton v-else-if="!view" :rows="5" animated />
    <template v-else>
      <el-card>
        <template #header>
          <div class="head">
            <span>集群 <span class="mono" data-testid="cluster-id">{{ view.id }}</span> · {{ view.size }} 台</span>
            <el-tag v-if="badge" :type="badge.type as 'primary'" data-testid="cluster-status" :data-status="view.status">{{ badge.text }}</el-tag>
          </div>
        </template>
        <el-alert
          v-for="n in notices" :key="n.key" :type="n.type" :title="n.text" show-icon :closable="false" class="notice"
          :data-testid="`cluster-notice-${n.key}`"
        />
        <el-alert v-if="autoRenewFailure" type="error" :title="autoRenewFailure" show-icon :closable="false" class="notice" data-testid="cluster-auto-renew-failed">
          <router-link v-if="view.auto_renew_failed_reason === 'insufficient_balance'" to="/wallet/recharge">去充值</router-link>
        </el-alert>
        <el-descriptions :column="1" border>
          <el-descriptions-item label="网络">
            <span data-testid="cluster-network" :data-network="view.network">{{ labelOf(clusterNetworkLabel, view.network) }}</span>
            <span class="muted small"> · {{ labelOf(fabricKindLabel, view.fabric_kind) }}</span>
          </el-descriptions-item>
          <el-descriptions-item label="集群网段"><code key="subnet" class="mono" data-testid="cluster-subnet">{{ view.subnet || '—' }}</code></el-descriptions-item>
          <el-descriptions-item label="head 地址"><code key="head" class="mono" data-testid="cluster-head-ip">{{ view.head_ip || '—' }}</code></el-descriptions-item>
          <el-descriptions-item v-if="view.batch_id" label="批次">
            <router-link key="batch" :to="{ path: '/instances', query: { batch: view.batch_id } }" class="mono" data-testid="cluster-batch">{{ view.batch_id }}</router-link>
          </el-descriptions-item>
          <el-descriptions-item label="创建时间"><span data-testid="cluster-created">{{ fmtTime(view.created_at) }}</span></el-descriptions-item>
          <el-descriptions-item label="到期时间">
            <span key="expires" data-testid="cluster-expires">{{ view.expires_at ? fmtTime(view.expires_at) : '—' }}</span>
            <span v-if="view.expires_mismatch" key="mismatch" class="warn small" data-testid="cluster-expires-mismatch">（成员到期时间不一致，此为最早）</span>
          </el-descriptions-item>
          <el-descriptions-item v-if="view.ended_at" label="结束时间"><span key="ended" data-testid="cluster-ended">{{ fmtTime(view.ended_at) }}</span></el-descriptions-item>
        </el-descriptions>
        <div v-if="actions?.active" class="actions">
          <el-button type="primary" :disabled="!actions.renew" data-testid="cluster-renew" @click="renewVisible = true">续费（{{ view.size }} 台）</el-button>
          <el-button type="danger" plain :disabled="!actions.release" :title="actions.releaseReason ?? ''" data-testid="cluster-release" @click="releaseVisible = true">释放集群</el-button>
        </div>
        <p v-if="actions?.active && !actions.renew" class="muted small" data-testid="cluster-renew-blocked">已到可续上限（有成员机器之后已有预订）</p>
        <p v-if="actions?.releaseReason" class="muted small" data-testid="cluster-release-reason">{{ actions.releaseReason }}</p>
      </el-card>

      <el-card header="成员">
        <ClusterMembers :members="view.members" :size="view.size" :show-expires="view.expires_mismatch" />
      </el-card>

      <el-card v-if="actions?.active" header="连接信息">
        <ClusterConnectInfo :view="view" />
      </el-card>

      <ClusterAppCard :view="view" :apps="catalog" :image-id="memberInst?.image_id ?? null" @changed="load" />

      <el-card v-if="actions?.active" header="自动续费" data-testid="cluster-auto-renew-card">
        <div class="row">
          <el-switch :model-value="view.auto_renew" :loading="togglingAutoRenew" data-testid="cluster-auto-renew" @change="(v: string | number | boolean) => setAutoRenew(v === true)" />
          <el-select
            :model-value="view.auto_renew_duration || undefined" :disabled="!view.auto_renew || togglingAutoRenew || !product"
            data-testid="cluster-auto-renew-duration" style="width: 110px" @change="(d: Duration) => setAutoRenew(true, d)"
          >
            <el-option v-for="d in autoRenewDurations" :key="d" :value="d" :label="labelOf(durationLabel, d)" />
          </el-select>
        </div>
        <p class="muted small">全部成员一起设置：到期前 1 小时为每台各续 1 个单位，全部成功或全部不扣；余额不足会重试到到期。</p>
      </el-card>
    </template>

    <ClusterRenewDialog v-if="view" v-model="renewVisible" :view="view" :product="product" @done="onRenewed" />
    <AppDialog v-model="releaseVisible" kind="confirm" title="释放集群" data-testid="cluster-release-dialog">
      <p data-testid="cluster-release-text">将释放集群的全部 {{ view?.size }} 台成员，所有成员的数据都会被清除，已付费用不退。确定释放吗？</p>
      <template #footer>
        <el-button data-testid="cluster-release-cancel" @click="releaseVisible = false">取消</el-button>
        <el-button type="danger" :loading="releasing" :disabled="releasing" data-testid="cluster-release-ok" @click="confirmRelease">确定释放 {{ view?.size }} 台</el-button>
      </template>
    </AppDialog>
    <GuideDialog :guide="guide" />
  </div>
</template>

<style scoped>
.detail { display: grid; gap: 16px; max-width: 860px; }
.head { display: flex; justify-content: space-between; align-items: center; gap: 8px; flex-wrap: wrap; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; word-break: break-all; }
.notice { margin-bottom: 8px; }
.row { display: flex; align-items: center; gap: 8px; margin: 8px 0; flex-wrap: wrap; }
.actions { display: flex; gap: 12px; margin-top: 16px; flex-wrap: wrap; }
.muted { color: #909399; }
.small { font-size: 12px; margin: 4px 0; }
.warn { color: var(--el-color-warning); }
</style>
