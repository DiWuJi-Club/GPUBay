<script setup lang="ts">
import { formatGB, labelOf, messageOf, workflowCategoryLabel, type WorkflowCatalogItem } from '@spark/shared'
import { computed, onMounted, ref, watch } from 'vue'
import { fetchWorkflows } from '../api'
import { MAX_WORKFLOWS, sortWorkflows, toggleWorkflow, totalDownloadGB } from '../logic/workflows'

/**
 * 工作流选择器（市场页式的小列表）。
 * - mode = 'multiple'（内嵌多选，RentDialog「预装工作流」）：v-model 为选中的工作流 ID（按选择顺序），最多 max 个，底部显示合计下载量。
 * - mode = 'single'（实例详情「添加工作流」）：点一项 emit('select', item)，v-model 不用。
 * disabledIds 灰显（如已装在该 ComfyUI 上的）。items 不传时自己拉 GET /workflows；拉到后 emit('loaded')。
 * testid 为 data-testid 前缀：每项 `<testid>-<slug>`，合计 `<testid>-total`。
 */
const props = withDefaults(defineProps<{
  mode?: 'multiple' | 'single'
  max?: number
  disabledIds?: number[]
  items?: WorkflowCatalogItem[] | null
  testid?: string
}>(), { mode: 'multiple', max: MAX_WORKFLOWS, disabledIds: () => [], items: null, testid: 'wf-pick' })
const model = defineModel<number[]>({ default: () => [] })
const emit = defineEmits<{ select: [item: WorkflowCatalogItem]; loaded: [items: WorkflowCatalogItem[]] }>()

const own = ref<WorkflowCatalogItem[]>([])
const loading = ref(false)
const error = ref('')
const list = computed(() => sortWorkflows(props.items ?? own.value))
const total = computed(() => totalDownloadGB(list.value, model.value))
const full = computed(() => props.mode === 'multiple' && model.value.length >= props.max)

async function load() {
  if (props.items) return
  loading.value = true
  error.value = ''
  try {
    own.value = await fetchWorkflows()
    emit('loaded', own.value)
  } catch (e) {
    error.value = messageOf(e)
  } finally {
    loading.value = false
  }
}
onMounted(load)
watch(() => props.items, (v) => { if (!v && !own.value.length) void load() })

const isDisabled = (w: WorkflowCatalogItem) => props.disabledIds.includes(w.id)
const picked = (w: WorkflowCatalogItem) => props.mode === 'multiple' && model.value.includes(w.id)
/** 不可选的原因：已装；多选已满（已选中的仍可取消）。 */
function reason(w: WorkflowCatalogItem): string {
  if (isDisabled(w)) return '已安装'
  if (full.value && !picked(w)) return `最多 ${props.max} 个`
  return ''
}
function pick(w: WorkflowCatalogItem) {
  if (reason(w)) return
  if (props.mode === 'single') emit('select', w)
  else model.value = toggleWorkflow(model.value, w.id, props.max)
}
</script>

<template>
  <div class="wf-picker" :data-testid="testid">
    <div v-if="loading" class="muted">正在加载工作流…</div>
    <div v-else-if="error" class="muted">{{ error }} <el-button link type="primary" :data-testid="`${testid}-retry`" @click="load">重试</el-button></div>
    <div v-else-if="!list.length" class="muted" :data-testid="`${testid}-empty`">暂无可安装的工作流</div>
    <template v-else>
      <button
        v-for="w in list" :key="w.id" type="button" class="item" :class="{ picked: picked(w), off: !!reason(w) }"
        :aria-pressed="mode === 'multiple' ? picked(w) : undefined" :aria-disabled="reason(w) ? 'true' : undefined" :data-testid="`${testid}-${w.slug}`" @click="pick(w)"
      >
        <span v-if="mode === 'multiple'" class="check" aria-hidden="true">{{ picked(w) ? '✓' : '' }}</span>
        <span class="body">
          <span class="name">{{ w.name }}</span>
          <span class="meta">{{ labelOf(workflowCategoryLabel, w.category) }} · 约 {{ formatGB(w.version.download_gb) }}<template v-if="w.version.quick_run"> · 快速出图</template></span>
          <span v-if="reason(w)" class="reason">{{ reason(w) }}</span>
        </span>
      </button>
      <div v-if="mode === 'multiple'" class="total" :data-testid="`${testid}-total`">已选 {{ model.length }}/{{ max }}<template v-if="model.length"> · 合计需下载约 {{ formatGB(total) }}</template></div>
    </template>
  </div>
</template>

<style scoped>
.wf-picker { display: grid; gap: 6px; grid-template-columns: repeat(auto-fill, minmax(200px, 1fr)); width: 100%; }
.item { display: flex; gap: 8px; align-items: flex-start; text-align: left; padding: 8px; border: 1px solid #dcdfe6; border-radius: 6px; background: #fff; cursor: pointer; font: inherit; color: inherit; }
.item:hover:not(.off) { border-color: var(--el-color-primary); }
.item.picked { border-color: var(--el-color-primary); background: var(--el-color-primary-light-9); }
.item.off { cursor: not-allowed; background: #f5f7fa; color: #c0c4cc; }
.check { flex: none; width: 16px; height: 16px; margin-top: 2px; border: 1px solid #dcdfe6; border-radius: 3px; font-size: 12px; line-height: 14px; text-align: center; color: #fff; }
.item.picked .check { background: var(--el-color-primary); border-color: var(--el-color-primary); }
.body { display: flex; flex-direction: column; min-width: 0; }
.name { font-weight: 600; font-size: 13px; }
.meta { font-size: 12px; color: #909399; }
.reason { font-size: 12px; color: var(--el-color-warning); }
.total { grid-column: 1 / -1; font-size: 13px; color: #606266; }
.muted { grid-column: 1 / -1; color: #909399; font-size: 13px; }
</style>
