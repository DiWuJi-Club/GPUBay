<script setup lang="ts">
import { clusterNetworkLabel, clusterStatusLabel, clusterStatusType, fabricKindLabel, fmtTime, labelOf, type FabricRow, type Listed, type Paged } from '@spark/shared'
import { onMounted, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import { usePaged } from '../composables/usePaged'
import { clusterListQuery, emptyClusterFilter, type AdminClusterView, type ClusterFilter } from '../logic/clusters'
import { firstOf } from '../logic/deployments'
import { notifyError } from '../logic/notify'

// 集群列表（计划 13 规格 §11）：全员可看；默认只看活动集群。?fabric_id= / ?user_id= 预填筛选（集群网络页、用户详情的链接）。
const route = useRoute()
const router = useRouter()
const filter = reactive<ClusterFilter>(emptyClusterFilter())
const fid = Number(firstOf(route.query.fabric_id))
if (Number.isInteger(fid) && fid > 0) filter.fabric_id = fid
filter.user_id = firstOf(route.query.user_id).replace(/\D/g, '')

const list = usePaged<AdminClusterView>((page, size) => api.get<Paged<AdminClusterView>>('/clusters', clusterListQuery(filter, page, size)))
const fabrics = ref<FabricRow[]>([])
async function loadFabrics() {
  try {
    fabrics.value = (await api.get<Listed<FabricRow>>('/fabrics')).items
  } catch (e) {
    notifyError(e) // 只影响筛选下拉
  }
}
onMounted(() => {
  void list.load()
  void loadFabrics()
})
function resetFilter() {
  Object.assign(filter, emptyClusterFilter())
  list.reset()
}
const statusTag = (s: string) => labelOf(clusterStatusType, s) as 'primary'
</script>

<template>
  <el-card>
    <el-form inline class="filters" @submit.prevent="list.reset()">
      <el-form-item label="状态">
        <el-select v-model="filter.state" placeholder="全部" style="width: 120px" data-testid="clusters-filter-state">
          <el-option label="全部" value="" /><el-option label="活动" value="active" /><el-option label="已释放" value="released" /><el-option label="开通失败" value="failed" />
        </el-select>
      </el-form-item>
      <el-form-item label="归属">
        <el-select v-model="filter.system" placeholder="全部" style="width: 130px" data-testid="clusters-filter-system">
          <el-option label="全部" value="" /><el-option label="用户集群" value="false" /><el-option label="平台节点组" value="true" />
        </el-select>
      </el-form-item>
      <el-form-item label="用户 ID"><el-input v-model="filter.user_id" clearable style="width: 120px" data-testid="clusters-filter-user" /></el-form-item>
      <el-form-item label="集群网络">
        <el-select v-model="filter.fabric_id" clearable placeholder="全部" style="width: 180px" data-testid="clusters-filter-fabric">
          <el-option v-for="f in fabrics" :key="f.id" :label="f.name" :value="f.id" />
        </el-select>
      </el-form-item>
      <el-button type="primary" native-type="submit" data-testid="clusters-search">查询</el-button>
      <el-button data-testid="clusters-reset" @click="resetFilter">重置</el-button>
    </el-form>
    <el-alert v-if="list.error.value" type="error" :title="list.error.value" show-icon :closable="false" class="mb" data-testid="clusters-error">
      <el-button size="small" :loading="list.loading.value" data-testid="clusters-retry" @click="list.load()">重试</el-button>
    </el-alert>
    <table v-loading="list.loading.value" class="grid clickable" data-testid="clusters-table">
      <thead><tr><th>ID</th><th>归属</th><th>规模</th><th>状态</th><th>网络</th><th>集群网络</th><th>子网</th><th>创建时间</th></tr></thead>
      <tbody>
        <tr v-for="c in list.items.value" :key="c.id" data-testid="cluster-row" :data-id="c.id" :data-status="c.status" @click="router.push(`/clusters/${c.id}`)">
          <td><router-link :to="`/clusters/${c.id}`" class="mono" data-testid="cluster-row-id" @click.stop>{{ c.id }}</router-link></td>
          <td data-testid="cluster-row-owner">
            <template v-if="c.system">平台</template>
            <router-link v-else :to="`/users/${c.user_id}`" class="mono" @click.stop>{{ c.user_id }}</router-link>
          </td>
          <td data-testid="cluster-row-size">{{ c.size }} 台</td>
          <td><el-tag size="small" :type="statusTag(c.status)" data-testid="cluster-row-status">{{ labelOf(clusterStatusLabel, c.status) }}</el-tag></td>
          <td data-testid="cluster-row-network">{{ labelOf(clusterNetworkLabel, c.network) }}</td>
          <td><span data-testid="cluster-row-fabric">{{ c.fabric_name }}</span><span class="muted">（{{ labelOf(fabricKindLabel, c.fabric_kind) }}）</span></td>
          <td class="mono" data-testid="cluster-row-subnet">{{ c.subnet }}</td>
          <td>{{ fmtTime(c.created_at) }}</td>
        </tr>
        <tr v-if="!list.items.value.length && !list.loading.value && !list.error.value"><td colspan="8" class="muted empty" data-testid="clusters-empty">没有集群</td></tr>
      </tbody>
    </table>
    <div class="pager-row">
      <span class="muted" data-testid="clusters-total">共 {{ list.total.value }} 个</span>
      <el-pagination v-model:current-page="list.page.value" v-model:page-size="list.size.value" :page-sizes="[20, 50, 100]" :total="list.total.value" layout="sizes, prev, pager, next" />
    </div>
  </el-card>
</template>

<style scoped>
.filters { display: flex; flex-wrap: wrap; gap: 8px 16px; align-items: center; margin-bottom: 12px; }
.mb { margin-bottom: 12px; }
.grid { width: 100%; border-collapse: collapse; font-size: 13px; }
.grid th, .grid td { padding: 8px; text-align: left; border-bottom: 1px solid var(--el-border-color-lighter); vertical-align: middle; }
.grid th { color: #909399; font-weight: 500; }
.clickable tbody tr { cursor: pointer; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.muted { color: #909399; }
.empty { text-align: center; }
.pager-row { display: flex; justify-content: space-between; align-items: center; margin-top: 12px; }
</style>
