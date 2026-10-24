<script setup lang="ts">
import { deploymentStateLabel, deploymentStateType, isApiError, labelOf, workflowErrorLabel, type Deployment, type Instance, type Listed, type WorkflowCatalogItem } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { api, installWorkflow } from '../api'
import { canInstallTo, installCandidates, installConfirmText, installTargets, MAX_WORKFLOWS, type InstallTarget } from '../logic/workflows'
import { notifyError } from '../logic/notify'
import AppDialog from './AppDialog.vue'

/**
 * 「一键安装」弹窗：列出当前用户实例上的 ComfyUI 部署（GET /instances，再对候选实例并行 GET /instances/{id}/apps，≤ 20 台），
 * 选一个 → POST …/workflows → 跳实例详情（?highlight=workflows）。「新开一台实例」→ emit('rent')，由父组件打开 RentDialog。
 */
const props = defineProps<{ workflow: Pick<WorkflowCatalogItem, 'id' | 'slug' | 'name' | 'version'> }>()
const visible = defineModel<boolean>({ required: true })
const emit = defineEmits<{ rent: [workflowId: number] }>()
const router = useRouter()

const loading = ref(false)
const targets = ref<InstallTarget[]>([])
const partial = ref(false) // 有实例的应用列表没拉到
const selected = ref<string | null>(null) // deploymentId
const error = ref('')
const submitting = ref(false)
let seq = 0

const selectedTarget = computed(() => targets.value.find((t) => t.deploymentId === selected.value) ?? null)
const confirmText = computed(() => installConfirmText(props.workflow))

async function load() {
  const mine = ++seq
  loading.value = true
  targets.value = []
  partial.value = false
  selected.value = null
  error.value = ''
  try {
    const instances = installCandidates((await api.get<Listed<Instance>>('/instances')).items)
    const results = await Promise.allSettled(instances.map((i) => api.get<Listed<Deployment>>(`/instances/${i.id}/apps`).then((r) => ({ instance: i, deployments: r.items }))))
    if (mine !== seq) return
    partial.value = results.some((r) => r.status === 'rejected')
    targets.value = installTargets(results.flatMap((r) => (r.status === 'fulfilled' ? [r.value] : [])), props.workflow.id)
    selected.value = targets.value.find((t) => canInstallTo(t).ok)?.deploymentId ?? null // 默认选第一个能装的
  } catch (e) {
    if (mine !== seq) return
    notifyError(e)
  } finally {
    if (mine === seq) loading.value = false
  }
}
watch(visible, (v) => { if (v) void load() }, { immediate: true })

function pick(t: InstallTarget) {
  if (!canInstallTo(t).ok) return
  selected.value = t.deploymentId
  error.value = ''
}

async function submit() {
  const t = selectedTarget.value
  if (!t || submitting.value) return
  submitting.value = true
  error.value = ''
  try {
    await installWorkflow(t.instanceId, t.deploymentId, props.workflow.id)
    ElMessage.success(`已开始安装 ${props.workflow.name}，ComfyUI 将重启`)
    visible.value = false
    await router.push({ path: `/instances/${t.instanceId}`, query: { highlight: 'workflows', dep: t.deploymentId, workflow: props.workflow.slug } })
  } catch (e) {
    // workflow_* / app_invalid_state / 限流等业务错误显示在弹窗里（后端文案为准，缺省用 workflowErrorLabel）
    if (isApiError(e) && e.status >= 400 && e.status < 500 && e.status !== 401) error.value = e.message || labelOf(workflowErrorLabel, e.code)
    else notifyError(e)
  } finally {
    submitting.value = false
  }
}

function rentNew() {
  visible.value = false
  emit('rent', props.workflow.id)
}
</script>

<template>
  <AppDialog v-model="visible" kind="form" :title="`安装「${workflow.name}」`" data-testid="wf-install-dialog">
    <div v-if="loading" class="muted" data-testid="wf-install-loading">正在查找您实例上的 ComfyUI…</div>
    <template v-else>
      <p v-if="!targets.length" class="muted" data-testid="wf-install-empty">您的实例上还没有可用的 ComfyUI。可以新开一台实例，开通时预装 ComfyUI 与该工作流。</p>
      <div v-else class="targets">
        <button
          v-for="t in targets" :key="t.deploymentId" type="button" class="target" :class="{ picked: t.deploymentId === selected, off: !canInstallTo(t).ok }"
          :aria-pressed="t.deploymentId === selected" :aria-disabled="canInstallTo(t).ok ? undefined : 'true'" :data-testid="`wf-target-${t.deploymentId}`" @click="pick(t)"
        >
          <span class="t-name">{{ t.instanceName }}</span>
          <span class="t-meta">
            <el-tag size="small" :type="labelOf(deploymentStateType, t.state) as 'primary'">ComfyUI {{ labelOf(deploymentStateLabel, t.state) }}</el-tag>
            <span>已装 {{ t.installedCount }}/{{ MAX_WORKFLOWS }}</span>
            <span v-if="!canInstallTo(t).ok" class="reason">{{ canInstallTo(t).reason }}</span>
          </span>
        </button>
      </div>
      <p v-if="partial" class="muted" data-testid="wf-install-partial">部分实例的应用列表加载失败，未全部列出。</p>
      <el-alert type="warning" :title="confirmText" :closable="false" show-icon data-testid="wf-install-text" />
      <el-alert v-if="error" type="error" :title="error" :closable="false" show-icon data-testid="wf-install-error" class="err" />
    </template>
    <template #footer>
      <el-button data-testid="wf-rent-new" @click="rentNew">新开一台实例</el-button>
      <el-button type="primary" :loading="submitting" :disabled="loading || !selectedTarget" data-testid="wf-install-confirm" @click="submit">安装</el-button>
    </template>
  </AppDialog>
</template>

<style scoped>
.targets { display: flex; flex-direction: column; gap: 8px; margin-bottom: 12px; }
.target { display: flex; flex-direction: column; gap: 4px; text-align: left; padding: 10px 12px; border: 1px solid #dcdfe6; border-radius: 6px; background: #fff; cursor: pointer; font: inherit; color: inherit; }
.target:hover:not(.off) { border-color: var(--el-color-primary); }
.target.picked { border-color: var(--el-color-primary); background: var(--el-color-primary-light-9); }
.target.off { cursor: not-allowed; background: #f5f7fa; color: #909399; }
.t-name { font-weight: 600; font-size: 13px; word-break: break-all; }
.t-meta { display: flex; flex-wrap: wrap; align-items: center; gap: 8px; font-size: 12px; color: #606266; }
.reason { color: var(--el-color-warning); }
.muted { color: #909399; font-size: 13px; }
.err { margin-top: 8px; }
</style>
