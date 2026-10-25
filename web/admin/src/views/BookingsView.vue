<script setup lang="ts">
import { fmtDuration, fmtTime, labelOf, machineStateLabel, type AdminGroup, type AdminInstance, type Listed, type Paged } from '@spark/shared'
import { computed, onMounted, reactive, ref } from 'vue'
import { api } from '../api'
import BatchLink from '../components/BatchLink.vue'
import BookingOpsDialogs from '../components/BookingOpsDialogs.vue'
import DateRange from '../components/DateRange.vue'
import IdLink from '../components/IdLink.vue'
import { useBookingOps } from '../composables/useBookingOps'
import { usePaged } from '../composables/usePaged'
import { showCancelBooking, showReassign } from '../logic/instanceOps'
import { notifyError } from '../logic/notify'
import { FIN_ROLES } from '../menu'
import { session } from '../store/session'

const digits = (v: string) => v.replace(/\D/g, '')
const filter = reactive({ state: 'reserved', user_id: '', group_id: '', machine_id: '', range: { from: '', to: '' } })
let applied = { ...filter, range: { ...filter.range } }
type BookingRow = AdminInstance & { machine_state: string }
const list = usePaged<BookingRow>((page, size) =>
  api.get<Paged<BookingRow>>('/bookings', { state: applied.state, user_id: applied.user_id, group_id: applied.group_id, machine_id: applied.machine_id, from: applied.range.from, to: applied.range.to, page, page_size: size }),
)
const groups = ref<AdminGroup[]>([])
const groupName = computed(() => new Map(groups.value.map((g) => [g.id, g.name])))
const role = computed(() => session.admin?.role ?? 'finance')
const isFinance = computed(() => role.value === 'finance')
const canSeeOrders = computed(() => FIN_ROLES.includes(role.value))

async function loadGroups() {
  try {
    groups.value = (await api.get<Listed<AdminGroup>>('/groups')).items
  } catch (e) {
    notifyError(e)
  }
}
onMounted(() => {
  void list.load()
  void loadGroups()
})
function search() {
  applied = { ...filter, range: { ...filter.range } }
  list.reset()
}
function resetFilter() {
  Object.assign(filter, { state: 'reserved', user_id: '', group_id: '', machine_id: '', range: { from: '', to: '' } })
  search()
}

// ---- 改派 / 取消预订（ops；取消的动态码由请求层弹出）----
const bookingOps = useBookingOps(() => list.load())
const { openReassign, openCancel } = bookingOps
</script>

<template>
  <el-card>
    <el-form inline class="filters" @submit.prevent="search">
      <el-form-item label="状态">
        <el-select v-model="filter.state" data-testid="bookings-filter-state" style="width: 120px">
          <el-option label="未开通" value="reserved" /><el-option label="已取消" value="cancelled" />
        </el-select>
      </el-form-item>
      <el-form-item label="用户 ID"><el-input :model-value="filter.user_id" clearable data-testid="bookings-filter-user" style="width: 110px" @update:model-value="(v: string) => (filter.user_id = digits(v))" /></el-form-item>
      <el-form-item label="机组">
        <el-select v-model="filter.group_id" clearable placeholder="全部" data-testid="bookings-filter-group" style="width: 140px">
          <el-option v-for="g in groups" :key="g.id" :label="g.name" :value="String(g.id)" />
        </el-select>
      </el-form-item>
      <el-form-item label="机器 ID"><el-input :model-value="filter.machine_id" clearable data-testid="bookings-filter-machine" style="width: 110px" @update:model-value="(v: string) => (filter.machine_id = digits(v))" /></el-form-item>
      <el-form-item label="开始日期"><DateRange v-model="filter.range" prefix="bookings" /></el-form-item>
      <el-button type="primary" native-type="submit" data-testid="bookings-search">查询</el-button>
      <el-button data-testid="bookings-reset" @click="resetFilter">重置</el-button>
    </el-form>
    <el-alert v-if="list.error.value" type="error" :title="list.error.value" show-icon :closable="false" class="error" data-testid="bookings-error">
      <el-button size="small" :loading="list.loading.value" data-testid="bookings-retry" @click="list.load()">重试</el-button>
    </el-alert>
    <el-table :data="list.items.value" v-loading="list.loading.value" row-key="id">
      <el-table-column label="实例" min-width="150"><template #default="{ row }"><span data-testid="booking-row"><span data-testid="booking-id"><IdLink kind="instance" :id="row.id" /></span></span></template></el-table-column>
      <el-table-column label="用户" width="90"><template #default="{ row }"><span data-testid="booking-user"><IdLink kind="user" :id="row.user_id" /></span></template></el-table-column>
      <el-table-column label="机组" width="120"><template #default="{ row }"><span data-testid="booking-group">{{ row.group_id ? (groupName.get(row.group_id) ?? row.group_id) : '不限' }}</span></template></el-table-column>
      <el-table-column label="机器" width="150">
        <template #default="{ row }">
          <span data-testid="booking-machine"><IdLink kind="machine" :id="row.machine_id" :disabled="isFinance" /></span>
          <el-tag v-if="row.machine_state === 'faulty' || row.machine_state === 'maintenance'" type="warning" size="small" data-testid="booking-machine-state">{{ labelOf(machineStateLabel, row.machine_state) }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column label="开始" width="150"><template #default="{ row }"><span data-testid="booking-start">{{ fmtTime(row.scheduled_start_at) }}</span></template></el-table-column>
      <el-table-column label="时长" width="110"><template #default="{ row }"><span data-testid="booking-duration">{{ fmtDuration(row.pending_seconds) }}</span></template></el-table-column>
      <el-table-column label="批次" min-width="150"><template #default="{ row }"><span data-testid="booking-batch"><BatchLink :id="row.batch_id" :disabled="!canSeeOrders" /></span></template></el-table-column>
      <el-table-column label="改派" width="70"><template #default="{ row }"><span data-testid="booking-reassigned">{{ row.reassign_count }}</span></template></el-table-column>
      <el-table-column label="下单" width="150"><template #default="{ row }">{{ fmtTime(row.created_at) }}</template></el-table-column>
      <el-table-column label="操作" width="180">
        <template #default="{ row }">
          <el-button v-if="showReassign(role, row.state, row.system)" size="small" data-testid="booking-reassign" @click="openReassign(row as AdminInstance)">改派</el-button>
          <el-button v-if="showCancelBooking(role, row.state, row.system)" size="small" type="danger" plain data-testid="booking-cancel" @click="openCancel(row as AdminInstance)">取消预订</el-button>
        </template>
      </el-table-column>
    </el-table>
    <div class="pager-row">
      <span class="muted" data-testid="bookings-total">共 {{ list.total.value }} 个</span>
      <el-pagination v-model:current-page="list.page.value" v-model:page-size="list.size.value" :total="list.total.value" layout="prev, pager, next" data-testid="bookings-pager" />
    </div>

    <BookingOpsDialogs :ops="bookingOps" />
  </el-card>
</template>

<style scoped>
.filters { display: flex; flex-wrap: wrap; gap: 8px 16px; align-items: center; margin-bottom: 12px; }
.error { margin-bottom: 12px; }
.pager-row { display: flex; justify-content: space-between; align-items: center; margin-top: 12px; }
.muted { color: #909399; font-size: 13px; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
</style>
