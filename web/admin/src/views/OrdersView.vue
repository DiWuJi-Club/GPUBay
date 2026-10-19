<script setup lang="ts">
import { durationLabel, fmtTime, fmtYuan, labelOf, orderKindLabel, orderStatusLabel, type AdminOrder } from '@spark/shared'
import FinancePage from '../components/FinancePage.vue'
import IdLink from '../components/IdLink.vue'

const kinds = Object.entries(orderKindLabel).map(([value, label]) => ({ value, label }))
const statuses = Object.entries(orderStatusLabel).map(([value, label]) => ({ value, label }))
</script>

<template>
  <FinancePage endpoint="orders" kind-param="kind" kind-label="类型" :kind-options="kinds" :status-options="statuses">
    <template #table="{ items }">
      <el-table :data="items as unknown as AdminOrder[]" row-key="id">
        <el-table-column label="订单号" min-width="160"><template #default="{ row }"><span class="mono" data-testid="ord-id">{{ row.id }}</span></template></el-table-column>
        <el-table-column label="用户" width="90"><template #default="{ row }"><IdLink kind="user" :id="row.user_id" data-testid="ord-user" /></template></el-table-column>
        <el-table-column label="类型" width="80"><template #default="{ row }"><span data-testid="ord-kind">{{ labelOf(orderKindLabel, row.kind) }}</span></template></el-table-column>
        <el-table-column label="实例" min-width="160"><template #default="{ row }"><IdLink kind="instance" :id="row.instance_id" data-testid="ord-instance" /></template></el-table-column>
        <el-table-column label="租期" width="80"><template #default="{ row }"><span data-testid="ord-duration">{{ labelOf(durationLabel, row.duration) }}</span></template></el-table-column>
        <el-table-column label="数量" width="70"><template #default="{ row }"><span data-testid="ord-quantity">{{ row.quantity }}</span></template></el-table-column>
        <el-table-column label="金额" width="110"><template #default="{ row }"><span data-testid="ord-amount">{{ fmtYuan(row.amount) }}</span></template></el-table-column>
        <el-table-column label="已退" width="110"><template #default="{ row }"><span data-testid="ord-refunded">{{ fmtYuan(row.refunded_amount) }}</span></template></el-table-column>
        <el-table-column label="状态" width="100"><template #default="{ row }"><span data-testid="ord-status">{{ labelOf(orderStatusLabel, row.status) }}</span></template></el-table-column>
        <el-table-column label="批次" min-width="150"><template #default="{ row }"><span class="mono" data-testid="ord-batch">{{ row.batch_id ?? '—' }}</span></template></el-table-column>
        <el-table-column label="自动" width="60"><template #default="{ row }"><span data-testid="ord-auto">{{ row.auto ? '是' : '' }}</span></template></el-table-column>
        <el-table-column label="下单时间" width="160"><template #default="{ row }"><span data-testid="ord-time">{{ fmtTime(row.created_at) }}</span></template></el-table-column>
      </el-table>
    </template>
  </FinancePage>
</template>

<style scoped>
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
</style>
