<script setup lang="ts">
import { messageOf, type AdminGroup, type GroupInput, type Listed, type Machine, type Paged } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, onMounted, reactive, ref } from 'vue'
import { api } from '../api'
import { notifyError } from '../logic/notify'

const items = ref<AdminGroup[]>([])
const loading = ref(false)
const error = ref('')
const visible = ref(false)
const editing = ref<AdminGroup | null>(null)
const busy = ref(false)
const blank = (): GroupInput => ({ name: '', region: '', description: '', sort: 0, visible: true })
const form = reactive<GroupInput>(blank())
const deleting = ref<AdminGroup | null>(null)

async function load() {
  loading.value = true
  try {
    items.value = (await api.get<Listed<AdminGroup>>('/groups')).items
    error.value = ''
  } catch (e) {
    notifyError(e)
    items.value = []
    error.value = messageOf(e)
  } finally {
    loading.value = false
  }
}
onMounted(load)

function openCreate() {
  editing.value = null
  Object.assign(form, blank())
  visible.value = true
}
function openEdit(g: AdminGroup) {
  editing.value = g
  Object.assign(form, { name: g.name, region: g.region, description: g.description, sort: g.sort, visible: g.visible })
  visible.value = true
}
async function submit() {
  if (busy.value) return
  busy.value = true
  try {
    if (editing.value) await api.put(`/groups/${editing.value.id}`, form)
    else await api.post('/groups', form)
    ElMessage.success(editing.value ? '机组已更新' : '机组已创建')
    visible.value = false
    await load()
  } catch (e) {
    notifyError(e) // duplicate_group_name / bad_request 原文；弹窗保留
  } finally {
    busy.value = false
  }
}
async function confirmDelete() {
  if (!deleting.value || busy.value) return
  busy.value = true
  try {
    await api.del(`/groups/${deleting.value.id}`, {})
    ElMessage.success('机组已删除')
    deleting.value = null
    await load()
  } catch (e) {
    deleting.value = null
    notifyError(e) // group_not_empty 原文
  } finally {
    busy.value = false
  }
}

// ---- 管理机器（抽屉）----
// 组内机器与候选机器都走服务端筛选 + 分页（GET /machines?group_id=<id|none>&page=&page_size=），机器多于一页也能看全。
const DRAWER_PAGE = 50
const drawer = reactive({
  visible: false,
  group: null as AdminGroup | null,
  members: [] as Machine[],
  memberTotal: 0,
  memberPage: 1,
  addInput: '',
  rack: '',
  searched: false,
  candidates: [] as Machine[],
  candidateTotal: 0,
  candidatePage: 1,
})
const displayDraft = reactive<Record<number, string>>({})
async function openMachines(g: AdminGroup) {
  Object.assign(drawer, { group: g, addInput: '', rack: '', searched: false, candidates: [], candidateTotal: 0, candidatePage: 1, members: [], memberTotal: 0, memberPage: 1 })
  drawer.visible = true
  await loadMembers()
}
async function loadMembers() {
  if (!drawer.group) return
  try {
    const out = await api.get<Paged<Machine>>('/machines', { group_id: drawer.group.id, page: drawer.memberPage, page_size: DRAWER_PAGE })
    drawer.members = out.items
    drawer.memberTotal = out.total
    for (const m of out.items) displayDraft[m.id] = m.display_name
  } catch (e) {
    notifyError(e)
  }
}
function memberPageChange(p: number) {
  drawer.memberPage = p
  void loadMembers()
}
/** 候选：未分组机器（group_id=none），可按机柜筛选；其它机组的机器用上面的 ID 输入加入（改派到本组）。 */
async function searchCandidates() {
  try {
    const out = await api.get<Paged<Machine>>('/machines', { group_id: 'none', rack: drawer.rack || undefined, page: drawer.candidatePage, page_size: DRAWER_PAGE })
    drawer.candidates = out.items
    drawer.candidateTotal = out.total
    drawer.searched = true
  } catch (e) {
    notifyError(e)
  }
}
function newSearch() {
  drawer.candidatePage = 1
  void searchCandidates()
}
function candidatePageChange(p: number) {
  drawer.candidatePage = p
  void searchCandidates()
}
const addIDs = computed(() => drawer.addInput.split(/[\s,，]+/).map((s) => Number(s)).filter((n) => Number.isInteger(n) && n > 0))
async function addMachines(ids: number[]) {
  if (!drawer.group || !ids.length || busy.value) return
  busy.value = true
  try {
    await api.post(`/groups/${drawer.group.id}/machines`, { machine_ids: ids })
    ElMessage.success(`已加入 ${ids.length} 台`)
    drawer.addInput = ''
    await Promise.all([loadMembers(), load()])
    if (drawer.searched) await searchCandidates()
  } catch (e) {
    notifyError(e)
  } finally {
    busy.value = false
  }
}
async function removeMachine(m: Machine) {
  if (!drawer.group || busy.value) return
  busy.value = true
  try {
    await api.del(`/groups/${drawer.group.id}/machines`, { machine_ids: [m.id] })
    // 移出后本页可能空了：退回上一页
    if (drawer.members.length === 1 && drawer.memberPage > 1) drawer.memberPage--
    await Promise.all([loadMembers(), load()])
    if (drawer.searched) await searchCandidates()
  } catch (e) {
    notifyError(e)
  } finally {
    busy.value = false
  }
}
async function saveDisplay(m: Machine) {
  try {
    await api.put(`/machines/${m.id}`, { display_name: displayDraft[m.id] ?? '' })
    ElMessage.success('展示名已保存')
    await loadMembers()
  } catch (e) {
    notifyError(e) // duplicate_display_name 原文
  }
}
</script>

<template>
  <el-card>
    <div class="toolbar"><el-button type="primary" data-testid="groups-create-btn" @click="openCreate">新建机组</el-button></div>
    <el-alert v-if="error" type="error" :title="error" show-icon :closable="false" class="error" data-testid="groups-error">
      <el-button size="small" :loading="loading" data-testid="groups-retry" @click="load">重试</el-button>
    </el-alert>
    <el-table :data="items" v-loading="loading" row-key="id">
      <el-table-column label="名称" min-width="140"><template #default="{ row }"><span data-testid="group-row"><span data-testid="group-name">{{ row.name }}</span></span></template></el-table-column>
      <el-table-column label="位置" width="120"><template #default="{ row }"><span data-testid="group-region">{{ row.region || '—' }}</span></template></el-table-column>
      <el-table-column label="机器数" width="90"><template #default="{ row }"><span data-testid="group-count">{{ row.machine_count }}</span></template></el-table-column>
      <el-table-column label="现空闲" width="90"><template #default="{ row }"><span data-testid="group-idle">{{ row.idle_now }}</span></template></el-table-column>
      <el-table-column label="未开通预订" width="110"><template #default="{ row }"><span data-testid="group-upcoming">{{ row.reserved_upcoming }}</span></template></el-table-column>
      <el-table-column label="可见" width="80"><template #default="{ row }"><el-tag :type="row.visible ? 'success' : 'info'" size="small" data-testid="group-visible">{{ row.visible ? '可见' : '仅后台' }}</el-tag></template></el-table-column>
      <el-table-column label="排序" width="70"><template #default="{ row }">{{ row.sort }}</template></el-table-column>
      <el-table-column label="操作" width="240">
        <template #default="{ row }">
          <el-button size="small" data-testid="group-machines" @click="openMachines(row as AdminGroup)">管理机器</el-button>
          <el-button size="small" data-testid="group-edit" @click="openEdit(row as AdminGroup)">编辑</el-button>
          <el-button size="small" type="danger" plain data-testid="group-delete" @click="deleting = row as AdminGroup">删除</el-button>
        </template>
      </el-table-column>
    </el-table>

    <el-dialog v-model="visible" :title="editing ? '编辑机组' : '新建机组'" width="520" align-center destroy-on-close data-testid="group-dialog">
      <el-form label-position="top" @submit.prevent="submit">
        <el-form-item label="名称（对用户展示，1～32 字）"><el-input v-model="form.name" maxlength="32" data-testid="group-form-name" /></el-form-item>
        <el-form-item label="位置标签（可选）"><el-input v-model="form.region" maxlength="32" data-testid="group-form-region" /></el-form-item>
        <el-form-item label="描述（可选）"><el-input v-model="form.description" type="textarea" maxlength="200" data-testid="group-form-description" /></el-form-item>
        <div class="two">
          <el-form-item label="排序"><el-input-number v-model="form.sort" :controls="false" data-testid="group-form-sort" /></el-form-item>
          <el-form-item label="控制台可见"><el-switch v-model="form.visible" data-testid="group-form-visible" /></el-form-item>
        </div>
        <p class="muted">不可见的机组留给运维 / 大客户：用户看不到、不能下单，其机器也不参与「不限机组」的分配。</p>
        <el-button type="primary" native-type="submit" :loading="busy" data-testid="group-form-submit">保存</el-button>
      </el-form>
    </el-dialog>

    <el-dialog :model-value="!!deleting" title="删除机组" width="420" align-center data-testid="group-delete-dialog" @update:model-value="(v: boolean) => { if (!v) deleting = null }">
      <p>删除「{{ deleting?.name }}」？组内不能有机器，也不能有未结束的实例引用它。</p>
      <template #footer>
        <el-button @click="deleting = null">取消</el-button>
        <el-button type="danger" :loading="busy" data-testid="group-delete-ok" @click="confirmDelete">确定删除</el-button>
      </template>
    </el-dialog>

    <el-drawer v-model="drawer.visible" :title="`管理机器 · ${drawer.group?.name ?? ''}`" size="720px" data-testid="group-machines-drawer">
      <h4>组内机器（<span data-testid="gm-total">{{ drawer.memberTotal }}</span>）</h4>
      <table class="grid">
        <thead><tr><th>ID</th><th>序列号</th><th>展示名（对用户）</th><th>状态</th><th></th></tr></thead>
        <tbody>
          <tr v-for="m in drawer.members" :key="m.id" data-testid="gm-row" :data-id="m.id">
            <td>{{ m.id }}</td>
            <td class="mono">{{ m.serial }}</td>
            <td class="row">
              <el-input v-model="displayDraft[m.id]" size="small" maxlength="32" placeholder="机器 #id" style="width: 140px" data-testid="gm-display-input" />
              <el-button size="small" data-testid="gm-display-save" @click="saveDisplay(m)">保存</el-button>
            </td>
            <td>{{ m.state }}</td>
            <td><el-button size="small" type="danger" plain data-testid="gm-remove" @click="removeMachine(m)">移出</el-button></td>
          </tr>
          <tr v-if="!drawer.members.length"><td colspan="5" class="muted">暂无机器</td></tr>
        </tbody>
      </table>
      <el-pagination
        v-if="drawer.memberTotal > DRAWER_PAGE"
        :current-page="drawer.memberPage"
        :page-size="DRAWER_PAGE"
        :total="drawer.memberTotal"
        layout="prev, pager, next"
        size="small"
        data-testid="gm-pager"
        @current-change="memberPageChange"
      />
      <h4>加入机器</h4>
      <div class="row">
        <el-input v-model="drawer.addInput" placeholder="机器 ID，逗号或空格分隔" data-testid="gm-add-input" style="width: 300px" />
        <el-button type="primary" :disabled="!addIDs.length" :loading="busy" data-testid="gm-add" @click="addMachines(addIDs)">加入</el-button>
      </div>
      <div class="row">
        <el-input v-model="drawer.rack" placeholder="按机柜筛选" data-testid="gm-rack" style="width: 160px" />
        <el-button data-testid="gm-search" @click="newSearch">查找未分组机器</el-button>
      </div>
      <template v-if="drawer.searched">
        <table class="grid">
          <thead><tr><th>ID</th><th>序列号</th><th>机柜</th><th>状态</th><th></th></tr></thead>
          <tbody>
            <tr v-for="m in drawer.candidates" :key="m.id" data-testid="gm-candidate">
              <td>{{ m.id }}</td><td class="mono">{{ m.serial }}</td><td>{{ m.rack }}</td><td>{{ m.state }}</td>
              <td><el-button size="small" data-testid="gm-candidate-add" @click="addMachines([m.id])">加入</el-button></td>
            </tr>
            <tr v-if="!drawer.candidates.length"><td colspan="5" class="muted">没有未分组的机器</td></tr>
          </tbody>
        </table>
        <el-pagination
          v-if="drawer.candidateTotal > DRAWER_PAGE"
          :current-page="drawer.candidatePage"
          :page-size="DRAWER_PAGE"
          :total="drawer.candidateTotal"
          layout="prev, pager, next"
          size="small"
          data-testid="gm-candidate-pager"
          @current-change="candidatePageChange"
        />
      </template>
    </el-drawer>
  </el-card>
</template>

<style scoped>
.toolbar { margin-bottom: 12px; display: flex; justify-content: flex-end; }
.error { margin-bottom: 12px; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.muted { color: #909399; font-size: 13px; }
.two { display: grid; grid-template-columns: 1fr 1fr; gap: 12px; }
.row { display: flex; align-items: center; gap: 8px; margin: 8px 0; }
.grid { width: 100%; border-collapse: collapse; font-size: 13px; }
.grid th, .grid td { padding: 6px 8px; text-align: left; border-bottom: 1px solid var(--el-border-color-lighter); }
</style>
