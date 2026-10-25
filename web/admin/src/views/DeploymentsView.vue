<script setup lang="ts">
import {
  deploymentErrorLabel, deploymentStateLabel, deploymentStateType, fmtTime, fmtTimeSec, labelOf, messageOf, workflowStateLabel, workflowStateType,
  type AdminApp, type AdminDeployment, type AdminDeploymentDetail, type DeploymentState, type Listed, type Paged,
} from '@spark/shared'
import { computed, onMounted, reactive, ref } from 'vue'
import { onBeforeRouteUpdate, useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import ForceRemoveDialog from '../components/ForceRemoveDialog.vue'
import IdLink from '../components/IdLink.vue'
import { usePaged } from '../composables/usePaged'
import {
  deploymentDesiredLabel, deploymentHealthLabel, digits, envRows, filterFromQuery, firstOf, onlyDepChanged, progressText, showForceRemove,
} from '../logic/deployments'
import { notifyError } from '../logic/notify'
import { session } from '../store/session'

const route = useRoute()
const router = useRouter()
const role = computed(() => session.admin?.role ?? 'finance')
// ?state=&app_id=&user_id=&instance_id= 预填（来自实例详情「应用」卡片等跳转）；?dep= 直接打开该部署的详情抽屉
const filter = reactive(filterFromQuery(route.query))
const list = usePaged<AdminDeployment>((page, size) =>
  api.get<Paged<AdminDeployment>>('/deployments', {
    state: filter.state, app_id: filter.app_id, user_id: filter.user_id, instance_id: filter.instance_id.trim(), page, page_size: size,
  }),
)
const apps = ref<AdminApp[]>([])
/** 应用只用于筛选下拉：失败提示一次，列表照常可用。 */
async function loadApps() {
  try {
    apps.value = (await api.get<Listed<AdminApp>>('/apps')).items
  } catch (e) {
    notifyError(e)
  }
}
onMounted(() => {
  void list.load()
  void loadApps()
  const dep = firstOf(route.query.dep)
  if (dep) void openDetail(dep)
})
onBeforeRouteUpdate((to, from) => {
  if (to.path !== from.path) return
  // 只是 dep 变了（关抽屉时去掉 ?dep=、或打开某个部署的链接）：保留当前筛选、页码与列表，不重新加载
  if (!onlyDepChanged(from.query, to.query)) {
    Object.assign(filter, filterFromQuery(to.query))
    list.reset()
  }
  const dep = firstOf(to.query.dep)
  if (dep && dep !== detail.id) void openDetail(dep)
})
function resetFilter() {
  Object.assign(filter, { state: '', app_id: '', user_id: '', instance_id: '' })
  list.reset()
}

// ---- 详情抽屉 ----
const detail = reactive({ open: false, id: '', loading: false, error: '', data: null as AdminDeploymentDetail | null })
const dep = computed(() => detail.data?.deployment ?? null)
const env = computed(() => (dep.value ? envRows(dep.value) : []))
/** 计划 11：部署上的工作流集合；ComfyUI 部署（或已有工作流）才显示这一节 */
const workflows = computed(() => detail.data?.workflows ?? [])
const showWorkflows = computed(() => dep.value?.app_slug === 'comfyui' || workflows.value.length > 0)
let seq = 0
/** 只采用最后一次请求的结果：快速点不同的行时，先发后到的旧详情被丢弃。 */
async function loadDetail() {
  const mine = ++seq
  const id = detail.id
  detail.loading = true
  try {
    const d = await api.get<AdminDeploymentDetail>(`/deployments/${encodeURIComponent(id)}`)
    if (mine !== seq) return
    detail.data = d
    detail.error = ''
  } catch (e) {
    if (mine !== seq) return
    notifyError(e)
    if (!detail.data) detail.error = messageOf(e)
  } finally {
    if (mine === seq) detail.loading = false
  }
}
function openDetail(id: string) {
  Object.assign(detail, { open: true, id, error: '', data: null })
  return loadDetail()
}
function closeDetail() {
  detail.open = false
  seq++ // 关掉后迟到的响应不再写入
  if (route.query.dep) void router.replace({ query: { ...route.query, dep: undefined } })
}

// ---- 强制卸载（ops）----
const removing = ref<AdminDeployment | null>(null)
async function onRemoved() {
  removing.value = null
  await Promise.all([list.load(), detail.open ? loadDetail() : Promise.resolve()])
}

const stateType = (s: DeploymentState) => labelOf(deploymentStateType, s) as 'primary'
const errorText = (d: AdminDeployment) => (d.error_code ? labelOf(deploymentErrorLabel, d.error_code) : '')
</script>

<template>
  <el-card>
    <el-form inline class="filters" @submit.prevent="list.reset()">
      <el-form-item label="状态">
        <el-select v-model="filter.state" clearable placeholder="全部" data-testid="deployments-filter-state" style="width: 130px">
          <el-option v-for="(label, v) in deploymentStateLabel" :key="v" :label="label" :value="v" />
        </el-select>
      </el-form-item>
      <el-form-item label="应用">
        <el-select v-model="filter.app_id" clearable filterable placeholder="全部" data-testid="deployments-filter-app" style="width: 160px">
          <el-option v-for="a in apps" :key="a.id" :label="a.name" :value="String(a.id)" />
        </el-select>
      </el-form-item>
      <el-form-item label="用户 ID"><el-input :model-value="filter.user_id" clearable data-testid="deployments-filter-user" style="width: 110px" @update:model-value="(v: string) => (filter.user_id = digits(v))" /></el-form-item>
      <el-form-item label="实例 ID"><el-input v-model="filter.instance_id" clearable data-testid="deployments-filter-instance" style="width: 200px" /></el-form-item>
      <el-button type="primary" native-type="submit" data-testid="deployments-search">查询</el-button>
      <el-button data-testid="deployments-reset" @click="resetFilter">重置</el-button>
    </el-form>

    <el-alert v-if="list.error.value" type="error" :title="list.error.value" show-icon :closable="false" class="error" data-testid="deployments-load-error">
      <el-button size="small" :loading="list.loading.value" data-testid="deployments-retry" @click="list.load()">重试</el-button>
    </el-alert>
    <el-table :data="list.items.value" v-loading="list.loading.value" row-key="id" data-testid="deployments-table" @row-click="(row: AdminDeployment) => openDetail(row.id)">
      <el-table-column label="实例" min-width="150"><template #default="{ row }"><span data-testid="deployments-instance" @click.stop><IdLink kind="instance" :id="row.instance_id" /></span></template></el-table-column>
      <el-table-column label="用户" width="80"><template #default="{ row }"><span data-testid="deployments-user" @click.stop><IdLink kind="user" :id="row.user_id" /></span></template></el-table-column>
      <el-table-column label="应用 / 版本" min-width="150">
        <template #default="{ row }"><span data-testid="deployments-app" :data-dep="row.id">{{ row.app_name }} · {{ row.version }}</span></template>
      </el-table-column>
      <el-table-column label="状态" width="100">
        <template #default="{ row }"><el-tag :type="stateType(row.state)" data-testid="deployments-state" :data-state="row.state">{{ labelOf(deploymentStateLabel, row.state) }}</el-tag></template>
      </el-table-column>
      <el-table-column label="进度" min-width="130"><template #default="{ row }"><span data-testid="deployments-progress">{{ progressText(row as AdminDeployment) || '—' }}</span></template></el-table-column>
      <el-table-column label="错误码" min-width="140">
        <template #default="{ row }"><span data-testid="deployments-error" :data-code="row.error_code" :title="row.error_code">{{ errorText(row as AdminDeployment) || '—' }}</span></template>
      </el-table-column>
      <el-table-column label="创建时间" width="150"><template #default="{ row }"><span data-testid="deployments-created">{{ fmtTime(row.created_at) }}</span></template></el-table-column>
      <el-table-column label="更新时间" width="150"><template #default="{ row }"><span data-testid="deployments-updated">{{ fmtTime(row.updated_at) }}</span></template></el-table-column>
      <el-table-column label="操作" width="80">
        <template #default="{ row }"><el-button size="small" link type="primary" data-testid="deployments-detail" @click.stop="openDetail(row.id)">详情</el-button></template>
      </el-table-column>
    </el-table>
    <div class="pager-row">
      <span class="muted" data-testid="deployments-total">共 {{ list.total.value }} 个</span>
      <el-pagination v-model:current-page="list.page.value" v-model:page-size="list.size.value" :total="list.total.value" layout="prev, pager, next" data-testid="deployments-pager" />
    </div>

    <el-drawer :model-value="detail.open" size="640px" title="部署详情" data-testid="deployment-drawer" @close="closeDetail">
      <el-alert v-if="detail.error && !dep" type="error" :title="detail.error" show-icon :closable="false" data-testid="deployment-load-error">
        <el-button size="small" :loading="detail.loading" data-testid="deployment-retry" @click="loadDetail">重试</el-button>
      </el-alert>
      <el-skeleton v-else-if="!dep" :rows="6" animated />
      <div v-if="dep && detail.data" class="drawer">
        <div class="head">
          <span>
            <b data-testid="deployment-app">{{ dep.app_name }} · {{ dep.version }}</b>
            <el-tag v-if="dep.update_available" size="small" type="warning" class="gap" data-testid="deployment-update">有新版本 {{ dep.latest_version }}</el-tag>
          </span>
          <span>
            <el-tag :type="stateType(dep.state)" data-testid="deployment-state" :data-state="dep.state">{{ labelOf(deploymentStateLabel, dep.state) }}</el-tag>
            <el-button size="small" class="gap" :loading="detail.loading" data-testid="deployment-refresh" @click="loadDetail">刷新</el-button>
            <el-button v-if="showForceRemove(role, dep.state)" size="small" type="danger" plain data-testid="deployment-force-remove" @click="removing = dep">强制卸载</el-button>
          </span>
        </div>
        <el-descriptions :column="2" border size="small">
          <el-descriptions-item label="部署 ID"><span class="mono" data-testid="deployment-id">{{ dep.id }}</span></el-descriptions-item>
          <el-descriptions-item label="实例"><span data-testid="deployment-instance"><IdLink kind="instance" :id="dep.instance_id" /></span></el-descriptions-item>
          <el-descriptions-item label="用户"><span data-testid="deployment-user"><IdLink kind="user" :id="dep.user_id" /></span></el-descriptions-item>
          <el-descriptions-item label="目标状态"><span data-testid="deployment-desired">{{ labelOf(deploymentDesiredLabel, dep.desired) }}</span></el-descriptions-item>
          <el-descriptions-item label="进度"><span data-testid="deployment-progress">{{ progressText(dep) || '—' }}</span></el-descriptions-item>
          <el-descriptions-item label="健康"><span data-testid="deployment-health">{{ labelOf(deploymentHealthLabel, dep.health) }}</span></el-descriptions-item>
          <el-descriptions-item label="错误">
            <span data-testid="deployment-error-code" :data-code="dep.error_code">{{ dep.error_code ? `${labelOf(deploymentErrorLabel, dep.error_code)}（${dep.error_code}）` : '—' }}</span>
          </el-descriptions-item>
          <el-descriptions-item label="尝试次数"><span data-testid="deployment-attempt">{{ dep.attempt }}</span></el-descriptions-item>
          <el-descriptions-item label="说明" :span="2"><span data-testid="deployment-message">{{ dep.message || '—' }}</span></el-descriptions-item>
          <el-descriptions-item label="端口 / 链接" :span="2">
            <span data-testid="deployment-url">{{ dep.port }}{{ dep.url ? ` · ${dep.url}` : ' · （无链接）' }}</span>
          </el-descriptions-item>
          <el-descriptions-item label="开始"><span>{{ fmtTime(dep.started_at) || '—' }}</span></el-descriptions-item>
          <el-descriptions-item label="结束"><span>{{ fmtTime(dep.finished_at) || '—' }}</span></el-descriptions-item>
          <el-descriptions-item label="创建"><span>{{ fmtTime(dep.created_at) }}</span></el-descriptions-item>
          <el-descriptions-item label="更新"><span>{{ fmtTime(dep.updated_at) }}</span></el-descriptions-item>
        </el-descriptions>
        <p v-if="dep.cleanup_pending" class="warn" data-testid="deployment-cleanup">应用目录待清理（实例下次启动时清理）</p>

        <h4>变量</h4>
        <table class="grid" data-testid="deployment-env">
          <thead><tr><th>变量</th><th>说明</th><th>值</th></tr></thead>
          <tbody>
            <tr v-for="r in env" :key="r.name" data-testid="deployment-env-row" :data-name="r.name" :data-kind="r.kind">
              <td class="mono" data-testid="deployment-env-name">{{ r.name }}</td>
              <td>{{ r.label }}</td>
              <td :class="{ mono: r.kind === 'value', muted: r.kind !== 'value' }" data-testid="deployment-env-value">{{ r.value }}</td>
            </tr>
            <tr v-if="!env.length"><td colspan="3" class="muted empty">无变量</td></tr>
          </tbody>
        </table>

        <template v-if="showWorkflows">
          <h4>工作流</h4>
          <table class="grid" data-testid="adm-dep-workflows">
            <thead><tr><th>工作流</th><th>版本</th><th>状态</th><th>快速出图</th><th>加入</th><th>装好</th></tr></thead>
            <tbody>
              <tr v-for="w in workflows" :key="w.workflow_id" data-testid="adm-dep-workflow-row" :data-slug="w.slug">
                <td><router-link :to="`/workflows/${w.workflow_id}`" data-testid="adm-dep-workflow-name">{{ w.name }}</router-link> <span class="muted mono">{{ w.slug }}</span></td>
                <td>
                  <span data-testid="adm-dep-workflow-version">{{ w.version }}</span>
                  <el-tag v-if="w.update_available" size="small" type="warning" class="gap" data-testid="adm-dep-workflow-update">有新版本 {{ w.latest_version }}</el-tag>
                </td>
                <td><el-tag size="small" :type="labelOf(workflowStateType, w.state) as 'primary'" data-testid="adm-dep-workflow-state" :data-state="w.state">{{ labelOf(workflowStateLabel, w.state) }}</el-tag></td>
                <td>{{ w.quick_run ? '支持' : '—' }}</td>
                <td class="time">{{ fmtTime(w.added_at) }}</td>
                <td class="time">{{ fmtTime(w.installed_at) || '—' }}</td>
              </tr>
              <tr v-if="!workflows.length"><td colspan="6" class="muted empty">未安装工作流</td></tr>
            </tbody>
          </table>
        </template>

        <h4>事件（最近 100 条）</h4>
        <table class="grid" data-testid="deployment-events">
          <tbody>
            <tr v-for="e in detail.data.events" :key="e.id" data-testid="deployment-event">
              <td class="time">{{ fmtTimeSec(e.at) }}</td>
              <td>{{ labelOf(deploymentStateLabel, e.from_state) || '—' }} → {{ labelOf(deploymentStateLabel, e.to_state) }}</td>
              <td data-testid="deployment-event-detail">{{ e.detail }}</td>
            </tr>
            <tr v-if="!detail.data.events.length"><td class="muted empty">无事件</td></tr>
          </tbody>
        </table>

        <h4>日志尾部（已脱敏）</h4>
        <!-- 按文本渲染（插值），从不 v-html：日志来自用户容器 -->
        <pre class="log" data-testid="deployment-log">{{ dep.log_tail || '（暂无日志）' }}</pre>
      </div>
    </el-drawer>

    <ForceRemoveDialog :target="removing" @close="removing = null" @done="onRemoved" />
  </el-card>
</template>

<style scoped>
.filters { display: flex; flex-wrap: wrap; gap: 8px 16px; align-items: center; margin-bottom: 12px; }
.error { margin-bottom: 12px; }
.pager-row { display: flex; justify-content: space-between; align-items: center; margin-top: 12px; }
.muted { color: #909399; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.warn { color: var(--el-color-warning); font-size: 13px; }
.drawer { display: grid; gap: 12px; }
.drawer h4 { margin: 8px 0 0; }
.head { display: flex; justify-content: space-between; align-items: center; gap: 8px; }
.gap { margin-left: 8px; }
.grid { width: 100%; border-collapse: collapse; font-size: 13px; }
.grid th, .grid td { padding: 6px 8px; text-align: left; border-bottom: 1px solid var(--el-border-color-lighter); vertical-align: top; }
.grid th { color: #909399; font-weight: 500; }
.time { white-space: nowrap; color: #909399; }
.empty { text-align: center; }
.log { margin: 0; max-height: 360px; overflow: auto; white-space: pre-wrap; word-break: break-all; background: #1e1e1e; color: #e6e6e6; padding: 12px; border-radius: 4px; font-size: 12px; font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
</style>
