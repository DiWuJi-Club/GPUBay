<script setup lang="ts">
import type { useBookingOps } from '../composables/useBookingOps'

/** 预订的改派 / 取消预订弹窗：状态与提交逻辑在 useBookingOps，预订列表与实例详情共用这一份标记。 */
defineProps<{ ops: ReturnType<typeof useBookingOps> }>()
</script>

<template>
  <el-dialog v-model="ops.reassign.visible" title="改派" width="460" align-center data-testid="reassign-dialog">
    <p class="muted">留空机器 ID 由系统在同机组内自动挑选（只挑可见机组）；指定机器须空闲 / 已分配且所需时段无冲突，可以是任意机组（含仅后台可见的留池），不要求此刻在线——开通时再判断。</p>
    <el-form label-position="top" @submit.prevent="ops.submitReassign">
      <el-form-item label="目标机器 ID（可空）"><el-input :model-value="ops.reassign.machineId" data-testid="reassign-machine" @update:model-value="ops.setReassignMachine" /></el-form-item>
      <el-form-item label="原因（必填）"><el-input v-model="ops.reassign.reason" data-testid="reassign-reason" /></el-form-item>
      <el-button type="primary" native-type="submit" :loading="ops.reassign.busy" data-testid="reassign-submit">确定改派</el-button>
    </el-form>
  </el-dialog>
  <el-dialog v-model="ops.cancel.visible" title="取消预订" width="460" align-center data-testid="cancel-booking-dialog">
    <p class="muted">平台原因取消：全额退款到用户余额，并发短信通知；需要动态码。</p>
    <el-form label-position="top" @submit.prevent="ops.submitCancel">
      <el-form-item label="原因（必填，写入审计）"><el-input v-model="ops.cancel.reason" data-testid="cancel-booking-reason" /></el-form-item>
      <el-button type="danger" native-type="submit" :loading="ops.cancel.busy" data-testid="cancel-booking-submit">取消并退款</el-button>
    </el-form>
  </el-dialog>
</template>

<style scoped>
.muted { color: #909399; font-size: 13px; }
</style>
