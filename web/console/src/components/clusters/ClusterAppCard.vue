<script setup lang="ts">
import {
  clusterAppStatusLabel, clusterAppStatusType, clusterRoleLabel, deploymentPhaseLabel, deploymentStateLabel, deploymentStateType, labelOf,
  type AppCatalogItem,
} from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, ref } from 'vue'
import { api } from '../../api'
import { deploymentParams } from '../../logic/apps'
import { copyText as copy } from '../../logic/clipboard'
import { clusterAppActions, clusterAppCallInfo, isClusterActive, type ConsoleClusterApp, type ConsoleClusterView } from '../../logic/cluster'
import { notifyError } from '../../logic/notify'
import AppDialog from '../AppDialog.vue'
import ClusterAppDeployDialog from './ClusterAppDeployDialog.vue'

/**
 * 集群详情「集群应用」卡片：没有时「部署应用」；有时显示聚合状态 / 进度、成员角色表（head / worker 各自状态）、
 * head 的访问地址与调用方式、参数；重新部署 / 停止 / 启动 / 卸载。操作完成后 emit('changed') 由详情页重新加载。
 */
const props = defineProps<{ view: ConsoleClusterView; apps: AppCatalogItem[]; imageId: number | null }>()
const emit = defineEmits<{ changed: [] }>()

const app = computed<ConsoleClusterApp | null>(() => props.view.app)
const active = computed(() => isClusterActive(props.view))
const actions = computed(() => (app.value ? clusterAppActions(app.value) : null))
const call = computed(() => (app.value ? clusterAppCallInfo(app.value) : null))
const params = computed(() => (app.value ? deploymentParams(app.value) : []))
const phase = computed(() => (app.value ? labelOf(deploymentPhaseLabel, app.value.phase) : ''))

const deployVisible = ref(false)
const deployMode = ref<'add' | 'redeploy'>('add')
function openDeploy(mode: 'add' | 'redeploy') {
  deployMode.value = mode
  deployVisible.value = true
}

const acting = ref('')
async function op(action: 'stop' | 'start') {
  const a = app.value
  if (!a || acting.value) return
  acting.value = action
  try {
    await api.post(`/clusters/${props.view.id}/apps/${a.id}/${action}`, {})
    ElMessage.success(action === 'stop' ? `正在停止 ${a.app_name}` : `正在启动 ${a.app_name}`)
  } catch (e) {
    notifyError(e)
  } finally {
    acting.value = ''
    emit('changed')
  }
}

const removeVisible = ref(false)
async function confirmRemove() {
  const a = app.value
  if (!a || acting.value) return
  acting.value = 'remove'
  try {
    await api.del(`/clusters/${props.view.id}/apps/${a.id}`)
    ElMessage.success(`正在卸载 ${a.app_name}`)
  } catch (e) {
    notifyError(e)
  } finally {
    acting.value = ''
    removeVisible.value = false
    emit('changed')
  }
}
const open = (url: string) => window.open(url, '_blank', 'noopener')
</script>

<template>
  <el-card data-testid="cluster-app-card">
    <template #header>
      <div class="head">
        <span>集群应用</span>
        <el-tag v-if="app" :type="labelOf(clusterAppStatusType, app.status) as 'primary'" data-testid="cluster-app-status" :data-status="app.status">{{ labelOf(clusterAppStatusLabel, app.status) }}</el-tag>
      </div>
    </template>
    <template v-if="!app">
      <p class="muted" data-testid="cluster-app-empty">在全部成员上一起部署分布式应用（如多机 vLLM：大模型按张量并行切到 {{ view.size }} 台上），head 对外提供接口。</p>
      <el-button v-if="active" type="primary" size="small" data-testid="cluster-app-deploy" @click="openDeploy('add')">部署应用</el-button>
    </template>
    <template v-else>
      <div class="row">
        <span class="icon" aria-hidden="true">{{ app.app_name.slice(0, 1) }}</span>
        <b data-testid="cluster-app-name">{{ app.app_name }}</b>
        <span class="muted" data-testid="cluster-app-version">v{{ app.version }}</span>
        <el-tag v-if="app.update_available" type="warning" size="small" data-testid="cluster-app-update">有新版本 v{{ app.latest_version }}</el-tag>
        <el-tag v-if="app.status === 'running' && app.health === 'unhealthy'" type="warning" size="small" data-testid="cluster-app-health">健康检查异常</el-tag>
      </div>
      <el-alert v-if="app.status === 'stale'" type="warning" title="成员已更换，请重新部署" :closable="false" show-icon data-testid="cluster-app-stale" />
      <div v-if="app.status === 'installing'" class="progress">
        <el-progress :percentage="app.progress" :stroke-width="10" data-testid="cluster-app-progress" />
        <div v-if="phase" class="muted small">{{ phase }}（进度取成员最慢的一台）</div>
      </div>
      <p v-if="app.status === 'failed'" class="danger small" data-testid="cluster-app-error">部署失败：请查看下方成员的失败原因，可在成员实例详情查看日志后重新部署</p>

      <div class="app-members" data-testid="cluster-app-members">
        <div v-for="m in app.members" :key="`${m.rank}-${m.deployment_id}`" class="app-member" data-testid="cluster-app-member" :data-rank="m.rank" :data-role="m.role" :data-state="m.state">
          <span class="role">rank {{ m.rank }} · {{ labelOf(clusterRoleLabel, m.role) }}</span>
          <router-link :to="`/instances/${m.instance_id}`" class="mono">{{ m.instance_id }}</router-link>
          <el-tag size="small" :type="labelOf(deploymentStateType, m.state) as 'primary'" data-testid="cluster-app-member-state">{{ m.state === 'installing' ? `部署中 ${m.progress}%` : labelOf(deploymentStateLabel, m.state) }}</el-tag>
          <el-tag v-if="!m.current" size="small" type="warning" data-testid="cluster-app-member-stale">已换机</el-tag>
          <span v-if="m.message" class="muted small msg">{{ m.message }}</span>
        </div>
      </div>

      <div v-if="params.length && app.status !== 'removing'" class="params" data-testid="cluster-app-params">
        <span v-for="p in params" :key="p.name" class="param" :data-testid="`cluster-app-param-${p.name}`"><span class="muted">{{ p.label }}：</span>{{ p.value }}</span>
      </div>

      <div v-if="call && app.status !== 'removing'" class="call" data-testid="cluster-app-call">
        <div class="call-row">
          <span class="muted">调用模型名：</span><code data-testid="cluster-app-call-model">{{ call.model }}</code>
          <el-button link type="primary" size="small" @click="copy(call.model)">复制</el-button>
        </div>
        <div class="call-row">
          <span class="muted">head 实例内地址：</span><code data-testid="cluster-app-call-local">{{ call.localBase }}</code>
          <el-button link type="primary" size="small" @click="copy(call.localBase)">复制</el-button>
          <span class="muted small">（在 head 实例 <router-link :to="`/instances/${call.headInstance}`" class="mono">{{ call.headInstance }}</router-link> 里访问；vLLM 只监听 head 的 127.0.0.1）</span>
        </div>
        <div v-if="call.publicBase" class="call-row">
          <span class="muted">外部地址：</span><code data-testid="cluster-app-call-public">{{ call.publicBase }}</code>
          <el-button link type="primary" size="small" @click="copy(call.publicBase)">复制</el-button>
          <span class="muted small">（端口为私有时只有登录控制台的浏览器能访问；程序从外部调用请在 head 实例的「Web 访问」里设为公开，并设置接口密钥）</span>
        </div>
        <div class="call-row"><span class="muted">接口密钥：</span>{{ call.keyed ? '已设置，请求头带 Authorization: Bearer <接口密钥>' : '未设置，不需要鉴权' }}</div>
        <pre class="curl" data-testid="cluster-app-call-curl">{{ call.curl }}</pre>
        <el-button link type="primary" size="small" data-testid="cluster-app-call-curl-copy" @click="copy(call.curl)">复制示例</el-button>
      </div>

      <div v-if="active && actions" class="row">
        <el-button v-if="app.status === 'running' && app.url" size="small" type="primary" data-testid="cluster-app-open" @click="open(app.url)">打开</el-button>
        <el-button v-if="actions.redeploy" size="small" :type="app.status === 'stale' ? 'warning' : 'default'" data-testid="cluster-app-redeploy" @click="openDeploy('redeploy')">重新部署</el-button>
        <el-button v-if="actions.stop" size="small" :loading="acting === 'stop'" data-testid="cluster-app-stop" @click="op('stop')">停止</el-button>
        <el-button v-if="actions.start" size="small" :loading="acting === 'start'" data-testid="cluster-app-start" @click="op('start')">启动</el-button>
        <el-button v-if="actions.remove" size="small" type="danger" plain data-testid="cluster-app-remove" @click="removeVisible = true">卸载</el-button>
      </div>
      <p class="muted small">日志在各成员实例详情的「应用」里查看（集群应用在成员页只读）。</p>
    </template>

    <ClusterAppDeployDialog v-model="deployVisible" :mode="deployMode" :view="view" :apps="apps" :image-id="imageId" :app="app" @done="emit('changed')" />
    <AppDialog v-model="removeVisible" kind="confirm" :title="`卸载 ${app?.app_name ?? ''}`" data-testid="cluster-app-remove-dialog">
      <p data-testid="cluster-app-remove-text">将在全部 {{ view.size }} 台成员上卸载该应用，应用目录会删除，模型缓存保留在 /root/data/models。</p>
      <template #footer>
        <el-button :disabled="acting === 'remove'" data-testid="cluster-app-remove-cancel" @click="removeVisible = false">取消</el-button>
        <el-button type="danger" :loading="acting === 'remove'" data-testid="cluster-app-remove-ok" @click="confirmRemove">确定卸载</el-button>
      </template>
    </AppDialog>
  </el-card>
</template>

<style scoped>
.head { display: flex; align-items: center; justify-content: space-between; gap: 8px; }
.row { display: flex; align-items: center; gap: 8px; margin: 6px 0; flex-wrap: wrap; }
.icon { flex: none; width: 24px; height: 24px; border-radius: 6px; display: inline-flex; align-items: center; justify-content: center; font-weight: 600; font-size: 13px; color: #fff; background: var(--el-color-primary); }
.progress { margin: 4px 0; max-width: 480px; }
.muted { color: #909399; }
.small { font-size: 12px; margin: 4px 0; }
.danger { color: var(--el-color-danger); }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; word-break: break-all; font-size: 13px; }
.app-members { display: grid; gap: 4px; margin: 8px 0; }
.app-member { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; padding: 4px 0; border-bottom: 1px solid var(--el-border-color-lighter); }
.role { min-width: 120px; font-size: 13px; }
.msg { word-break: break-all; }
.params { display: flex; flex-wrap: wrap; gap: 4px 16px; font-size: 12px; margin: 4px 0; }
.param { word-break: break-all; }
.call { font-size: 12px; margin: 6px 0; padding: 8px 10px; background: var(--el-fill-color-lighter); border-radius: 6px; }
.call-row { display: flex; align-items: center; gap: 4px; flex-wrap: wrap; margin: 2px 0; }
.call code { word-break: break-all; }
.curl { margin: 6px 0 0; padding: 8px; background: var(--el-fill-color); border-radius: 4px; white-space: pre-wrap; word-break: break-all; font-size: 12px; }
</style>
