<script setup lang="ts">
import {
  clusterAppStatusLabel, clusterAppStatusType, clusterNetworkLabel, clusterRoleLabel, clusterStatusLabel, clusterStatusType, deploymentStateLabel, fabricKindLabel, fmtDuration, fmtTime,
  instanceStateLabel, instanceStateType, labelOf, messageOf, type AdminInstance,
} from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, onMounted, reactive, ref } from 'vue'
import { onBeforeRouteUpdate, useRoute } from 'vue-router'
import { api } from '../api'
import BatchLink from '../components/BatchLink.vue'
import IdLink from '../components/IdLink.vue'
import {
  canExtendCluster, canForceRemoveClusterApp, canMigrateMember, canResyncCluster, MIGRATE_MEMBER_HINT, netStatusBad, netStatusText, type AdminClusterDetail, type AdminClusterView,
} from '../logic/clusters'
import { deploymentHealthLabel } from '../logic/deployments'
import { extendSeconds, validateExtend } from '../logic/instanceOps'
import { notifyError } from '../logic/notify'
import { FIN_ROLES } from '../menu'
import { session } from '../store/session'

// 集群详情（计划 13 规格 §10.2、§11）：概要、成员（实例 / 机器 / 集群 IP / 网络状态）、版本、集群应用、最近事件；
// 操作：延长（全员同步，ops）、重新下发集群网络（ops）、成员换机（ops，rental 限同一 fabric / 网段，走现有 POST /instances/{id}/migrate）。
const route = useRoute()
const id = computed(() => String(route.params.id))
const role = computed(() => session.admin?.role)
const data = ref<AdminClusterDetail | null>(null)
const loading = ref(false)
const error = ref('')
const c = computed(() => data.value?.cluster ?? null)
const app = computed(() => data.value?.app ?? null)
const isFinance = computed(() => role.value === 'finance')
const canSeeOrders = computed(() => !!role.value && FIN_ROLES.includes(role.value))

let seq = 0
async function load(target = id.value) {
  const mine = ++seq
  loading.value = true
  try {
    const d = await api.get<AdminClusterDetail>(`/clusters/${encodeURIComponent(target)}`)
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
onMounted(() => void load())
onBeforeRouteUpdate((to) => {
  if (to.params.id === route.params.id) return
  data.value = null
  error.value = ''
  extend.visible = false
  resync.visible = false
  migrate.visible = false
  migrate.result = null
  forceRemove.visible = false
  void load(String(to.params.id))
})
/** 操作接口返回新的 ClusterView：先就地替换，再整体刷新（事件 / 集群应用） */
function applyView(v: AdminClusterView | undefined) {
  if (data.value && v && v.id === data.value.cluster.id) data.value = { ...data.value, cluster: v }
}

// ---- 延长（ops；全部在位成员同步延长，审计 cluster.extend）----
const extend = reactive({ visible: false, amount: 1 as number | null, unit: 'day' as 'hour' | 'day', reason: '', error: '', busy: false })
function openExtend() {
  Object.assign(extend, { visible: true, amount: 1, unit: 'day', reason: '', error: '' })
}
async function submitExtend() {
  if (extend.busy) return
  const seconds = extendSeconds(extend.amount, extend.unit)
  extend.error = validateExtend(seconds) ?? (extend.reason.trim() ? '' : '请填写补偿原因')
  if (extend.error) return
  extend.busy = true
  try {
    const v = await api.post<AdminClusterView>(`/clusters/${encodeURIComponent(id.value)}/extend`, { seconds, reason: extend.reason.trim() })
    ElMessage.success(`已为全部成员延长 ${fmtDuration(seconds)}`)
    extend.visible = false
    applyView(v)
    await load()
  } catch (e) {
    notifyError(e) // invalid_state（系统 / 已结束）、bad_request 原文；弹窗保留
  } finally {
    extend.busy = false
  }
}

// ---- 重新下发集群网络（ops；version + 1，审计 cluster.resync）----
const resync = reactive({ visible: false, busy: false })
async function submitResync() {
  if (resync.busy) return
  resync.busy = true
  try {
    const v = await api.post<AdminClusterView>(`/clusters/${encodeURIComponent(id.value)}/resync`, {})
    ElMessage.success(`已重新下发集群网络（版本 ${v.version}）`)
    resync.visible = false
    applyView(v)
    await load()
  } catch (e) {
    resync.visible = false
    notifyError(e)
  } finally {
    resync.busy = false
  }
}

// ---- 强制卸载集群应用（ops；成员部署只能整组卸载，审计 cluster.app_force_remove）----
const forceRemove = reactive({ visible: false, reason: '', busy: false })
function openForceRemove() {
  Object.assign(forceRemove, { visible: true, reason: '' })
}
async function submitForceRemove() {
  if (forceRemove.busy) return
  if (!forceRemove.reason.trim()) return void ElMessage.warning('请填写卸载原因')
  forceRemove.busy = true
  try {
    await api.post(`/clusters/${encodeURIComponent(id.value)}/app/force-remove`, { reason: forceRemove.reason.trim() })
    ElMessage.success('已强制卸载集群应用')
    forceRemove.visible = false
    await load()
  } catch (e) {
    notifyError(e) // not_found（已无在用集群应用）等原文；弹窗保留
  } finally {
    forceRemove.busy = false
  }
}

// ---- 成员换机（ops）----
const migrate = reactive({ visible: false, instanceId: '', rank: 0, reason: '', busy: false, result: null as AdminInstance | null })
function openMigrate(m: { instance_id: string; rank: number }) {
  Object.assign(migrate, { visible: true, instanceId: m.instance_id, rank: m.rank, reason: '' })
}
async function submitMigrate() {
  if (migrate.busy) return
  if (!migrate.reason.trim()) return void ElMessage.warning('请填写换机原因')
  migrate.busy = true
  try {
    const out = await api.post<{ instance: AdminInstance }>(`/instances/${encodeURIComponent(migrate.instanceId)}/migrate`, { reason: migrate.reason.trim() })
    migrate.visible = false
    migrate.result = out.instance
    await load()
  } catch (e) {
    notifyError(e) // no_capacity（同一集群网络没有空闲机器）等原文；弹窗保留
  } finally {
    migrate.busy = false
  }
}

const statusTag = (s: string) => labelOf(clusterStatusType, s) as 'primary'
const eventText = (e: { from_state: string; to_state: string }) => `${labelOf(instanceStateLabel, e.from_state) || '—'} → ${labelOf(instanceStateLabel, e.to_state)}`
</script>

<template>
  <div>
    <el-alert v-if="error && !data" type="error" :title="error" show-icon :closable="false" data-testid="cluster-detail-error">
      <el-button size="small" :loading="loading" data-testid="cluster-detail-retry" @click="load()">重试</el-button>
    </el-alert>
    <div v-if="data && c" class="detail">
      <el-card>
        <template #header>
          <div class="head">
            <span><span class="mono" data-testid="cluster-detail-id">{{ c.id }}</span> · {{ c.size }} 台</span>
            <span>
              <el-tag :type="statusTag(c.status)" data-testid="cluster-detail-status" :data-status="c.status">{{ labelOf(clusterStatusLabel, c.status) }}</el-tag>
              <el-tag v-if="c.system" type="info" class="gap" data-testid="cluster-detail-system">平台节点组</el-tag>
              <el-button size="small" class="gap" :loading="loading" data-testid="cluster-detail-refresh" @click="load()">刷新</el-button>
            </span>
          </div>
        </template>
        <el-alert v-if="c.status === 'degraded'" type="warning" show-icon :closable="false" class="mb" data-testid="cluster-degraded-hint"
          title="有成员不在运行或集群网络异常：查看下方成员的网络状态；确认机器恢复后可「重新下发」集群网络，机器故障时对该成员「换机」" />
        <el-alert v-if="c.network === 'rdma_degraded'" type="warning" show-icon :closable="false" class="mb" data-testid="cluster-rdma-degraded-hint"
          title="集群网络要求 RDMA，但有成员的网卡 RDMA 不可用，已退回以太网（性能降低）" />
        <el-descriptions :column="3" border>
          <el-descriptions-item label="归属">
            <span data-testid="cluster-detail-owner"><template v-if="c.system">平台（Token 商城节点组）</template><template v-else>用户 <IdLink kind="user" :id="c.user_id" /></template></span>
          </el-descriptions-item>
          <el-descriptions-item label="网络"><span data-testid="cluster-detail-network">{{ labelOf(clusterNetworkLabel, c.network) }}</span></el-descriptions-item>
          <el-descriptions-item label="集群网络">
            <router-link :to="{ path: '/fabrics' }" data-testid="cluster-detail-fabric">{{ c.fabric_name || `#${c.fabric_id}` }}</router-link>（{{ labelOf(fabricKindLabel, c.fabric_kind) }}）
          </el-descriptions-item>
          <el-descriptions-item label="模式">{{ c.mode === 'macvlan' ? 'macvlan（CX-7）' : '三层路由' }}{{ c.vlan != null ? ` · VLAN ${c.vlan}` : '' }}{{ c.rdma ? ' · 要求 RDMA' : '' }}{{ c.mtu ? ` · MTU ${c.mtu}` : '' }}</el-descriptions-item>
          <el-descriptions-item label="集群子网"><span class="mono" data-testid="cluster-detail-subnet">{{ c.subnet }}</span></el-descriptions-item>
          <el-descriptions-item label="head 地址"><span class="mono" data-testid="cluster-detail-head">{{ c.head_ip || '—' }}</span></el-descriptions-item>
          <el-descriptions-item label="网络配置版本"><span data-testid="cluster-detail-version">{{ c.version }}</span></el-descriptions-item>
          <el-descriptions-item label="批次"><BatchLink v-if="c.batch_id" :id="c.batch_id" :disabled="!canSeeOrders" /><span v-else>—</span></el-descriptions-item>
          <el-descriptions-item label="创建">{{ fmtTime(c.created_at) }}</el-descriptions-item>
          <el-descriptions-item label="就绪">{{ fmtTime(c.ready_at) || '—' }}</el-descriptions-item>
          <el-descriptions-item label="结束">{{ fmtTime(c.ended_at) || '—' }}</el-descriptions-item>
        </el-descriptions>
        <div class="actions">
          <el-button v-if="canExtendCluster(role, c)" data-testid="cluster-extend" @click="openExtend">免费延长</el-button>
          <el-button v-if="canResyncCluster(role, c)" data-testid="cluster-resync" @click="resync.visible = true">重新下发集群网络</el-button>
        </div>
      </el-card>

      <el-card header="成员" data-testid="cluster-detail-members">
        <table class="grid">
          <thead><tr><th>rank</th><th>实例</th><th>机器</th><th>集群 IP</th><th>实例状态</th><th>网络状态</th><th>到期</th><th>操作</th></tr></thead>
          <tbody>
            <tr v-for="m in c.members" :key="m.rank" data-testid="cluster-member-row" :data-rank="m.rank" :data-instance="m.instance_id">
              <td>{{ m.rank }}{{ m.rank === 0 ? '（head）' : '' }}</td>
              <td data-testid="cluster-member-instance"><IdLink kind="instance" :id="m.instance_id" /></td>
              <td data-testid="cluster-member-machine">{{ m.machine_label }}（<IdLink kind="machine" :id="m.machine_id" :disabled="isFinance" />）</td>
              <td class="mono" data-testid="cluster-member-ip">{{ m.ip }}</td>
              <td>
                <el-tag size="small" :type="labelOf(instanceStateType, m.state) as 'primary'" data-testid="cluster-member-state">{{ labelOf(instanceStateLabel, m.state) }}</el-tag>
                <el-tag v-if="m.suspended" size="small" type="warning" class="gap">计时已暂停</el-tag>
              </td>
              <td><span :class="{ bad: netStatusBad(m.net_status) }" data-testid="cluster-member-net" :data-net="m.net_status">{{ netStatusText(m.net_status) }}</span></td>
              <td>{{ c.system ? '不计时' : fmtTime(m.expires_at) || '—' }}</td>
              <td><el-button v-if="canMigrateMember(role, c, m.state)" size="small" data-testid="cluster-member-migrate" @click="openMigrate(m)">换机</el-button></td>
            </tr>
            <tr v-if="!c.members.length"><td colspan="8" class="muted empty" data-testid="cluster-members-empty">没有在位成员</td></tr>
          </tbody>
        </table>
      </el-card>

      <el-card data-testid="cluster-detail-app">
        <template #header>
          <div class="head">
            <span>集群应用</span>
            <span>
              <router-link v-if="app" :to="{ path: '/deployments', query: { instance_id: c.members[0]?.instance_id } }" data-testid="cluster-app-deployments">在部署列表中查看</router-link>
              <el-button v-if="canForceRemoveClusterApp(role, app)" size="small" type="danger" plain class="gap" data-testid="cluster-app-force-remove" @click="openForceRemove">强制卸载</el-button>
            </span>
          </div>
        </template>
        <p v-if="!app" class="muted" data-testid="cluster-app-none">没有集群应用</p>
        <template v-else>
          <el-alert v-if="app.status === 'stale'" type="warning" show-icon :closable="false" class="mb" data-testid="cluster-app-stale" title="成员已更换，集群应用需要由用户（或 Token 商城）重新部署" />
          <el-descriptions :column="3" border size="small">
            <el-descriptions-item label="应用"><span data-testid="cluster-app-name">{{ app.app_name }} · {{ app.version }}</span></el-descriptions-item>
            <el-descriptions-item label="状态">
              <el-tag size="small" :type="labelOf(clusterAppStatusType, app.status) as 'primary'" data-testid="cluster-app-status" :data-status="app.status">{{ labelOf(clusterAppStatusLabel, app.status) }}</el-tag>
            </el-descriptions-item>
            <el-descriptions-item label="健康 / 进度">{{ labelOf(deploymentHealthLabel, app.health) }} · {{ app.progress }}%</el-descriptions-item>
            <el-descriptions-item label="head 访问地址" :span="3"><span class="mono" data-testid="cluster-app-url">{{ app.url || '—' }}</span></el-descriptions-item>
          </el-descriptions>
          <table class="grid app-members" data-testid="cluster-app-members">
            <thead><tr><th>rank</th><th>角色</th><th>实例</th><th>在位</th><th>部署状态</th><th>健康</th><th>进度</th><th>部署</th></tr></thead>
            <tbody>
              <tr v-for="m in app.members" :key="`${m.rank}-${m.deployment_id}`" data-testid="cluster-app-member" :data-rank="m.rank" :data-role="m.role">
                <td>{{ m.rank }}</td>
                <td data-testid="cluster-app-member-role">{{ labelOf(clusterRoleLabel, m.role) }}</td>
                <td><IdLink kind="instance" :id="m.instance_id" /></td>
                <td data-testid="cluster-app-member-current">{{ m.current ? '在位' : '已换出' }}</td>
                <td data-testid="cluster-app-member-state">{{ labelOf(deploymentStateLabel, m.state) }}</td>
                <td>{{ labelOf(deploymentHealthLabel, m.health) }}</td>
                <td>{{ m.progress }}%</td>
                <td><router-link v-if="m.deployment_id" :to="{ path: '/deployments', query: { dep: m.deployment_id } }">{{ m.deployment_id }}</router-link></td>
              </tr>
            </tbody>
          </table>
        </template>
      </el-card>

      <el-card header="最近事件（成员实例，含已换出的旧成员，最近 50 条）" data-testid="cluster-detail-events">
        <table class="grid">
          <tbody>
            <tr v-for="e in data.events" :key="e.id" data-testid="cluster-event">
              <td class="time">{{ fmtTime(e.at) }}</td>
              <td><IdLink kind="instance" :id="e.instance_id" /></td>
              <td>{{ eventText(e) }}</td>
              <td>{{ e.detail }}</td>
            </tr>
            <tr v-if="!data.events.length"><td class="muted empty" data-testid="cluster-events-empty">无事件</td></tr>
          </tbody>
        </table>
      </el-card>

      <el-dialog v-model="extend.visible" title="免费延长（全部成员）" width="460" align-center data-testid="cluster-extend-dialog">
        <el-form label-position="top" novalidate @submit.prevent="submitExtend">
          <div class="two">
            <el-form-item label="时长"><el-input-number v-model="extend.amount" :min="0" :controls="false" data-testid="cluster-extend-amount" /></el-form-item>
            <el-form-item label="单位">
              <el-select v-model="extend.unit" data-testid="cluster-extend-unit"><el-option label="小时" value="hour" /><el-option label="天" value="day" /></el-select>
            </el-form-item>
          </div>
          <p class="muted">范围 1 分钟～30 天；所有在位成员同时延长，到期时间保持一致</p>
          <el-form-item label="原因（必填）"><el-input v-model="extend.reason" data-testid="cluster-extend-reason" /></el-form-item>
          <p v-if="extend.error" class="error" data-testid="cluster-extend-error">{{ extend.error }}</p>
          <el-button type="primary" native-type="submit" :loading="extend.busy" data-testid="cluster-extend-submit">确定延长</el-button>
        </el-form>
      </el-dialog>

      <el-dialog v-model="resync.visible" title="重新下发集群网络" width="460" align-center data-testid="cluster-resync-dialog">
        <p>将向全部在位成员重新下发集群网络配置（版本 + 1），用于成员网络异常、Agent 重装或换机后的修复。</p>
        <template #footer>
          <el-button data-testid="cluster-resync-cancel" @click="resync.visible = false">取消</el-button>
          <el-button type="primary" :loading="resync.busy" data-testid="cluster-resync-ok" @click="submitResync">确定下发</el-button>
        </template>
      </el-dialog>

      <el-dialog v-model="forceRemove.visible" title="强制卸载集群应用" width="460" align-center data-testid="cluster-app-force-remove-dialog">
        <p>将卸载全部成员上的 {{ app?.app_name }}（整组卸载），应用目录会删除，模型缓存保留；用户会看到应用被平台卸载。</p>
        <el-form label-position="top" @submit.prevent="submitForceRemove">
          <el-form-item label="原因（必填）"><el-input v-model="forceRemove.reason" data-testid="cluster-app-force-remove-reason" /></el-form-item>
          <el-button type="danger" native-type="submit" :loading="forceRemove.busy" data-testid="cluster-app-force-remove-submit">确定卸载</el-button>
        </el-form>
      </el-dialog>

      <el-dialog v-model="migrate.visible" :title="`成员换机（rank ${migrate.rank}）`" width="480" align-center data-testid="cluster-migrate-dialog">
        <p>将在另一台空闲机器上用相同镜像重建实例 <span class="mono">{{ migrate.instanceId }}</span>，沿用原 rank 与剩余时长；原实例进入洗机。</p>
        <p class="warn" data-testid="cluster-migrate-hint">{{ MIGRATE_MEMBER_HINT }}</p>
        <el-form label-position="top" @submit.prevent="submitMigrate">
          <el-form-item label="原因（必填）"><el-input v-model="migrate.reason" data-testid="cluster-migrate-reason" /></el-form-item>
          <el-button type="primary" native-type="submit" :loading="migrate.busy" data-testid="cluster-migrate-submit">确定换机</el-button>
        </el-form>
      </el-dialog>
      <el-dialog :model-value="!!migrate.result" title="换机成功" width="460" align-center @close="migrate.result = null">
        <p v-if="migrate.result" data-testid="cluster-migrate-result">
          已在机器 <IdLink kind="machine" :id="migrate.result.machine_id" :disabled="isFinance" /> 上创建新成员 <IdLink kind="instance" :id="migrate.result.id" />，SSH 密码已变化，请通知用户。
        </p>
      </el-dialog>
    </div>
  </div>
</template>

<style scoped>
.detail { display: grid; gap: 16px; }
.head { display: flex; justify-content: space-between; align-items: center; gap: 8px; }
.gap { margin-left: 8px; }
.mb { margin-bottom: 12px; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.muted { color: #909399; font-size: 13px; }
.warn { color: var(--el-color-warning); font-size: 13px; }
.error { color: var(--el-color-danger); font-size: 13px; }
.bad { color: var(--el-color-danger); }
.actions { display: flex; gap: 12px; margin-top: 16px; }
.two { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; }
.grid { width: 100%; border-collapse: collapse; font-size: 13px; }
.grid th, .grid td { padding: 8px; text-align: left; border-bottom: 1px solid var(--el-border-color-lighter); vertical-align: middle; }
.grid th { color: #909399; font-weight: 500; }
.app-members { margin-top: 12px; }
.time { white-space: nowrap; color: #909399; }
.empty { text-align: center; }
</style>
