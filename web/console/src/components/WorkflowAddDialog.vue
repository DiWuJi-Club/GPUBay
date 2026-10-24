<script setup lang="ts">
import { formatGB, labelOf, messageOf, workflowCategoryLabel, type InstalledWorkflow, type WorkflowCatalogItem } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, ref, watch } from 'vue'
import { addDeploymentWorkflow, panelWorkflowCatalog } from '../api'
import { addWorkflowReason, workflowAddConfirmText } from '../logic/apps'
import { notifyError } from '../logic/notify'
import AppDialog from './AppDialog.vue'

/**
 * 实例详情工作流子区块的「添加工作流」：列出工作流市场（GET /workflows），已装的灰显；选中 → 确认文案（重启 ComfyUI、下载量）→
 * POST …/workflows。成功或失败都 emit('done') 让应用卡片重新拉部署列表（失败可能是别处已装 / 到上限）。
 * 市场页的 WorkflowPicker（Task 12）面向下单，这里只需单选，单独实现。
 */
const props = defineProps<{ instanceId: string; deploymentId: string; installed: InstalledWorkflow[] }>()
const visible = defineModel<boolean>({ required: true })
const emit = defineEmits<{ done: [] }>()

const items = ref<WorkflowCatalogItem[]>([])
const loading = ref(false)
const loadError = ref('')
const selected = ref<number | null>(null)
const submitting = ref(false)
let seq = 0

const sorted = computed(() => [...items.value].sort((a, b) => a.sort - b.sort || a.name.localeCompare(b.name, 'zh-CN')))
const chosen = computed(() => items.value.find((i) => i.id === selected.value) ?? null)
const reason = (i: WorkflowCatalogItem) => addWorkflowReason(i, props.installed)

async function load() {
  const mine = ++seq
  loading.value = true
  loadError.value = ''
  try {
    const res = await panelWorkflowCatalog()
    if (mine !== seq) return
    items.value = res.items
  } catch (e) {
    if (mine === seq) loadError.value = messageOf(e)
  } finally {
    if (mine === seq) loading.value = false
  }
}
watch(visible, (v) => {
  selected.value = null
  if (v) void load()
}, { immediate: true })

function pick(i: WorkflowCatalogItem) {
  if (!reason(i)) selected.value = i.id
}

async function confirm() {
  const item = chosen.value
  if (!item || submitting.value) return
  submitting.value = true
  try {
    await addDeploymentWorkflow(props.instanceId, props.deploymentId, item.id)
    ElMessage.success(`正在安装 ${item.name}`)
    visible.value = false
  } catch (e) {
    notifyError(e)
  } finally {
    submitting.value = false
    emit('done')
  }
}
</script>

<template>
  <AppDialog v-model="visible" kind="form" title="添加工作流" data-testid="wf-add-dialog">
    <el-skeleton v-if="loading" :rows="4" animated />
    <el-alert v-else-if="loadError" type="error" :title="`工作流列表加载失败：${loadError}`" show-icon :closable="false" />
    <p v-else-if="!items.length" class="muted" data-testid="wf-add-empty">工作流市场暂时没有可安装的工作流</p>
    <div v-else class="list">
      <div
        v-for="i in sorted" :key="i.id" class="item" :class="{ disabled: !!reason(i), active: selected === i.id }" role="radio"
        :aria-checked="selected === i.id" :aria-disabled="!!reason(i)" :tabindex="reason(i) ? -1 : 0"
        :data-testid="`wf-add-item-${i.slug}`" :data-disabled="String(!!reason(i))" @click="pick(i)" @keydown.enter.space.prevent="pick(i)"
      >
        <div class="head">
          <b>{{ i.name }}</b>
          <el-tag size="small" type="info">{{ labelOf(workflowCategoryLabel, i.category) }}</el-tag>
          <el-tag v-if="i.version.quick_run" size="small" type="success">支持快速出图</el-tag>
          <span v-if="reason(i)" class="muted small">{{ reason(i) }}</span>
        </div>
        <div v-if="i.summary" class="muted small">{{ i.summary }}</div>
        <div class="muted small">
          v{{ i.version.version }} · 需下载约 {{ formatGB(i.version.download_gb) }}<template v-if="i.version.nodes"> · 含 {{ i.version.nodes }} 个自定义节点</template>
        </div>
      </div>
    </div>
    <p v-if="chosen" class="warn" data-testid="wf-add-confirm-text">{{ workflowAddConfirmText(chosen) }}</p>
    <template #footer>
      <el-button :disabled="submitting" @click="visible = false">取消</el-button>
      <el-button type="primary" :disabled="!chosen" :loading="submitting" data-testid="wf-add-confirm" @click="confirm">安装</el-button>
    </template>
  </AppDialog>
</template>

<style scoped>
.list { display: flex; flex-direction: column; gap: 8px; max-height: 50vh; overflow: auto; }
.item { padding: 8px 12px; border: 1px solid var(--el-border-color); border-radius: 6px; cursor: pointer; }
.item.active { border-color: var(--el-color-primary); background: var(--el-color-primary-light-9); }
.item.disabled { cursor: not-allowed; opacity: 0.55; }
.head { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.muted { color: #909399; }
.small { font-size: 12px; margin: 2px 0; }
.warn { color: var(--el-color-warning); margin: 12px 0 0; }
</style>
