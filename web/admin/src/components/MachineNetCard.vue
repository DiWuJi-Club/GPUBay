<script setup lang="ts">
import { fabricIsolationLabel, fabricKindLabel, fmtTime, labelOf, type Fabric, type FabricRow, type Listed } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, reactive, ref } from 'vue'
import { api } from '../api'
import { agentFeatureLabels, fmtSpeed, ifaceChoices, machineNetPatch, mgmtIface, rdmaText, readinessText, supportsCluster, type NetMachine } from '../logic/clusters'
import { notifyError } from '../logic/notify'

/**
 * 机器详情的「集群网络」卡（计划 13 规格 §3.2、§11）：所属 fabric、集群网卡、就绪 / RDMA 判定与未就绪原因、Agent 能力位、
 * 探测到的网卡表；ops 可改 fabric 与集群网卡（PUT /machines/{id}，只带变化的键；有集群成员时 409 invalid_state、直连满 2 台 409 fabric_full）。
 */
const props = defineProps<{ machine: NetMachine; fabric: Fabric | null; canEdit: boolean }>()
const emit = defineEmits<{ saved: [] }>()

const m = computed(() => props.machine)
const mgmt = computed(() => mgmtIface(m.value.net_info, m.value.lan_ip))
const features = computed(() => agentFeatureLabels(m.value.agent_features ?? 0))
/** 当前实际使用的集群网卡说明 */
const ifaceText = computed(() => {
  if (m.value.cluster_iface) return m.value.cluster_iface
  if (props.fabric?.kind === 'cx7') return '自动（第一块已连接的 CX-7 网卡）'
  return mgmt.value ? `自动（管理网卡 ${mgmt.value}）` : '自动（管理网卡）'
})

// ---- 修改 ----
const fabrics = ref<FabricRow[]>([])
const edit = reactive({ visible: false, loading: false, fabricId: null as number | null, iface: '', busy: false })
const editKind = computed(() => fabrics.value.find((f) => f.id === edit.fabricId)?.kind ?? (edit.fabricId === props.fabric?.id ? props.fabric?.kind : undefined))
const choices = computed(() => ifaceChoices(m.value.net_info, editKind.value, m.value.lan_ip, edit.iface))
async function openEdit() {
  Object.assign(edit, { visible: true, fabricId: m.value.fabric_id ?? null, iface: m.value.cluster_iface ?? '', busy: false, loading: true })
  try {
    fabrics.value = (await api.get<Listed<FabricRow>>('/fabrics')).items
  } catch (e) {
    notifyError(e)
  } finally {
    edit.loading = false
  }
}
function setFabric(v: number | null | undefined) {
  edit.fabricId = v ?? null
  // 换了网络类型，原来选的网卡可能不可用（cx7 不能用管理网卡）：退回自动
  if (choices.value.find((c) => c.value === edit.iface)?.disabled) edit.iface = ''
}
async function save() {
  if (edit.busy) return
  const patch = machineNetPatch(m.value, edit.fabricId, edit.iface)
  if (!Object.keys(patch).length) {
    edit.visible = false
    return void ElMessage.info('没有改动')
  }
  edit.busy = true
  try {
    await api.put(`/machines/${m.value.id}`, patch)
    ElMessage.success('已保存，就绪判定按新设置重新计算')
    edit.visible = false
    emit('saved')
  } catch (e) {
    notifyError(e) // invalid_state（机器上有集群成员）/ fabric_full（直连最多 2 台）/ fabric_not_found 原文；弹窗保留
  } finally {
    edit.busy = false
  }
}
</script>

<template>
  <el-card data-testid="machine-net-card">
    <template #header>
      <div class="head">
        <span>集群网络</span>
        <el-button v-if="canEdit" size="small" data-testid="machine-net-edit" @click="openEdit">修改集群网络 / 网卡</el-button>
      </div>
    </template>
    <el-descriptions :column="3" border>
      <el-descriptions-item label="所属集群网络">
        <span data-testid="machine-fabric">
          <template v-if="fabric">
            <router-link :to="{ path: '/fabrics' }">{{ fabric.name }}</router-link>（{{ labelOf(fabricKindLabel, fabric.kind) }} · {{ labelOf(fabricIsolationLabel, fabric.isolation) }}{{ fabric.rdma ? ' · RDMA' : '' }}）
          </template>
          <template v-else-if="m.fabric_id">{{ m.fabric_name || `#${m.fabric_id}` }}</template>
          <span v-else class="muted">未加入</span>
        </span>
      </el-descriptions-item>
      <el-descriptions-item label="集群网卡"><span class="mono" data-testid="machine-cluster-iface">{{ ifaceText }}</span></el-descriptions-item>
      <el-descriptions-item label="就绪判定">
        <span :class="m.cluster_ready ? 'ok' : 'bad'" data-testid="machine-cluster-ready" :data-ready="String(!!m.cluster_ready)" :data-rdma="String(!!m.cluster_rdma)">{{ readinessText(m) }}</span>
      </el-descriptions-item>
      <el-descriptions-item label="Agent 能力">
        <span data-testid="machine-agent-features">{{ features.length ? features.join('、') : '无' }}{{ supportsCluster(m.agent_features) ? '' : '（不支持多机集群，请升级 Agent）' }}</span>
      </el-descriptions-item>
      <el-descriptions-item label="网卡探测时间"><span data-testid="machine-net-at">{{ fmtTime(m.net_info_at) || '从未上报' }}</span></el-descriptions-item>
      <el-descriptions-item label="管理网卡"><span class="mono">{{ mgmt || '—' }}</span></el-descriptions-item>
    </el-descriptions>

    <p v-if="!m.net_info?.ifaces?.length" class="muted" data-testid="machine-net-none">尚未上报网卡（Agent 版本过旧或尚未连接）</p>
    <table v-else class="grid" data-testid="machine-net-table">
      <thead><tr><th>名称</th><th>状态</th><th>速率</th><th>MTU</th><th>IPv4</th><th>驱动</th><th>无线</th><th>RDMA（设备 · 状态 · uverbs）</th></tr></thead>
      <tbody>
        <tr v-for="f in m.net_info.ifaces" :key="f.name" data-testid="machine-net-iface" :data-name="f.name">
          <td class="mono">
            {{ f.name }}
            <el-tag v-if="f.name === mgmt" size="small" class="gap">管理网卡</el-tag>
            <el-tag v-if="m.cluster_iface === f.name" size="small" type="success" class="gap">集群网卡</el-tag>
          </td>
          <td :class="{ bad: !f.up }">{{ f.up ? '已连接' : '未连接' }}</td>
          <td>{{ fmtSpeed(f.speed_mbps) }}</td>
          <td>{{ f.mtu }}</td>
          <td class="mono">{{ f.ipv4?.length ? f.ipv4.join('、') : '—' }}</td>
          <td class="mono">{{ f.driver || '—' }}</td>
          <td>{{ f.wireless ? '是' : '' }}</td>
          <td class="mono">{{ rdmaText(f) }}</td>
        </tr>
      </tbody>
    </table>

    <el-dialog v-model="edit.visible" title="修改集群网络 / 集群网卡" width="520" align-center data-testid="machine-net-dialog">
      <el-form v-loading="edit.loading" label-position="top" @submit.prevent="save">
        <el-form-item label="集群网络">
          <el-select :model-value="edit.fabricId" clearable placeholder="不加入" data-testid="machine-fabric-select" @update:model-value="setFabric">
            <el-option v-for="f in fabrics" :key="f.id" :value="f.id" :label="`${f.name}（${labelOf(fabricKindLabel, f.kind)} · ${labelOf(fabricIsolationLabel, f.isolation)}）`" data-testid="machine-fabric-option" />
          </el-select>
          <div class="help">机器上有集群成员时不能修改；直连网络最多 2 台机器。</div>
        </el-form-item>
        <el-form-item label="集群网卡">
          <el-select v-model="edit.iface" :disabled="edit.fabricId == null" data-testid="machine-iface-select">
            <el-option v-for="c in choices" :key="c.value || '-auto'" :value="c.value" :label="c.label" :disabled="c.disabled" data-testid="machine-iface-option" />
          </el-select>
          <div class="help">普通以太网默认用管理网卡；ConnectX-7 默认用第一块已连接的 CX-7 网卡（不能是管理网卡或无线网卡）。</div>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button data-testid="machine-net-cancel" @click="edit.visible = false">取消</el-button>
        <el-button type="primary" :loading="edit.busy" data-testid="machine-net-save" @click="save">保存</el-button>
      </template>
    </el-dialog>
  </el-card>
</template>

<style scoped>
.head { display: flex; justify-content: space-between; align-items: center; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.muted { color: #909399; font-size: 13px; }
.ok { color: var(--el-color-success); }
.bad { color: var(--el-color-danger); }
.gap { margin-left: 6px; }
.help { color: #909399; font-size: 12px; line-height: 1.5; width: 100%; }
.grid { width: 100%; border-collapse: collapse; font-size: 13px; margin-top: 12px; }
.grid th, .grid td { padding: 6px 8px; text-align: left; border-bottom: 1px solid var(--el-border-color-lighter); }
.grid th { color: #909399; font-weight: 500; }
.el-select { width: 100%; }
</style>
