<script setup lang="ts">
import {
  appStatusLabel, fmtTime, formatGB, isApiError, labelOf, messageOf, workflowCategoryLabel, type AdminWorkflow, type AdminWorkflowRow, type AdminWorkflowVersionRow,
  type Listed,
} from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import WorkflowFormDialog from '../components/WorkflowFormDialog.vue'
import { notifyError } from '../logic/notify'
import { session } from '../store/session'

// 工作流详情（计划 11 规格 §12）：信息 + 版本表；编辑 / 删除 / 新建版本只有 super。
const route = useRoute()
const router = useRouter()
const wfId = Number(route.params.id)
const wf = ref<AdminWorkflow | null>(null)
/** 列表里的统计（安装数、7 日出图）；单个 GET 不带，取不到时不显示 */
const stats = ref<AdminWorkflowRow | null>(null)
const versions = ref<AdminWorkflowVersionRow[]>([])
const loading = ref(false)
const error = ref('')
const notFound = ref(false)
const editing = ref(false)
const deleting = ref(false)
const busy = ref(false)
const copyFrom = ref<number | undefined>()
const canWrite = computed(() => session.admin?.role === 'super')
const canDelete = computed(() => canWrite.value && wf.value?.status === 'draft' && versions.value.length === 0)

const statusType: Record<string, 'info' | 'success' | 'warning'> = { draft: 'info', published: 'success', retired: 'warning' }

async function load() {
  loading.value = true
  try {
    const [w, vs, list] = await Promise.all([
      api.get<AdminWorkflow>(`/workflows/${wfId}`),
      api.get<Listed<AdminWorkflowVersionRow>>(`/workflows/${wfId}/versions`),
      api.get<Listed<AdminWorkflowRow>>('/workflows').catch(() => null),
    ])
    wf.value = w
    versions.value = vs.items
    stats.value = list?.items.find((x) => x.id === wfId) ?? null
    notFound.value = false
    error.value = ''
  } catch (e) {
    if (isApiError(e) && e.code === 'workflow_not_found') {
      notFound.value = true
      return
    }
    notifyError(e)
    error.value = messageOf(e)
  } finally {
    loading.value = false
  }
}
onMounted(load)

function openVersion(v: AdminWorkflowVersionRow) {
  void router.push(`/workflows/${wfId}/versions/${v.id}`)
}
function createVersion() {
  void router.push({ path: `/workflows/${wfId}/versions/new`, query: copyFrom.value ? { copy_from: String(copyFrom.value) } : {} })
}
async function confirmDelete() {
  if (busy.value) return
  busy.value = true
  try {
    await api.del(`/workflows/${wfId}`, {})
    ElMessage.success('工作流已删除')
    deleting.value = false
    void router.push('/workflows')
  } catch (e) {
    deleting.value = false
    notifyError(e) // invalid_state：已有版本或不是草稿
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="page">
    <el-alert v-if="error" type="error" :title="error" show-icon :closable="false" data-testid="adm-wf-detail-error">
      <el-button size="small" :loading="loading" @click="load">重试</el-button>
    </el-alert>
    <el-empty v-if="notFound" description="工作流不存在" data-testid="adm-wf-detail-missing" />

    <el-card v-if="wf" v-loading="loading" data-testid="adm-wf-detail">
      <template #header>
        <div class="head">
          <span>
            <router-link to="/workflows" class="back">工作流</router-link> /
            <b data-testid="adm-wf-detail-name">{{ wf.name }}</b>
            <el-tag size="small" :type="statusType[wf.status]" class="ml" data-testid="adm-wf-detail-status">{{ labelOf(appStatusLabel, wf.status) }}</el-tag>
          </span>
          <span class="row">
            <router-link :to="`/workflows/${wfId}/installs`" data-testid="adm-wf-installs"><el-button size="small">安装列表</el-button></router-link>
            <template v-if="canWrite">
              <el-button size="small" data-testid="adm-wf-detail-edit" @click="editing = true">编辑</el-button>
              <el-button v-if="canDelete" size="small" type="danger" plain data-testid="adm-wf-delete" @click="deleting = true">删除</el-button>
            </template>
          </span>
        </div>
      </template>
      <div class="info">
        <div class="cover">
          <img v-if="wf.cover_url" :src="wf.cover_url" alt="封面" data-testid="adm-wf-detail-cover" />
          <span v-else class="muted">无封面</span>
        </div>
        <el-descriptions :column="3" border size="small" class="desc-table">
          <el-descriptions-item label="slug"><span class="mono" data-testid="adm-wf-detail-slug">{{ wf.slug }}</span></el-descriptions-item>
          <el-descriptions-item label="分类"><span data-testid="adm-wf-detail-category">{{ labelOf(workflowCategoryLabel, wf.category) }}</span></el-descriptions-item>
          <el-descriptions-item label="排序">{{ wf.sort }}</el-descriptions-item>
          <el-descriptions-item label="标签" :span="3">
            <span data-testid="adm-wf-detail-tags">
              <el-tag v-for="t in wf.tags" :key="t" size="small" class="tag">{{ t }}</el-tag>
              <span v-if="!wf.tags.length" class="muted">—</span>
            </span>
          </el-descriptions-item>
          <el-descriptions-item label="版本数">{{ versions.length }}</el-descriptions-item>
          <el-descriptions-item label="在用安装"><span data-testid="adm-wf-detail-installs">{{ stats?.installs_live ?? '—' }}</span></el-descriptions-item>
          <el-descriptions-item label="7 日快速出图"><span data-testid="adm-wf-detail-runs">{{ stats?.runs_7d ?? '—' }}</span></el-descriptions-item>
          <el-descriptions-item label="简介" :span="3"><span data-testid="adm-wf-detail-summary">{{ wf.summary || '—' }}</span></el-descriptions-item>
          <el-descriptions-item label="介绍" :span="3"><div class="desc">{{ wf.description || '—' }}</div></el-descriptions-item>
        </el-descriptions>
      </div>
    </el-card>

    <el-card v-if="wf">
      <template #header>
        <div class="head">
          <span>版本</span>
          <span v-if="canWrite" class="row">
            <el-select v-model="copyFrom" clearable placeholder="从版本复制（可选）" style="width: 200px" data-testid="adm-wf-version-copy-from">
              <el-option v-for="v in versions" :key="v.id" :label="`${v.version}（${labelOf(appStatusLabel, v.status)}）`" :value="v.id" />
            </el-select>
            <el-button type="primary" data-testid="adm-wf-version-new" @click="createVersion">新建版本</el-button>
          </span>
        </div>
      </template>
      <el-table :data="versions" v-loading="loading" row-key="id" class="clickable" data-testid="adm-wf-versions" @row-click="openVersion">
        <el-table-column label="版本" min-width="110">
          <template #default="{ row }"><span :data-testid="`adm-wf-version-row-${row.version}`" :data-id="row.id"><b data-testid="adm-wf-version-row-version">{{ row.version }}</b></span></template>
        </el-table-column>
        <el-table-column label="状态" width="90">
          <template #default="{ row }"><el-tag size="small" :type="statusType[row.status]" data-testid="adm-wf-version-row-status">{{ labelOf(appStatusLabel, row.status) }}</el-tag></template>
        </el-table-column>
        <el-table-column label="下载量" width="100"><template #default="{ row }"><span data-testid="adm-wf-version-row-download">{{ formatGB(row.download_gb) }}</span></template></el-table-column>
        <el-table-column label="模型数" width="80"><template #default="{ row }">{{ row.models }}</template></el-table-column>
        <el-table-column label="节点数" width="80"><template #default="{ row }">{{ row.nodes }}</template></el-table-column>
        <el-table-column label="快速出图" width="90">
          <template #default="{ row }"><span data-testid="adm-wf-version-row-quickrun">{{ row.quick_run ? '支持' : '—' }}</span></template>
        </el-table-column>
        <el-table-column label="发布时间" width="150"><template #default="{ row }">{{ fmtTime(row.published_at) || '—' }}</template></el-table-column>
        <el-table-column label="安装数" width="80"><template #default="{ row }"><span data-testid="adm-wf-version-row-installs">{{ row.installs }}</span></template></el-table-column>
        <el-table-column label="更新说明" min-width="180" show-overflow-tooltip><template #default="{ row }">{{ row.changelog || '—' }}</template></el-table-column>
      </el-table>
      <p class="muted">草稿可改全部内容（保存也要通过完整校验）；发布需要动态码，发布后只能改更新说明或退役。「从版本复制」会带上工作流图、API 格式与清单。</p>
    </el-card>

    <WorkflowFormDialog v-model="editing" :workflow="wf" @saved="load" />

    <el-dialog v-model="deleting" title="删除工作流" width="420" align-center data-testid="adm-wf-delete-dialog">
      <p>删除「{{ wf?.name }}」？只有没有任何版本的草稿可以删除，其余请改为「已退役」。</p>
      <template #footer>
        <el-button @click="deleting = false">取消</el-button>
        <el-button type="danger" :loading="busy" data-testid="adm-wf-delete-ok" @click="confirmDelete">确定删除</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.page { display: flex; flex-direction: column; gap: 16px; }
.head { display: flex; justify-content: space-between; align-items: center; gap: 12px; }
.row { display: flex; align-items: center; gap: 8px; }
.ml { margin-left: 8px; }
.back { color: var(--el-color-primary); text-decoration: none; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.muted { color: #909399; font-size: 13px; }
.desc { white-space: pre-wrap; }
.tag { margin-right: 4px; }
.info { display: grid; grid-template-columns: 200px minmax(0, 1fr); gap: 16px; align-items: start; }
.cover { width: 200px; height: 124px; border-radius: 4px; overflow: hidden; background: var(--el-fill-color-lighter); display: flex; align-items: center; justify-content: center; }
.cover img { width: 100%; height: 100%; object-fit: cover; }
.clickable :deep(tbody tr) { cursor: pointer; }
</style>
