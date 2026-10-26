<script setup lang="ts">
import { fabricIsolationLabel, fabricKindLabel, labelOf, messageOf, type FabricRow, type Listed } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, onMounted, reactive, ref } from 'vue'
import { api } from '../api'
import FabricDrawer from '../components/FabricDrawer.vue'
import { canEditFabrics, fabricDeleteBlocker, fabricNetsWarning, mtuText, vlanRangeText } from '../logic/clusters'
import { notifyError } from '../logic/notify'
import { session } from '../store/session'

// 集群网络（计划 13 规格 §3.1、§11）：全员可看；新建 / 编辑 / 删除 ops 与 super（接口层同样按角色 403）。
// 机器加入 fabric 在机器详情的「集群网络」卡里操作。
const canEdit = computed(() => canEditFabrics(session.admin?.role))
const rows = ref<FabricRow[]>([])
const st = reactive({ loading: false, error: '' })

let seq = 0
async function load() {
  const mine = ++seq
  st.loading = true
  try {
    const out = await api.get<Listed<FabricRow>>('/fabrics')
    if (mine !== seq) return
    rows.value = out.items
    st.error = ''
  } catch (e) {
    if (mine !== seq) return
    notifyError(e)
    st.error = messageOf(e)
  } finally {
    if (mine === seq) st.loading = false
  }
}
onMounted(() => void load())

// ---- 新建 / 编辑 ----
const drawer = reactive({ open: false, fabric: null as FabricRow | null })
function openCreate() {
  Object.assign(drawer, { open: true, fabric: null })
}
function openEdit(f: FabricRow) {
  Object.assign(drawer, { open: true, fabric: f })
}
function onSaved() {
  drawer.open = false
  void load()
}

// ---- 删除 ----
const removing = reactive({ visible: false, target: null as FabricRow | null, busy: false })
function openDelete(f: FabricRow) {
  Object.assign(removing, { visible: true, target: f, busy: false })
}
async function confirmDelete() {
  const f = removing.target
  if (!f || removing.busy) return
  removing.busy = true
  try {
    await api.del(`/fabrics/${f.id}`)
    ElMessage.success('已删除')
    removing.visible = false
    await load()
  } catch (e) {
    notifyError(e) // fabric_in_use（409）原文
  } finally {
    removing.busy = false
  }
}
</script>

<template>
  <el-card>
    <div class="bar">
      <p class="hint">集群网络是一组物理互通的机器（同一交换机 / 网段，或 CX-7 直连）。多机集群只在同一集群网络内组队；机器在机器详情里加入。</p>
      <span>
        <el-button :loading="st.loading" data-testid="fabrics-refresh" @click="load">刷新</el-button>
        <el-button v-if="canEdit" type="primary" data-testid="fabric-create" @click="openCreate">新建集群网络</el-button>
      </span>
    </div>
    <el-alert v-if="st.error" type="error" :title="st.error" show-icon :closable="false" class="mb" data-testid="fabrics-error">
      <el-button size="small" :loading="st.loading" data-testid="fabrics-retry" @click="load">重试</el-button>
    </el-alert>
    <table class="grid" data-testid="fabrics-table">
      <thead>
        <tr><th>ID</th><th>名称</th><th>类型</th><th>隔离</th><th>RDMA</th><th>MTU</th><th>VLAN 范围</th><th>机器数</th><th>活跃集群</th><th>集群网段</th><th>说明</th><th v-if="canEdit">操作</th></tr>
      </thead>
      <tbody>
        <tr v-for="f in rows" :key="f.id" data-testid="fabric-row" :data-id="f.id" :data-kind="f.kind">
          <td>{{ f.id }}</td>
          <td data-testid="fabric-name">{{ f.name }}</td>
          <td data-testid="fabric-kind">{{ labelOf(fabricKindLabel, f.kind) }}</td>
          <td data-testid="fabric-isolation">{{ labelOf(fabricIsolationLabel, f.isolation) }}</td>
          <td data-testid="fabric-rdma">{{ f.rdma ? '是' : '否' }}</td>
          <td data-testid="fabric-mtu">{{ mtuText(f.mtu) }}</td>
          <td data-testid="fabric-vlan">{{ vlanRangeText(f) }}</td>
          <td><router-link :to="{ path: '/machines', query: { fabric_id: f.id } }" data-testid="fabric-machines">{{ f.machine_count }}</router-link></td>
          <td><router-link :to="{ path: '/clusters', query: { fabric_id: f.id } }" data-testid="fabric-clusters">{{ f.active_clusters }}</router-link></td>
          <td data-testid="fabric-nets">
            <span class="mono">{{ f.cluster_nets?.length ? f.cluster_nets.map((n) => (n === 'l2' ? '二层' : n)).join('、') : '—' }}</span>
            <div v-if="fabricNetsWarning(f)" class="warn" data-testid="fabric-nets-warning">{{ fabricNetsWarning(f) }}</div>
          </td>
          <td class="desc">{{ f.description || '—' }}</td>
          <td v-if="canEdit" class="ops">
            <el-button size="small" data-testid="fabric-edit" @click="openEdit(f)">编辑</el-button>
            <el-button size="small" type="danger" plain data-testid="fabric-delete" @click="openDelete(f)">删除</el-button>
          </td>
        </tr>
        <tr v-if="!rows.length && !st.loading && !st.error"><td :colspan="canEdit ? 12 : 11" class="muted empty" data-testid="fabrics-empty">还没有集群网络</td></tr>
      </tbody>
    </table>

    <FabricDrawer :open="drawer.open" :fabric="drawer.fabric" @close="drawer.open = false" @saved="onSaved" />

    <el-dialog v-model="removing.visible" title="删除集群网络" width="440" align-center data-testid="fabric-delete-dialog">
      <template v-if="removing.target">
        <p>确定删除「{{ removing.target.name }}」？</p>
        <p v-if="fabricDeleteBlocker(removing.target)" class="warn" data-testid="fabric-delete-blocker">{{ fabricDeleteBlocker(removing.target) }}，删除会被拒绝。</p>
      </template>
      <template #footer>
        <el-button data-testid="fabric-delete-cancel" @click="removing.visible = false">取消</el-button>
        <el-button type="danger" :loading="removing.busy" data-testid="fabric-delete-ok" @click="confirmDelete">确定删除</el-button>
      </template>
    </el-dialog>
  </el-card>
</template>

<style scoped>
.bar { display: flex; justify-content: space-between; align-items: center; gap: 12px; margin-bottom: 12px; }
.hint { color: #909399; font-size: 13px; margin: 0; }
.mb { margin-bottom: 12px; }
.grid { width: 100%; border-collapse: collapse; font-size: 13px; }
.grid th, .grid td { padding: 8px; text-align: left; border-bottom: 1px solid var(--el-border-color-lighter); vertical-align: middle; }
.grid th { color: #909399; font-weight: 500; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.muted { color: #909399; }
.empty { text-align: center; }
.warn { color: var(--el-color-warning); font-size: 12px; }
.desc { max-width: 200px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.ops { white-space: nowrap; }
</style>
