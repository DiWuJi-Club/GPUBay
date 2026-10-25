<script setup lang="ts">
import { appCategoryLabel, appStatusLabel, fmtTime, labelOf, messageOf, type AdminApp, type AdminAppVersion, type AdminImage, type Listed } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import AppFormDialog from '../components/AppFormDialog.vue'
import { coverageLow, coverageText } from '../logic/appSpec'
import { notifyError } from '../logic/notify'
import { session } from '../store/session'

// 应用详情（规格 §12）：应用信息 + 版本表（覆盖率）；编辑 / 删除 / 新建版本只有 super。
const route = useRoute()
const router = useRouter()
const appId = Number(route.params.id)
const app = ref<AdminApp | null>(null)
const versions = ref<AdminAppVersion[]>([])
const images = ref<AdminImage[]>([])
const loading = ref(false)
const error = ref('')
const notFound = ref(false)
const editing = ref(false)
const deleting = ref(false)
const busy = ref(false)
const copyFrom = ref<number | undefined>()
const canWrite = computed(() => session.admin?.role === 'super')
const canDelete = computed(() => canWrite.value && app.value?.status === 'draft' && versions.value.length === 0)
const imageName = computed(() => new Map(images.value.map((i) => [i.id, i.name])))

const statusType: Record<string, 'info' | 'success' | 'warning'> = { draft: 'info', published: 'success', retired: 'warning' }

async function load() {
  loading.value = true
  try {
    // 后台没有单个应用的 GET：从列表里取（应用数量很少）
    const [apps, vs, imgs] = await Promise.all([
      api.get<Listed<AdminApp>>('/apps'),
      api.get<Listed<AdminAppVersion>>(`/apps/${appId}/versions`),
      api.get<Listed<AdminImage>>('/images'),
    ])
    app.value = apps.items.find((a) => a.id === appId) ?? null
    notFound.value = !app.value
    versions.value = vs.items
    images.value = imgs.items
    error.value = ''
  } catch (e) {
    notifyError(e)
    error.value = messageOf(e)
  } finally {
    loading.value = false
  }
}
onMounted(load)

function openVersion(v: AdminAppVersion) {
  void router.push(`/apps/${appId}/versions/${v.id}`)
}
function createVersion() {
  void router.push({ path: `/apps/${appId}/versions/new`, query: copyFrom.value ? { copy_from: String(copyFrom.value) } : {} })
}
async function confirmDelete() {
  if (busy.value) return
  busy.value = true
  try {
    await api.del(`/apps/${appId}`, {})
    ElMessage.success('应用已删除')
    deleting.value = false
    void router.push('/apps')
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
    <el-alert v-if="error" type="error" :title="error" show-icon :closable="false" data-testid="app-detail-error">
      <el-button size="small" :loading="loading" @click="load">重试</el-button>
    </el-alert>
    <el-empty v-if="notFound" description="应用不存在" data-testid="app-detail-missing" />

    <el-card v-if="app" v-loading="loading" data-testid="app-detail-card">
      <template #header>
        <div class="head">
          <span>
            <router-link to="/apps" class="back">应用</router-link> /
            <b data-testid="app-detail-name">{{ app.name }}</b>
            <el-tag size="small" :type="statusType[app.status]" class="ml" data-testid="app-detail-status">{{ labelOf(appStatusLabel, app.status) }}</el-tag>
            <el-tag v-if="app.internal" size="small" type="info" class="ml" data-testid="app-detail-internal">内部</el-tag>
          </span>
          <span v-if="canWrite">
            <el-button size="small" data-testid="app-detail-edit" @click="editing = true">编辑</el-button>
            <el-button v-if="canDelete" size="small" type="danger" plain data-testid="app-detail-delete" @click="deleting = true">删除</el-button>
          </span>
        </div>
      </template>
      <el-descriptions :column="3" border size="small">
        <el-descriptions-item label="slug"><span class="mono" data-testid="app-detail-slug">{{ app.slug }}</span></el-descriptions-item>
        <el-descriptions-item label="分类">{{ labelOf(appCategoryLabel, app.category) }}</el-descriptions-item>
        <el-descriptions-item label="图标">{{ app.icon }}</el-descriptions-item>
        <el-descriptions-item label="排序">{{ app.sort }}</el-descriptions-item>
        <el-descriptions-item label="版本数">{{ app.versions }}</el-descriptions-item>
        <el-descriptions-item label="活跃部署"><span data-testid="app-detail-live">{{ app.deployments_live }}</span></el-descriptions-item>
        <el-descriptions-item label="简介" :span="3">{{ app.summary || '—' }}</el-descriptions-item>
        <el-descriptions-item label="介绍" :span="3"><div class="desc">{{ app.description || '—' }}</div></el-descriptions-item>
      </el-descriptions>
    </el-card>

    <el-card v-if="app">
      <template #header>
        <div class="head">
          <span>版本</span>
          <span v-if="canWrite" class="row">
            <el-select v-model="copyFrom" clearable placeholder="从版本复制（可选）" style="width: 200px" data-testid="app-version-copy-from">
              <el-option v-for="v in versions" :key="v.id" :label="`${v.version}（${labelOf(appStatusLabel, v.status)}）`" :value="v.id" />
            </el-select>
            <el-button type="primary" data-testid="app-version-create" @click="createVersion">新建版本</el-button>
          </span>
        </div>
      </template>
      <el-table :data="versions" v-loading="loading" row-key="id" class="clickable" data-testid="app-versions-table" @row-click="openVersion">
        <el-table-column label="版本" min-width="110">
          <template #default="{ row }"><span data-testid="app-version-row" :data-id="row.id"><b data-testid="app-version-row-version">{{ row.version }}</b></span></template>
        </el-table-column>
        <el-table-column label="状态" width="100">
          <template #default="{ row }"><el-tag size="small" :type="statusType[row.status]" data-testid="app-version-row-status">{{ labelOf(appStatusLabel, row.status) }}</el-tag></template>
        </el-table-column>
        <el-table-column label="兼容镜像" min-width="160">
          <template #default="{ row }">
            <div v-for="(id, i) in (row as AdminAppVersion).image_ids" :key="id">{{ imageName.get(id) ?? `#${id}` }}<span v-if="i === 0" class="muted">（默认）</span></div>
          </template>
        </el-table-column>
        <el-table-column label="覆盖率" min-width="230">
          <template #default="{ row }">
            <div v-for="c in (row as AdminAppVersion).coverage" :key="c.image_id" data-testid="app-version-coverage" :class="{ low: coverageLow(c) }">
              <span v-if="(row as AdminAppVersion).image_ids.length > 1" class="muted">{{ c.image_name }}：</span>{{ coverageText(c) }}
            </div>
            <span v-if="!(row as AdminAppVersion).coverage.length" class="muted">—</span>
          </template>
        </el-table-column>
        <el-table-column label="发布时间" width="150"><template #default="{ row }">{{ fmtTime(row.published_at) || '—' }}</template></el-table-column>
        <el-table-column label="活跃部署" width="90"><template #default="{ row }">{{ row.deployments_live ?? '—' }}</template></el-table-column>
      </el-table>
      <p class="muted">覆盖率：在线机器中已缓存该镜像的台数；缓存上报超过 5 分钟或旧 Agent 未上报的计入「未知」。发布不受覆盖率限制，未缓存的机器部署时现场拉取镜像（较慢）。</p>
    </el-card>

    <AppFormDialog v-model="editing" :app="app" @saved="load" />

    <el-dialog v-model="deleting" title="删除应用" width="420" align-center data-testid="app-delete-dialog">
      <p>删除「{{ app?.name }}」？只有没有任何版本的草稿应用可以删除，其余请改为「已退役」。</p>
      <template #footer>
        <el-button @click="deleting = false">取消</el-button>
        <el-button type="danger" :loading="busy" data-testid="app-delete-ok" @click="confirmDelete">确定删除</el-button>
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
.low { color: var(--el-color-warning); }
.clickable :deep(tbody tr) { cursor: pointer; }
</style>
