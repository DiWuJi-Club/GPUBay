<script setup lang="ts">
import { fmtTime, fmtYuan, labelOf, rechargeChannelLabel, rechargeStatusLabel, type AdminRecharge } from '@spark/shared'
import FinancePage from '../components/FinancePage.vue'
import IdLink from '../components/IdLink.vue'

const channels = Object.entries(rechargeChannelLabel).map(([value, label]) => ({ value, label }))
const statuses = Object.entries(rechargeStatusLabel).map(([value, label]) => ({ value, label }))
</script>

<template>
  <FinancePage endpoint="recharges" kind-param="channel" kind-label="渠道" :kind-options="channels" :status-options="statuses">
    <template #table="{ items }">
      <el-table :data="items as unknown as AdminRecharge[]" row-key="id">
        <el-table-column label="充值单号" min-width="160"><template #default="{ row }"><span class="mono" data-testid="rch-id">{{ row.id }}</span></template></el-table-column>
        <el-table-column label="用户" width="90"><template #default="{ row }"><IdLink kind="user" :id="row.user_id" data-testid="rch-user" /></template></el-table-column>
        <el-table-column label="渠道" width="90"><template #default="{ row }"><span data-testid="rch-channel">{{ labelOf(rechargeChannelLabel, row.channel) }}</span></template></el-table-column>
        <el-table-column label="金额" width="110"><template #default="{ row }"><span data-testid="rch-amount">{{ fmtYuan(row.amount) }}</span></template></el-table-column>
        <el-table-column label="状态" width="90"><template #default="{ row }"><span data-testid="rch-status">{{ labelOf(rechargeStatusLabel, row.status) }}</span></template></el-table-column>
        <el-table-column label="渠道单号" min-width="160"><template #default="{ row }"><span class="mono" data-testid="rch-trade-no">{{ row.channel_trade_no || '—' }}</span></template></el-table-column>
        <el-table-column label="操作人" width="80"><template #default="{ row }"><span data-testid="rch-operator">{{ row.operator_id ?? '—' }}</span></template></el-table-column>
        <el-table-column label="备注" min-width="140"><template #default="{ row }"><span data-testid="rch-remark">{{ row.remark }}</span></template></el-table-column>
        <el-table-column label="创建时间" width="160"><template #default="{ row }"><span data-testid="rch-created">{{ fmtTime(row.created_at) }}</span></template></el-table-column>
        <el-table-column label="支付时间" width="160"><template #default="{ row }"><span data-testid="rch-paid">{{ fmtTime(row.paid_at) || '—' }}</span></template></el-table-column>
      </el-table>
    </template>
  </FinancePage>
</template>

<style scoped>
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
</style>
