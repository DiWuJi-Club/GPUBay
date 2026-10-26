<script setup lang="ts">
import { isApiError, type AppCatalogItem, type AppEnvSpec } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, ref, watch } from 'vue'
import { api } from '../../api'
import { envDefaults, redeployEnv, redeployForm, redeployVersions, stripEmpty } from '../../logic/apps'
import { clusterAppsFor, defaultTP, TP_ENV, type ConsoleClusterApp, type ConsoleClusterView } from '../../logic/cluster'
import { notifyError } from '../../logic/notify'
import AppDialog from '../AppDialog.vue'
import AppEnvForm from '../AppEnvForm.vue'
import AppPicker from '../AppPicker.vue'

/**
 * 集群应用的部署（add：选支持集群的应用 → 变量表单（TP 默认 = 台数）→ POST /clusters/{id}/apps）
 * 与重新部署（redeploy：版本 + 变量 → POST …/apps/{cd}/redeploy，body 用 version_id）。secret 处理同单实例：不回显，留空沿用，可勾选清除。
 */
const props = defineProps<{
  mode: 'add' | 'redeploy'
  view: Pick<ConsoleClusterView, 'id' | 'size'>
  apps: AppCatalogItem[]
  /** 成员实例的镜像（用于灰显不兼容的应用）；未知为 null */
  imageId: number | null
  app?: ConsoleClusterApp | null
}>()
const visible = defineModel<boolean>({ required: true })
const emit = defineEmits<{ done: [app: ConsoleClusterApp] }>()

const appId = ref<number | null>(null)
const versionId = ref<number | null>(null)
const values = ref<Record<string, string>>({})
const secretSet = ref<Record<string, boolean>>({})
const cleared = ref<Record<string, boolean>>({})
const formSecretSet = computed(() => Object.fromEntries(Object.keys(secretSet.value).filter((n) => !cleared.value[n]).map((n) => [n, true])))
const error = ref('')
const submitting = ref(false)
const envForm = ref<InstanceType<typeof AppEnvForm> | null>(null)

const candidates = computed(() => clusterAppsFor(props.apps, props.view.size))
const pickedApp = computed(() => candidates.value.find((a) => a.id === appId.value) ?? null)
function pickReason(a: AppCatalogItem): string | null {
  if (props.imageId != null && !a.images.some((i) => i.id === props.imageId)) return `需要镜像：${a.images.map((i) => i.name).join('、') || '—'}`
  return null
}
const versions = computed(() => (props.app ? redeployVersions(props.app, props.apps) : []))
const spec = computed<AppEnvSpec[]>(() => {
  if (props.mode === 'add') return pickedApp.value?.version.env ?? []
  return versions.value.find((v) => v.id === versionId.value)?.env ?? props.app?.spec.env ?? []
})
const clearable = computed(() => spec.value.filter((e) => e.type === 'secret' && secretSet.value[e.name]))

function resetEnv() {
  error.value = ''
  cleared.value = {}
  if (props.mode === 'add') {
    const v = envDefaults(spec.value)
    if (spec.value.some((e) => e.name === TP_ENV) && !v[TP_ENV]) v[TP_ENV] = defaultTP(props.view.size) // TP 默认 = 台数
    values.value = v
    secretSet.value = {}
  } else if (props.app) {
    const f = redeployForm(props.app, spec.value)
    values.value = f.values
    secretSet.value = f.secretSet
  }
}

watch(visible, (v) => {
  if (!v) return
  appId.value = props.mode === 'add' && candidates.value.length === 1 && !pickReason(candidates.value[0]) ? candidates.value[0].id : null
  versionId.value = props.app?.version_id ?? null
  resetEnv()
}, { immediate: true })
watch([appId, versionId], resetEnv)
watch([values, cleared], () => (error.value = ''))

async function submit() {
  if (submitting.value) return
  if (props.mode === 'add' && !pickedApp.value) {
    error.value = '请选择应用'
    return
  }
  if (envForm.value && !envForm.value.validate()) return
  submitting.value = true
  try {
    let out: ConsoleClusterApp
    if (props.mode === 'add') {
      out = await api.post<ConsoleClusterApp>(`/clusters/${props.view.id}/apps`, { app_id: pickedApp.value!.id, env: stripEmpty(values.value) })
      ElMessage.success(`已开始在 ${props.view.size} 台成员上部署 ${out.app_name}`)
    } else {
      out = await api.post<ConsoleClusterApp>(`/clusters/${props.view.id}/apps/${props.app!.id}/redeploy`, {
        version_id: versionId.value, env: redeployEnv(values.value, cleared.value),
      })
      ElMessage.success(`已提交重新部署 ${out.app_name}`)
    }
    visible.value = false
    emit('done', out)
  } catch (e) {
    if (isApiError(e) && e.status >= 400 && e.status < 500 && e.status !== 401) error.value = e.message // app_cluster_* / app_env_* / invalid_state 等显示在弹窗里
    else notifyError(e)
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <AppDialog v-model="visible" kind="form" :title="mode === 'add' ? `部署集群应用（${view.size} 台）` : `重新部署 ${app?.app_name ?? ''}`" data-testid="cluster-app-deploy-dialog">
    <el-form label-position="top" @submit.prevent>
      <template v-if="mode === 'add'">
        <el-form-item label="应用">
          <AppPicker v-model="appId" :apps="candidates" :disabled="pickReason" testid="cluster-app-deploy-pick" />
        </el-form-item>
        <p v-if="!candidates.length" class="muted" data-testid="cluster-app-deploy-empty">暂无支持 {{ view.size }} 台集群的应用</p>
        <p v-else class="muted small">集群应用会在每台成员上各部署一份：rank 0 为主节点（head，对外提供接口），其余为工作节点（worker）。</p>
      </template>
      <el-form-item v-else label="版本">
        <el-select v-model="versionId" data-testid="cluster-app-deploy-version" style="width: 220px">
          <el-option v-for="v in versions" :key="v.id" :value="v.id" :label="v.label" :disabled="v.disabled" />
        </el-select>
      </el-form-item>
      <AppEnvForm v-if="mode === 'redeploy' || pickedApp" :key="`${appId}-${versionId}`" ref="envForm" v-model="values" :spec="spec" :secret-set="formSecretSet" />
      <div v-if="mode === 'redeploy' && clearable.length" class="clear">
        <el-checkbox
          v-for="e in clearable" :key="e.name" :model-value="!!cleared[e.name]" :data-testid="`cluster-app-deploy-clear-${e.name}`"
          @update:model-value="(v: string | number | boolean) => (cleared = { ...cleared, [e.name]: v === true })"
        >清除已设置的「{{ e.label || e.name }}」</el-checkbox>
      </div>
      <p v-if="mode === 'redeploy'" class="muted small">重新部署会在全部成员上重装（成员更换过机器时会为新成员补建部署）；模型缓存保留。</p>
      <el-alert v-if="error" type="error" :title="error" show-icon :closable="false" data-testid="cluster-app-deploy-error" />
    </el-form>
    <template #footer>
      <el-button data-testid="cluster-app-deploy-cancel" @click="visible = false">取消</el-button>
      <el-button type="primary" :loading="submitting" :disabled="mode === 'add' && !pickedApp" data-testid="cluster-app-deploy-ok" @click="submit">{{ mode === 'add' ? '部署' : '重新部署' }}</el-button>
    </template>
  </AppDialog>
</template>

<style scoped>
.muted { color: #909399; font-size: 13px; }
.small { font-size: 12px; margin: 4px 0 8px; }
.clear { display: flex; flex-direction: column; align-items: flex-start; margin-bottom: 8px; }
</style>
