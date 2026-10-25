<script setup lang="ts">
import { deploymentStateLabel, deploymentStateType, isApiError, labelOf, messageOf, workflowErrorLabel, type Deployment, type Instance, type Listed, type WorkflowCatalogItem } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { api, installWorkflow } from '../api'
import {
  canInstallTo, INSTALL_SCAN_CONCURRENCY, INSTALL_SCAN_LIMIT, installCandidates, installConfirmText, installTargets, mapLimit, MAX_WORKFLOWS, targetHint, type InstallTarget,
} from '../logic/workflows'
import { notifyError } from '../logic/notify'
import AppDialog from './AppDialog.vue'

/**
 * 「一键安装」弹窗：列出当前用户实例上的 ComfyUI 部署（GET /instances，再对候选实例 GET /instances/{id}/apps：≤ 20 台、并发 ≤ 6），
 * 选一个 → POST …/workflows → 跳实例详情（?highlight=workflows&dep=…）。「新开一台实例」→ emit('rent')，由父组件打开 RentDialog。
 * 冲突类错误（已装 / 到上限 / 部署状态变了 / 部署没了）保留文案并重新拉目标，让列表反映最新状态。
 */
const props = defineProps<{ workflow: Pick<WorkflowCatalogItem, 'id' | 'slug' | 'name' | 'version'> }>()
const visible = defineModel<boolean>({ required: true })
const emit = defineEmits<{ rent: [workflowId: number] }>()
const router = useRouter()

const loading = ref(false)
const targets = ref<InstallTarget[]>([])
const partial = ref(false) // 有实例的应用列表没拉到
const truncated = ref(false) // 实例超过 INSTALL_SCAN_LIMIT 台，只查了最近的
const loadError = ref('') // GET /instances 失败
const selected = ref<string | null>(null) // deploymentId
const error = ref('')
const submitting = ref(false)
let seq = 0

const selectedTarget = computed(() => targets.value.find((t) => t.deploymentId === selected.value) ?? null)
const confirmText = computed(() => installConfirmText(props.workflow))

/** 拉目标列表；keepError：提交失败后的刷新，保留错误文案。 */
async function load(keepError = false) {
  const mine = ++seq
  loading.value = true
  targets.value = []
  partial.value = false
  truncated.value = false
  loadError.value = ''
  selected.value = null
  if (!keepError) error.value = ''
  try {
    const c = installCandidates((await api.get<Listed<Instance>>('/instances')).items)
    const results = await mapLimit(c.instances, INSTALL_SCAN_CONCURRENCY, (i) =>
      api.get<Listed<Deployment>>(`/instances/${i.id}/apps`).then((r) => ({ instance: i, deployments: r.items })))
    if (mine !== seq) return
    truncated.value = c.truncated
    partial.value = results.some((r) => r.status === 'rejected')
    targets.value = installTargets(results.flatMap((r) => (r.status === 'fulfilled' ? [r.value] : [])), props.workflow.id)
    selected.value = targets.value.find((t) => canInstallTo(t).ok)?.deploymentId ?? null // 默认选第一个能装的
  } catch (e) {
    if (mine !== seq) return
    if (isApiError(e) && e.code === 'unauthorized') return // 已由会话处理跳登录
    loadError.value = messageOf(e)
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
    await router.push({ path: `/instances/${t.instanceId}`, query: { highlight: 'workflows', dep: t.deploymentId, workflow: props.workflow.slug } }) // 实例详情按 dep 滚到该部署的工作流子区块
  } catch (e) {
    // workflow_* / app_invalid_state / 限流等业务错误显示在弹窗里（后端文案为准，缺省用 workflowErrorLabel）
    if (isApiError(e) && e.status >= 400 && e.status < 500 && e.status !== 401) {
      error.value = e.message || labelOf(workflowErrorLabel, e.code)
      if (STALE_CODES.has(e.code)) void load(true)
    } else notifyError(e)
  } finally {
    submitting.value = false
  }
}
/** 说明目标列表已过时的错误码：刷新列表。 */
const STALE_CODES = new Set(['workflow_exists', 'workflow_limit', 'app_invalid_state', 'not_found'])

function rentNew() {
  if (submitting.value) return
  visible.value = false
  emit('rent', props.workflow.id)
}
</script>

<template>
  <AppDialog v-model="visible" kind="form" :title="`安装「${workflow.name}」`" data-testid="wf-install-dialog">
    <div v-if="loading" class="muted" data-testid="wf-install-loading">正在查找您实例上的 ComfyUI…</div>
    <el-alert v-else-if="loadError" type="error" :title="`实例列表加载失败：${loadError}`" :closable="false" show-icon data-testid="wf-install-load-error">
      <el-button size="small" data-testid="wf-install-retry" @click="load()">重试</el-button>
    </el-alert>
    <template v-else>
      <p v-if="!targets.length" class="muted" data-testid="wf-install-empty">您的实例上还没有可用的 ComfyUI。可以新开一台实例，开通时预装 ComfyUI 与该工作流。</p>
      <div v-else class="targets" role="radiogroup" aria-label="选择安装到哪个 ComfyUI">
        <button
          v-for="t in targets" :key="t.deploymentId" type="button" role="radio" class="target" :class="{ picked: t.deploymentId === selected, off: !canInstallTo(t).ok }"
          :aria-checked="t.deploymentId === selected" :aria-disabled="canInstallTo(t).ok ? undefined : 'true'" :data-testid="`wf-target-${t.deploymentId}`" @click="pick(t)"
        >
          <span class="t-name">{{ t.instanceName }}</span>
          <span class="t-meta">
            <el-tag size="small" :type="labelOf(deploymentStateType, t.state) as 'primary'">ComfyUI {{ labelOf(deploymentStateLabel, t.state) }}</el-tag>
            <span title="含待安装与安装失败的工作流">已添加 {{ t.installedCount }}/{{ MAX_WORKFLOWS }}</span>
            <span v-if="!canInstallTo(t).ok" class="reason">{{ canInstallTo(t).reason }}</span>
            <span v-else-if="targetHint(t.state)" class="hint">{{ targetHint(t.state) }}</span>
          </span>
        </button>
      </div>
      <p v-if="truncated" class="muted" data-testid="wf-install-truncated">只列出最近的 {{ INSTALL_SCAN_LIMIT }} 台实例；其它实例请到实例详情的「应用」卡片里添加工作流。</p>
      <p v-if="partial" class="muted" data-testid="wf-install-partial">部分实例的应用列表加载失败，未全部列出。</p>
      <el-alert type="warning" :title="confirmText" :closable="false" show-icon data-testid="wf-install-text" />
      <el-alert v-if="error" type="error" :title="error" :closable="false" show-icon data-testid="wf-install-error" class="err" />
    </template>
    <template #footer>
      <el-button :disabled="submitting" data-testid="wf-install-cancel" @click="visible = false">取消</el-button>
      <el-button :disabled="submitting" data-testid="wf-rent-new" @click="rentNew">新开一台实例</el-button>
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
.hint { color: #909399; }
.muted { color: #909399; font-size: 13px; }
.err { margin-top: 8px; }
</style>
