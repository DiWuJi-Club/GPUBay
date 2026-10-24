<script setup lang="ts">
import {
  deploymentErrorText, deploymentPhaseLabel, labelOf, messageOf,
  type AppCatalogItem, type Deployment, type Instance, type Listed,
} from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, onMounted, ref, watch } from 'vue'
import { api } from '../api'
import { usePolling } from '../composables/usePolling'
import {
  MAX_APPS, appOpBlockReason, appRowStatus, canAddApp, deploymentActions, deploymentBusy, instanceAppHint,
} from '../logic/apps'
import { notifyError } from '../logic/notify'
import AppDeployDialog from './AppDeployDialog.vue'
import AppDialog from './AppDialog.vue'
import AppLogDrawer from './AppLogDrawer.vue'

/**
 * 实例详情「应用」卡片：部署列表（进度、打开、操作菜单）、添加 / 重新部署弹窗、日志抽屉、卸载确认。
 * 每次加载后 emit('deployments') 给详情页（「动态」合并部署事件、端口刷新）。
 */
const props = defineProps<{ instance: Instance; apps: AppCatalogItem[] }>()
const emit = defineEmits<{ deployments: [items: Deployment[]] }>()

const items = ref<Deployment[]>([])
const loaded = ref(false)
const loadError = ref('')
let seq = 0

async function load() {
  const mine = ++seq
  try {
    const res = await api.get<Listed<Deployment>>(`/instances/${props.instance.id}/apps`)
    if (mine !== seq) return // 操作后的刷新 / 轮询交错：只采用最后发出的那次
    items.value = res.items
    loaded.value = true
    loadError.value = ''
    emit('deployments', res.items)
  } catch (e) {
    if (mine !== seq) return
    if (!loaded.value) loadError.value = messageOf(e) // 首次加载失败：卡片内提示；轮询失败保留已显示的数据
  }
}
onMounted(load)
// 实例状态变化（开通、停机、续费恢复）时部署状态也会变：立即刷新
watch(() => props.instance.state, () => void load())
// Runner 每 5 秒推进一次：实例运行中且有排队 / 部署中 / 卸载中 / 停止中的部署时同频刷新（页面不可见时暂停）
usePolling(load, 5_000, { enabled: () => props.instance.state === 'running' && items.value.some(deploymentBusy) })

const blockReason = computed(() => appOpBlockReason(props.instance.state))
const hint = computed(() => instanceAppHint(props.instance.state))
const canAdd = computed(() => canAddApp(props.instance.state))
const atLimit = computed(() => items.value.length >= MAX_APPS)

const rowStatus = (d: Deployment) => appRowStatus(d, props.instance.state)
const phaseText = (d: Deployment) => labelOf(deploymentPhaseLabel, d.phase)

function open(url: string) {
  // 私有端口：实例域名没有票据 Cookie 时会自己跳到控制台的 authorize，再带票据回来
  window.open(url, '_blank', 'noopener')
}

// ---- 停止 / 启动 ----
const acting = ref('')
async function op(d: Deployment, action: 'stop' | 'start') {
  if (acting.value) return
  acting.value = d.id
  try {
    await api.post<Deployment>(`/instances/${props.instance.id}/apps/${d.id}/${action}`, {})
    ElMessage.success(action === 'stop' ? `正在停止 ${d.app_name}` : `正在启动 ${d.app_name}`)
  } catch (e) {
    notifyError(e)
  } finally {
    acting.value = ''
    await load()
  }
}

// ---- 添加 / 重新部署 ----
const deployVisible = ref(false)
const deployMode = ref<'add' | 'redeploy'>('add')
const deployTarget = ref<Deployment | null>(null)
function openAdd() {
  deployMode.value = 'add'
  deployTarget.value = null
  deployVisible.value = true
}
function openRedeploy(d: Deployment) {
  deployMode.value = 'redeploy'
  deployTarget.value = d
  deployVisible.value = true
}

// ---- 日志 ----
const logVisible = ref(false)
const logTarget = ref<Deployment | null>(null)
function openLog(d: Deployment) {
  logTarget.value = d
  logVisible.value = true
}

// ---- 卸载 ----
const removeTarget = ref<Deployment | null>(null)
const removing = ref(false)
async function confirmRemove() {
  const d = removeTarget.value
  if (!d || removing.value) return
  removing.value = true
  try {
    const out = await api.del<Deployment>(`/instances/${props.instance.id}/apps/${d.id}`)
    removeTarget.value = null
    ElMessage.success(out?.cleanup_pending ? '已卸载，应用文件会在实例下次启动时清理' : `正在卸载 ${d.app_name}`)
  } catch (e) {
    removeTarget.value = null
    notifyError(e)
  } finally {
    removing.value = false
    await load()
  }
}

function onCommand(d: Deployment, cmd: string) {
  if (cmd === 'redeploy') openRedeploy(d)
  else if (cmd === 'stop' || cmd === 'start') void op(d, cmd)
  else if (cmd === 'log') openLog(d)
  else if (cmd === 'remove') removeTarget.value = d
}
</script>

<template>
  <el-card header="应用" data-testid="detail-apps-card">
    <p class="muted">一键部署常用 AI 应用到这台实例（在实例里以进程运行，不另收费）；部署完成后点「打开」访问，默认只有您登录控制台后能打开。</p>
    <el-alert v-if="loadError" type="error" :title="`应用列表加载失败：${loadError}`" show-icon :closable="false" data-testid="detail-apps-error" />
    <p v-else-if="loaded && !items.length" class="muted" data-testid="detail-apps-empty">还没有部署应用</p>

    <div v-for="d in items" :key="d.id" class="app-row" data-testid="detail-app-row" :data-dep="d.id" :data-state="d.state">
      <div class="row">
        <span class="icon" aria-hidden="true">{{ d.app_name.slice(0, 1) }}</span>
        <b data-testid="detail-app-name">{{ d.app_name }}</b>
        <span class="muted" data-testid="detail-app-version">v{{ d.version }}</span>
        <el-tag v-if="d.update_available" type="warning" size="small" data-testid="detail-app-update">有新版本 v{{ d.latest_version }}</el-tag>
        <el-tag :type="rowStatus(d).type as 'primary'" size="small" data-testid="detail-app-state">{{ rowStatus(d).text }}</el-tag>
        <el-tag v-if="d.state === 'running' && d.health === 'unhealthy'" type="warning" size="small" data-testid="detail-app-health">健康检查异常</el-tag>
      </div>

      <div v-if="d.state === 'installing'" class="progress">
        <el-progress :percentage="d.progress" :stroke-width="10" data-testid="detail-app-progress" />
        <div class="muted small">{{ phaseText(d) }}<template v-if="phaseText(d) && d.message"> · </template><span data-testid="detail-app-message">{{ d.message }}</span></div>
      </div>
      <div v-else-if="d.state === 'queued' && d.message" class="muted small">
        <span data-testid="detail-app-message">{{ d.message }}</span>
      </div>

      <p v-if="d.state === 'failed'" class="danger small" data-testid="detail-app-error">
        {{ deploymentErrorText(d.error_code, d.message, '部署失败') }}
        <el-button link type="primary" size="small" data-testid="detail-app-error-log" @click="openLog(d)">查看日志</el-button>
      </p>
      <p v-if="hint" class="muted small" data-testid="detail-app-instance-hint">{{ hint }}</p>
      <p v-if="d.state === 'running' && !d.url" class="warn small" data-testid="detail-app-port-deleted">端口已删除，重新部署可恢复</p>

      <div class="row">
        <template v-if="d.state === 'running' && d.url">
          <el-button size="small" type="primary" :disabled="instance.state !== 'running'" data-testid="detail-app-open" @click="open(d.url)">打开</el-button>
          <el-dropdown v-if="d.links.length" trigger="click" @command="(u: string) => open(u)">
            <el-button size="small" data-testid="detail-app-links">链接 ▾</el-button>
            <template #dropdown>
              <el-dropdown-menu>
                <el-dropdown-item v-for="l in d.links" :key="l.url" :command="l.url" data-testid="detail-app-link" :data-url="l.url">{{ l.label }}</el-dropdown-item>
              </el-dropdown-menu>
            </template>
          </el-dropdown>
        </template>
        <el-dropdown trigger="click" @command="(c: string) => onCommand(d, c)">
          <el-button size="small" :loading="acting === d.id" data-testid="detail-app-actions">操作 ▾</el-button>
          <template #dropdown>
            <el-dropdown-menu>
              <el-dropdown-item v-if="deploymentActions(d).redeploy" command="redeploy" :disabled="!!blockReason" data-testid="detail-app-redeploy">
                重新部署<span v-if="blockReason" class="reason">（{{ blockReason }}）</span>
              </el-dropdown-item>
              <el-dropdown-item v-if="deploymentActions(d).stop" command="stop" :disabled="!!blockReason" data-testid="detail-app-stop">
                停止<span v-if="blockReason" class="reason">（{{ blockReason }}）</span>
              </el-dropdown-item>
              <el-dropdown-item v-if="deploymentActions(d).start" command="start" :disabled="!!blockReason" data-testid="detail-app-start">
                启动<span v-if="blockReason" class="reason">（{{ blockReason }}）</span>
              </el-dropdown-item>
              <el-dropdown-item command="log" data-testid="detail-app-log">查看日志</el-dropdown-item>
              <el-dropdown-item v-if="deploymentActions(d).remove" command="remove" divided data-testid="detail-app-remove">卸载</el-dropdown-item>
            </el-dropdown-menu>
          </template>
        </el-dropdown>
      </div>
    </div>

    <template v-if="canAdd && loaded">
      <div v-if="!atLimit" class="row">
        <el-button type="primary" size="small" data-testid="detail-app-add" @click="openAdd">添加应用</el-button>
        <span v-if="instance.state !== 'running'" class="muted small">{{ hint }}</span>
      </div>
      <p v-else class="muted" data-testid="detail-app-limit">每个实例最多部署 {{ MAX_APPS }} 个应用</p>
    </template>

    <AppDeployDialog
      v-model="deployVisible" :mode="deployMode" :instance="instance" :apps="apps" :deployments="items" :deployment="deployTarget"
      @done="load"
    />
    <AppLogDrawer v-model="logVisible" :instance-id="instance.id" :deployment="logTarget" />
    <AppDialog :model-value="!!removeTarget" kind="confirm" :title="`卸载 ${removeTarget?.app_name ?? ''}`" data-testid="app-remove-dialog" @update:model-value="(v: boolean) => { if (!v) removeTarget = null }">
      <p data-testid="app-remove-text">卸载后应用目录会删除，模型缓存保留在 /root/data/models</p>
      <p v-if="instance.state === 'stopped'" class="warn" data-testid="app-remove-stopped">实例停机中，应用文件会在实例下次启动时清理</p>
      <template #footer>
        <el-button :disabled="removing" data-testid="app-remove-cancel" @click="removeTarget = null">取消</el-button>
        <el-button type="danger" :loading="removing" data-testid="app-remove-ok" @click="confirmRemove">确定卸载</el-button>
      </template>
    </AppDialog>
  </el-card>
</template>

<style scoped>
.app-row { padding: 8px 0; border-bottom: 1px solid var(--el-border-color-lighter); }
.row { display: flex; align-items: center; gap: 8px; margin: 4px 0; flex-wrap: wrap; }
.icon { flex: none; width: 24px; height: 24px; border-radius: 6px; display: inline-flex; align-items: center; justify-content: center; font-weight: 600; font-size: 13px; color: #fff; background: var(--el-color-primary); }
.progress { margin: 4px 0; max-width: 480px; }
.muted { color: #909399; }
.small { font-size: 12px; margin: 4px 0; }
.warn { color: var(--el-color-warning); }
.danger { color: var(--el-color-danger); }
.reason { color: #c0c4cc; font-size: 12px; }
</style>
