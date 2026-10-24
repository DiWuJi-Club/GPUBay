<script setup lang="ts">
import { appStatusLabel, labelOf, messageOf, workflowCategoryLabel, type AdminWorkflow, type AdminWorkflowRow, type Listed } from '@spark/shared'
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { api } from '../api'
import WorkflowFormDialog from '../components/WorkflowFormDialog.vue'
import { notifyError } from '../logic/notify'
import { session } from '../store/session'

// 工作流目录（计划 11 规格 §12）：所有角色可看；新建 / 编辑（含封面）只有 super。版本与发布在详情页维护。
const router = useRouter()
const items = ref<AdminWorkflowRow[]>([])
const loading = ref(false)
const error = ref('')
const dialog = ref(false)
const editing = ref<AdminWorkflow | null>(null)
const canWrite = computed(() => session.admin?.role === 'super')

const statusType: Record<string, 'info' | 'success' | 'warning'> = { draft: 'info', published: 'success', retired: 'warning' }

async function load() {
  loading.value = true
  try {
    items.value = (await api.get<Listed<AdminWorkflowRow>>('/workflows')).items
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

function open(row: AdminWorkflowRow) {
  void router.push(`/workflows/${row.id}`)
}
function create() {
  editing.value = null
  dialog.value = true
}
function edit(row: AdminWorkflowRow) {
  editing.value = row
  dialog.value = true
}
function onSaved(w: AdminWorkflow) {
  if (!editing.value) void router.push(`/workflows/${w.id}`)
  else void load()
}
</script>

<template>
  <el-card>
    <div class="toolbar">
      <span class="muted">版本（工作流图、模型、节点、快速出图参数）与发布在工作流详情页维护；也可用 <code>platform admin workflow import</code> 批量导入。</span>
      <el-button v-if="canWrite" type="primary" data-testid="adm-wf-new" @click="create">新建工作流</el-button>
    </div>
    <el-alert v-if="error" type="error" :title="error" show-icon :closable="false" class="error" data-testid="adm-wf-load-error">
      <el-button size="small" :loading="loading" data-testid="adm-wf-retry" @click="load">重试</el-button>
    </el-alert>
    <el-table :data="items" v-loading="loading" row-key="id" class="clickable" data-testid="adm-wf-table" @row-click="open">
      <el-table-column label="封面" width="96">
        <template #default="{ row }">
          <img v-if="row.cover_url" :src="row.cover_url" alt="" class="thumb" data-testid="adm-wf-row-cover" />
          <span v-else class="thumb empty">{{ labelOf(workflowCategoryLabel, row.category) }}</span>
        </template>
      </el-table-column>
      <el-table-column label="名称" min-width="160">
        <template #default="{ row }"><span :data-testid="`adm-wf-row-${row.slug}`" :data-id="row.id"><span data-testid="adm-wf-row-name">{{ row.name }}</span></span></template>
      </el-table-column>
      <el-table-column label="slug" min-width="150"><template #default="{ row }"><span class="mono" data-testid="adm-wf-row-slug">{{ row.slug }}</span></template></el-table-column>
      <el-table-column label="分类" width="100"><template #default="{ row }">{{ labelOf(workflowCategoryLabel, row.category) }}</template></el-table-column>
      <el-table-column label="状态" width="90">
        <template #default="{ row }"><el-tag size="small" :type="statusType[row.status]" data-testid="adm-wf-row-status">{{ labelOf(appStatusLabel, row.status) }}</el-tag></template>
      </el-table-column>
      <el-table-column label="版本数" width="80"><template #default="{ row }"><span data-testid="adm-wf-row-versions">{{ row.versions }}</span></template></el-table-column>
      <el-table-column label="安装数" width="80"><template #default="{ row }"><span data-testid="adm-wf-row-installs">{{ row.installs_live }}</span></template></el-table-column>
      <el-table-column label="7 日出图" width="90"><template #default="{ row }"><span data-testid="adm-wf-row-runs">{{ row.runs_7d }}</span></template></el-table-column>
      <el-table-column label="排序" width="70"><template #default="{ row }">{{ row.sort }}</template></el-table-column>
      <el-table-column v-if="canWrite" label="操作" width="80">
        <template #default="{ row }">
          <span :data-testid="`adm-wf-actions-${row.slug}`" @click.stop><el-button size="small" link type="primary" data-testid="adm-wf-edit" @click="edit(row as AdminWorkflowRow)">编辑</el-button></span>
        </template>
      </el-table-column>
    </el-table>

    <WorkflowFormDialog v-model="dialog" :workflow="editing" @saved="onSaved" />
  </el-card>
</template>

<style scoped>
.toolbar { margin-bottom: 12px; display: flex; justify-content: space-between; align-items: center; gap: 12px; }
.error { margin-bottom: 12px; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.muted { color: #909399; font-size: 13px; }
.thumb { width: 72px; height: 44px; object-fit: cover; border-radius: 3px; display: block; }
.thumb.empty { display: flex; align-items: center; justify-content: center; background: var(--el-fill-color-light); color: #909399; font-size: 12px; }
.clickable :deep(tbody tr) { cursor: pointer; }
</style>
