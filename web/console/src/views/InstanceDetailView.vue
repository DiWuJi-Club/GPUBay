<script setup lang="ts">
import {
  durationLabel, fmtDuration, fmtTime, fmtYuan, instanceStateLabel, instanceStateType, labelOf, messageOf, useCountdown, useIdempotencyKey,
  type Duration, type Instance, type Listed, type Order, type Product,
} from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import AppDialog from '../components/AppDialog.vue'
import GuideDialog from '../components/GuideDialog.vue'
import { useGuide } from '../composables/useGuide'
import { canRelease, canRenew, releaseWarning } from '../logic/instance'
import { notifyError } from '../logic/notify'
import { refreshBalance, session } from '../store/session'

const route = useRoute()
const router = useRouter()
const guide = useGuide()
const id = computed(() => String(route.params.id))
const inst = ref<Instance | null>(null)
const loadError = ref('')
const countdown = useCountdown()

async function load() {
  try {
    inst.value = await api.get<Instance>(`/instances/${id.value}`)
    loadError.value = ''
    if (inst.value.state === 'running' && !inst.value.suspended) countdown.start(inst.value.remaining_seconds)
    else {
      countdown.stop()
      countdown.remaining.value = inst.value.remaining_seconds
    }
  } catch (e) {
    loadError.value = messageOf(e)
  }
}
onMounted(load)

// ---- SSH 密码：只在点击时请求，只放内存，离开页面即丢弃 ----
const password = ref('')
const loadingPassword = ref(false)
async function showPassword() {
  loadingPassword.value = true
  try {
    password.value = (await api.get<{ password: string }>(`/instances/${id.value}/password`)).password
  } catch (e) {
    notifyError(e)
  } finally {
    loadingPassword.value = false
  }
}
async function copy(text: string) {
  try {
    await navigator.clipboard.writeText(text)
    ElMessage.success('已复制')
  } catch {
    ElMessage.error('复制失败，请手动选择复制')
  }
}

// ---- 续费 ----
const renewVisible = ref(false)
const renewDuration = ref<Duration>('1d')
const product = ref<Product | null>(null)
const renewing = ref(false)
const renewKey = useIdempotencyKey(() => [id.value, renewDuration.value])
const renewPrice = computed(() => product.value?.prices.find((p) => p.duration === renewDuration.value)?.amount ?? null)
const balance = computed(() => session.balance ?? 0)
const renewAfter = computed(() => (renewPrice.value == null ? null : balance.value - renewPrice.value))
const renewInsufficient = computed(() => renewPrice.value != null && balance.value < renewPrice.value)

async function openRenew() {
  try {
    const [{ items }] = await Promise.all([api.get<Listed<Product>>('/products'), refreshBalance()])
    product.value = items.find((p) => p.id === inst.value?.product_id) ?? null
    renewDuration.value = product.value?.prices[0]?.duration ?? '1d'
    renewVisible.value = true
  } catch (e) {
    notifyError(e)
  }
}
async function confirmRenew() {
  if (!inst.value || renewing.value) return // 双击保护
  if (renewInsufficient.value) {
    renewVisible.value = false
    await router.push({ path: '/wallet/recharge', query: { redirect: route.fullPath } }) // 「去充值」直接跳转，带 redirect 回来
    return
  }
  renewing.value = true
  const wasStopped = inst.value.state === 'stopped'
  try {
    await api.post<{ order: Order }>(`/instances/${id.value}/renew`, { duration: renewDuration.value }, { idempotencyKey: renewKey.current() })
    renewKey.rotate()
    renewVisible.value = false
    ElMessage.success(wasStopped ? '续费成功，正在重新启动' : '续费成功')
    await Promise.all([load(), refreshBalance()])
  } catch (e) {
    renewVisible.value = false
    guide.handle(e)
  } finally {
    renewing.value = false
  }
}

// ---- 释放：一层确认框，确认按钮点击即执行（评审裁定 2） ----
const releaseVisible = ref(false)
const releasing = ref(false)
const warning = computed(() => (inst.value ? releaseWarning(inst.value) : null))
async function confirmRelease() {
  if (releasing.value) return // 双击保护：请求进行中第二次点击不发请求
  releasing.value = true
  try {
    await api.post(`/instances/${id.value}/release`, {})
    releaseVisible.value = false
    ElMessage.success('已提交释放，正在清除数据')
    await load()
  } catch (e) {
    releaseVisible.value = false
    guide.handle(e)
  } finally {
    releasing.value = false
  }
}

watch(id, () => {
  password.value = ''
  void load()
})
</script>

<template>
  <div class="detail">
    <el-alert v-if="loadError" type="error" :title="loadError" show-icon :closable="false" />
    <template v-else-if="inst">
      <el-card>
        <template #header>
          <div class="head">
            <span class="mono" data-testid="detail-id">{{ inst.id }}</span>
            <el-tag :type="labelOf(instanceStateType, inst.state) as 'primary'" data-testid="detail-state">{{ labelOf(instanceStateLabel, inst.state) }}</el-tag>
          </div>
        </template>
        <el-alert v-if="inst.state === 'failed'" type="error" title="开通失败，费用已原路退回余额" show-icon :closable="false" data-testid="detail-failed-hint" />
        <el-alert v-if="inst.suspended" type="warning" title="计时已暂停（机器维护中）" show-icon :closable="false" data-testid="detail-suspended" />
        <!-- el-descriptions 会把条目展平重排：v-if 条目增减时 Vue 按位置复用 span，静态 data-testid 不会被更新，所以条件条目的 span 必须带 key -->
        <el-descriptions :column="1" border>
          <el-descriptions-item label="商品"><span data-testid="detail-product">{{ inst.product_name }}</span></el-descriptions-item>
          <el-descriptions-item label="镜像"><span data-testid="detail-image">{{ inst.image_name }}</span></el-descriptions-item>
          <el-descriptions-item label="创建时间"><span data-testid="detail-created">{{ fmtTime(inst.created_at) }}</span></el-descriptions-item>
          <el-descriptions-item label="到期时间"><span data-testid="detail-expires">{{ inst.expires_at ? fmtTime(inst.expires_at) : '—' }}</span></el-descriptions-item>
          <el-descriptions-item label="剩余时长">
            <span data-testid="detail-remaining" :data-remaining="countdown.remaining.value">{{ inst.state === 'running' ? fmtDuration(countdown.remaining.value) : '—' }}</span>
          </el-descriptions-item>
          <el-descriptions-item v-if="inst.pending_seconds > 0" label="已付待开始">
            <span key="pending" data-testid="detail-pending">{{ fmtDuration(inst.pending_seconds) }}</span>
          </el-descriptions-item>
          <el-descriptions-item v-if="inst.state === 'stopped' && inst.retain_until" label="数据保留至">
            <span key="retain" data-testid="detail-retain">{{ fmtTime(inst.retain_until) }}</span>
          </el-descriptions-item>
        </el-descriptions>
        <div class="actions">
          <el-button v-if="canRenew(inst.state)" type="primary" data-testid="detail-renew-btn" @click="openRenew">续费</el-button>
          <el-button v-if="canRelease(inst.state)" type="danger" plain data-testid="detail-release-btn" @click="releaseVisible = true">释放</el-button>
        </div>
      </el-card>

      <el-card header="SSH 连接">
        <template v-if="inst.state === 'running' && inst.ssh_command">
          <div class="row">
            <code class="mono" data-testid="detail-ssh-command">{{ inst.ssh_command }}</code>
            <el-button size="small" data-testid="detail-ssh-copy" @click="copy(inst.ssh_command)">复制</el-button>
          </div>
          <div class="row">
            <span>密码：</span>
            <code class="mono" data-testid="detail-password">{{ password || '••••••••' }}</code>
            <el-button v-if="!password" size="small" :loading="loadingPassword" data-testid="detail-password-show" @click="showPassword">显示</el-button>
            <el-button v-else size="small" data-testid="detail-password-copy" @click="copy(password)">复制</el-button>
          </div>
        </template>
        <p v-else class="muted" data-testid="detail-ssh-hint">实例运行中才能连接</p>
      </el-card>
    </template>

    <AppDialog v-model="renewVisible" kind="form" title="续费" data-testid="detail-renew-dialog">
      <el-form label-position="top">
        <el-form-item label="租期">
          <el-radio-group v-model="renewDuration">
            <el-radio-button v-for="p in product?.prices ?? []" :key="p.duration" :value="p.duration" :data-testid="`detail-renew-duration-${p.duration}`">
              {{ labelOf(durationLabel, p.duration) }} {{ fmtYuan(p.amount) }}
            </el-radio-button>
          </el-radio-group>
        </el-form-item>
        <el-descriptions :column="1" border>
          <el-descriptions-item label="价格"><span data-testid="detail-renew-price">{{ renewPrice == null ? '—' : fmtYuan(renewPrice) }}</span></el-descriptions-item>
          <el-descriptions-item label="当前余额"><span data-testid="detail-renew-balance">{{ fmtYuan(balance) }}</span></el-descriptions-item>
          <el-descriptions-item label="扣款后余额">
            <span data-testid="detail-renew-after" :class="{ warn: renewInsufficient }">{{ renewAfter == null ? '—' : fmtYuan(renewAfter) }}<template v-if="renewInsufficient">（余额不足）</template></span>
          </el-descriptions-item>
        </el-descriptions>
      </el-form>
      <template #footer>
        <el-button data-testid="detail-renew-cancel" @click="renewVisible = false">取消</el-button>
        <el-button type="primary" :loading="renewing" data-testid="detail-renew-ok" @click="confirmRenew">{{ renewInsufficient ? '去充值' : '确认续费' }}</el-button>
      </template>
    </AppDialog>

    <AppDialog v-model="releaseVisible" kind="confirm" title="释放实例" data-testid="detail-release-dialog">
      <p data-testid="detail-release-text">释放后数据将被清除，已付费用不退，确定释放吗？</p>
      <p v-if="warning" class="danger" data-testid="detail-release-warning">{{ warning }}</p>
      <template #footer>
        <el-button data-testid="detail-release-cancel" @click="releaseVisible = false">取消</el-button>
        <el-button type="danger" :loading="releasing" :disabled="releasing" data-testid="detail-release-ok" @click="confirmRelease">确定释放</el-button>
      </template>
    </AppDialog>
    <GuideDialog :guide="guide" />
  </div>
</template>

<style scoped>
.detail { display: grid; gap: 16px; max-width: 760px; }
.head { display: flex; justify-content: space-between; align-items: center; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; word-break: break-all; }
.row { display: flex; align-items: center; gap: 8px; margin: 8px 0; flex-wrap: wrap; }
.actions { display: flex; gap: 12px; margin-top: 16px; }
.muted { color: #909399; }
.warn { color: var(--el-color-warning); }
.danger { color: var(--el-color-danger); }
</style>
