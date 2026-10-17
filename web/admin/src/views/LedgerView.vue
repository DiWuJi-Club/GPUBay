<script setup lang="ts">
import { fmtTime, fmtYuan, labelOf, ledgerKindLabel, type AdminLedgerRow } from '@spark/shared'
import FinancePage from '../components/FinancePage.vue'
import IdLink from '../components/IdLink.vue'

const kinds = Object.entries(ledgerKindLabel).map(([value, label]) => ({ value, label }))
const signed = (n: number) => (n >= 0 ? `+${fmtYuan(n)}` : fmtYuan(n))
</script>

<template>
  <FinancePage endpoint="ledger" kind-param="kind" kind-label="类型" :kind-options="kinds" :status-options="[]">
    <template #table="{ items }">
      <el-table :data="items as unknown as AdminLedgerRow[]" row-key="id">
        <el-table-column label="流水 ID" width="90"><template #default="{ row }"><span class="mono" data-testid="led-id">{{ row.id }}</span></template></el-table-column>
        <el-table-column label="用户" width="90"><template #default="{ row }"><IdLink kind="user" :id="row.user_id" data-testid="led-user" /></template></el-table-column>
        <el-table-column label="类型" width="80"><template #default="{ row }"><span data-testid="led-kind">{{ labelOf(ledgerKindLabel, row.kind) }}</span></template></el-table-column>
        <el-table-column label="金额" width="120">
          <template #default="{ row }"><span data-testid="led-amount" :class="row.amount >= 0 ? 'money-plus' : 'money-minus'">{{ signed(row.amount) }}</span></template>
        </el-table-column>
        <el-table-column label="变动后余额" width="120"><template #default="{ row }"><span data-testid="led-after">{{ fmtYuan(row.balance_after) }}</span></template></el-table-column>
        <el-table-column label="关联" min-width="200"><template #default="{ row }"><span class="mono" data-testid="led-ref">{{ [row.ref_type, row.ref_id].filter(Boolean).join(' ') || '—' }}</span></template></el-table-column>
        <el-table-column label="操作人" width="80"><template #default="{ row }"><span data-testid="led-operator">{{ row.operator_id ?? '—' }}</span></template></el-table-column>
        <el-table-column label="备注" min-width="160"><template #default="{ row }"><span data-testid="led-remark">{{ row.remark }}</span></template></el-table-column>
        <el-table-column label="时间" width="160"><template #default="{ row }"><span data-testid="led-time">{{ fmtTime(row.created_at) }}</span></template></el-table-column>
      </el-table>
    </template>
  </FinancePage>
</template>

<style scoped>
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
</style>
