<script setup lang="ts">
import { isApiError, type AppCatalogItem, type AppEnvSpec, type Deployment, type Instance } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, ref, watch } from 'vue'
import { api } from '../api'
import { addPickReason, envDefaults, redeployForm, stripEmpty } from '../logic/apps'
import { notifyError } from '../logic/notify'
import AppDialog from './AppDialog.vue'
import AppEnvForm from './AppEnvForm.vue'
import AppPicker from './AppPicker.vue'

/**
 * 添加应用（add：选应用 → 变量表单 → POST /instances/{id}/apps）与重新部署（redeploy：版本 + 变量 → POST …/redeploy）。
 * 重新部署时 secret 从不回显：已设置的显示「已设置，留空沿用」，提交时空值不发送（服务器沿用旧值）。
 */
const props = defineProps<{
  mode: 'add' | 'redeploy'
  instance: Pick<Instance, 'id' | 'image_id'>
  apps: AppCatalogItem[]
  deployments: Deployment[]
  deployment?: Deployment | null
}>()
const visible = defineModel<boolean>({ required: true })
const emit = defineEmits<{ done: [d: Deployment] }>()

const appId = ref<number | null>(null)
const versionId = ref<number | null>(null)
const values = ref<Record<string, string>>({})
const secretSet = ref<Record<string, boolean>>({})
const error = ref('')
const submitting = ref(false)
const envForm = ref<InstanceType<typeof AppEnvForm> | null>(null)

const pickedApp = computed(() => props.apps.find((a) => a.id === appId.value) ?? null)
const pickReason = (a: AppCatalogItem) => addPickReason(a, props.instance.image_id, props.deployments)

/** 重新部署可选的版本：当前版本；有新版本时加上最新已发布版本。 */
const versions = computed(() => {
  const d = props.deployment
  if (!d) return []
  const out = [{ id: d.version_id, label: `v${d.version}（当前）` }]
  if (d.update_available && d.latest_version_id != null) out.push({ id: d.latest_version_id, label: `v${d.latest_version}（最新）` })
  return out
})

/** 变量表单按目标版本的 spec 渲染：当前版本用部署自带的 spec，最新版本用应用目录里的最新版本。 */
const spec = computed<AppEnvSpec[]>(() => {
  if (props.mode === 'add') return pickedApp.value?.version.env ?? []
  const d = props.deployment
  if (!d) return []
  if (versionId.value !== d.version_id) {
    const cat = props.apps.find((a) => a.id === d.app_id)
    if (cat && cat.version.id === versionId.value) return cat.version.env
  }
  return d.spec.env ?? []
})

function resetEnv() {
  error.value = ''
  if (props.mode === 'add') {
    values.value = envDefaults(spec.value)
    secretSet.value = {}
  } else if (props.deployment) {
    const f = redeployForm(props.deployment, spec.value)
    values.value = f.values
    secretSet.value = f.secretSet
  }
}

watch(visible, (v) => {
  if (!v) return
  appId.value = null
  versionId.value = props.deployment?.version_id ?? null
  resetEnv()
})
watch([appId, versionId], resetEnv)
watch(values, () => (error.value = ''))

async function submit() {
  if (submitting.value) return
  if (props.mode === 'add' && !pickedApp.value) {
    error.value = '请选择应用'
    return
  }
  if (!envForm.value?.validate()) return
  submitting.value = true
  // 空串不发送：普通变量由服务器补默认值，留空的 secret 沿用已设置的值
  const env = stripEmpty(values.value)
  try {
    let d: Deployment
    if (props.mode === 'add') {
      d = await api.post<Deployment>(`/instances/${props.instance.id}/apps`, { app_id: pickedApp.value!.id, env })
      ElMessage.success(`已添加 ${d.app_name}`)
    } else {
      const dep = props.deployment!
      d = await api.post<Deployment>(`/instances/${props.instance.id}/apps/${dep.id}/redeploy`, { app_version_id: versionId.value, env })
      ElMessage.success(`已提交重新部署 ${d.app_name}`)
    }
    visible.value = false
    emit('done', d)
  } catch (e) {
    if (isApiError(e) && e.status >= 400 && e.status < 500 && e.status !== 401) error.value = e.message // 业务错误（app_* / port_limit / 限流等）显示在弹窗里
    else notifyError(e)
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <AppDialog v-model="visible" kind="form" :title="mode === 'add' ? '添加应用' : `重新部署 ${deployment?.app_name ?? ''}`" data-testid="app-deploy-dialog">
    <el-form label-position="top" @submit.prevent>
      <template v-if="mode === 'add'">
        <el-form-item label="应用">
          <AppPicker v-model="appId" :apps="apps" :disabled="pickReason" testid="app-deploy-pick" />
        </el-form-item>
        <p v-if="!apps.length" class="muted">暂无可部署的应用</p>
      </template>
      <el-form-item v-else label="版本">
        <el-select v-model="versionId" data-testid="app-deploy-version" style="width: 220px">
          <el-option v-for="v in versions" :key="v.id" :value="v.id" :label="v.label" />
        </el-select>
      </el-form-item>
      <AppEnvForm v-if="mode === 'redeploy' || pickedApp" :key="`${appId}-${versionId}`" ref="envForm" v-model="values" :spec="spec" :secret-set="secretSet" />
      <p v-if="mode === 'redeploy'" class="muted small">重新部署会用平台的部署脚本覆盖应用目录里的 app.sh 与 env；模型缓存保留。</p>
      <el-alert v-if="error" type="error" :title="error" show-icon :closable="false" data-testid="app-deploy-error" />
    </el-form>
    <template #footer>
      <el-button data-testid="app-deploy-cancel" @click="visible = false">取消</el-button>
      <el-button type="primary" :loading="submitting" :disabled="mode === 'add' && !pickedApp" data-testid="app-deploy-ok" @click="submit">{{ mode === 'add' ? '部署' : '重新部署' }}</el-button>
    </template>
  </AppDialog>
</template>

<style scoped>
.muted { color: #909399; font-size: 13px; }
.small { font-size: 12px; margin: 4px 0 8px; }
</style>
