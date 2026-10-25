<script setup lang="ts">
import { digitsOnly, fmtTime, kycStatusLabel, labelOf, userKindLabel, userStatusLabel, type Paged, type User } from '@spark/shared'
import { onMounted, reactive } from 'vue'
import { api } from '../api'
import IdLink from '../components/IdLink.vue'
import { usePaged } from '../composables/usePaged'

/** q：完整手机号、手机尾号或用户 ID，后端只接受数字：输入时全角数字转半角，其余非数字去掉 */
// include_system：计划 9 的平台系统账号（如 Token 商城的 system:llm）默认不列出
const filter = reactive({ q: '', kyc_status: '', status: '', include_system: false })
const list = usePaged<User>((page, size) =>
  api.get<Paged<User>>('/users', {
    q: filter.q, kyc_status: filter.kyc_status, status: filter.status, include_system: filter.include_system ? 'true' : undefined, page, page_size: size,
  }),
)
onMounted(list.load)
function resetFilter() {
  Object.assign(filter, { q: '', kyc_status: '', status: '', include_system: false })
  list.reset()
}
</script>

<template>
  <el-card>
    <el-form inline class="filters" @submit.prevent="list.reset()">
      <el-form-item label="手机号 / 尾号 / ID">
        <el-input :model-value="filter.q" clearable placeholder="只能输入数字" data-testid="users-q" style="width: 200px" @update:model-value="(v: string) => (filter.q = digitsOnly(v))" />
      </el-form-item>
      <el-form-item label="实名">
        <el-select v-model="filter.kyc_status" clearable placeholder="全部" data-testid="users-filter-kyc" style="width: 120px">
          <el-option v-for="(label, v) in kycStatusLabel" :key="v" :label="label" :value="v" />
        </el-select>
      </el-form-item>
      <el-form-item label="账号">
        <el-select v-model="filter.status" clearable placeholder="全部" data-testid="users-filter-status" style="width: 120px">
          <el-option v-for="(label, v) in userStatusLabel" :key="v" :label="label" :value="v" />
        </el-select>
      </el-form-item>
      <el-form-item label="含系统账号">
        <el-switch v-model="filter.include_system" data-testid="users-include-system" @change="list.reset()" />
      </el-form-item>
      <el-button type="primary" native-type="submit" data-testid="users-search">查询</el-button>
      <el-button data-testid="users-reset" @click="resetFilter">重置</el-button>
    </el-form>

    <el-alert v-if="list.error.value" type="error" :title="list.error.value" show-icon :closable="false" class="error" data-testid="users-error">
      <el-button size="small" :loading="list.loading.value" data-testid="users-retry" @click="list.load()">重试</el-button>
    </el-alert>
    <el-table :data="list.items.value" v-loading="list.loading.value" row-key="id">
      <el-table-column label="ID" width="90"><template #default="{ row }"><span data-testid="user-id"><IdLink kind="user" :id="row.id" /></span></template></el-table-column>
      <el-table-column label="手机号" width="140"><template #default="{ row }"><span class="mono" data-testid="user-phone">{{ row.phone }}</span></template></el-table-column>
      <el-table-column label="类型" width="80"><template #default="{ row }"><span data-testid="user-kind">{{ labelOf(userKindLabel, row.kind) }}</span></template></el-table-column>
      <el-table-column label="实名" width="100"><template #default="{ row }"><span data-testid="user-kyc">{{ labelOf(kycStatusLabel, row.kyc_status) }}</span></template></el-table-column>
      <el-table-column label="账号" width="100"><template #default="{ row }"><el-tag :type="row.status === 'banned' ? 'danger' : 'success'" data-testid="user-status">{{ labelOf(userStatusLabel, row.status) }}</el-tag></template></el-table-column>
      <el-table-column label="注册时间" min-width="160"><template #default="{ row }"><span data-testid="user-created">{{ fmtTime(row.created_at) }}</span></template></el-table-column>
    </el-table>
    <div class="pager-row">
      <span class="muted" data-testid="users-total">共 {{ list.total.value }} 人</span>
      <el-pagination v-model:current-page="list.page.value" v-model:page-size="list.size.value" :total="list.total.value" layout="prev, pager, next" data-testid="users-pager" />
    </div>
  </el-card>
</template>

<style scoped>
.filters { display: flex; flex-wrap: wrap; gap: 8px 16px; align-items: center; margin-bottom: 12px; }
.error { margin-bottom: 12px; }
.pager-row { display: flex; justify-content: space-between; align-items: center; margin-top: 12px; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.muted { color: #909399; }
</style>
