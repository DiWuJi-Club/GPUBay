<script setup lang="ts">
import {
  labelOf, llmNodeStateLabel, llmNodeStateType, machineStateLabel, messageOf,
  type LLMModelAdmin, type LLMNode, type LLMNodeState, type LLMPool, type LLMPoolMachine, type Listed,
} from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, onBeforeUnmount, reactive, ref, watch } from 'vue'
import { api } from '../../api'
import { deploymentHealthLabel } from '../../logic/deployments'
import { canDrainNode, canRebuildNode, canRemoveNode, NODES_REFRESH_MS, nodeDeployText, poolLeaseText, validateNodeRemoveReason } from '../../logic/llm'
import { notifyError } from '../../logic/notify'
import { OPS_ROLES } from '../../menu'
import { hasRole } from '../../store/session'
import IdLink from '../IdLink.vue'
import AddPoolMachineDialog from './AddPoolMachineDialog.vue'
import NodeDrawer from './NodeDrawer.vue'

// 节点与机器池 Tab（规格 §11，ops / super）：机器池表（GET /llm/pool）+ 加入 / 移出机器（现有机组接口）；
// 节点表（GET /llm/nodes）+ 排空 / 重建 / 移除（移除要原因 + 动态码，动态码由请求层弹出）；Tab 可见时每 10 秒自动刷新。
const props = defineProps<{ active: boolean }>()

const canOperate = computed(() => hasRole(OPS_ROLES))

// ---- 数据 ----
const pool = ref<LLMPool | null>(null)
const nodes = ref<LLMNode[]>([])
const models = ref<LLMModelAdmin[]>([])
const filter = reactive({ model_id: '' as number | '', state: '' as LLMNodeState | '' })
const st = reactive({ loading: false, poolError: '', nodesError: '' })

let seq = 0
/**
 * 同时刷新机器池与节点表；只采用最后一次的结果。quiet（自动刷新）时失败不弹提示，只在表格上方显示错误，避免每 10 秒弹一次。
 */
async function load(quiet = false) {
  const mine = ++seq
  st.loading = true
  const query = { model_id: filter.model_id || undefined, state: filter.state || undefined }
  const [p, n] = await Promise.allSettled([api.get<LLMPool>('/llm/pool'), api.get<Listed<LLMNode>>('/llm/nodes', query)])
  if (mine !== seq) return
  if (p.status === 'fulfilled') {
    pool.value = p.value
    st.poolError = ''
  } else {
    if (!quiet) notifyError(p.reason)
    st.poolError = messageOf(p.reason)
  }
  if (n.status === 'fulfilled') {
    nodes.value = n.value.items
    st.nodesError = ''
  } else {
    if (!quiet && p.status === 'fulfilled') notifyError(n.reason) // 两个都失败只提示一次
    st.nodesError = messageOf(n.reason)
  }
  st.loading = false
}
/** 模型只用于筛选下拉：失败提示一次，表格照常显示。 */
async function loadModels() {
  try {
    models.value = (await api.get<Listed<LLMModelAdmin>>('/llm/models')).items
  } catch (e) {
    notifyError(e)
  }
}

// ---- 自动刷新：只在 Tab 可见时运行；切走 / 卸载时清掉定时器 ----
let timer: ReturnType<typeof setInterval> | null = null
function stopTimer() {
  if (timer) clearInterval(timer)
  timer = null
}
function tick() {
  if (st.loading) return // 上一轮还没回来：跳过，不叠加请求
  void load(true)
  if (drawerId.value) void drawerRef.value?.load(true)
}
watch(
  () => props.active,
  (on) => {
    stopTimer()
    if (!on) return
    void load()
    if (!models.value.length) void loadModels()
    timer = setInterval(tick, NODES_REFRESH_MS)
  },
  { immediate: true },
)
onBeforeUnmount(() => {
  stopTimer()
  seq++
})
function applyFilter() {
  void load()
}

// ---- 节点详情抽屉 ----
const drawerId = ref<string | null>(null)
const drawerRef = ref<InstanceType<typeof NodeDrawer> | null>(null)
const openNode = (id: string) => {
  drawerId.value = id
}

// ---- 机器池：加入 / 移出 ----
const addVisible = ref(false)
function onAdded() {
  void load()
}
const unassign = reactive({ visible: false, target: null as LLMPoolMachine | null, busy: false })
function openUnassign(m: LLMPoolMachine) {
  Object.assign(unassign, { visible: true, target: m, busy: false })
}
async function confirmUnassign() {
  const m = unassign.target
  if (!m || !pool.value || unassign.busy) return
  unassign.busy = true
  try {
    await api.del(`/groups/${pool.value.group_id}/machines`, { machine_ids: [m.machine.id] })
    ElMessage.success('已移出 Token 服务池')
    unassign.visible = false
    await load()
  } catch (e) {
    unassign.visible = false
    notifyError(e) // llm_pool_machine_busy（409）等服务器文案原样显示
  } finally {
    unassign.busy = false
  }
}
const machineName = (m: LLMPoolMachine['machine']) => m.display_name || `机器 #${m.id}`

// ---- 节点行操作 ----
type NodeOp = 'drain' | 'rebuild' | 'remove'
const opTitle: Record<NodeOp, string> = { drain: '排空节点', rebuild: '重建节点', remove: '移除节点' }
const opDone: Record<NodeOp, string> = { drain: '节点开始排空', rebuild: '已开始重建', remove: '节点正在移除' }
const op = reactive({ visible: false, kind: 'drain' as NodeOp, node: null as LLMNode | null, reason: '', error: '', busy: false })
function openOp(kind: NodeOp, n: LLMNode) {
  Object.assign(op, { visible: true, kind, node: n, reason: '', error: '', busy: false })
}
const opText = computed(() => {
  const n = op.node
  if (!n) return ''
  if (op.kind === 'drain') return '节点不再接新请求，在途请求完成（最多 60 秒）后释放机器；模型缺额时平台会在其它空闲机器上补建节点。'
  if (op.kind === 'rebuild') return n.state === 'failed' ? '将在同一台机器上新建一个节点（机器仍可用时）。' : '将用当前模型参数重新部署该节点，期间节点不接请求。'
  return '立即释放机器（不等在途请求），节点进入移除。需要填写原因并验证动态码。'
})
async function submitOp() {
  const n = op.node
  if (!n || op.busy) return // 同步守卫：连点只发一次
  let body: Record<string, string> = {}
  if (op.kind === 'remove') {
    op.error = validateNodeRemoveReason(op.reason) ?? ''
    if (op.error) return
    body = { reason: op.reason.trim() } // totp 由请求层在 totp_required 时补上重发
  }
  op.busy = true
  try {
    await api.post(`/llm/nodes/${encodeURIComponent(n.id)}/${op.kind}`, body)
    ElMessage.success(opDone[op.kind])
    op.visible = false
    await load()
  } catch (e) {
    notifyError(e) // llm_node_invalid_state（409）原文；动态码取消静默；弹窗保留
  } finally {
    op.busy = false
  }
}

const nodeStateTag = (s: string) => labelOf(llmNodeStateType, s) as 'primary'
const healthText = (n: LLMNode) => (n.deployment ? labelOf(deploymentHealthLabel, n.deployment.health) : '—')
</script>

<template>
  <div class="nodes" data-testid="llm-nodes">
    <!-- 机器池 -->
    <div class="bar">
      <h4>
        机器池
        <span v-if="pool" class="sub" data-testid="llm-pool-summary">空闲 <b data-testid="llm-pool-free">{{ pool.free }}</b> / 共 <b data-testid="llm-pool-total">{{ pool.total }}</b> 台</span>
      </h4>
      <span>
        <el-button :loading="st.loading" data-testid="llm-nodes-refresh" @click="load()">刷新</el-button>
        <el-button v-if="canOperate && pool" type="primary" data-testid="llm-pool-add" @click="addVisible = true">加入机器</el-button>
      </span>
    </div>
    <p class="hint">加入池的机器不再对外出租；移出前请先下线机器上的节点。自动每 10 秒刷新。</p>
    <el-alert v-if="st.poolError" type="error" :title="st.poolError" show-icon :closable="false" data-testid="llm-pool-error" />
    <table class="grid" data-testid="llm-pool-table">
      <thead><tr><th>机器展示名</th><th>序列号</th><th>状态</th><th>在线</th><th>节点</th><th>租约到期</th><th v-if="canOperate">操作</th></tr></thead>
      <tbody>
        <tr v-for="m in pool?.machines ?? []" :key="m.machine.id" data-testid="llm-pool-row" :data-id="m.machine.id">
          <td><router-link :to="`/machines/${m.machine.id}`" data-testid="llm-pool-name">{{ machineName(m.machine) }}</router-link></td>
          <td class="mono" data-testid="llm-pool-serial">{{ m.machine.serial }}</td>
          <td data-testid="llm-pool-state">{{ labelOf(machineStateLabel, m.machine.state) }}</td>
          <td data-testid="llm-pool-online"><span :class="{ bad: !m.machine.agent_online }">{{ m.machine.agent_online ? '在线' : '离线' }}</span></td>
          <td data-testid="llm-pool-node">
            <el-button v-if="m.node" link type="primary" data-testid="llm-pool-node-open" @click="openNode(m.node.id)">
              {{ m.node.model_slug }}<el-tag size="small" :type="nodeStateTag(m.node.state)" class="gap">{{ labelOf(llmNodeStateLabel, m.node.state) }}</el-tag>
            </el-button>
            <span v-else class="muted">—</span>
          </td>
          <td data-testid="llm-pool-lease">{{ poolLeaseText(m) }}</td>
          <td v-if="canOperate"><el-button size="small" data-testid="llm-pool-remove" @click="openUnassign(m)">移出</el-button></td>
        </tr>
        <tr v-if="pool && !pool.machines.length"><td :colspan="canOperate ? 7 : 6" class="muted empty" data-testid="llm-pool-empty">池里还没有机器，点「加入机器」</td></tr>
      </tbody>
    </table>

    <!-- 节点 -->
    <div class="bar">
      <h4>节点</h4>
      <el-form inline class="filters" @submit.prevent="applyFilter">
        <el-form-item label="模型">
          <el-select v-model="filter.model_id" clearable placeholder="全部" style="width: 180px" data-testid="llm-nodes-filter-model" @change="applyFilter">
            <el-option v-for="m in models" :key="m.id" :label="m.slug" :value="m.id" />
          </el-select>
        </el-form-item>
        <el-form-item label="状态">
          <el-select v-model="filter.state" clearable placeholder="全部" style="width: 120px" data-testid="llm-nodes-filter-state" @change="applyFilter">
            <el-option v-for="(label, v) in llmNodeStateLabel" :key="v" :label="label" :value="v" />
          </el-select>
        </el-form-item>
      </el-form>
    </div>
    <el-alert v-if="st.nodesError" type="error" :title="st.nodesError" show-icon :closable="false" data-testid="llm-nodes-error" />
    <table class="grid" data-testid="llm-nodes-table">
      <thead><tr><th>模型</th><th>机器</th><th>状态</th><th>健康</th><th>在途</th><th>部署进度</th><th>最近错误</th><th>重建次数</th><th>操作</th></tr></thead>
      <tbody>
        <tr v-for="n in nodes" :key="n.id" data-testid="llm-node-row" :data-id="n.id" :data-state="n.state">
          <td><el-button link type="primary" data-testid="llm-node-open" @click="openNode(n.id)">{{ n.model_slug }}</el-button></td>
          <td data-testid="llm-node-row-machine">{{ n.machine_name }}（<IdLink kind="machine" :id="n.machine_id" />）</td>
          <td><el-tag size="small" :type="nodeStateTag(n.state)" data-testid="llm-node-row-state">{{ labelOf(llmNodeStateLabel, n.state) }}</el-tag></td>
          <td data-testid="llm-node-row-health">{{ healthText(n) }}<span v-if="n.probe_fail_count > 0" class="bad">（探测失败 {{ n.probe_fail_count }}）</span></td>
          <td data-testid="llm-node-row-inflight">{{ n.inflight }}</td>
          <td data-testid="llm-node-row-progress">{{ nodeDeployText(n) }}</td>
          <td class="err" data-testid="llm-node-row-error" :title="n.error">{{ n.error || '—' }}</td>
          <td data-testid="llm-node-row-rebuilds">{{ n.rebuild_count }}</td>
          <td class="ops">
            <template v-if="canOperate">
              <el-button v-if="canDrainNode(n.state)" size="small" data-testid="llm-node-drain" @click="openOp('drain', n)">排空</el-button>
              <el-button v-if="canRebuildNode(n.state)" size="small" data-testid="llm-node-rebuild" @click="openOp('rebuild', n)">重建</el-button>
              <el-button v-if="canRemoveNode(n.state)" size="small" type="danger" plain data-testid="llm-node-remove" @click="openOp('remove', n)">移除</el-button>
            </template>
          </td>
        </tr>
        <tr v-if="!nodes.length && !st.nodesError"><td colspan="9" class="muted empty" data-testid="llm-nodes-empty">没有节点</td></tr>
      </tbody>
    </table>

    <AddPoolMachineDialog v-if="pool" v-model:visible="addVisible" :group-id="pool.group_id" @done="onAdded" />
    <NodeDrawer ref="drawerRef" :node-id="drawerId" @close="drawerId = null" />

    <el-dialog v-model="unassign.visible" title="移出 Token 服务池" width="440" align-center data-testid="llm-pool-remove-dialog">
      <p v-if="unassign.target">将把「{{ machineName(unassign.target.machine) }}」移出 Token 服务池，移出后变为未分组机器、恢复对外出租。机器上有运行中的节点时会被拒绝。</p>
      <template #footer>
        <el-button data-testid="llm-pool-remove-cancel" @click="unassign.visible = false">取消</el-button>
        <el-button type="primary" :loading="unassign.busy" data-testid="llm-pool-remove-ok" @click="confirmUnassign">确定移出</el-button>
      </template>
    </el-dialog>

    <el-dialog v-model="op.visible" :title="opTitle[op.kind]" width="460" align-center data-testid="llm-node-op-dialog">
      <p v-if="op.node">节点 <span class="mono">{{ op.node.id }}</span>（{{ op.node.model_slug }} · {{ op.node.machine_name }}）</p>
      <p class="hint" data-testid="llm-node-op-text">{{ opText }}</p>
      <el-form v-if="op.kind === 'remove'" label-position="top" @submit.prevent="submitOp">
        <el-form-item label="原因（必填，写入审计）"><el-input v-model="op.reason" maxlength="200" data-testid="llm-node-remove-reason" /></el-form-item>
      </el-form>
      <p v-if="op.error" class="error" data-testid="llm-node-op-error">{{ op.error }}</p>
      <template #footer>
        <el-button data-testid="llm-node-op-cancel" @click="op.visible = false">取消</el-button>
        <el-button :type="op.kind === 'remove' ? 'danger' : 'primary'" :loading="op.busy" data-testid="llm-node-op-ok" @click="submitOp">确定</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.nodes { display: grid; gap: 12px; }
.bar { display: flex; justify-content: space-between; align-items: center; gap: 12px; flex-wrap: wrap; }
h4 { margin: 0; font-size: 14px; }
.sub { color: #909399; font-weight: 400; margin-left: 8px; font-size: 13px; }
.hint { color: #909399; font-size: 13px; margin: 0; }
.filters :deep(.el-form-item) { margin-bottom: 0; }
.grid { width: 100%; border-collapse: collapse; font-size: 13px; }
.grid th, .grid td { padding: 8px; text-align: left; border-bottom: 1px solid var(--el-border-color-lighter); vertical-align: middle; }
.grid th { color: #909399; font-weight: 500; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.muted { color: #909399; }
.empty { text-align: center; }
.bad { color: var(--el-color-danger); }
.err { color: var(--el-color-danger); max-width: 240px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.ops { white-space: nowrap; }
.gap { margin-left: 6px; }
.error { color: var(--el-color-danger); font-size: 13px; }
</style>
