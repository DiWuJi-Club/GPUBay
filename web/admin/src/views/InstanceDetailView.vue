<script setup lang="ts">
import {
  centsToYuanInput, clusterNetworkLabel, clusterStatusLabel, clusterStatusType, commandStatusLabel, commandTypeLabel, deploymentErrorText, deploymentStateLabel, deploymentStateType, durationLabel, fmtDuration, fmtTime, fmtYuan, instanceStateLabel, instanceStateType, labelOf, messageOf,
  orderKindLabel, orderStatusLabel, type AdminDeployment, type AdminImage, type AdminInstance, type AdminInstanceDetail, type AdminProduct, type AdminSshSession, type InstancePort, type Listed, type Paged, type RefundQuote,
} from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, onMounted, reactive, ref } from 'vue'
import { onBeforeRouteUpdate, useRoute } from 'vue-router'
import { api } from '../api'
import BatchLink from '../components/BatchLink.vue'
import BookingOpsDialogs from '../components/BookingOpsDialogs.vue'
import ForceRemoveDialog from '../components/ForceRemoveDialog.vue'
import IdLink from '../components/IdLink.vue'
import { useBookingOps } from '../composables/useBookingOps'
import { MEMBER_EXTEND_HINT, MIGRATE_MEMBER_HINT, netStatusBad, netStatusText, type InstanceClusterBlock } from '../logic/clusters'
import { progressText, showForceRemove } from '../logic/deployments'
import { extendSeconds, isZeroRefund, showCancelBooking, showExtend, showMigrate, showPortActions, showReassign, showRefund, showRewipe, validateExtend, validateRefund } from '../logic/instanceOps'
import { notifyError } from '../logic/notify'
import { FIN_ROLES } from '../menu'
import { session } from '../store/session'

const route = useRoute()
const id = computed(() => String(route.params.id))
/** 计划 13：cluster = 所属集群视图 + 本实例 rank（非成员 null） */
const data = ref<(AdminInstanceDetail & { cluster?: InstanceClusterBlock | null }) | null>(null)
const loading = ref(false)
const error = ref('')
const images = ref<AdminImage[]>([])
const products = ref<AdminProduct[]>([])
const role = computed(() => session.admin?.role ?? 'finance')
const inst = computed(() => data.value?.instance)
const imageName = computed(() => images.value.find((i) => i.id === inst.value?.image_id)?.name ?? inst.value?.image_id)
const productName = computed(() => products.value.find((p) => p.id === inst.value?.product_id)?.name ?? inst.value?.product_id)
const isFinance = computed(() => role.value === 'finance')
const canSeeOrders = computed(() => FIN_ROLES.includes(role.value))
/** 计划 13：集群成员（延长在集群页统一做；换机只能换到同一集群网络） */
const cluster = computed(() => data.value?.cluster ?? null)

let seq = 0
/**
 * 首次加载失败：页内错误 + 重试按钮（不自动重试）；操作后的刷新失败只提示，保留已显示的数据。
 * 只采用最后一次请求的结果：从一个实例切到另一个（换机后点新实例）时，先发后到的旧响应被丢弃。
 */
async function load(target = id.value) {
  const mine = ++seq
  loading.value = true
  try {
    const d = await api.get<AdminInstanceDetail & { cluster?: InstanceClusterBlock | null }>(`/instances/${target}`)
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
  void loadSessions()
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
  sessions.value = []
  sessionsTotal.value = 0
  portAction.visible = false
  removingApp.value = null
  booking.closeAll()
  void load(String(to.params.id))
  void loadSessions(String(to.params.id))
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
    Object.assign(refund, { quote: q, amount: centsToYuanInput(q.suggested), reason: '', error: '', visible: true })
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

// ---- 预订：改派 / 取消预订（ops；取消的动态码由请求层弹出）----
const booking = useBookingOps(() => load())
function openReassign() {
  if (inst.value) booking.openReassign(inst.value)
}
function openCancelBooking() {
  if (inst.value) booking.openCancel(inst.value)
}

// ---- 访问：端口处置与 SSH 会话 ----
const sessions = ref<AdminSshSession[]>([])
const sessionsTotal = ref(0)
let sessionsSeq = 0
/** 与 load() 相同：只采用最后一次请求、且仍是当前路由实例的结果（换实例后旧实例迟到的会话列表丢弃）。 */
async function loadSessions(target = id.value) {
  const mine = ++sessionsSeq
  const stale = () => mine !== sessionsSeq || target !== String(route.params.id)
  try {
    const p = await api.get<Paged<AdminSshSession>>(`/instances/${target}/ssh-sessions`, { page: 1, page_size: 20 })
    if (stale()) return
    sessions.value = p.items
    sessionsTotal.value = p.total
  } catch (e) {
    if (stale()) return
    notifyError(e)
  }
}
const portAction = reactive({ visible: false, kind: 'force' as 'force' | 'delete', port: 0, reason: '', error: '', busy: false })
function openPortAction(kind: 'force' | 'delete', p: InstancePort) {
  Object.assign(portAction, { visible: true, kind, port: p.port, reason: '', error: '' })
}
async function submitPortAction() {
  if (portAction.busy) return
  portAction.error = portAction.reason.trim() ? '' : '请填写原因'
  if (portAction.error) return
  portAction.busy = true
  try {
    if (portAction.kind === 'force') await api.post(`/instances/${id.value}/ports/${portAction.port}/force-private`, { reason: portAction.reason.trim() })
    else await api.del(`/instances/${id.value}/ports/${portAction.port}`, { reason: portAction.reason.trim() })
    ElMessage.success(portAction.kind === 'force' ? '已限制为私有' : '已删除端口')
    portAction.visible = false
    await load()
  } catch (e) {
    notifyError(e)
  } finally {
    portAction.busy = false
  }
}

// ---- 应用（只读 + 强制卸载，ops）----
const apps = computed(() => data.value?.apps ?? [])
const removingApp = ref<AdminDeployment | null>(null)
async function onAppRemoved() {
  removingApp.value = null
  await load()
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
              <el-tag v-if="inst.suspended" type="warning" data-testid="adetail-suspended">计时已暂停</el-tag>
              <el-tag v-if="inst.system" type="info" class="tag-gap" data-testid="adetail-system">平台节点</el-tag></span>
          </div>
        </template>
        <el-descriptions :column="3" border>
          <el-descriptions-item label="用户"><span data-testid="adetail-user"><IdLink kind="user" :id="inst.user_id" /></span></el-descriptions-item>
          <el-descriptions-item label="机器"><span data-testid="adetail-machine"><IdLink kind="machine" :id="inst.machine_id" :disabled="isFinance" /></span></el-descriptions-item>
          <el-descriptions-item label="商品"><span data-testid="adetail-product">{{ productName }}</span></el-descriptions-item>
          <el-descriptions-item label="镜像"><span data-testid="adetail-image">{{ imageName }}</span></el-descriptions-item>
          <el-descriptions-item label="剩余时长"><span data-testid="adetail-remaining" :data-remaining="inst.remaining_seconds">{{ inst.system ? '不计时' : inst.state === 'running' ? fmtDuration(inst.remaining_seconds) : '—' }}</span></el-descriptions-item>
          <el-descriptions-item label="待开始"><span data-testid="adetail-pending">{{ inst.pending_seconds > 0 ? fmtDuration(inst.pending_seconds) : '—' }}</span></el-descriptions-item>
          <el-descriptions-item label="到期时间"><span data-testid="adetail-expires">{{ inst.system ? '不计时' : fmtTime(inst.expires_at) || '—' }}</span></el-descriptions-item>
          <el-descriptions-item label="停机时间">{{ fmtTime(inst.stopped_at) || '—' }}</el-descriptions-item>
          <el-descriptions-item label="创建时间">{{ fmtTime(inst.created_at) }}</el-descriptions-item>
          <el-descriptions-item v-if="inst.scheduled_start_at" label="预订开始"><span key="start" data-testid="adetail-start">{{ fmtTime(inst.scheduled_start_at) }} ～ {{ fmtTime(inst.scheduled_end_at) }}</span></el-descriptions-item>
          <el-descriptions-item v-if="inst.batch_id" label="批次"><span key="batch" data-testid="adetail-batch"><BatchLink :id="inst.batch_id" :disabled="!canSeeOrders" /></span></el-descriptions-item>
          <el-descriptions-item label="改派次数"><span data-testid="adetail-reassigned">{{ inst.reassign_count }}</span></el-descriptions-item>
          <el-descriptions-item label="自动续费"><span data-testid="adetail-auto-renew">{{ inst.auto_renew ? `开启（${labelOf(durationLabel, inst.auto_renew_duration)}）` : '关闭' }}{{ inst.auto_renew_failed_reason ? ` · 最近失败：${inst.auto_renew_failed_reason}` : '' }}</span></el-descriptions-item>
        </el-descriptions>
        <div class="actions">
          <el-button v-if="showExtend(role, inst.state, inst.system) && !cluster" data-testid="adetail-extend-btn" @click="openExtend">免费延长</el-button>
          <el-tooltip v-else-if="showExtend(role, inst.state, inst.system) && cluster" :content="MEMBER_EXTEND_HINT" placement="top">
            <el-button data-testid="adetail-extend-cluster-btn" @click="$router.push(`/clusters/${cluster.id}`)">免费延长（到集群页）</el-button>
          </el-tooltip>
          <el-button v-if="showMigrate(role, inst.state, inst.system)" data-testid="adetail-migrate-btn" @click="openMigrate">换机</el-button>
          <el-button v-if="showRewipe(role, inst.state)" data-testid="adetail-rewipe-btn" @click="rewipe.visible = true">重新洗机</el-button>
          <el-button v-if="showRefund(role, inst.state, inst.system)" type="danger" plain :loading="refund.quoting" data-testid="adetail-refund-btn" @click="openRefund">退款并释放</el-button>
          <el-button v-if="showReassign(role, inst.state, inst.system)" data-testid="adetail-reassign-btn" @click="openReassign">改派</el-button>
          <el-button v-if="showCancelBooking(role, inst.state, inst.system)" type="danger" plain data-testid="adetail-cancel-booking-btn" @click="openCancelBooking">取消预订</el-button>
        </div>
      </el-card>

      <!-- 计划 13：集群成员 -->
      <el-card v-if="cluster" data-testid="instance-cluster-block">
        <template #header>
          <div class="head">
            <span>属于集群 <router-link :to="`/clusters/${cluster.id}`" class="mono" data-testid="instance-cluster-link">{{ cluster.id }}</router-link>
              <span data-testid="instance-cluster-rank">（rank {{ cluster.rank }} / {{ cluster.size }} 台{{ cluster.rank === 0 ? '，head' : '' }}）</span></span>
            <span>
              <el-tag size="small" :type="labelOf(clusterStatusType, cluster.status) as 'primary'" data-testid="instance-cluster-status">{{ labelOf(clusterStatusLabel, cluster.status) }}</el-tag>
              <span class="muted tag-gap">{{ labelOf(clusterNetworkLabel, cluster.network) }} · {{ cluster.fabric_name }} · {{ cluster.subnet }}</span>
            </span>
          </div>
        </template>
        <p class="muted" data-testid="instance-cluster-hint">{{ cluster.system ? '平台节点组的成员由 Token 商城管理。' : '集群成员的续费、自动续费、释放与延长在集群页统一操作；' }}{{ cluster.system ? '' : MIGRATE_MEMBER_HINT }}</p>
        <table class="grid">
          <thead><tr><th>rank</th><th>实例</th><th>机器</th><th>集群 IP</th><th>状态</th><th>网络状态</th></tr></thead>
          <tbody>
            <tr v-for="m in cluster.members" :key="m.rank" data-testid="instance-cluster-member" :data-rank="m.rank" :class="{ self: m.instance_id === inst.id }">
              <td>{{ m.rank }}</td>
              <td><IdLink kind="instance" :id="m.instance_id" /></td>
              <td>{{ m.machine_label }}（<IdLink kind="machine" :id="m.machine_id" :disabled="isFinance" />）</td>
              <td class="mono">{{ m.ip }}</td>
              <td>{{ labelOf(instanceStateLabel, m.state) }}</td>
              <td :class="{ error: netStatusBad(m.net_status) }">{{ netStatusText(m.net_status) }}</td>
            </tr>
          </tbody>
        </table>
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
            <thead><tr><th>订单号</th><th>类型</th><th>租期</th><th>数量</th><th>金额</th><th>已退</th><th>状态</th><th>自动</th><th>时间</th></tr></thead>
            <tbody>
              <tr v-for="o in data.orders" :key="o.id" data-testid="adetail-order-row">
                <td class="mono" data-testid="adetail-order-id">{{ o.id }}</td>
                <td data-testid="adetail-order-kind">{{ labelOf(orderKindLabel, o.kind) }}</td>
                <td>{{ labelOf(durationLabel, o.duration) }}</td>
                <td data-testid="adetail-order-quantity">{{ o.quantity }}</td>
                <td data-testid="adetail-order-amount">{{ fmtYuan(o.amount) }}</td>
                <td data-testid="adetail-order-refunded">{{ fmtYuan(o.refunded_amount) }}</td>
                <td data-testid="adetail-order-status">{{ labelOf(orderStatusLabel, o.status) }}</td>
                <td data-testid="adetail-order-auto">{{ o.auto ? '是' : '' }}</td>
                <td>{{ fmtTime(o.created_at) }}</td>
              </tr>
              <tr v-if="!data.orders.length"><td colspan="9" class="muted empty">无订单</td></tr>
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

      <el-card data-testid="admin-instance-apps">
        <template #header>
          <div class="head"><span>应用<span v-if="inst.system" class="muted tag-gap" data-testid="admin-instance-apps-system-hint">平台节点的部署由 Token 商城管理，请在「节点与机器池」里排空 / 重建 / 移除节点</span></span><router-link :to="{ path: '/deployments', query: { instance_id: inst.id } }" data-testid="admin-instance-apps-all">在部署列表中查看</router-link></div>
        </template>
        <table class="grid">
          <thead><tr><th>应用 / 版本</th><th>状态</th><th>进度 / 错误</th><th>端口</th><th>更新时间</th><th>操作</th></tr></thead>
          <tbody>
            <tr v-for="a in apps" :key="a.id" data-testid="admin-instance-app" :data-dep="a.id" :data-state="a.state">
              <td data-testid="admin-instance-app-name">{{ a.app_name }} · {{ a.version }}<el-tag v-if="a.update_available" size="small" type="warning" class="tag-gap">有新版本</el-tag></td>
              <td><el-tag size="small" :type="labelOf(deploymentStateType, a.state) as 'primary'" data-testid="admin-instance-app-state">{{ labelOf(deploymentStateLabel, a.state) }}</el-tag></td>
              <td>
                <span v-if="a.error_code" class="error" data-testid="admin-instance-app-error" :data-code="a.error_code">{{ deploymentErrorText(a.error_code, a.message) }}</span>
                <span v-else data-testid="admin-instance-app-progress">{{ progressText(a) || '—' }}</span>
              </td>
              <td>{{ a.port }}</td>
              <td>{{ fmtTime(a.updated_at) }}</td>
              <td>
                <router-link :to="{ path: '/deployments', query: { instance_id: inst.id, dep: a.id } }" data-testid="admin-instance-app-detail">详情</router-link>
                <el-button v-if="!inst.system && showForceRemove(role, a.state)" size="small" type="danger" plain class="tag-gap" data-testid="admin-instance-app-force-remove" @click="removingApp = a">强制卸载</el-button>
              </td>
            </tr>
            <tr v-if="!apps.length"><td colspan="6" class="muted empty" data-testid="admin-instance-apps-empty">无应用部署</td></tr>
          </tbody>
        </table>
      </el-card>
      <ForceRemoveDialog :target="removingApp" @close="removingApp = null" @done="onAppRemoved" />

      <el-card header="访问" data-testid="adetail-access-card">
        <el-descriptions :column="2" border>
          <el-descriptions-item label="密码登录"><span data-testid="adetail-password-auth">{{ inst.password_auth ? '已开启' : '已关闭（仅公钥）' }}</span></el-descriptions-item>
          <el-descriptions-item label="容器主机密钥"><span class="mono" data-testid="adetail-hostkey">{{ data.ssh_host_pubkey_fingerprint || '（存量实例，首连后登记）' }}</span></el-descriptions-item>
        </el-descriptions>
        <h4>Web 端口</h4>
        <table class="grid">
          <thead><tr><th>端口</th><th>备注</th><th>网址</th><th>可见性</th><th v-if="showPortActions(role)">操作</th></tr></thead>
          <tbody>
            <tr v-for="p in data.ports" :key="p.port" data-testid="adetail-port-row">
              <td>{{ p.port }}</td>
              <td>{{ p.note }}</td>
              <td class="mono" data-testid="adetail-port-url">{{ p.url }}</td>
              <td>
                <span data-testid="adetail-port-visibility">{{ p.visibility === 'public' ? '公开' : '私有' }}</span>
                <el-tag v-if="p.forced_private" type="danger" size="small" data-testid="adetail-port-forced">平台限制</el-tag>
              </td>
              <td v-if="showPortActions(role)">
                <el-button v-if="!p.forced_private" size="small" data-testid="adetail-port-force" @click="openPortAction('force', p)">强制私有</el-button>
                <el-button size="small" type="danger" plain data-testid="adetail-port-delete" @click="openPortAction('delete', p)">删除</el-button>
              </td>
            </tr>
            <tr v-if="!data.ports.length"><td :colspan="showPortActions(role) ? 5 : 4" class="muted empty">无端口</td></tr>
          </tbody>
        </table>
        <h4>最近 SSH 会话（共 {{ sessionsTotal }}）</h4>
        <table class="grid">
          <thead><tr><th>方式</th><th>来源 IP</th><th>开始</th><th>结束</th><th>上行 / 下行</th></tr></thead>
          <tbody>
            <tr v-for="s in sessions" :key="s.id" data-testid="adetail-session-row">
              <td data-testid="adetail-session-method">{{ s.auth_method === 'password' ? '密码' : '公钥' }}</td>
              <td class="mono" data-testid="adetail-session-ip">{{ s.client_ip }}</td>
              <td>{{ fmtTime(s.started_at) }}</td>
              <td>{{ s.ended_at ? fmtTime(s.ended_at) : '进行中' }}</td>
              <td>{{ s.bytes_up }} / {{ s.bytes_down }}</td>
            </tr>
            <tr v-if="!sessions.length"><td colspan="5" class="muted empty" data-testid="adetail-sessions-empty">无会话</td></tr>
          </tbody>
        </table>
      </el-card>

      <el-dialog v-model="portAction.visible" :title="portAction.kind === 'force' ? `将端口 ${portAction.port} 限制为私有` : `删除端口 ${portAction.port}`" width="460" align-center data-testid="port-action-dialog">
        <p class="muted">{{ portAction.kind === 'force' ? '用户将无法再把该端口设为公开（可改备注、可删除）。' : '删除后用户需要重新添加。' }}</p>
        <el-form label-position="top" @submit.prevent="submitPortAction">
          <el-form-item label="原因（必填，写入审计）"><el-input v-model="portAction.reason" data-testid="port-action-reason" /></el-form-item>
          <p v-if="portAction.error" class="error" data-testid="port-action-error">{{ portAction.error }}</p>
          <el-button :type="portAction.kind === 'force' ? 'warning' : 'danger'" native-type="submit" :loading="portAction.busy" data-testid="port-action-submit">确定</el-button>
        </el-form>
      </el-dialog>

      <BookingOpsDialogs :ops="booking" />
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
        <p v-if="cluster" class="warn" data-testid="migrate-cluster-hint">集群成员：{{ MIGRATE_MEMBER_HINT }}</p>
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
.tag-gap { margin-left: 8px; }
.warn { color: var(--el-color-warning); font-size: 13px; }
.self td { background: var(--el-color-primary-light-9); }
</style>
