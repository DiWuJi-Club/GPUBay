<script setup lang="ts">
import { alertKindLabel, alertLevelLabel, fmtTime, labelOf, type Alert, type Paged } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { onMounted, reactive, ref, watch } from 'vue'
import { api } from '../api'
import IdLink from '../components/IdLink.vue'
import { usePaged } from '../composables/usePaged'
import { notifyError } from '../logic/notify'
import { OPS_ROLES } from '../menu'
import { refreshUnresolved } from '../store/alerts'
import { hasRole } from '../store/session'

const scope = ref<'unresolved' | 'all'>('unresolved')
const list = usePaged<Alert>((page, size) => api.get<Paged<Alert>>('/alerts', { resolved: scope.value === 'unresolved' ? 'false' : undefined, page, page_size: size }))
watch(scope, () => list.reset())
onMounted(list.load)

// 财务没有机器详情权限：机器 ID 只显示不可点
const machineLinkable = hasRole(OPS_ROLES)

// 请求进行中的告警：同步登记，双击只发一次
const resolving = reactive(new Set<number>())
async function resolve(a: Alert) {
  if (resolving.has(a.id)) return
  resolving.add(a.id)
  try {
    await api.post(`/alerts/${a.id}/resolve`, {})
    ElMessage.success('已标记为已处理')
    await Promise.all([list.load(), refreshUnresolved()])
  } catch (e) {
    notifyError(e)
  } finally {
    resolving.delete(a.id)
  }
}
</script>

<template>
  <el-card>
    <el-radio-group v-model="scope" class="toolbar">
      <el-radio-button value="unresolved" data-testid="alerts-filter-unresolved">未处理</el-radio-button>
      <el-radio-button value="all" data-testid="alerts-filter-all">全部</el-radio-button>
    </el-radio-group>
    <el-alert v-if="list.error.value" type="error" :title="list.error.value" show-icon :closable="false" class="error" data-testid="alerts-error">
      <el-button size="small" :loading="list.loading.value" data-testid="alerts-retry" @click="list.load()">重试</el-button>
    </el-alert>
    <el-table :data="list.items.value" v-loading="list.loading.value" row-key="id">
      <el-table-column label="级别" width="90">
        <template #default="{ row }"><el-tag :type="row.level === 'critical' ? 'danger' : 'warning'" data-testid="alert-level">{{ labelOf(alertLevelLabel, row.level) }}</el-tag></template>
      </el-table-column>
      <el-table-column label="类型" width="130"><template #default="{ row }"><span data-testid="alert-kind">{{ labelOf(alertKindLabel, row.kind) }}</span></template></el-table-column>
      <el-table-column label="内容" min-width="240"><template #default="{ row }"><span data-testid="alert-message">{{ row.message }}</span></template></el-table-column>
      <el-table-column label="机器" width="100"><template #default="{ row }"><IdLink kind="machine" :id="row.machine_id" :disabled="!machineLinkable" data-testid="alert-machine" /></template></el-table-column>
      <el-table-column label="实例" width="160"><template #default="{ row }"><IdLink kind="instance" :id="row.instance_id" data-testid="alert-instance" /></template></el-table-column>
      <el-table-column label="时间" width="160"><template #default="{ row }"><span data-testid="alert-time">{{ fmtTime(row.created_at) }}</span></template></el-table-column>
      <el-table-column label="操作" width="160">
        <template #default="{ row }">
          <el-button v-if="!row.resolved_at" size="small" type="primary" :loading="resolving.has(row.id)" data-testid="alert-resolve" @click="resolve(row as Alert)">标记已处理</el-button>
          <span v-else class="muted" data-testid="alert-resolved-at">{{ fmtTime(row.resolved_at) }} 已处理</span>
        </template>
      </el-table-column>
    </el-table>
    <el-pagination v-model:current-page="list.page.value" v-model:page-size="list.size.value" :total="list.total.value" layout="total, prev, pager, next" class="pager" data-testid="alerts-pager" />
  </el-card>
</template>

<style scoped>
.toolbar { margin-bottom: 12px; }
.error { margin-bottom: 12px; }
.pager { margin-top: 12px; justify-content: flex-end; }
.muted { color: #909399; }
</style>
