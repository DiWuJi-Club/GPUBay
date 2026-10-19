<script setup lang="ts">
import type { AdminDeployment } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, reactive, watch } from 'vue'
import { api } from '../api'
import { FORCE_REMOVE_REASON_MAX, runeCount, validateForceRemoveReason } from '../logic/deployments'
import { notifyError } from '../logic/notify'

/** 强制卸载（ops，原因必填、≤ 200 字，不要动态码）：部署列表抽屉与实例详情「应用」卡片共用。target 非空即打开。 */
const props = defineProps<{ target: Pick<AdminDeployment, 'id' | 'app_name' | 'instance_id'> | null }>()
const emit = defineEmits<{ close: []; done: [d: AdminDeployment] }>()

const form = reactive({ reason: '', error: '', busy: false })
watch(
  () => props.target?.id,
  (id) => {
    if (id) Object.assign(form, { reason: '', error: '' })
  },
)
const count = computed(() => runeCount(form.reason.trim()))

async function submit() {
  const t = props.target
  if (!t || form.busy) return // 同步守卫：连点只发一次
  form.error = validateForceRemoveReason(form.reason) ?? ''
  if (form.error) return
  form.busy = true
  try {
    const d = await api.post<AdminDeployment>(`/deployments/${t.id}/force-remove`, { reason: form.reason.trim() })
    ElMessage.success(d.state === 'removing' ? '已开始卸载' : '已卸载')
    emit('done', d)
  } catch (e) {
    notifyError(e) // 弹窗保留，原因不丢
  } finally {
    form.busy = false
  }
}
</script>

<template>
  <el-dialog :model-value="!!props.target" title="强制卸载应用" width="480" align-center data-testid="deployment-force-remove-dialog" @close="emit('close')">
    <template v-if="props.target">
      <p data-testid="deployment-force-remove-text">
        将卸载实例 <span class="mono">{{ props.target.instance_id }}</span> 上的「{{ props.target.app_name }}」：停止服务、删除应用目录与应用端口，模型缓存保留。
        原因会以「平台卸载：&lt;原因&gt;」显示在用户的部署动态里，并写入审计。
      </p>
      <el-form label-position="top" @submit.prevent="submit">
        <el-form-item :label="`原因（必填，最多 ${FORCE_REMOVE_REASON_MAX} 字）`">
          <el-input v-model="form.reason" type="textarea" :rows="3" data-testid="deployment-force-remove-reason" />
        </el-form-item>
        <p class="count" :class="{ over: count > FORCE_REMOVE_REASON_MAX }" data-testid="deployment-force-remove-count">{{ count }} / {{ FORCE_REMOVE_REASON_MAX }}</p>
        <p v-if="form.error" class="error" data-testid="deployment-force-remove-error">{{ form.error }}</p>
        <div class="buttons">
          <el-button data-testid="deployment-force-remove-cancel" @click="emit('close')">取消</el-button>
          <el-button type="danger" native-type="submit" :loading="form.busy" data-testid="deployment-force-remove-submit">强制卸载</el-button>
        </div>
      </el-form>
    </template>
  </el-dialog>
</template>

<style scoped>
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.count { margin: -8px 0 8px; text-align: right; color: #909399; font-size: 12px; }
.count.over { color: var(--el-color-danger); }
.error { color: var(--el-color-danger); font-size: 13px; }
.buttons { display: flex; justify-content: flex-end; gap: 8px; }
</style>
