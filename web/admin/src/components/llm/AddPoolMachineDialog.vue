<script setup lang="ts">
import { labelOf, machineStateLabel, type AdminGroup, type Listed, type Paged } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, reactive, ref, watch } from 'vue'
import { api } from '../../api'
import type { NetMachine } from '../../logic/clusters'
import { POOL_CANDIDATE_PAGE, poolCandidateQuery, type PoolCandidateSource } from '../../logic/llm'
import { notifyError } from '../../logic/notify'

// 「加入机器」（规格 §3.3、§11）：列出不在池里的机器——未分组（GET /machines?group_id=none）或某个出租机组的机器，
// 可按机柜筛选、服务端分页；勾选后调用现有 POST /groups/{池}/machines {machine_ids}（审计 group.assign）。
// 从出租机组加入会把机器移出原机组；机器上的现有租约到期后才会建节点。
const props = defineProps<{ visible: boolean; groupId: number }>()
const emit = defineEmits<{ 'update:visible': [boolean]; done: [count: number] }>()

const groups = ref<AdminGroup[]>([])
const st = reactive({
  source: 'none' as PoolCandidateSource,
  rack: '',
  page: 1,
  items: [] as NetMachine[],
  total: 0,
  loading: false,
  busy: false,
})
const selected = ref<number[]>([])
/** 候选来源：其它（非池）机组 */
const sourceGroups = computed(() => groups.value.filter((g) => g.id !== props.groupId && g.purpose !== 'llm'))

let seq = 0
async function search() {
  const mine = ++seq
  st.loading = true
  try {
    const out = await api.get<Paged<NetMachine>>('/machines', poolCandidateQuery(st.source, st.rack, st.page))
    if (mine !== seq) return
    st.items = out.items.filter((m) => m.group_id !== props.groupId) // 防御：池内机器不作为候选
    st.total = out.total
  } catch (e) {
    if (mine !== seq) return
    notifyError(e)
  } finally {
    if (mine === seq) st.loading = false
  }
}
function newSearch() {
  st.page = 1
  void search()
}
function pageChange(p: number) {
  st.page = p
  void search()
}
async function loadGroups() {
  try {
    groups.value = (await api.get<Listed<AdminGroup>>('/groups')).items
  } catch (e) {
    notifyError(e)
  }
}

watch(
  () => props.visible,
  (v) => {
    if (!v) {
      seq++
      return
    }
    Object.assign(st, { source: 'none', rack: '', page: 1, items: [], total: 0 })
    selected.value = []
    void loadGroups()
    void search()
  },
  { immediate: true },
)

function toggle(id: number, on: boolean) {
  selected.value = on ? [...new Set([...selected.value, id])] : selected.value.filter((x) => x !== id)
}
const close = () => emit('update:visible', false)

async function submit() {
  if (st.busy || !selected.value.length) return // 同步守卫：连点只发一次
  st.busy = true
  const ids = [...selected.value]
  try {
    await api.post(`/groups/${props.groupId}/machines`, { machine_ids: ids })
    ElMessage.success(`已加入 ${ids.length} 台机器`)
    emit('done', ids.length)
    close()
  } catch (e) {
    notifyError(e) // 服务器文案原样显示；对话框与勾选保留
  } finally {
    st.busy = false
  }
}
</script>

<template>
  <el-dialog :model-value="props.visible" title="加入机器到 Token 服务池" width="760" align-center data-testid="llm-pool-add-dialog" @close="close">
    <p class="hint">加入后机器不再对外出租；机器上的现有租约到期后才会建节点。从其它机组加入会把机器移出原机组。多机副本（每副本 2 / 4 台）只用同一集群网络、集群就绪的机器组队。</p>
    <el-form inline class="filters" @submit.prevent="newSearch">
      <el-form-item label="来源">
        <el-select v-model="st.source" style="width: 180px" data-testid="llm-pool-add-source" @change="newSearch">
          <el-option label="未分组" value="none" />
          <el-option v-for="g in sourceGroups" :key="g.id" :label="`机组：${g.name}`" :value="g.id" />
        </el-select>
      </el-form-item>
      <el-form-item label="机柜"><el-input v-model="st.rack" clearable placeholder="全部" style="width: 120px" data-testid="llm-pool-add-rack" /></el-form-item>
      <el-form-item><el-button native-type="submit" :loading="st.loading" data-testid="llm-pool-add-search">查询</el-button></el-form-item>
    </el-form>
    <table class="grid" data-testid="llm-pool-add-table">
      <thead><tr><th></th><th>机器</th><th>序列号</th><th>机柜 / 槽位</th><th>状态</th><th>在线</th><th>集群网络</th></tr></thead>
      <tbody>
        <tr v-for="m in st.items" :key="m.id" data-testid="llm-pool-add-row" :data-id="m.id">
          <td><el-checkbox :model-value="selected.includes(m.id)" data-testid="llm-pool-add-check" @update:model-value="(v: unknown) => toggle(m.id, !!v)" /></td>
          <td>{{ m.display_name || `机器 #${m.id}` }}</td>
          <td class="mono">{{ m.serial }}</td>
          <td>{{ m.rack }} / {{ m.slot }}</td>
          <td>{{ labelOf(machineStateLabel, m.state) }}</td>
          <td>{{ m.agent_online ? '在线' : '离线' }}</td>
          <td data-testid="llm-pool-add-fabric">
            <template v-if="m.fabric_id">{{ m.fabric_name || `#${m.fabric_id}` }}<span :class="m.cluster_ready ? 'ok' : 'warn'" :title="m.cluster_reason || ''">（{{ m.cluster_ready ? (m.cluster_rdma ? '就绪 · RDMA' : '就绪') : '未就绪' }}）</span></template>
            <span v-else class="muted">—</span>
          </td>
        </tr>
        <tr v-if="!st.items.length && !st.loading"><td colspan="7" class="muted empty" data-testid="llm-pool-add-empty">没有可加入的机器</td></tr>
      </tbody>
    </table>
    <el-pagination
      v-if="st.total > POOL_CANDIDATE_PAGE" class="pager" :current-page="st.page" :page-size="POOL_CANDIDATE_PAGE" :total="st.total" layout="prev, pager, next"
      data-testid="llm-pool-add-pager" @current-change="pageChange"
    />
    <template #footer>
      <span class="muted">已选 <b data-testid="llm-pool-add-count">{{ selected.length }}</b> 台</span>
      <el-button data-testid="llm-pool-add-cancel" @click="close">取消</el-button>
      <el-button type="primary" :disabled="!selected.length" :loading="st.busy" data-testid="llm-pool-add-submit" @click="submit">加入</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.hint { color: #909399; font-size: 13px; margin: 0 0 12px; }
.filters { margin-bottom: 4px; }
.grid { width: 100%; border-collapse: collapse; font-size: 13px; }
.grid th, .grid td { padding: 6px 8px; text-align: left; border-bottom: 1px solid var(--el-border-color-lighter); }
.grid th { color: #909399; font-weight: 500; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.muted { color: #909399; font-size: 13px; margin-right: 12px; }
.empty { text-align: center; }
.ok { color: var(--el-color-success); }
.warn { color: var(--el-color-warning); }
.pager { margin-top: 8px; justify-content: flex-end; }
</style>
