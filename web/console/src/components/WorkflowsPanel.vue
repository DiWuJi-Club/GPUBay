<script setup lang="ts">
import { labelOf, workflowStateLabel, workflowStateType, type Deployment, type InstalledWorkflow, type Instance } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, ref } from 'vue'
import { fetchWorkflow, removeDeploymentWorkflow, updateDeploymentWorkflow } from '../api'
import {
  MAX_WORKFLOWS, canChangeWorkflows, deploymentWorkflows, workflowChangeBlockReason, workflowPanelNotice, workflowRemoveConfirmText, workflowRowActions,
  workflowUpdateConfirmText,
} from '../logic/apps'
import { notifyError } from '../logic/notify'
import AppDialog from './AppDialog.vue'
import QuickRunDrawer from './QuickRunDrawer.vue'
import WorkflowAddDialog from './WorkflowAddDialog.vue'

/**
 * 实例详情「应用」卡片里 ComfyUI 部署行下方的「工作流」子区块（规格 §4）：已装工作流（版本、有新版本、状态）、
 * 打开 ComfyUI、快速出图、更新 / 移除（D8、D9：都会重启 ComfyUI）、添加工作流（≤ 8 个）。
 * 增删改成功后 emit('refresh') 让 AppsCard 重新拉部署列表（部署会变 queued，进度条沿用应用行的渲染）。
 */
const props = defineProps<{ instance: Instance; deployment: Deployment }>()
const emit = defineEmits<{ refresh: [] }>()

const list = computed(() => deploymentWorkflows(props.deployment))
const notice = computed(() => workflowPanelNotice(props.deployment))
const canChange = computed(() => canChangeWorkflows(props.deployment))
const atLimit = computed(() => list.value.length >= MAX_WORKFLOWS)
/** 添加 / 更新 / 移除都会重新部署：实例非运行时（排队中的部署除外）置灰并说明原因，同应用卡片的「重新部署」 */
const blockReason = computed(() => workflowChangeBlockReason(props.deployment, props.instance.state))
const actions = (w: InstalledWorkflow) => workflowRowActions(w, props.deployment, props.instance.state)

function openComfy() {
  if (props.deployment.url) window.open(props.deployment.url, '_blank', 'noopener')
}

// ---- 添加 ----
const addVisible = ref(false)

// ---- 快速出图 ----
const qrVisible = ref(false)
const qrTarget = ref<InstalledWorkflow | null>(null)
function openQuickRun(w: InstalledWorkflow) {
  qrTarget.value = w
  qrVisible.value = true
}

// ---- 更新 / 移除（确认后执行） ----
const confirmTarget = ref<{ kind: 'update' | 'remove'; w: InstalledWorkflow } | null>(null)
const changelog = ref('')
const busy = ref(false)
let seq = 0

async function ask(kind: 'update' | 'remove', w: InstalledWorkflow) {
  const mine = ++seq
  confirmTarget.value = { kind, w }
  changelog.value = ''
  if (kind !== 'update' || !w.update_available) return
  // 新版本的更新说明：市场详情里的最新已发布版本正是要更新到的版本时才显示（拿不到不影响更新）
  try {
    const d = await fetchWorkflow(w.slug)
    if (mine === seq && d.version.id === w.latest_version_id) changelog.value = d.version.changelog
  } catch {
    // 忽略：确认文案已含新版本号
  }
}

async function confirmAction() {
  const t = confirmTarget.value
  if (!t || busy.value || blockReason.value) return
  busy.value = true
  try {
    if (t.kind === 'update') {
      // 更新：指定确认框里展示的新版本；重试：明确带当前已装版本（后端对失败的同版本当作重试，绝不换版本——不带 version_id
      // 会取最新已发布版本，已装的是更新的已下线版本时会被换成旧版本）
      await updateDeploymentWorkflow(props.instance.id, props.deployment.id, t.w.workflow_id, t.w.update_available ? t.w.latest_version_id : t.w.version_id)
      ElMessage.success(t.w.update_available ? `正在更新 ${t.w.name}` : `正在重新安装 ${t.w.name}`)
    } else {
      await removeDeploymentWorkflow(props.instance.id, props.deployment.id, t.w.workflow_id)
      ElMessage.success(`正在移除 ${t.w.name}`)
    }
  } catch (e) {
    notifyError(e)
  } finally {
    busy.value = false
    confirmTarget.value = null
    emit('refresh')
  }
}

function onCommand(w: InstalledWorkflow, cmd: string) {
  if (blockReason.value) return
  if (cmd === 'update' || cmd === 'remove') void ask(cmd, w)
}
</script>

<template>
  <div class="wf-panel" data-testid="wf-panel" :data-dep="deployment.id">
    <div class="title">
      <b>工作流</b>
      <span class="muted small">{{ list.length }}/{{ MAX_WORKFLOWS }}</span>
    </div>
    <p v-if="notice" class="muted small" data-testid="wf-installing">{{ notice }}</p>
    <p v-if="!list.length" class="muted small" data-testid="wf-empty">还没有安装工作流；可从工作流市场添加，安装时自动下载模型与节点。</p>

    <div v-for="w in list" :key="w.workflow_id" class="wf-row" :data-testid="`wf-row-${w.slug}`" :data-state="w.state">
      <span class="name">{{ w.name }}</span>
      <span class="muted small">v{{ w.version }}</span>
      <el-tag v-if="w.update_available" type="warning" size="small" :data-testid="`wf-update-tag-${w.slug}`">有新版本 v{{ w.latest_version }}</el-tag>
      <el-tag :type="labelOf(workflowStateType, w.state) as 'primary'" size="small" :data-testid="`wf-state-${w.slug}`">{{ labelOf(workflowStateLabel, w.state) }}</el-tag>
      <template v-if="!notice">
        <el-button v-if="actions(w).open" size="small" :disabled="instance.state !== 'running'" :data-testid="`wf-open-${w.slug}`" @click="openComfy">打开 ComfyUI</el-button>
        <el-button
          v-if="actions(w).quickRun" size="small" type="primary" :disabled="actions(w).quickRunDisabled" :data-testid="`wf-quickrun-${w.slug}`"
          @click="openQuickRun(w)"
        >快速出图</el-button>
        <el-dropdown v-if="actions(w).update || actions(w).remove" trigger="click" @command="(c: string) => onCommand(w, c)">
          <el-button size="small" :data-testid="`wf-more-${w.slug}`">更多 ▾</el-button>
          <template #dropdown>
            <el-dropdown-menu>
              <el-dropdown-item v-if="actions(w).update" command="update" :disabled="!!actions(w).blocked" :data-testid="`wf-update-${w.slug}`">
                {{ actions(w).update === 'update' ? `更新到 v${w.latest_version}` : '重试安装' }}<span v-if="actions(w).blocked" class="reason">（{{ actions(w).blocked }}）</span>
              </el-dropdown-item>
              <el-dropdown-item v-if="actions(w).remove" command="remove" divided :disabled="!!actions(w).blocked" :data-testid="`wf-remove-${w.slug}`">
                移除<span v-if="actions(w).blocked" class="reason">（{{ actions(w).blocked }}）</span>
              </el-dropdown-item>
            </el-dropdown-menu>
          </template>
        </el-dropdown>
      </template>
      <span v-if="w.state === 'failed' && !notice" class="danger small" :data-testid="`wf-failed-${w.slug}`">安装失败，可「重试安装」或在应用「查看日志」里看原因</span>
    </div>

    <template v-if="canChange">
      <div v-if="!atLimit" class="add">
        <el-button size="small" :disabled="!!blockReason" data-testid="wf-add" @click="addVisible = true">添加工作流</el-button>
        <span v-if="blockReason" class="muted small" data-testid="wf-add-blocked">{{ blockReason }}</span>
      </div>
      <p v-else class="muted small" data-testid="wf-limit">每个 ComfyUI 最多安装 {{ MAX_WORKFLOWS }} 个工作流</p>
    </template>

    <WorkflowAddDialog v-model="addVisible" :instance-id="instance.id" :deployment-id="deployment.id" :installed="list" @done="emit('refresh')" />
    <QuickRunDrawer v-model="qrVisible" :instance-id="instance.id" :deployment="deployment" :workflow="qrTarget" />
    <AppDialog
      :model-value="!!confirmTarget" kind="confirm" :title="confirmTarget?.kind === 'remove' ? `移除 ${confirmTarget?.w.name ?? ''}` : `更新 ${confirmTarget?.w.name ?? ''}`"
      data-testid="wf-confirm-dialog" @update:model-value="(v: boolean) => { if (!v && !busy) confirmTarget = null }"
    >
      <template v-if="confirmTarget">
        <p data-testid="wf-confirm-text">
          {{ confirmTarget.kind === 'remove' ? workflowRemoveConfirmText(confirmTarget.w) : workflowUpdateConfirmText(confirmTarget.w) }}
        </p>
        <div v-if="changelog" class="changelog" data-testid="wf-confirm-changelog">
          <div class="muted small">v{{ confirmTarget.w.latest_version }} 更新说明</div>
          <p>{{ changelog }}</p>
        </div>
      </template>
      <template #footer>
        <el-button :disabled="busy" data-testid="wf-confirm-cancel" @click="confirmTarget = null">取消</el-button>
        <el-button :type="confirmTarget?.kind === 'remove' ? 'danger' : 'primary'" :loading="busy" data-testid="wf-confirm-ok" @click="confirmAction">
          {{ confirmTarget?.kind === 'remove' ? '确定移除' : '确定更新' }}
        </el-button>
      </template>
    </AppDialog>
  </div>
</template>

<style scoped>
.wf-panel { margin: 8px 0 4px 32px; padding: 8px 12px; border-left: 2px solid var(--el-border-color-lighter); background: var(--el-fill-color-lighter); border-radius: 4px; }
.title { display: flex; align-items: center; gap: 8px; margin-bottom: 4px; }
.wf-row { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; padding: 4px 0; }
.name { font-weight: 500; }
.muted { color: #909399; }
.small { font-size: 12px; margin: 4px 0; }
.danger { color: var(--el-color-danger); }
.add { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.reason { color: #c0c4cc; font-size: 12px; }
.changelog { margin-top: 8px; padding: 8px; background: var(--el-fill-color-light); border-radius: 4px; }
.changelog p { margin: 4px 0 0; white-space: pre-wrap; }
@media (max-width: 768px) { .wf-panel { margin-left: 0; } }
</style>
