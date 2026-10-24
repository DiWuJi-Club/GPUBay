<script setup lang="ts">
import {
  deploymentStateLabel, deploymentStateType, fmtTime, isApiError, labelOf, workflowStateLabel, workflowStateType, type AdminWorkflow,
  type AdminWorkflowInstall, type Paged,
} from '@spark/shared'
import { onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { api } from '../api'
import IdLink from '../components/IdLink.vue'
import { usePaged } from '../composables/usePaged'
import { notifyError } from '../logic/notify'

// 某个工作流的安装列表（计划 11 规格 §12）：部署、实例、用户、版本、安装状态与时间；按加入时间倒序。所有角色可看。
const route = useRoute()
const wfId = Number(route.params.id)
const wf = ref<AdminWorkflow | null>(null)
const notFound = ref(false)
const list = usePaged<AdminWorkflowInstall>((page, size) => api.get<Paged<AdminWorkflowInstall>>(`/workflows/${wfId}/installs`, { page, page_size: size }))

onMounted(async () => {
  void list.load()
  try {
    wf.value = await api.get<AdminWorkflow>(`/workflows/${wfId}`)
  } catch (e) {
    if (isApiError(e) && e.code === 'workflow_not_found') notFound.value = true
    else notifyError(e)
  }
})

const depType = (s: string) => labelOf(deploymentStateType, s) as 'primary'
const wfType = (s: string) => labelOf(workflowStateType, s) as 'primary'
</script>

<template>
  <el-card data-testid="adm-wf-installs-page">
    <template #header>
      <span>
        <router-link to="/workflows" class="back">工作流</router-link> /
        <router-link :to="`/workflows/${wfId}`" class="back" data-testid="adm-wf-installs-back">{{ wf?.name ?? `#${wfId}` }}</router-link> /
        <b>安装列表</b>
      </span>
    </template>
    <el-empty v-if="notFound" description="工作流不存在" data-testid="adm-wf-installs-missing" />
    <template v-else>
      <el-alert v-if="list.error.value" type="error" :title="list.error.value" show-icon :closable="false" class="error" data-testid="adm-wf-installs-error">
        <el-button size="small" :loading="list.loading.value" data-testid="adm-wf-installs-retry" @click="list.load()">重试</el-button>
      </el-alert>
      <el-table :data="list.items.value" v-loading="list.loading.value" row-key="deployment_id" data-testid="adm-wf-installs-table">
        <el-table-column label="部署" min-width="170">
          <template #default="{ row }">
            <router-link :to="{ path: '/deployments', query: { dep: row.deployment_id } }" class="mono" data-testid="adm-wf-install-row" :data-dep="row.deployment_id">{{ row.deployment_id }}</router-link>
          </template>
        </el-table-column>
        <el-table-column label="部署状态" width="100">
          <template #default="{ row }"><el-tag size="small" :type="depType(row.deployment_state)" data-testid="adm-wf-install-dep-state">{{ labelOf(deploymentStateLabel, row.deployment_state) }}</el-tag></template>
        </el-table-column>
        <el-table-column label="实例" min-width="150"><template #default="{ row }"><span data-testid="adm-wf-install-instance"><IdLink kind="instance" :id="row.instance_id" /></span></template></el-table-column>
        <el-table-column label="用户" min-width="140">
          <template #default="{ row }"><span data-testid="adm-wf-install-user"><IdLink kind="user" :id="row.user_id" /> <span class="muted">{{ row.phone }}</span></span></template>
        </el-table-column>
        <el-table-column label="版本" width="100"><template #default="{ row }"><span data-testid="adm-wf-install-version">{{ row.version }}</span></template></el-table-column>
        <el-table-column label="安装状态" width="100">
          <template #default="{ row }"><el-tag size="small" :type="wfType(row.state)" data-testid="adm-wf-install-state" :data-state="row.state">{{ labelOf(workflowStateLabel, row.state) }}</el-tag></template>
        </el-table-column>
        <el-table-column label="加入时间" width="150"><template #default="{ row }">{{ fmtTime(row.added_at) }}</template></el-table-column>
        <el-table-column label="装好时间" width="150"><template #default="{ row }">{{ fmtTime(row.installed_at) || '—' }}</template></el-table-column>
      </el-table>
      <div class="pager-row">
        <span class="muted" data-testid="adm-wf-installs-total">共 {{ list.total.value }} 个</span>
        <el-pagination v-model:current-page="list.page.value" v-model:page-size="list.size.value" :total="list.total.value" layout="prev, pager, next" />
      </div>
    </template>
  </el-card>
</template>

<style scoped>
.back { color: var(--el-color-primary); text-decoration: none; }
.error { margin-bottom: 12px; }
.pager-row { display: flex; justify-content: space-between; align-items: center; margin-top: 12px; }
.muted { color: #909399; font-size: 13px; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
</style>
