<script setup lang="ts">
import { fmtTime, instanceStateLabel, labelOf, machineStateLabel, messageOf, type AdminGroup, type EnrollCommand, type LLMPool, type Listed } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { onBeforeRouteUpdate, useRoute } from 'vue-router'
import { api } from '../api'
import IdLink from '../components/IdLink.vue'
import MachineNetCard from '../components/MachineNetCard.vue'
import type { NetMachineDetail } from '../logic/clusters'
import { isPoolGroup } from '../logic/llm'
import { fmtBytes, fmtPercent, fmtTemp } from '../logic/metrics'
import { notifyError } from '../logic/notify'
import { OPS_ROLES } from '../menu'
import { hasRole } from '../store/session'

const route = useRoute()
const id = computed(() => String(route.params.id))
const data = ref<NetMachineDetail | null>(null)
const canEditNet = computed(() => hasRole(OPS_ROLES))
const loading = ref(false)
const error = ref('')
const maintenanceVisible = ref(false)
const powerVisible = ref(false)
const busy = ref(false)
const enrollVisible = ref(false)
const enroll = ref<EnrollCommand | null>(null)
const revokeVisible = ref(false)
const groups = ref<AdminGroup[]>([])
const edit = reactive({ visible: false, groupId: '' as string, displayName: '', busy: false })
/** Agent 上报的镜像（repository:tag），按文本显示 */
const cachedImages = computed(() => data.value?.machine.cached_images ?? [])
/** 机器在 Token 服务池（purpose='llm' 的机组）：显示标签与节点页链接（详情的 group 或 GET /groups 带 purpose） */
const inPool = computed(() => isPoolGroup(data.value?.machine.group_id ?? null, data.value?.group ?? null, groups.value))
/** 池内机器上的节点（GET /llm/pool 里本机那一行）：有则链接直接打开节点详情；查不到只链接到节点页，不提示错误 */
const poolNode = ref<{ id: string; model_slug: string } | null>(null)
let poolSeq = 0
async function loadPoolNode() {
  const mine = ++poolSeq
  const machineId = data.value?.machine.id
  poolNode.value = null
  if (!inPool.value || machineId == null) return
  try {
    const p = await api.get<LLMPool>('/llm/pool')
    if (mine !== poolSeq) return
    const n = p.machines.find((m) => m.machine.id === machineId)?.node
    poolNode.value = n && n.state !== 'removed' && n.state !== 'failed' ? { id: n.id, model_slug: n.model_slug } : null
  } catch {
    // 节点链接只是便利：失败时退回到节点页链接
  }
}
watch(() => [inPool.value, data.value?.machine.id] as const, () => void loadPoolNode())
const poolLink = computed(() => ({ path: '/llm', query: poolNode.value ? { tab: 'nodes', node: poolNode.value.id } : { tab: 'nodes' } }))
const inMaintenance = computed(() => data.value?.machine.state === 'maintenance' || data.value?.machine.state === 'faulty')

let seq = 0
/**
 * 首次加载失败：页内错误 + 重试按钮（不自动重试）；操作后的刷新失败只提示，保留已显示的数据。
 * 只采用最后一次请求的结果：从一台机器的详情切到另一台时，先发后到的旧响应被丢弃。
 */
async function load(target = id.value) {
  const mine = ++seq
  loading.value = true
  try {
    const d = await api.get<NetMachineDetail>(`/machines/${target}`)
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
async function loadGroups() {
  try {
    groups.value = (await api.get<Listed<AdminGroup>>('/groups')).items
  } catch (e) {
    notifyError(e)
  }
}
onMounted(() => {
  void load()
  void loadGroups()
})
// 同一组件换了机器（/machines/1 → /machines/2）：清掉旧机器的数据与弹窗，按新 ID 重新加载。
// 用 onBeforeRouteUpdate 而不是 watch(route.params.id)：离开到实例详情时 params.id 也会变，watch 会拿实例 ID 去请求机器
onBeforeRouteUpdate((to) => {
  if (to.params.id === route.params.id) return
  data.value = null
  error.value = ''
  maintenanceVisible.value = false
  powerVisible.value = false
  closeEnroll() // 接入命令只属于原机器，换机器时一并丢掉
  revokeVisible.value = false
  edit.visible = false
  void load(String(to.params.id))
})

async function toggleMaintenance() {
  if (busy.value) return // 同步守卫：连点只发一次
  busy.value = true
  const on = !inMaintenance.value
  try {
    await api.post(`/machines/${id.value}/maintenance`, { on })
    ElMessage.success(on ? '已进入维护' : '已结束维护')
    maintenanceVisible.value = false
    await load()
  } catch (e) {
    maintenanceVisible.value = false
    notifyError(e) // 「该机器上还有未结束的实例，不能结束维护」等原文
  } finally {
    busy.value = false
  }
}
async function powerCycle() {
  if (busy.value) return // 同步守卫：断电重启不是幂等操作，连点只发一次
  busy.value = true
  try {
    await api.post(`/machines/${id.value}/power-cycle`, {})
    ElMessage.success('已通过 PDU 断电重启')
    powerVisible.value = false
  } catch (e) {
    powerVisible.value = false
    notifyError(e) // 「该机器未配置 PDU」等原文
  } finally {
    busy.value = false
  }
}

function openEdit() {
  Object.assign(edit, { visible: true, groupId: data.value?.machine.group_id ? String(data.value.machine.group_id) : '', displayName: data.value?.machine.display_name ?? '' })
}
async function submitEdit() {
  if (edit.busy) return
  edit.busy = true
  try {
    await api.put(`/machines/${id.value}`, { group_id: edit.groupId ? Number(edit.groupId) : null, display_name: edit.displayName.trim() })
    ElMessage.success('已保存')
    edit.visible = false
    await load()
  } catch (e) {
    notifyError(e) // duplicate_display_name / group_not_found 原文
  } finally {
    edit.busy = false
  }
}

/** 生成接入命令：请求层遇到 totp_required 会弹动态码框；命令只显示一次，关闭后不再可见（需要重新生成）。 */
async function generateEnroll() {
  if (busy.value) return // 同步守卫：每次生成都会作废上一条令牌，连点只发一次
  busy.value = true
  const target = id.value
  try {
    const out = await api.post<EnrollCommand>(`/machines/${target}/enroll-command`, {})
    if (target !== id.value) return // 等待期间切到了另一台机器：不把旧机器的命令显示在新机器页上
    enroll.value = out
    enrollVisible.value = true
  } catch (e) {
    notifyError(e) // 「未配置 Agent 网关」等原文
  } finally {
    busy.value = false
  }
}
async function copyEnroll() {
  if (!enroll.value) return
  try {
    await navigator.clipboard.writeText(enroll.value.command)
    ElMessage.success('已复制')
  } catch {
    ElMessage.error('复制失败，请手动选中复制')
  }
}
function closeEnroll() {
  enrollVisible.value = false
  enroll.value = null // 明文令牌不留在内存里，关闭后无法再看到
}
async function revokeEnrollment() {
  if (busy.value) return // 同步守卫：连点只发一次
  busy.value = true
  try {
    await api.post(`/machines/${id.value}/revoke-enrollment`, {})
    ElMessage.success('已撤销接入')
    revokeVisible.value = false
    await load()
  } catch (e) {
    revokeVisible.value = false
    notifyError(e) // 「该机器尚未接入」等原文
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div>
    <el-alert v-if="error && !data" type="error" :title="error" show-icon :closable="false" data-testid="mdetail-error">
      <el-button size="small" :loading="loading" data-testid="mdetail-retry" @click="load()">重试</el-button>
    </el-alert>
    <div v-if="data" class="detail">
      <el-card>
        <template #header><div class="head"><span class="mono" data-testid="mdetail-serial">{{ data.machine.serial }}</span><el-tag data-testid="mdetail-state">{{ labelOf(machineStateLabel, data.machine.state) }}</el-tag></div></template>
        <el-descriptions :column="3" border>
          <el-descriptions-item label="机柜 / 槽位">{{ data.machine.rack }} / {{ data.machine.slot }}</el-descriptions-item>
          <el-descriptions-item label="内网 IP"><span class="mono">{{ data.machine.lan_ip }}</span></el-descriptions-item>
          <el-descriptions-item label="在线"><span data-testid="mdetail-online">{{ data.machine.agent_online ? '在线' : '离线' }}</span>（{{ fmtTime(data.machine.last_seen_at) || '从未心跳' }}）</el-descriptions-item>
          <el-descriptions-item label="PDU">{{ data.machine.pdu_id ? `#${data.machine.pdu_id} 插座 ${data.machine.pdu_outlet}` : '未配置' }}</el-descriptions-item>
          <el-descriptions-item label="机组"><span data-testid="mdetail-group">{{ data.group?.name ?? '未分组' }}</span>
            <template v-if="inPool">
              <el-tag size="small" type="warning" class="pool-tag" data-testid="mdetail-pool-tag">Token 服务池</el-tag>
              <router-link :to="poolLink" class="pool-link" data-testid="mdetail-pool-link">{{ poolNode ? `节点 ${poolNode.model_slug}` : '查看节点' }}</router-link>
            </template>
          </el-descriptions-item>
          <el-descriptions-item label="展示名"><span data-testid="mdetail-display">{{ data.machine.display_name || `机器 #${data.machine.id}` }}</span></el-descriptions-item>
          <el-descriptions-item label="登记时间">{{ fmtTime(data.machine.created_at) }}</el-descriptions-item>
          <el-descriptions-item label="当前实例">
            <span v-if="data.current_instance" data-testid="mdetail-current"><IdLink kind="instance" :id="data.current_instance.id" /></span>
            <span v-else class="muted" data-testid="mdetail-none">无</span>
          </el-descriptions-item>
        </el-descriptions>
        <el-descriptions :column="3" border class="agent">
          <el-descriptions-item label="接入状态">
            <span data-testid="mdetail-enrolled" :data-enrolled="String(data.machine.enrolled)">{{ data.machine.enrolled ? `已接入 ${data.machine.fingerprint_prefix}…` : '未接入' }}</span>
          </el-descriptions-item>
          <el-descriptions-item label="连接"><span data-testid="mdetail-connected" :data-connected="String(data.machine.connected)">{{ data.machine.connected ? '已连接' : '未连接' }}</span></el-descriptions-item>
          <el-descriptions-item label="Agent 版本"><span data-testid="mdetail-agent-version">{{ data.machine.agent_version || '—' }}</span></el-descriptions-item>
        </el-descriptions>
        <div class="actions">
          <el-button data-testid="mdetail-edit-btn" @click="openEdit">编辑机组 / 展示名</el-button>
          <el-button data-testid="mdetail-maintenance-btn" @click="maintenanceVisible = true">{{ inMaintenance ? '结束维护' : '进入维护' }}</el-button>
          <el-button type="danger" plain data-testid="mdetail-power-btn" @click="powerVisible = true">断电重启</el-button>
          <el-button type="primary" plain :loading="busy" data-testid="mdetail-enroll-btn" @click="generateEnroll">生成接入命令</el-button>
          <el-button type="danger" plain :disabled="!data.machine.enrolled" data-testid="mdetail-revoke-btn" @click="revokeVisible = true">撤销接入</el-button>
        </div>
      </el-card>

      <!-- 计划 13：集群网络（fabric、集群网卡、就绪判定、网卡表） -->
      <MachineNetCard :machine="data.machine" :fabric="data.fabric ?? null" :can-edit="canEditNet" @saved="load()" />

      <el-card header="最近指标" data-testid="mdetail-metrics">
        <el-descriptions v-if="data.machine.last_metrics" :column="3" border>
          <el-descriptions-item label="GPU"><span data-testid="mdetail-metric-gpu">{{ Math.round(data.machine.last_metrics.gpu_util) }}% · {{ fmtTemp(data.machine.last_metrics) }} · {{ Math.round(data.machine.last_metrics.gpu_power_w) }}W</span></el-descriptions-item>
          <el-descriptions-item label="内存"><span data-testid="mdetail-metric-mem">{{ fmtBytes(data.machine.last_metrics.mem_used) }} / {{ fmtBytes(data.machine.last_metrics.mem_total) }}</span></el-descriptions-item>
          <el-descriptions-item label="根分区"><span data-testid="mdetail-metric-root">{{ fmtPercent(data.machine.last_metrics.root_disk_used, data.machine.last_metrics.root_disk_total) }}</span></el-descriptions-item>
          <el-descriptions-item label="数据盘已用"><span data-testid="mdetail-metric-data">{{ fmtBytes(data.machine.last_metrics.data_disk_used) }}</span></el-descriptions-item>
          <el-descriptions-item label="负载"><span data-testid="mdetail-metric-load">{{ data.machine.last_metrics.load1.toFixed(2) }}</span></el-descriptions-item>
          <el-descriptions-item label="上报时间"><span data-testid="mdetail-metric-at">{{ fmtTime(data.machine.last_seen_at) }}</span></el-descriptions-item>
        </el-descriptions>
        <p v-else class="muted" data-testid="mdetail-metrics-none">尚未收到指标</p>
      </el-card>

      <el-card data-testid="machine-cached-images">
        <template #header>
          <div class="head">
            <span>已缓存镜像<template v-if="data.machine.cached_images_at">（{{ cachedImages.length }}）</template></span>
            <span class="muted small" data-testid="machine-cached-images-at">{{ data.machine.cached_images_at ? `上报于 ${fmtTime(data.machine.cached_images_at)}` : '' }}</span>
          </div>
        </template>
        <p v-if="!data.machine.cached_images_at" class="muted" data-testid="machine-cached-images-none">未上报（Agent 版本过旧或尚未上报）</p>
        <template v-else>
          <ul v-if="cachedImages.length" class="images">
            <li v-for="img in cachedImages" :key="img" class="mono" data-testid="machine-cached-image">{{ img }}</li>
          </ul>
          <p v-else class="muted" data-testid="machine-cached-images-empty">无</p>
        </template>
        <p class="muted small" data-testid="machine-cached-images-note">应用版本的缓存覆盖率只统计在线机器；上报时间早于 5 分钟或从未上报的机器计为「未知」。最多显示 200 项。</p>
      </el-card>

      <el-card header="档期（未来 60 天）" data-testid="mdetail-leases">
        <table class="grid">
          <thead><tr><th>开始</th><th>结束</th><th>实例</th><th>用户</th></tr></thead>
          <tbody>
            <tr v-for="l in data.leases" :key="l.lower" data-testid="mdetail-lease-row">
              <td>{{ fmtTime(l.lower) }}</td><td>{{ fmtTime(l.upper) }}</td>
              <td><IdLink kind="instance" :id="l.instance_id" /></td><td><IdLink kind="user" :id="l.user_id" /></td>
            </tr>
            <tr v-if="!data.leases.length"><td colspan="4" class="muted">无占用</td></tr>
          </tbody>
        </table>
      </el-card>

      <el-card header="历史实例">
        <el-table :data="data.history" row-key="id">
          <el-table-column label="实例"><template #default="{ row }"><span data-testid="mdetail-history-row"><span data-testid="mdetail-history-id"><IdLink kind="instance" :id="row.id" /></span></span></template></el-table-column>
          <el-table-column label="用户" width="100"><template #default="{ row }"><IdLink kind="user" :id="row.user_id" /></template></el-table-column>
          <el-table-column label="状态" width="120"><template #default="{ row }">{{ labelOf(instanceStateLabel, row.state) }}</template></el-table-column>
          <el-table-column label="创建时间" width="160"><template #default="{ row }">{{ fmtTime(row.created_at) }}</template></el-table-column>
        </el-table>
      </el-card>

      <el-dialog v-model="enrollVisible" title="接入命令（只显示一次）" width="720" align-center :close-on-click-modal="false" data-testid="mdetail-enroll-dialog" @close="closeEnroll">
        <template v-if="enroll">
          <p class="muted">在该机器上以 root 执行下面的命令。令牌 24 小时内有效、只能用一次；关闭本窗口后不再显示，需要时重新生成。</p>
          <pre class="cmd" data-testid="mdetail-enroll-command">{{ enroll.command }}</pre>
          <p class="muted" data-testid="mdetail-enroll-expires">有效期至 {{ fmtTime(enroll.expires_at) }}</p>
        </template>
        <template #footer>
          <el-button data-testid="mdetail-enroll-copy" @click="copyEnroll">复制</el-button>
          <el-button type="primary" data-testid="mdetail-enroll-close" @click="closeEnroll">关闭</el-button>
        </template>
      </el-dialog>
      <el-dialog v-model="revokeVisible" title="撤销接入" width="420" align-center data-testid="mdetail-revoke-dialog">
        <p data-testid="mdetail-revoke-text">撤销后该机器的 Agent 证书立即作废、连接断开，机器上的实例会在 5 分钟后被判失联；重新接入需要再次生成接入命令。确定撤销？</p>
        <template #footer>
          <el-button data-testid="mdetail-revoke-cancel" @click="revokeVisible = false">取消</el-button>
          <el-button type="danger" :loading="busy" data-testid="mdetail-revoke-ok" @click="revokeEnrollment">确定撤销</el-button>
        </template>
      </el-dialog>
      <el-dialog v-model="maintenanceVisible" :title="inMaintenance ? '结束维护' : '进入维护'" width="420" align-center data-testid="mdetail-maintenance-dialog">
        <p>{{ inMaintenance ? '结束维护后机器重新进入可售库存。' : '进入维护后该机器不再分配给新订单。' }}</p>
        <p v-if="!inMaintenance && data.upcoming_bookings > 0" class="warn" data-testid="mdetail-maintenance-upcoming">该机器有 {{ data.upcoming_bookings }} 个未开通的预订，将在开通时自动改派。</p>
        <template #footer>
          <el-button @click="maintenanceVisible = false">取消</el-button>
          <el-button type="primary" :loading="busy" data-testid="mdetail-maintenance-ok" @click="toggleMaintenance">确定</el-button>
        </template>
      </el-dialog>
      <el-dialog v-model="edit.visible" title="编辑机组 / 展示名" width="460" align-center data-testid="medit-dialog">
        <el-form label-position="top" @submit.prevent="submitEdit">
          <el-form-item label="机组">
            <el-select v-model="edit.groupId" clearable placeholder="未分组" :disabled="inPool" data-testid="medit-group"><el-option v-for="g in groups" :key="g.id" :label="g.name" :value="String(g.id)" /></el-select>
            <p v-if="inPool" class="muted" data-testid="medit-pool-hint">Token 服务池机器请在 Token 商城 → 节点与机器池 中移出</p>
          </el-form-item>
          <el-form-item label="展示名（对用户，≤ 32 字；留空显示「机器 #ID」）"><el-input v-model="edit.displayName" maxlength="32" data-testid="medit-display" /></el-form-item>
          <el-button type="primary" native-type="submit" :loading="edit.busy" data-testid="medit-submit">保存</el-button>
        </el-form>
      </el-dialog>
      <el-dialog v-model="powerVisible" title="断电重启" width="420" align-center data-testid="mdetail-power-dialog">
        <p data-testid="mdetail-power-text">将通过 PDU 对该机器断电重启，机器上的实例会中断，确定执行？</p>
        <template #footer>
          <el-button data-testid="mdetail-power-cancel" @click="powerVisible = false">取消</el-button>
          <el-button type="danger" :loading="busy" data-testid="mdetail-power-ok" @click="powerCycle">确定执行</el-button>
        </template>
      </el-dialog>
    </div>
  </div>
</template>

<style scoped>
.detail { display: grid; gap: 16px; }
.pool-tag { margin-left: 6px; }
.pool-link { margin-left: 6px; font-size: 13px; }
.head { display: flex; justify-content: space-between; align-items: center; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.muted { color: #909399; }
.actions { display: flex; gap: 12px; margin-top: 16px; }
.agent { margin-top: 12px; }
.small { font-size: 12px; }
.images { margin: 0 0 8px; padding-left: 18px; display: grid; gap: 4px; font-size: 13px; word-break: break-all; }
.warn { color: var(--el-color-warning); }
.grid { width: 100%; border-collapse: collapse; font-size: 13px; }
.grid th, .grid td { padding: 6px 8px; text-align: left; border-bottom: 1px solid var(--el-border-color-lighter); }
.cmd { white-space: pre-wrap; word-break: break-all; background: #f5f7fa; padding: 12px; font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
</style>
