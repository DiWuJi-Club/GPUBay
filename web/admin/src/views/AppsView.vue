<script setup lang="ts">
import { appCategoryLabel, appStatusLabel, labelOf, messageOf, type AdminApp, type Listed } from '@spark/shared'
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { api } from '../api'
import AppFormDialog from '../components/AppFormDialog.vue'
import { notifyError } from '../logic/notify'
import { session } from '../store/session'

// 应用目录（规格 §12）：所有角色可看；新建 / 编辑只有 super。
const router = useRouter()
const items = ref<AdminApp[]>([])
const loading = ref(false)
const error = ref('')
const dialog = ref(false)
const canWrite = computed(() => session.admin?.role === 'super')

const statusType: Record<string, 'info' | 'success' | 'warning'> = { draft: 'info', published: 'success', retired: 'warning' }

async function load() {
  loading.value = true
  try {
    items.value = (await api.get<Listed<AdminApp>>('/apps')).items
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

function open(row: AdminApp) {
  void router.push(`/apps/${row.id}`)
}
function onCreated(a: AdminApp) {
  void router.push(`/apps/${a.id}`)
}
</script>

<template>
  <el-card>
    <div class="toolbar">
      <span class="muted">版本、脚本与发布在应用详情页维护。</span>
      <el-button v-if="canWrite" type="primary" data-testid="apps-create" @click="dialog = true">新建应用</el-button>
    </div>
    <el-alert v-if="error" type="error" :title="error" show-icon :closable="false" class="error" data-testid="apps-error">
      <el-button size="small" :loading="loading" data-testid="apps-retry" @click="load">重试</el-button>
    </el-alert>
    <el-table :data="items" v-loading="loading" row-key="id" class="clickable" data-testid="apps-table" @row-click="open">
      <el-table-column label="名称" min-width="160">
        <template #default="{ row }"><span data-testid="apps-row" :data-id="row.id"><span data-testid="apps-row-name">{{ row.name }}</span></span></template>
      </el-table-column>
      <el-table-column label="slug" min-width="140"><template #default="{ row }"><span class="mono" data-testid="apps-row-slug">{{ row.slug }}</span></template></el-table-column>
      <el-table-column label="分类" width="110"><template #default="{ row }">{{ labelOf(appCategoryLabel, row.category) }}</template></el-table-column>
      <el-table-column label="状态" width="100">
        <template #default="{ row }"><el-tag size="small" :type="statusType[row.status]" data-testid="apps-row-status">{{ labelOf(appStatusLabel, row.status) }}</el-tag></template>
      </el-table-column>
      <el-table-column label="版本数" width="90"><template #default="{ row }"><span data-testid="apps-row-versions">{{ row.versions }}</span></template></el-table-column>
      <el-table-column label="活跃部署" width="100"><template #default="{ row }"><span data-testid="apps-row-live">{{ row.deployments_live }}</span></template></el-table-column>
      <el-table-column label="排序" width="80"><template #default="{ row }">{{ row.sort }}</template></el-table-column>
    </el-table>

    <AppFormDialog v-model="dialog" :app="null" @saved="onCreated" />
  </el-card>
</template>

<style scoped>
.toolbar { margin-bottom: 12px; display: flex; justify-content: space-between; align-items: center; gap: 12px; }
.error { margin-bottom: 12px; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.muted { color: #909399; font-size: 13px; }
.clickable :deep(tbody tr) { cursor: pointer; }
</style>
