<script setup lang="ts">
import { messageOf, type InstalledWorkflow, type WorkflowCatalogItem } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, ref, watch } from 'vue'
import { fetchWorkflows, installWorkflow } from '../api'
import { workflowAddConfirmText } from '../logic/apps'
import { notifyError } from '../logic/notify'
import AppDialog from './AppDialog.vue'
import WorkflowPicker from './WorkflowPicker.vue'

/**
 * 实例详情工作流子区块的「添加工作流」：列出工作流市场（GET /workflows），已装的灰显；选中 → 确认文案（重启 ComfyUI、下载量）→
 * POST …/workflows。成功或失败都 emit('done') 让应用卡片重新拉部署列表（失败可能是别处已装 / 到上限）。
 * 列表用 WorkflowPicker 单选模式（规格 §4：市场页式选择器，灰显已装的）。
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

const chosen = computed(() => items.value.find((i) => i.id === selected.value) ?? null)
const installedIds = computed(() => props.installed.map((w) => w.workflow_id))

async function load() {
  const mine = ++seq
  loading.value = true
  loadError.value = ''
  try {
    const res = await fetchWorkflows()
    if (mine !== seq) return
    items.value = res
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
  selected.value = i.id // 已装的由选择器拦掉，不会 emit
}

async function confirm() {
  const item = chosen.value
  if (!item || submitting.value) return
  submitting.value = true
  try {
    await installWorkflow(props.instanceId, props.deploymentId, item.id)
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
      <WorkflowPicker mode="single" :items="items" :disabled-ids="installedIds" :selected-id="selected" testid="wf-add-item" @select="pick" />
    </div>
    <p v-if="chosen" class="warn" data-testid="wf-add-confirm-text">{{ workflowAddConfirmText(chosen) }}</p>
    <template #footer>
      <el-button :disabled="submitting" @click="visible = false">取消</el-button>
      <el-button type="primary" :disabled="!chosen" :loading="submitting" data-testid="wf-add-confirm" @click="confirm">安装</el-button>
    </template>
  </AppDialog>
</template>

<style scoped>
.list { max-height: 50vh; overflow: auto; }
.muted { color: #909399; }
.warn { color: var(--el-color-warning); margin: 12px 0 0; }
</style>
