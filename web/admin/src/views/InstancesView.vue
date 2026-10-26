<script setup lang="ts">
import { fmtDuration, fmtTime, instanceStateLabel, instanceStateType, labelOf, type AdminImage, type AdminInstance, type Listed, type Paged } from '@spark/shared'
import { computed, onMounted, reactive, ref } from 'vue'
import { onBeforeRouteUpdate, useRoute, type LocationQuery } from 'vue-router'
import { api } from '../api'
import IdLink from '../components/IdLink.vue'
import { usePaged } from '../composables/usePaged'
import { notifyError } from '../logic/notify'
import { session } from '../store/session'

const digits = (v: string) => v.replace(/\D/g, '')
const first = (v: LocationQuery[string]) => String((Array.isArray(v) ? v[0] : v) ?? '')
/** ?user_id=&machine_id=&state= 预填（来自用户 / 机器详情等跳转）；ID 只留数字，未知状态忽略。 */
function fromQuery(q: LocationQuery) {
  const state = first(q.state)
  return { state: state in instanceStateLabel ? state : '', user_id: digits(first(q.user_id)), machine_id: digits(first(q.machine_id)) }
}

const route = useRoute()
const filter = reactive(fromQuery(route.query))
const list = usePaged<AdminInstance>((page, size) =>
  api.get<Paged<AdminInstance>>('/instances', { state: filter.state, user_id: filter.user_id, machine_id: filter.machine_id, page, page_size: size }),
)
const images = ref<AdminImage[]>([])
const imageName = computed(() => new Map(images.value.map((i) => [i.id, i.name])))
const isFinance = computed(() => session.admin?.role === 'finance')

/** 镜像只用于显示名称：失败提示一次，列表照常显示（名称退回 ID）。 */
async function loadImages() {
  try {
    images.value = (await api.get<Listed<AdminImage>>('/images')).items
  } catch (e) {
    notifyError(e)
  }
}
onMounted(() => {
  void list.load()
  void loadImages()
})
// 同一页面换了查询串（如从 /instances?user_id=1 点菜单回到 /instances）：按新查询串重新预填并回到第一页
onBeforeRouteUpdate((to) => {
  if (to.path !== route.path) return
  Object.assign(filter, fromQuery(to.query))
  list.reset()
})
function resetFilter() {
  Object.assign(filter, { state: '', user_id: '', machine_id: '' })
  list.reset()
}
// 平台节点（系统实例）不计时、不到期
const timeText = (i: AdminInstance) => (i.system ? '不计时' : i.state === 'running' ? `剩余 ${fmtDuration(i.remaining_seconds)}` : i.pending_seconds > 0 ? `待开始 ${fmtDuration(i.pending_seconds)}` : '—')
</script>

<template>
  <el-card>
    <el-form inline class="filters" @submit.prevent="list.reset()">
      <el-form-item label="状态">
        <el-select v-model="filter.state" clearable placeholder="全部" data-testid="ainst-filter-state" style="width: 140px">
          <el-option v-for="(label, v) in instanceStateLabel" :key="v" :label="label" :value="v" />
        </el-select>
      </el-form-item>
      <el-form-item label="用户 ID"><el-input :model-value="filter.user_id" clearable data-testid="ainst-filter-user" style="width: 120px" @update:model-value="(v: string) => (filter.user_id = digits(v))" /></el-form-item>
      <el-form-item label="机器 ID"><el-input :model-value="filter.machine_id" clearable data-testid="ainst-filter-machine" style="width: 120px" @update:model-value="(v: string) => (filter.machine_id = digits(v))" /></el-form-item>
      <el-button type="primary" native-type="submit" data-testid="ainst-search">查询</el-button>
      <el-button data-testid="ainst-reset" @click="resetFilter">重置</el-button>
    </el-form>

    <el-alert v-if="list.error.value" type="error" :title="list.error.value" show-icon :closable="false" class="error" data-testid="ainst-error">
      <el-button size="small" :loading="list.loading.value" data-testid="ainst-retry" @click="list.load()">重试</el-button>
    </el-alert>
    <el-table :data="list.items.value" v-loading="list.loading.value" row-key="id">
      <el-table-column label="实例" min-width="160"><template #default="{ row }"><span data-testid="ainst-id"><IdLink kind="instance" :id="row.id" /></span><el-tag v-if="row.system" type="info" size="small" class="tag-gap" data-testid="ainst-system">平台节点</el-tag>
        <!-- 计划 13：集群成员标记（点击进集群详情） -->
        <router-link v-if="row.cluster_id" :to="`/clusters/${row.cluster_id}`" class="tag-gap" data-testid="ainst-cluster" :data-cluster="row.cluster_id" @click.stop><el-tag size="small" type="warning">集群 · rank {{ row.cluster_rank ?? 0 }}</el-tag></router-link></template></el-table-column>
      <el-table-column label="用户" width="90"><template #default="{ row }"><span data-testid="ainst-user"><IdLink kind="user" :id="row.user_id" /></span></template></el-table-column>
      <el-table-column label="机器" width="90"><template #default="{ row }"><span data-testid="ainst-machine"><IdLink kind="machine" :id="row.machine_id" :disabled="isFinance" /></span></template></el-table-column>
      <el-table-column label="镜像" min-width="140"><template #default="{ row }"><span data-testid="ainst-image">{{ imageName.get(row.image_id) ?? row.image_id }}</span></template></el-table-column>
      <el-table-column label="状态" width="170">
        <template #default="{ row }">
          <el-tag :type="labelOf(instanceStateType, row.state) as 'primary'" data-testid="ainst-state">{{ labelOf(instanceStateLabel, row.state) }}</el-tag>
          <el-tag v-if="row.suspended" type="warning" size="small" data-testid="ainst-suspended">计时已暂停</el-tag>
        </template>
      </el-table-column>
      <el-table-column label="剩余 / 待开始" min-width="160"><template #default="{ row }"><span data-testid="ainst-time">{{ timeText(row as AdminInstance) }}</span></template></el-table-column>
      <el-table-column label="到期时间" width="160"><template #default="{ row }"><span data-testid="ainst-expires">{{ fmtTime(row.expires_at) || '—' }}</span></template></el-table-column>
      <el-table-column label="创建时间" width="160"><template #default="{ row }"><span data-testid="ainst-created">{{ fmtTime(row.created_at) }}</span></template></el-table-column>
    </el-table>
    <div class="pager-row">
      <span class="muted" data-testid="ainst-total">共 {{ list.total.value }} 个</span>
      <el-pagination v-model:current-page="list.page.value" v-model:page-size="list.size.value" :total="list.total.value" layout="prev, pager, next" data-testid="ainst-pager" />
    </div>
  </el-card>
</template>

<style scoped>
.filters { display: flex; flex-wrap: wrap; gap: 8px 16px; align-items: center; margin-bottom: 12px; }
.error { margin-bottom: 12px; }
.pager-row { display: flex; justify-content: space-between; align-items: center; margin-top: 12px; }
.muted { color: #909399; }
.tag-gap { margin-left: 6px; }
</style>
