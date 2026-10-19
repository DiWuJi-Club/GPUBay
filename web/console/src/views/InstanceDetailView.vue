<script setup lang="ts">
import {
  durationLabel, fmtDuration, fmtTime, fmtYuan, instanceStateLabel, instanceStateType, isApiError, labelOf, messageOf, portVisibilityLabel,
  useCountdown, useIdempotencyKey,
  type CancelQuote, type Duration, type Instance, type InstancePort, type Listed, type Order, type PortVisibility, type Product, type UserEvent,
} from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import AppDialog from '../components/AppDialog.vue'
import GuideDialog from '../components/GuideDialog.vue'
import { useGuide } from '../composables/useGuide'
import { usePolling } from '../composables/usePolling'
import { MAX_PORTS, validatePort } from '../logic/access'
import { describeDuration } from '../logic/booking'
import { autoRenewFailureText, canCancel, canRelease, canRenew, canSetAutoRenew, releaseWarning, renewQuantityMax } from '../logic/instance'
import { notifyError } from '../logic/notify'
import { refreshBalance, session } from '../store/session'

const route = useRoute()
const router = useRouter()
const guide = useGuide()
const id = computed(() => String(route.params.id))
const inst = ref<Instance | null>(null)
const fetchedAt = ref(0) // 拿到 inst 的本地时刻：cancellable_seconds 从这里起递减
const loadError = ref('')
// 运行中倒计时归零：后端此时已（或即将）停机，刷新一次显示新状态
const countdown = useCountdown({ onZero: () => void load() })
let seq = 0 // 查询序号：只采用最后发出的那次查询的结果
let failing = false

async function load() {
  const mine = ++seq
  try {
    const next = await api.get<Instance>(`/instances/${id.value}`)
    if (mine !== seq) return // 之后又发出过查询（轮询 / 操作后刷新 / 换实例）：丢弃迟到的旧响应
    const prevState = inst.value?.state
    inst.value = next
    fetchedAt.value = Date.now()
    if (canManagePorts(next.state) && prevState !== next.state) void loadPorts() // 首次加载与状态变化时拉端口，轮询不重复拉
    if (prevState !== undefined && prevState !== next.state) void loadEvents() // 状态变化时刷新时间线（首次由 onMounted 拉）
    loadError.value = ''
    failing = false
    if (next.state === 'running' && !next.suspended) countdown.start(next.remaining_seconds)
    else {
      countdown.stop()
      countdown.remaining.value = next.remaining_seconds
    }
  } catch (e) {
    if (mine !== seq) return
    if (!inst.value) loadError.value = messageOf(e) // 首次加载失败：页内错误
    else if (!failing) notifyError(e) // 刷新失败：保留已显示的数据，同一次故障只提示一次
    failing = true
  }
}
onMounted(() => {
  // 商品价格依赖实例的 product_id：实例加载后再拉；失败不提示（续费对话框打开时会再拉一次并提示）
  void load().then(() => loadProduct().catch(() => (productError.value = true)))
  void loadEvents()
})
const nowMs = ref(Date.now())
const ticker = setInterval(() => (nowMs.value = Date.now()), 1000)
onUnmounted(() => clearInterval(ticker))
// 已预订 / 开通中 / 洗机中每 10 秒刷新一次（预订到开通时刻自动变为开通中）（页面不可见时暂停，卸载自动停止）。
// 运行中且服务器已报剩余 0 秒：到期停机由后端每分钟的清扫执行，归零那次刷新多半还是「运行中」，同样每 10 秒再查，状态变了就停
const awaitingExpiry = () => inst.value?.state === 'running' && !inst.value.suspended && inst.value.remaining_seconds === 0
usePolling(load, 10_000, {
  enabled: () => inst.value?.state === 'reserved' || inst.value?.state === 'creating' || inst.value?.state === 'wiping' || awaitingExpiry(),
})

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

// ---- 密码重置：新密码只在确认框里显示一次（只放这个组件的内存），关闭即清除 ----
const resetVisible = ref(false)
const resetting = ref(false)
const resetPassword = ref('')
async function confirmReset() {
  if (resetting.value) return
  resetting.value = true
  const of = id.value
  try {
    const { password: next } = await api.post<{ password: string }>(`/instances/${of}/password/reset`)
    if (of !== id.value) return // 已换实例：响应作废（watch(id) 已关掉对话框、清掉页面密码）
    password.value = '' // 页面上显示过的旧密码已失效
    // 请求期间对话框可以关闭；关闭后迟到的响应不再写入，避免新密码留在内存里、下次打开时又显示出来
    if (!resetVisible.value) {
      ElMessage.success('密码已重置，可点「显示」查看新密码')
      return
    }
    resetPassword.value = next
    ElMessage.success('密码已重置，请复制保存')
  } catch (e) {
    if (of === id.value) resetVisible.value = false
    notifyError(e)
  } finally {
    resetting.value = false
  }
}
watch(resetVisible, (v) => {
  if (!v) resetPassword.value = ''
})

// ---- 密码登录开关 ----
const togglingAuth = ref(false)
async function setPasswordAuth(enabled: boolean) {
  if (togglingAuth.value || !inst.value) return
  togglingAuth.value = true
  try {
    inst.value = { ...inst.value, ...(await api.put<Instance>(`/instances/${id.value}/access`, { password_auth: enabled })) }
    ElMessage.success(enabled ? '已打开密码登录' : '已关闭密码登录，只能用公钥登录')
  } catch (e) {
    if (isApiError(e) && e.code === 'password_auth_requires_key') ElMessage.warning('关闭密码登录前请先在「我的 → SSH 公钥」添加公钥')
    else notifyError(e)
  } finally {
    togglingAuth.value = false
  }
}

// ---- Web 访问（端口）----
const ports = ref<InstancePort[]>([])
const portInput = ref('')
const portNote = ref('')
const portError = ref('')
const addingPort = ref(false)
const publicTarget = ref<InstancePort | null>(null)
const togglingPort = ref(false)
function canManagePorts(s: string) {
  return s === 'reserved' || s === 'creating' || s === 'running' || s === 'stopped'
}
async function loadPorts() {
  const of = id.value
  try {
    const { items } = await api.get<Listed<InstancePort>>(`/instances/${of}/ports`)
    if (of === id.value) ports.value = items // 换实例后迟到的响应丢弃
  } catch (e) {
    notifyError(e)
  }
}
async function addPort() {
  if (addingPort.value) return
  const v = validatePort(portInput.value)
  if ('error' in v) {
    portError.value = v.error
    return
  }
  portError.value = ''
  addingPort.value = true
  try {
    await api.post(`/instances/${id.value}/ports`, { port: v.port, note: portNote.value.trim() })
    portInput.value = ''
    portNote.value = ''
    await loadPorts()
  } catch (e) {
    if (isApiError(e) && e.code.startsWith('port_')) portError.value = e.message
    else notifyError(e)
  } finally {
    addingPort.value = false
  }
}
async function setVisibility(p: InstancePort, visibility: PortVisibility) {
  if (togglingPort.value) return
  togglingPort.value = true
  try {
    await api.put(`/instances/${id.value}/ports/${p.port}`, { visibility })
    publicTarget.value = null
    await loadPorts()
  } catch (e) {
    publicTarget.value = null
    if (isApiError(e) && e.code === 'port_forced_private') {
      ElMessage.warning(e.message)
      await loadPorts()
    } else notifyError(e)
  } finally {
    togglingPort.value = false
  }
}
function togglePort(p: InstancePort) {
  if (p.visibility === 'public') void setVisibility(p, 'private')
  else publicTarget.value = p // 设为公开需二次确认
}
async function deletePort(p: InstancePort) {
  try {
    await api.del(`/instances/${id.value}/ports/${p.port}`)
    await loadPorts()
  } catch (e) {
    notifyError(e)
  }
}
function openPort(p: InstancePort) {
  // 私有端口：实例域名没有票据 Cookie 时会自己跳到控制台的 authorize，再带票据回来
  window.open(p.url, '_blank', 'noopener')
}

// ---- 续费 ----
const renewVisible = ref(false)
const renewDuration = ref<Duration>('1d')
const renewQuantity = ref(1)
const product = ref<Product | null>(null)
const renewing = ref(false)
const renewKey = useIdempotencyKey(() => [id.value, renewDuration.value, renewQuantity.value])
const renewUnit = computed(() => product.value?.prices.find((p) => p.duration === renewDuration.value)?.amount ?? null)
const renewPrice = computed(() => (renewUnit.value == null ? null : renewUnit.value * renewQuantity.value))
const renewMax = computed(() => (inst.value ? renewQuantityMax(inst.value, renewDuration.value) : 1))
// 档位切换或实例刷新导致上限变化时，把数量夹回 [1, 上限]
watch([renewDuration, renewMax], () => {
  renewQuantity.value = Math.min(Math.max(renewQuantity.value || 1, 1), Math.max(renewMax.value, 1))
})
const balance = computed(() => session.balance ?? 0)
const renewAfter = computed(() => (renewPrice.value == null ? null : balance.value - renewPrice.value))
const renewInsufficient = computed(() => renewPrice.value != null && balance.value < renewPrice.value)

/** 商品价格：续费对话框与自动续费档位下拉共用。 */
const productError = ref(false) // 商品价格加载失败：自动续费档位只显示当前值，不能改
async function loadProduct() {
  if (!inst.value) return
  const pid = inst.value.product_id
  const { items } = await api.get<Listed<Product>>('/products')
  product.value = items.find((p) => p.id === pid) ?? null
  productError.value = false
}
/** 自动续费档位选项：价格没加载到时只列当前值，下拉仍能显示它。 */
const autoRenewDurations = computed<Duration[]>(() =>
  product.value ? product.value.prices.map((p) => p.duration) : inst.value ? [inst.value.auto_renew_duration] : [],
)
async function openRenew() {
  try {
    await Promise.all([loadProduct(), refreshBalance()])
    const prices = product.value?.prices ?? []
    const i = inst.value
    renewDuration.value = (i ? prices.find((p) => renewQuantityMax(i, p.duration) > 0) : undefined)?.duration ?? prices[0]?.duration ?? '1d'
    renewQuantity.value = 1
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
    await api.post<{ order: Order }>(
      `/instances/${id.value}/renew`,
      { duration: renewDuration.value, quantity: renewQuantity.value },
      { idempotencyKey: renewKey.current() },
    )
    renewKey.rotate()
    renewVisible.value = false
    ElMessage.success(wasStopped ? '续费成功，正在重新启动' : '续费成功')
    await Promise.all([load(), refreshBalance(), loadEvents()])
  } catch (e) {
    renewVisible.value = false
    if (isApiError(e) && e.code === 'renew_blocked') {
      ElMessage.warning(e.message) // 后端文案原样显示（含可续至时间）
      await load()
    } else guide.handle(e)
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

// ---- 取消预订 ----
const cancelVisible = ref(false)
const cancelQuote = ref<CancelQuote | null>(null)
const cancelling = ref(false)
async function openCancel() {
  try {
    cancelQuote.value = await api.get<CancelQuote>(`/instances/${id.value}/cancel-quote`)
    cancelQuoteChanged.value = false
    cancelVisible.value = true
  } catch (e) {
    notifyError(e)
  }
}
const cancelQuoteChanged = ref(false)
async function confirmCancel() {
  if (cancelling.value) return // 双击保护
  cancelling.value = true
  try {
    // 确认时重新报价：跨过冷静期 / 24 小时档位后金额会变，变了就更新显示并请用户再确认一次
    const shown = cancelQuote.value
    const latest = await api.get<CancelQuote>(`/instances/${id.value}/cancel-quote`)
    if (!shown || latest.refund !== shown.refund || latest.percent !== shown.percent) {
      cancelQuote.value = latest
      cancelQuoteChanged.value = true
      return
    }
    const next = await api.post<Instance>(`/instances/${id.value}/cancel`, {})
    seq++ // 作废进行中的轮询，避免旧的 reserved 响应覆盖取消结果
    inst.value = next
    fetchedAt.value = Date.now()
    cancelVisible.value = false
    ElMessage.success('已取消预订，退款已退回余额')
    await Promise.all([refreshBalance(), loadEvents()])
  } catch (e) {
    cancelVisible.value = false
    notifyError(e) // cancel_not_allowed 等后端原文
    await load()
  } finally {
    cancelling.value = false
  }
}

// ---- 自动续费 ----
const togglingAutoRenew = ref(false)
async function setAutoRenew(enabled: boolean, duration?: Duration) {
  if (togglingAutoRenew.value || !inst.value) return
  togglingAutoRenew.value = true
  try {
    const next = await api.put<Instance>(`/instances/${id.value}/auto-renew`, { enabled, duration: duration ?? inst.value.auto_renew_duration })
    seq++ // 作废进行中的轮询，避免旧响应把开关拨回去
    inst.value = next
    fetchedAt.value = Date.now()
    ElMessage.success(enabled ? '已开启自动续费' : '已关闭自动续费')
  } catch (e) {
    notifyError(e)
  } finally {
    togglingAutoRenew.value = false
  }
}

// ---- 时间线 ----
const events = ref<UserEvent[]>([])
async function loadEvents() {
  const of = id.value
  try {
    const { items } = await api.get<Listed<UserEvent>>(`/instances/${of}/events`)
    if (of === id.value) events.value = items // 换实例后迟到的响应丢弃
  } catch (e) {
    notifyError(e)
  }
}

watch(id, () => {
  password.value = ''
  events.value = []
  cancelVisible.value = false
  resetVisible.value = false
  ports.value = []
  portInput.value = ''
  portNote.value = ''
  portError.value = ''
  publicTarget.value = null
  inst.value = null
  loadError.value = ''
  failing = false
  countdown.stop()
  void load()
  void loadEvents()
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
        <el-alert v-if="inst.auto_renew_failed_reason" type="error" :title="autoRenewFailureText(inst)" show-icon :closable="false" data-testid="detail-auto-renew-failed">
          <router-link v-if="inst.auto_renew_failed_reason === 'insufficient_balance'" to="/wallet/recharge">去充值</router-link>
        </el-alert>
        <el-alert v-if="inst.state === 'cancelled'" type="info" title="预订已取消，退款已退回余额" show-icon :closable="false" data-testid="detail-cancelled-hint" />
        <el-alert v-if="inst.suspended" type="warning" title="计时已暂停（机器维护中）" show-icon :closable="false" data-testid="detail-suspended" />
        <!-- el-descriptions 会把条目展平重排：v-if 条目增减时 Vue 按位置复用 span，静态 data-testid 不会被更新，所以条件条目的 span 必须带 key -->
        <el-descriptions :column="1" border>
          <el-descriptions-item label="商品"><span data-testid="detail-product">{{ inst.product_name }}</span></el-descriptions-item>
          <el-descriptions-item label="镜像"><span data-testid="detail-image">{{ inst.image_name }}</span></el-descriptions-item>
          <el-descriptions-item v-if="inst.group_name" label="机组"><span key="group" data-testid="detail-group">{{ inst.group_name }}</span></el-descriptions-item>
          <el-descriptions-item label="机器">
            <span key="machine" data-testid="detail-machine">{{ inst.machine_label || '—' }}<template v-if="inst.reassign_count > 0">（<span data-testid="detail-reassigned">已更换机器 {{ inst.reassign_count }} 次</span>）</template></span>
          </el-descriptions-item>
          <el-descriptions-item v-if="inst.scheduled_start_at" label="预订开始"><span key="start" data-testid="detail-start">{{ fmtTime(inst.scheduled_start_at) }}</span></el-descriptions-item>
          <el-descriptions-item v-if="inst.scheduled_end_at" label="预订结束"><span key="end" data-testid="detail-end">{{ fmtTime(inst.scheduled_end_at) }}</span></el-descriptions-item>
          <el-descriptions-item v-if="inst.batch_id" label="批次">
            <router-link key="batch" :to="{ path: '/instances', query: { batch: inst.batch_id } }" class="mono" data-testid="detail-batch">{{ inst.batch_id }}</router-link>
          </el-descriptions-item>
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
          <el-button v-if="canRenew(inst.state)" type="primary" data-testid="detail-renew-btn" @click="openRenew">{{ inst.state === 'reserved' ? '续费（加时）' : '续费' }}</el-button>
          <el-button v-if="canCancel(inst, fetchedAt, nowMs)" type="danger" plain data-testid="detail-cancel-btn" @click="openCancel">取消预订</el-button>
          <el-button v-if="canRelease(inst.state)" type="danger" plain data-testid="detail-release-btn" @click="releaseVisible = true">释放</el-button>
        </div>
      </el-card>

      <el-card header="SSH 连接">
        <template v-if="inst.state === 'running' && inst.ssh_command">
          <div class="row">
            <code class="mono" data-testid="detail-ssh-command">{{ inst.ssh_command }}</code>
            <el-button size="small" data-testid="detail-ssh-copy" @click="copy(inst.ssh_command)">复制</el-button>
          </div>
          <p class="muted">主机 <span data-testid="detail-ssh-host">{{ inst.ssh_host }}</span>，端口 {{ inst.ssh_port }}；首次连接请在<router-link to="/help">帮助页</router-link>核对主机密钥指纹。</p>
        </template>
        <p v-else class="muted" data-testid="detail-ssh-hint">{{ inst.state === 'reserved' ? '开通后可连接' : '实例运行中才能连接' }}</p>
        <template v-if="canManagePorts(inst.state)">
          <div class="row">
            <span>密码：</span>
            <code class="mono" data-testid="detail-password">{{ password || '••••••••' }}</code>
            <el-button v-if="!password" size="small" :loading="loadingPassword" data-testid="detail-password-show" @click="showPassword">显示</el-button>
            <el-button v-else size="small" data-testid="detail-password-copy" @click="copy(password)">复制</el-button>
            <el-button size="small" plain data-testid="detail-password-reset" @click="resetVisible = true">重置密码</el-button>
          </div>
          <div class="row">
            <span>密码登录：</span>
            <el-switch
              :model-value="inst.password_auth"
              :loading="togglingAuth"
              data-testid="detail-password-auth"
              @change="(v: string | number | boolean) => setPasswordAuth(v === true)"
            />
            <span class="muted">关闭后只能用<router-link to="/account/ssh-keys">SSH 公钥</router-link>登录</span>
          </div>
        </template>
      </el-card>

      <el-card v-if="canManagePorts(inst.state)" header="Web 访问" data-testid="detail-ports-card">
        <p class="muted">把容器里监听的端口（如 JupyterLab 8888）开放为网址；默认只有您登录控制台后能访问，可设为公开。应用需监听 127.0.0.1 或 0.0.0.0。</p>
        <p v-if="inst.state !== 'running'" class="muted" data-testid="detail-port-hint">实例运行中才可访问，现在可以先添加端口</p>
        <div v-for="p in ports" :key="p.port" class="port" data-testid="detail-port-row">
          <div class="row">
            <b>{{ p.port }}</b>
            <span v-if="p.note" class="muted">{{ p.note }}</span>
            <el-tag :type="p.visibility === 'public' ? 'warning' : 'info'" data-testid="detail-port-visibility">{{ labelOf(portVisibilityLabel, p.visibility) }}</el-tag>
            <el-tag v-if="p.forced_private" type="danger" data-testid="detail-port-forced">已锁定：平台限制为私有</el-tag>
          </div>
          <div v-if="p.url" class="row">
            <a :href="p.url" target="_blank" rel="noopener" class="mono" data-testid="detail-port-url">{{ p.url }}</a>
          </div>
          <div class="row">
            <el-button v-if="p.url && inst.state === 'running'" size="small" type="primary" plain data-testid="detail-port-open" @click="openPort(p)">打开</el-button>
            <el-button v-if="p.url" size="small" data-testid="detail-port-copy" @click="copy(p.url)">复制网址</el-button>
            <el-button v-if="!p.forced_private" size="small" :loading="togglingPort" data-testid="detail-port-toggle" @click="togglePort(p)">{{ p.visibility === 'public' ? '设为私有' : '设为公开' }}</el-button>
            <el-button size="small" type="danger" plain data-testid="detail-port-delete" @click="deletePort(p)">删除</el-button>
          </div>
          <p v-if="p.forced_private" class="muted" data-testid="detail-port-forced-text">平台已将此端口限制为私有，只能在登录控制台后访问，不能改为公开；如有疑问请联系客服。</p>
          <p v-else-if="p.visibility === 'public'" class="warn small" data-testid="detail-port-public-warn">公开端口是不经平台鉴权的互联网服务：任何人都能访问，平台不做鉴权与限流，请确认应用自带密码。</p>
        </div>
        <div v-if="ports.length < MAX_PORTS" class="row">
          <el-input v-model="portInput" placeholder="端口，如 8888" style="width: 140px" inputmode="numeric" data-testid="detail-port-input" @keyup.enter="addPort" />
          <el-input v-model="portNote" placeholder="备注（可选）" maxlength="32" style="width: 180px" data-testid="detail-port-note" />
          <el-button type="primary" size="small" :loading="addingPort" data-testid="detail-port-add" @click="addPort">添加端口</el-button>
        </div>
        <p v-else class="muted" data-testid="detail-port-limit">每个实例最多开放 {{ MAX_PORTS }} 个端口</p>
        <p v-if="portError" class="danger" data-testid="detail-port-error">{{ portError }}</p>
      </el-card>

      <el-card v-if="canSetAutoRenew(inst.state)" header="自动续费" data-testid="detail-auto-renew-card">
        <div class="row">
          <el-switch :model-value="inst.auto_renew" :loading="togglingAutoRenew" data-testid="detail-auto-renew" @change="(v: string | number | boolean) => setAutoRenew(v === true)" />
          <el-select
            :model-value="inst.auto_renew_duration"
            :disabled="!inst.auto_renew || togglingAutoRenew || !product"
            data-testid="detail-auto-renew-duration"
            style="width: 110px"
            @change="(d: Duration) => setAutoRenew(true, d)"
          >
            <el-option v-for="d in autoRenewDurations" :key="d" :value="d" :label="labelOf(durationLabel, d)" />
          </el-select>
        </div>
        <p v-if="productError" class="warn small" data-testid="detail-auto-renew-product-error">档位价格加载失败，暂时只能开关自动续费，刷新页面后可修改档位。</p>
        <p class="muted small">到期前 1 小时自动扣款续 1 个单位；余额不足会重试到到期，被后续预订挡住时按能放下的档位办理；停机后不自动续。</p>
      </el-card>

      <el-card header="动态" data-testid="detail-timeline">
        <el-timeline v-if="events.length">
          <el-timeline-item v-for="(ev, i) in events" :key="i" :timestamp="fmtTime(ev.at)" placement="top">
            <span data-testid="detail-event" :data-kind="ev.reason_kind">{{ ev.detail }}</span>
          </el-timeline-item>
        </el-timeline>
        <p v-else class="muted">暂无记录</p>
      </el-card>
    </template>

    <AppDialog v-model="renewVisible" kind="form" title="续费" data-testid="detail-renew-dialog">
      <el-form label-position="top">
        <p v-if="inst?.renewable_until" class="muted" data-testid="detail-renew-until">可续至 {{ fmtTime(inst.renewable_until) }}（机器之后已有预订）</p>
        <el-form-item label="租期">
          <el-radio-group v-model="renewDuration">
            <el-radio-button
              v-for="p in product?.prices ?? []"
              :key="p.duration"
              :value="p.duration"
              :disabled="!!inst && renewQuantityMax(inst, p.duration) === 0"
              :data-testid="`detail-renew-duration-${p.duration}`"
            >
              {{ labelOf(durationLabel, p.duration) }} {{ fmtYuan(p.amount) }}
              <span v-if="inst && renewQuantityMax(inst, p.duration) === 0" class="small">超出可续时间</span>
            </el-radio-button>
          </el-radio-group>
        </el-form-item>
        <el-form-item label="数量">
          <div class="row">
            <el-input-number
              v-model="renewQuantity"
              :min="1"
              :max="Math.max(renewMax, 1)"
              :value-on-clear="1"
              :disabled="renewMax === 0"
              data-testid="detail-renew-quantity"
            />
            <span class="muted">{{ describeDuration(renewDuration, renewQuantity) }}</span>
          </div>
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
        <el-button type="primary" :loading="renewing" :disabled="renewMax === 0 || !renewQuantity" data-testid="detail-renew-ok" @click="confirmRenew">{{ renewInsufficient ? '去充值' : '确认续费' }}</el-button>
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
    <AppDialog v-model="cancelVisible" kind="confirm" title="取消预订" data-testid="detail-cancel-dialog">
      <p v-if="cancelQuoteChanged" class="warn" data-testid="detail-cancel-changed">退款金额已按最新规则更新，请确认后再次点击。</p>
      <p v-if="cancelQuote" data-testid="detail-cancel-text">将退款 {{ fmtYuan(cancelQuote.refund) }}（{{ cancelQuote.percent }}%），可取消至 {{ fmtTime(cancelQuote.deadline) }}。开通后将不能取消。</p>
      <template #footer>
        <el-button :disabled="cancelling" data-testid="detail-cancel-cancel" @click="cancelVisible = false">再想想</el-button>
        <el-button type="danger" :loading="cancelling" data-testid="detail-cancel-ok" @click="confirmCancel">确定取消</el-button>
      </template>
    </AppDialog>
    <AppDialog v-model="resetVisible" kind="confirm" title="重置 SSH 密码" data-testid="detail-reset-dialog">
      <template v-if="resetPassword">
        <p>新密码只在这里显示一次，请复制保存（之后也可在详情页点「显示」查看）：</p>
        <div class="row">
          <code class="mono" data-testid="detail-reset-password">{{ resetPassword }}</code>
          <el-button size="small" data-testid="detail-reset-copy" @click="copy(resetPassword)">复制</el-button>
        </div>
      </template>
      <p v-else>将生成新的 16 位密码，旧密码立即失效（已连接的会话不受影响）。</p>
      <template #footer>
        <el-button v-if="resetPassword" type="primary" data-testid="detail-reset-done" @click="resetVisible = false">我已保存</el-button>
        <template v-else>
          <el-button data-testid="detail-reset-cancel" @click="resetVisible = false">取消</el-button>
          <el-button type="primary" :loading="resetting" data-testid="detail-reset-ok" @click="confirmReset">确定重置</el-button>
        </template>
      </template>
    </AppDialog>

    <AppDialog :model-value="!!publicTarget" kind="confirm" title="设为公开" data-testid="detail-port-public-dialog" @update:model-value="(v: boolean) => { if (!v) publicTarget = null }">
      <p data-testid="detail-port-public-text">设为公开后，<b>任何人</b>都能通过网址访问端口 {{ publicTarget?.port }}，平台不做鉴权与限流；请确认应用自带鉴权（例如 Jupyter 的 token）。</p>
      <template #footer>
        <el-button data-testid="detail-port-public-cancel" @click="publicTarget = null">取消</el-button>
        <el-button type="warning" :loading="togglingPort" data-testid="detail-port-public-ok" @click="publicTarget && setVisibility(publicTarget, 'public')">确认公开</el-button>
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
.small { font-size: 12px; margin: 4px 0; }
.port { padding: 8px 0; border-bottom: 1px solid var(--el-border-color-lighter); }
.port .row { margin: 4px 0; }
.warn { color: var(--el-color-warning); }
.danger { color: var(--el-color-danger); }
</style>
