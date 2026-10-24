<script setup lang="ts">
import {
  appStatusLabel, isApiError, labelOf, llmModelKindLabel, messageOf,
  type AdminApp, type AdminAppVersion, type AdminImage, type AppEnvSpec, type LLMModelAdmin, type Listed,
} from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, reactive, ref, watch } from 'vue'
import { api } from '../../api'
import {
  compatibleImages, emptyModelForm, LLM_FEATURES, llmFeatureLabel, llmSourceLabel, latestPublished, modelToForm, nodeEnvSpec, staleNodesWarning,
  validateModelForm, type ModelForm,
} from '../../logic/llm'
import { notifyError } from '../../logic/notify'

/**
 * 模型新建 / 编辑抽屉（规格 §3.1、§11）。节点参数按 vllm-node 所选版本（未指定 = 最新已发布）的 spec 渲染，
 * 平台保留变量不展示；vllm-node 目录没加载成功时不能保存（否则会把已有 node_env 清掉）。readonly 时只看不改。
 */
const props = defineProps<{ open: boolean; model: LLMModelAdmin | null; readonly?: boolean }>()
const emit = defineEmits<{ close: []; saved: [] }>()

const NODE_APP_SLUG = 'vllm-node'

const catalog = reactive({ loading: false, error: '', app: null as AdminApp | null, versions: [] as AdminAppVersion[], images: [] as AdminImage[] })
const form = ref<ModelForm>(emptyModelForm())
const errors = ref<Record<string, string>>({})
const busy = ref(false)

const publishedVersions = computed(() => catalog.versions.filter((v) => v.status === 'published' || v.id === form.value.app_version_id))
/** 版本下拉：0 = 「最新已发布」（app_version_id 为 null；版本 id 从 1 开始） */
const versionSel = computed({ get: () => form.value.app_version_id ?? 0, set: (v: number) => (form.value.app_version_id = v || null) })
const latest = computed(() => latestPublished(catalog.versions))
/** 建节点时实际使用的版本：指定的，或最新已发布 */
const version = computed(() => (form.value.app_version_id != null ? catalog.versions.find((v) => v.id === form.value.app_version_id) : latest.value))
const envSpec = computed<AppEnvSpec[]>(() => nodeEnvSpec(version.value?.spec.env))
const images = computed(() => compatibleImages(version.value, catalog.images, form.value.image_id))
const imageName = (id: number) => catalog.images.find((i) => i.id === id)?.name ?? `#${id}`
/** 不能保存的原因（目录没加载好 / vllm-node 没有可用版本） */
const blocker = computed(() => {
  if (catalog.loading) return '正在加载 vllm-node 应用信息…'
  if (catalog.error) return catalog.error
  if (!catalog.app) return `未找到内部应用 ${NODE_APP_SLUG}：请先导入应用目录（platform admin app import deploy/apps）并发布`
  if (!version.value) return `${NODE_APP_SLUG} 还没有已发布版本：请先在应用目录发布`
  return ''
})
const title = computed(() => (props.readonly ? '模型详情' : props.model ? `编辑模型：${props.model.slug}` : '新建模型'))

/** 实时提示：已有节点且改了只影响新节点的参数 */
const stale = computed(() => {
  if (!props.model || props.readonly || blocker.value) return null
  const out = validateModelForm(form.value, { creating: false, envSpec: envSpec.value })
  return out.ok ? staleNodesWarning(props.model, out.input) : null
})

async function loadCatalog() {
  catalog.loading = true
  try {
    const [apps, imgs] = await Promise.all([api.get<Listed<AdminApp>>('/apps'), api.get<Listed<AdminImage>>('/images')])
    catalog.app = apps.items.find((a) => a.slug === NODE_APP_SLUG) ?? null
    catalog.images = [...imgs.items].sort((a, b) => a.sort - b.sort || a.id - b.id)
    catalog.versions = catalog.app ? (await api.get<Listed<AdminAppVersion>>(`/apps/${catalog.app.id}/versions`)).items : []
    catalog.error = ''
  } catch (e) {
    notifyError(e)
    catalog.error = `加载 vllm-node 应用信息失败：${messageOf(e)}`
  } finally {
    catalog.loading = false
  }
}

async function init() {
  errors.value = {}
  await loadCatalog()
  if (props.model) {
    form.value = modelToForm(props.model, envSpec.value)
  } else {
    const f = emptyModelForm(envSpec.value)
    f.image_id = compatibleImages(latest.value, catalog.images)[0]?.id
    form.value = f
  }
}
watch(
  () => props.open,
  (open) => {
    if (open) void init()
  },
  { immediate: true },
)

/** 换了 vllm-node 版本：变量表跟着换，同名变量保留已填的值。 */
watch(envSpec, (spec) => {
  const cur = form.value.node_env
  const src = props.model?.node_env ?? {}
  form.value.node_env = Object.fromEntries(spec.map((e) => [e.name, cur[e.name] ?? src[e.name] ?? '']))
})

const envPlaceholder = (e: AppEnvSpec) => (e.default ? `默认 ${e.default}` : '可留空')
const intOf = (v: string) => (v === '' || Number.isNaN(Number(v)) ? undefined : Number(v))
function setEnv(name: string, v: string | number | null | undefined) {
  form.value.node_env = { ...form.value.node_env, [name]: v == null ? '' : String(v) }
}

async function save() {
  if (busy.value || props.readonly || blocker.value) return
  const out = validateModelForm(form.value, { creating: !props.model, envSpec: envSpec.value })
  if (!out.ok) {
    errors.value = out.errors
    return
  }
  errors.value = {}
  const warn = staleNodesWarning(props.model, out.input)
  busy.value = true
  try {
    if (props.model) {
      const { slug: _slug, ...body } = out.input // slug 创建后不可改，PUT 不带
      await api.put(`/llm/models/${props.model.id}`, body)
    } else {
      await api.post('/llm/models', out.input)
    }
    ElMessage.success(props.model ? '模型已保存' : '模型已创建')
    if (warn) ElMessage.warning({ message: warn, duration: 8000 })
    emit('saved')
  } catch (e) {
    if (isApiError(e) && e.code === 'llm_model_slug_exists') errors.value = { slug: e.message }
    notifyError(e) // 抽屉与输入保留
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <el-drawer :model-value="open" :title="title" size="680px" destroy-on-close data-testid="llm-model-drawer" @close="emit('close')">
    <el-alert v-if="blocker && !catalog.loading" :type="catalog.error ? 'error' : 'warning'" :title="blocker" show-icon :closable="false" class="mb" data-testid="llm-model-catalog-error">
      <el-button v-if="catalog.error" size="small" :loading="catalog.loading" data-testid="llm-model-catalog-retry" @click="init">重试</el-button>
    </el-alert>
    <el-skeleton v-if="catalog.loading" :rows="8" animated />
    <el-form v-else label-position="top" :disabled="readonly" data-testid="llm-model-form" @submit.prevent="save">
      <h4>基本</h4>
      <div class="two">
        <el-form-item label="slug（请求里的 model，创建后不可改）" :error="errors.slug" :required="!model">
          <el-input v-model="form.slug" :disabled="!!model" placeholder="qwen3-32b" data-testid="llm-model-slug" />
        </el-form-item>
        <el-form-item label="名称" :error="errors.name" required>
          <el-input v-model="form.name" maxlength="64" data-testid="llm-model-name" />
        </el-form-item>
        <el-form-item label="类型" :error="errors.kind">
          <el-radio-group v-model="form.kind" data-testid="llm-model-kind">
            <el-radio-button v-for="k in ['chat', 'embedding']" :key="k" :value="k">{{ labelOf(llmModelKindLabel, k) }}</el-radio-button>
          </el-radio-group>
        </el-form-item>
        <el-form-item label="排序（小的在前）" :error="errors.sort">
          <el-input-number v-model="form.sort" :controls="false" :precision="0" data-testid="llm-model-sort" />
        </el-form-item>
      </div>
      <el-form-item label="标签" :error="errors.features">
        <el-checkbox-group v-model="form.features" data-testid="llm-model-features">
          <el-checkbox v-for="f in LLM_FEATURES" :key="f" :value="f">{{ llmFeatureLabel[f] }}</el-checkbox>
        </el-checkbox-group>
      </el-form-item>
      <el-form-item label="介绍" :error="errors.description">
        <el-input v-model="form.description" type="textarea" :rows="3" maxlength="2000" show-word-limit data-testid="llm-model-description" />
      </el-form-item>

      <h4>来源与镜像</h4>
      <div class="two">
        <el-form-item label="下载源" :error="errors.source">
          <el-select v-model="form.source" data-testid="llm-model-source">
            <el-option v-for="s in ['modelscope', 'hf-mirror']" :key="s" :value="s" :label="llmSourceLabel[s]" />
          </el-select>
        </el-form-item>
        <el-form-item label="模型 ID" :error="errors.source_id" required>
          <el-input v-model="form.source_id" placeholder="Qwen/Qwen3-32B" data-testid="llm-model-source-id" />
        </el-form-item>
        <el-form-item label="vllm-node 版本">
          <el-select v-model="versionSel" data-testid="llm-model-app-version">
            <el-option :value="0" :label="latest ? `最新已发布（当前 ${latest.version}）` : '最新已发布'" />
            <el-option v-for="v in publishedVersions" :key="v.id" :value="v.id" :label="`${v.version}（${labelOf(appStatusLabel, v.status)}）`" />
          </el-select>
        </el-form-item>
        <el-form-item label="节点镜像" :error="errors.image_id" required>
          <el-select v-model="form.image_id" placeholder="请选择" data-testid="llm-model-image">
            <el-option v-for="i in images" :key="i.id" :value="i.id" :label="i.enabled ? i.name : `${i.name}（已停用）`" />
          </el-select>
          <div v-if="form.image_id != null && !images.some((i) => i.id === form.image_id)" class="help">当前镜像 {{ imageName(form.image_id) }} 不在可选列表</div>
        </el-form-item>
      </div>

      <h4>价格（元 / 百万 token）</h4>
      <div class="two">
        <el-form-item label="输入价格" :error="errors.input_price" required>
          <el-input v-model="form.input_price" inputmode="decimal" placeholder="如 1.50" data-testid="llm-model-input-price"><template #append>元 / 百万</template></el-input>
        </el-form-item>
        <el-form-item label="输出价格" :error="errors.output_price" required>
          <el-input v-model="form.output_price" inputmode="decimal" placeholder="如 6.00" data-testid="llm-model-output-price"><template #append>元 / 百万</template></el-input>
        </el-form-item>
        <el-form-item label="最大输出 token" :error="errors.max_output_tokens">
          <el-input-number v-model="form.max_output_tokens" :min="1" :max="262144" :precision="0" controls-position="right" data-testid="llm-model-max-output" />
        </el-form-item>
        <el-form-item label="默认输出 token（未带 max_tokens 时的预算估算）" :error="errors.default_max_tokens">
          <el-input-number v-model="form.default_max_tokens" :min="1" :max="262144" :precision="0" controls-position="right" data-testid="llm-model-default-max" />
        </el-form-item>
      </div>

      <h4>节点参数</h4>
      <el-form-item label="上下文长度（MAX_MODEL_LEN）" :error="errors.context_len">
        <el-input-number v-model="form.context_len" :min="1024" :max="1048576" :step="1024" :precision="0" controls-position="right" data-testid="llm-model-context-len" />
      </el-form-item>
      <div class="two">
        <el-form-item v-for="e in envSpec" :key="e.name" :label="e.label ? `${e.label}（${e.name}）` : e.name" :error="errors[`node_env.${e.name}`]">
          <el-select v-if="e.type === 'enum'" :model-value="form.node_env[e.name]" clearable :placeholder="envPlaceholder(e)" :data-testid="`llm-model-env-${e.name}`" @update:model-value="(v: string | undefined) => setEnv(e.name, v)">
            <el-option v-for="o in e.options ?? []" :key="o" :value="o" :label="o" />
          </el-select>
          <el-input-number
            v-else-if="e.type === 'int'" :model-value="intOf(form.node_env[e.name] ?? '')" :min="e.min ?? -Infinity" :max="e.max ?? Infinity" :precision="0"
            :placeholder="envPlaceholder(e)" controls-position="right" :data-testid="`llm-model-env-${e.name}`" @update:model-value="(n: number | null | undefined) => setEnv(e.name, n)"
          />
          <el-select v-else-if="e.type === 'bool'" :model-value="form.node_env[e.name]" clearable :placeholder="envPlaceholder(e)" :data-testid="`llm-model-env-${e.name}`" @update:model-value="(v: string | undefined) => setEnv(e.name, v)">
            <el-option value="true" label="是" /><el-option value="false" label="否" />
          </el-select>
          <el-input v-else :model-value="form.node_env[e.name]" :type="e.type === 'secret' ? 'password' : 'text'" clearable :placeholder="envPlaceholder(e)" :data-testid="`llm-model-env-${e.name}`" @update:model-value="(v: string) => setEnv(e.name, v)" />
          <div v-if="e.help" class="help">{{ e.help }}</div>
        </el-form-item>
      </div>
      <p v-if="!blocker && envSpec.length === 0" class="help">该版本没有可覆盖的节点变量</p>

      <h4>副本与并发</h4>
      <div class="two">
        <el-form-item label="期望副本数（上线后按此建节点）" :error="errors.replicas">
          <el-input-number v-model="form.replicas" :min="0" :max="200" :precision="0" controls-position="right" data-testid="llm-model-replicas" />
        </el-form-item>
        <el-form-item label="单节点并发上限" :error="errors.max_concurrency">
          <el-input-number v-model="form.max_concurrency" :min="1" :max="512" :precision="0" controls-position="right" data-testid="llm-model-max-concurrency" />
        </el-form-item>
      </div>

      <el-alert v-if="stale" type="warning" :title="stale" show-icon :closable="false" data-testid="llm-model-stale-warning">
        来源、镜像、版本、上下文、最大输出与节点参数只影响之后新建的节点
      </el-alert>
    </el-form>
    <template v-if="!readonly" #footer>
      <el-button data-testid="llm-model-cancel" @click="emit('close')">取消</el-button>
      <el-button type="primary" :loading="busy" :disabled="!!blocker" data-testid="llm-model-save" @click="save">保存</el-button>
    </template>
  </el-drawer>
</template>

<style scoped>
h4 { margin: 16px 0 8px; font-size: 14px; color: var(--el-text-color-primary); border-left: 3px solid var(--el-color-primary); padding-left: 8px; }
h4:first-child { margin-top: 0; }
.two { display: grid; grid-template-columns: 1fr 1fr; gap: 0 16px; }
.help { color: #909399; font-size: 12px; line-height: 1.5; width: 100%; }
.mb { margin-bottom: 12px; }
.el-select, .el-input-number { width: 100%; }
</style>
