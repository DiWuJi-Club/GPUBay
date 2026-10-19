<script setup lang="ts">
import { auditActions, fmtTime, type AuditEntry, type Paged } from '@spark/shared'
import { onMounted, reactive } from 'vue'
import { api } from '../api'
import DateRange from '../components/DateRange.vue'
import IdLink from '../components/IdLink.vue'
import { usePaged } from '../composables/usePaged'
import { OPS_ROLES } from '../menu'
import { hasRole } from '../store/session'

const filter = reactive({ actor_id: '', action: '', target_type: '', target_id: '', range: { from: '', to: '' } })
const list = usePaged<AuditEntry>((page, size) =>
  api.get<Paged<AuditEntry>>('/audit', {
    actor_id: filter.actor_id, action: filter.action, target_type: filter.target_type, target_id: filter.target_id,
    from: filter.range.from, to: filter.range.to, page, page_size: size,
  }),
)
onMounted(list.load)
function resetFilter() {
  Object.assign(filter, { actor_id: '', action: '', target_type: '', target_id: '', range: { from: '', to: '' } })
  list.reset()
}

// 对象可跳详情：机器 / 实例 / 用户；财务没有机器详情权限，机器只显示 ID
const machineLinkable = hasRole(OPS_ROLES)
const linkKind = (t: string) => (t === 'machine' || t === 'instance' || t === 'user' ? t : null)
const actorText = (e: AuditEntry) => (e.actor_type === 'system' ? '系统' : e.actor_name || `#${e.actor_id}`)
const pretty = (d: unknown) => JSON.stringify(d ?? {}, null, 2)
</script>

<template>
  <el-card>
    <el-form inline class="filters" @submit.prevent="list.reset()">
      <el-form-item label="操作人 ID"><el-input v-model="filter.actor_id" data-testid="audit-actor" clearable style="width: 120px" /></el-form-item>
      <el-form-item label="动作">
        <el-select v-model="filter.action" filterable allow-create default-first-option clearable placeholder="全部" data-testid="audit-action" style="width: 220px">
          <el-option v-for="a in auditActions" :key="a" :label="a" :value="a" />
        </el-select>
      </el-form-item>
      <el-form-item label="对象类型"><el-input v-model="filter.target_type" data-testid="audit-target-type" clearable style="width: 120px" /></el-form-item>
      <el-form-item label="对象 ID"><el-input v-model="filter.target_id" data-testid="audit-target-id" clearable style="width: 160px" /></el-form-item>
      <el-form-item label="时间"><DateRange v-model="filter.range" prefix="audit" /></el-form-item>
      <el-button type="primary" native-type="submit" data-testid="audit-search">查询</el-button>
      <el-button data-testid="audit-reset" @click="resetFilter">重置</el-button>
    </el-form>
    <el-alert v-if="list.error.value" type="error" :title="list.error.value" show-icon :closable="false" class="error" data-testid="audit-error">
      <el-button size="small" :loading="list.loading.value" data-testid="audit-retry" @click="list.load()">重试</el-button>
    </el-alert>
    <el-table :data="list.items.value" v-loading="list.loading.value" row-key="id">
      <el-table-column type="expand">
        <template #default="{ row }"><pre class="diff" data-testid="audit-diff">{{ pretty(row.diff) }}</pre></template>
      </el-table-column>
      <el-table-column label="时间" width="160"><template #default="{ row }"><span data-testid="audit-time">{{ fmtTime(row.created_at) }}</span></template></el-table-column>
      <el-table-column label="操作人" width="140"><template #default="{ row }"><span data-testid="audit-actor-cell">{{ actorText(row as AuditEntry) }}</span></template></el-table-column>
      <el-table-column label="动作" width="200"><template #default="{ row }"><code data-testid="audit-action-cell">{{ row.action }}</code></template></el-table-column>
      <el-table-column label="对象" min-width="220">
        <template #default="{ row }">
          <span data-testid="audit-target">{{ row.target_type }} <IdLink v-if="linkKind(row.target_type)" :kind="linkKind(row.target_type)!" :id="row.target_id" :disabled="row.target_type === 'machine' && !machineLinkable" /><span v-else class="mono">{{ row.target_id }}</span></span>
        </template>
      </el-table-column>
      <el-table-column label="IP" width="140"><template #default="{ row }"><span data-testid="audit-ip">{{ row.ip }}</span></template></el-table-column>
    </el-table>
    <el-pagination v-model:current-page="list.page.value" v-model:page-size="list.size.value" :total="list.total.value" layout="total, prev, pager, next" class="pager" data-testid="audit-pager" />
  </el-card>
</template>

<style scoped>
.filters { display: flex; flex-wrap: wrap; gap: 8px 16px; align-items: center; margin-bottom: 12px; }
.error { margin-bottom: 12px; }
.diff { margin: 0; padding: 8px 16px; background: #f5f7fa; font-size: 12px; white-space: pre-wrap; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.pager { margin-top: 12px; justify-content: flex-end; }
</style>
