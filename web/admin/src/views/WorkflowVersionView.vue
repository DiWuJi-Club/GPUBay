<script setup lang="ts">
import {
  appStatusLabel, fmtTime, formatGB, isApiError, labelOf, messageOf, type AdminWorkflow, type AdminWorkflowVersion, type WorkflowValidateResult,
} from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import { notifyError } from '../logic/notify'
import {
  collectRefs, downloadGB, emptyInputRow, emptyManifest, emptyModelRow, emptyNodeRow, emptyOutputRow, formatBytes, formToManifest, hasNaN, INPUT_TYPES,
  inputHas, LIMITS, manifestIssues, manifestToForm, missingRefs, MODEL_DIRS, OUTPUT_CLASS_TYPES, parseApiFile, parseGraphFile, pathToCell,
  splitFieldError, suggestInput, versionIssues, type FieldIssue, type InputRow, type ManifestForm, type ManifestTable,
} from '../logic/workflowManifest'
import { session } from '../store/session'

// 工作流版本编辑页（计划 11 规格 §12）：/workflows/:id/versions/:vid 与 /workflows/:id/versions/new?copy_from=<版本 id>。
// 左：版本号、更新说明、graph.json / api.json；右：manifest 结构化表单 +「查看 JSON」；顶部：校验（?validate=1）/ 保存 / 发布（super + 动态码）。
// 草稿（super）可改全部；已发布只能改更新说明与退役；已退役只读；ops / finance 只读。前端即时校验只做提示，以后端为准。
const route = useRoute()
const router = useRouter()
const wfId = computed(() => Number(route.params.id))
const vid = computed(() => String(route.params.vid))
const isNew = computed(() => vid.value === 'new')

const wf = ref<AdminWorkflow | null>(null)
const version = ref<AdminWorkflowVersion | null>(null)
const loading = ref(false)
const loadError = ref('')
const notFound = ref(false)
const busy = ref(false)

const isSuper = computed(() => session.admin?.role === 'super')
const status = computed(() => version.value?.status ?? 'draft')
/** 可改全部字段：新建或草稿，且是 super。 */
const editable = computed(() => isSuper.value && (isNew.value || status.value === 'draft'))
const changelogEditable = computed(() => isSuper.value && status.value !== 'retired')
const statusType: Record<string, 'info' | 'success' | 'warning'> = { draft: 'info', published: 'success', retired: 'warning' }

interface VersionForm {
  version: string
  changelog: string
  graph: string
  api: string
  manifest: ManifestForm
}
const blankForm = (): VersionForm => ({ version: '', changelog: '', graph: '', api: '', manifest: manifestToForm(emptyManifest()) })
const form = reactive<VersionForm>(blankForm())
const savedSnapshot = ref('')
const snapshot = () => JSON.stringify(form)
const dirty = computed(() => snapshot() !== savedSnapshot.value)

// ---- 解析与即时校验 ----
const graphInfo = computed(() => parseGraphFile(form.graph))
const apiInfo = computed(() => parseApiFile(form.api))
/** null = 没有 api_prompt（不支持快速出图） */
const apiNodes = computed(() => (apiInfo.value.empty ? null : apiInfo.value.nodes))
const apiById = computed(() => new Map((apiNodes.value ?? []).map((n) => [n.id, n])))
const outputNodes = computed(() => (apiNodes.value ?? []).filter((n) => (OUTPUT_CLASS_TYPES as readonly string[]).includes(n.class_type)))
const manifest = computed(() => formToManifest(form.manifest))
const totalGB = computed(() => downloadGB(manifest.value.models))
const missing = computed(() => missingRefs(manifest.value, collectRefs(graphInfo.value.value, apiNodes.value ?? [])))
/** 即时提示：已有版本打开即显示；新建在第一次校验 / 保存后显示（空表单不必满屏飘红）。 */
const showHints = ref(false)
const localIssues = computed<FieldIssue[]>(() =>
  editable.value && showHints.value
    ? [...versionIssues({ version: form.version, changelog: form.changelog, graph: graphInfo.value, api: apiInfo.value }), ...manifestIssues(manifest.value, apiNodes.value)]
    : [],
)

// ---- 后端结果与字段定位 ----
const result = ref<{ ok: boolean; text: string; warnings: string[]; downloadGB?: number } | null>(null)
const fieldError = ref<FieldIssue | null>(null)
const issues = computed(() => (fieldError.value ? [fieldError.value, ...localIssues.value] : localIssues.value))
const cells = computed(() => issues.value.map((i) => ({ ...i, cell: pathToCell(i.path) })))
const rowErr = (t: ManifestTable, i: number) => cells.value.some((c) => c.cell?.table === t && c.cell.index === i)
const cellErr = (t: ManifestTable, i: number, ...fields: string[]) =>
  cells.value.some((c) => c.cell?.table === t && c.cell.index === i && (fields.includes(c.cell.field) || c.cell.field === ''))
const COLUMN: Record<string, string> = {
  name: '名称', dir: '目录', size_gb: '大小', ms_repo: 'ModelScope 仓库', ms_file: 'ModelScope 文件', hf_repo: 'HF 仓库', hf_file: 'HF 文件', sha256: 'sha256',
  git: 'Git 地址', commit: 'commit', node_types: '节点类型', key: 'key', label: '名称', type: '类型', node: '节点', input: '输入', default: '默认值',
  min: 'min', max: 'max', step: 'step', max_len: '最长', options: '选项',
}
/** 某个表的问题（行内 + 表级），去重后逐条显示。 */
function tableErrors(t: ManifestTable): string[] {
  const out = new Set<string>()
  for (const c of cells.value) {
    if (c.cell?.table === t) out.add(`第 ${c.cell.index + 1} 行${c.cell.field ? ` ${COLUMN[c.cell.field] ?? c.cell.field}` : ''}：${c.reason}`)
    else if (c.path === `manifest.${t}`) out.add(c.reason)
  }
  return [...out]
}
/** 非表格字段的问题（version / changelog / graph… / api_prompt… / manifest.comfyui_min）。 */
function errFor(prefix: string): string {
  const hit = issues.value.find((i) => i.path === prefix || i.path.startsWith(prefix + '.') || i.path.startsWith(prefix + '['))
  return hit ? (hit.path === prefix ? hit.reason : `${hit.path}：${hit.reason}`) : ''
}

function clearResult() {
  result.value = null
  fieldError.value = null
}
/** 后端错误：workflow_version_invalid 的「路径：原因」定位到字段；其余（frozen / duplicate…）原文显示。 */
function showError(e: unknown) {
  fieldError.value = isApiError(e) && e.code === 'workflow_version_invalid' ? splitFieldError(e.message) : null
  result.value = { ok: false, text: messageOf(e), warnings: [] }
}

// ---- 加载 ----
async function load() {
  if (route.matched[0]?.path !== '/workflows/:id/versions/:vid') return // 离开本页时 route 先变，不再加载
  loading.value = true
  loadError.value = ''
  notFound.value = false
  clearResult()
  try {
    const copyFrom = isNew.value && route.query.copy_from ? Number(route.query.copy_from) : null
    const srcId = isNew.value ? copyFrom : Number(vid.value)
    const [w, src] = await Promise.all([
      api.get<AdminWorkflow>(`/workflows/${wfId.value}`),
      srcId ? api.get<AdminWorkflowVersion>(`/workflows/${wfId.value}/versions/${srcId}`) : Promise.resolve(null),
    ])
    wf.value = w
    version.value = isNew.value ? null : src
    const next = blankForm()
    if (src) {
      Object.assign(next, {
        graph: src.graph == null ? '' : JSON.stringify(src.graph, null, 2),
        api: src.api_prompt == null ? '' : JSON.stringify(src.api_prompt, null, 2),
        manifest: manifestToForm(src.manifest ?? emptyManifest()),
      })
      if (!isNew.value) Object.assign(next, { version: src.version, changelog: src.changelog }) // 复制不带版本号与更新说明
    }
    Object.assign(form, next)
    savedSnapshot.value = isNew.value ? JSON.stringify(blankForm()) : snapshot()
    showHints.value = !isNew.value
  } catch (e) {
    if (isApiError(e) && (e.code === 'workflow_not_found' || e.code === 'not_found')) {
      notFound.value = true
      return
    }
    notifyError(e)
    loadError.value = messageOf(e)
  } finally {
    loading.value = false
  }
}

// ---- 请求体 ----
const fullBody = () => ({
  version: form.version.trim(),
  graph: graphInfo.value.value,
  api_prompt: apiInfo.value.empty ? null : apiInfo.value.value, // 显式 null = 不支持快速出图（PUT 省略会沿用旧值）
  manifest: manifest.value,
  changelog: form.changelog,
})
/** 已发布版本只发 changelog（省略的字段后端沿用当前值）；改其它字段会 409 workflow_version_frozen。 */
const body = () => (editable.value ? fullBody() : { changelog: form.changelog })

/** 本地拦下没法组成请求体的情况（graph / api.json 不是合法 JSON、数字列不是数字）；其余规则交给后端。 */
function localCheck(): boolean {
  showHints.value = true
  if (!editable.value) return true
  const blocking = !graphInfo.value.ok || !apiInfo.value.ok || hasNaN(manifest.value)
  if (!blocking) return true
  const first = localIssues.value[0]
  fieldError.value = null
  result.value = { ok: false, text: first ? `${first.path}：${first.reason}` : '表单有未填写或格式不正确的内容', warnings: [] }
  return false
}

/** ?validate=1：只校验不保存（新建走 POST，已有版本走 PUT，同一套冻结规则）。 */
async function validate(): Promise<WorkflowValidateResult | null> {
  if (!localCheck()) return null
  try {
    const out = isNew.value
      ? await api.post<WorkflowValidateResult>(`/workflows/${wfId.value}/versions?validate=1`, body())
      : await api.put<WorkflowValidateResult>(`/workflows/${wfId.value}/versions/${vid.value}?validate=1`, body())
    fieldError.value = null
    result.value = { ok: true, text: `校验通过（manifest_hash ${out.manifest_hash.slice(0, 12)}，下载约 ${formatGB(out.download_gb)}）`, warnings: out.warnings ?? [], downloadGB: out.download_gb }
    return out
  } catch (e) {
    if (isApiError(e) && (e.code === 'unauthorized' || e.code === 'cancelled')) return null
    showError(e)
    return null
  }
}
const validating = ref(false)
async function onValidate() {
  if (validating.value) return
  validating.value = true
  try {
    await validate()
  } finally {
    validating.value = false
  }
}

// ---- 保存 ----
async function save() {
  if (busy.value || !localCheck()) return
  busy.value = true
  try {
    if (isNew.value) {
      const v = await api.post<AdminWorkflowVersion>(`/workflows/${wfId.value}/versions`, fullBody())
      ElMessage.success('版本已创建（草稿）')
      savedSnapshot.value = snapshot()
      await router.replace(`/workflows/${wfId.value}/versions/${v.id}`) // 路由变化触发重新加载
    } else {
      await api.put(`/workflows/${wfId.value}/versions/${vid.value}`, body())
      ElMessage.success('已保存')
      await load()
    }
  } catch (e) {
    showError(e)
    notifyError(e)
  } finally {
    busy.value = false
  }
}

// ---- 发布（super + 动态码，D6）----
const publish = reactive({ visible: false, checking: false, ok: false, text: '', downloadGB: 0, warnings: [] as string[] })
async function openPublish() {
  if (dirty.value) {
    ElMessage.warning('有未保存的修改，请先保存再发布')
    return
  }
  Object.assign(publish, { visible: true, checking: true, ok: false, text: '', downloadGB: version.value?.download_gb ?? totalGB.value, warnings: [] })
  // 先校验：注定失败的发布不弹动态码、不消耗动态码
  const out = await validate()
  Object.assign(publish, { checking: false, ok: !!out, text: result.value?.text ?? '', warnings: out?.warnings ?? [], downloadGB: out?.download_gb ?? publish.downloadGB })
}
async function confirmPublish() {
  if (busy.value || !publish.ok) return
  busy.value = true
  try {
    // totp_required 由请求层弹全局动态码框（totp-dialog）并带码重发
    await api.post<AdminWorkflowVersion>(`/workflows/${wfId.value}/versions/${vid.value}/publish`, {})
    ElMessage.success('已发布，控制台工作流市场可安装该版本')
    publish.visible = false
    await load()
  } catch (e) {
    if (isApiError(e) && e.code === 'workflow_version_invalid') {
      showError(e)
      Object.assign(publish, { ok: false, text: e.message })
    }
    notifyError(e) // 取消动态码时静默
  } finally {
    busy.value = false
  }
}

// ---- 退役（super）----
const retiring = ref(false)
async function confirmRetire() {
  if (busy.value) return
  busy.value = true
  try {
    await api.put(`/workflows/${wfId.value}/versions/${vid.value}`, { status: 'retired' })
    ElMessage.success('版本已退役')
    retiring.value = false
    await load()
  } catch (e) {
    retiring.value = false
    notifyError(e)
  } finally {
    busy.value = false
  }
}

// ---- 文件框 ----
async function readFile(e: Event, field: 'graph' | 'api') {
  const input = e.target as HTMLInputElement
  const file = input.files?.[0]
  input.value = '' // 同一个文件可以再选一次
  if (!file) return
  if (file.size > 2 << 20) {
    ElMessage.error('文件太大（> 2MB），不像是 ComfyUI 导出的工作流')
    return
  }
  form[field] = await file.text()
}
const graphFile = ref<HTMLInputElement | null>(null)
const apiFile = ref<HTMLInputElement | null>(null)

// ---- 表格 ----
const showJson = ref(false)
const manifestJson = computed(() => JSON.stringify(manifest.value, null, 2))
const removeAt = <T,>(list: T[], i: number) => list.splice(i, 1)
const addModel = () => form.manifest.models.push(emptyModelRow())
const addNode = () => form.manifest.nodes.push(emptyNodeRow())
const addInput = () => form.manifest.inputs.push(emptyInputRow())
const addOutput = () => form.manifest.outputs.push(emptyOutputRow())
/** D16：把 graph 引用但未声明的模型补成模型行（目录取加载节点的默认目录；大小 / 来源仍需手填）。 */
function addMissingModels() {
  for (const m of missing.value.models) form.manifest.models.push({ ...emptyModelRow(), name: m.name, dir: m.dir, ms_file: m.name, hf_file: m.name })
}
function onPickNode(row: InputRow) {
  const n = apiById.value.get(row.node)
  if (!n || !(row.input in n.values)) row.input = '' // 换了节点：原来的输入不属于它
}
function onPickInput(row: InputRow) {
  Object.assign(row, suggestInput(row, apiById.value.get(row.node)))
}
const nodeInputs = (row: InputRow) => apiById.value.get(row.node)?.inputs ?? []
const currentValue = (row: InputRow) => {
  const v = apiById.value.get(row.node)?.values[row.input]
  return v === undefined ? '' : `现值 ${typeof v === 'string' ? v.slice(0, 40) : JSON.stringify(v)}`
}

const GIT_PLACEHOLDER = 'https://github.com/owner/repo.git'

// 放在最后：immediate 加载会用到上面声明的 ref
watch(() => [route.params.id, route.params.vid, route.query.copy_from], load, { immediate: true })
</script>

<template>
  <div class="page">
    <el-alert v-if="loadError" type="error" :title="loadError" show-icon :closable="false" data-testid="adm-wf-version-load-error">
      <el-button size="small" :loading="loading" @click="load">重试</el-button>
    </el-alert>
    <el-empty v-if="notFound" description="工作流或版本不存在" data-testid="adm-wf-version-missing" />

    <el-card v-if="wf && !notFound" v-loading="loading" data-testid="adm-wf-version">
      <template #header>
        <div class="head">
          <span>
            <router-link to="/workflows" class="back">工作流</router-link> /
            <router-link :to="`/workflows/${wfId}`" class="back" data-testid="adm-wf-version-back">{{ wf.name }}</router-link> /
            <b>{{ isNew ? '新建版本' : version?.version }}</b>
            <el-tag v-if="!isNew" size="small" :type="statusType[status]" class="ml" data-testid="adm-wf-version-status">{{ labelOf(appStatusLabel, status) }}</el-tag>
            <span v-if="version?.published_at" class="muted ml">发布于 {{ fmtTime(version.published_at) }}</span>
            <span v-if="isNew && route.query.copy_from" class="muted ml" data-testid="adm-wf-version-copied">从版本 #{{ route.query.copy_from }} 复制</span>
          </span>
          <span class="row">
            <span class="muted" data-testid="adm-wf-download">下载约 {{ formatGB(totalGB) }}</span>
            <el-button v-if="editable" :loading="validating" data-testid="adm-wf-validate" @click="onValidate">校验</el-button>
            <el-button v-if="changelogEditable" type="primary" :loading="busy" data-testid="adm-wf-save" @click="save">保存</el-button>
            <el-button v-if="isSuper && !isNew && status === 'draft'" type="success" data-testid="adm-wf-publish" @click="openPublish">发布</el-button>
            <el-button v-if="isSuper && status === 'published'" type="danger" plain data-testid="adm-wf-retire" @click="retiring = true">退役</el-button>
          </span>
        </div>
      </template>

      <el-alert
        v-if="status === 'published'"
        type="info"
        :closable="false"
        show-icon
        class="mb"
        data-testid="adm-wf-readonly"
        title="已发布的版本不能修改，只能改更新说明或退役；要改工作流图 / 清单请在工作流页「从版本复制」新建版本。"
      />
      <el-alert v-else-if="status === 'retired'" type="info" :closable="false" show-icon class="mb" data-testid="adm-wf-readonly" title="已退役的版本不能修改，也不能新安装；已装的照常使用。" />
      <el-alert v-if="result && !result.ok" type="error" :title="result.text" :closable="false" show-icon class="mb" data-testid="adm-wf-error" />
      <el-alert v-if="result && result.ok" type="success" :title="result.text" :closable="false" show-icon class="mb" data-testid="adm-wf-validate-ok" />
      <el-alert v-if="result?.warnings.length" type="warning" :closable="false" show-icon class="mb" title="提示（不影响保存 / 发布）" data-testid="adm-wf-warnings">
        <div v-for="w in result.warnings" :key="w" data-testid="adm-wf-warning">{{ w }}</div>
      </el-alert>
      <el-alert
        v-if="editable && (missing.models.length || missing.nodeTypes.length)"
        type="error"
        :closable="false"
        show-icon
        class="mb"
        title="工作流图引用了清单里没有声明的内容（D16，发布校验会报错）"
        data-testid="adm-wf-missing"
      >
        <div v-if="missing.models.length" data-testid="adm-wf-missing-models">
          缺少模型：<span v-for="(m, i) in missing.models" :key="m.name" class="mono">{{ i ? '、' : '' }}{{ m.name }}（{{ m.where }}）</span>
          <el-button size="small" link type="primary" data-testid="adm-wf-add-missing" @click="addMissingModels">添加到模型表</el-button>
        </div>
        <div v-if="missing.nodeTypes.length" data-testid="adm-wf-missing-nodes">
          未声明的自定义节点类型：<span class="mono">{{ missing.nodeTypes.join('、') }}</span>（在节点表的「节点类型」里列出，并填写 Git 地址与 commit）
        </div>
      </el-alert>

      <div class="layout">
        <div class="left">
          <el-form label-position="top" :disabled="!editable" @submit.prevent>
            <div class="two">
              <el-form-item label="版本号（如 1.0.0）">
                <el-input v-model="form.version" maxlength="32" data-testid="adm-wf-version-number" />
                <div v-if="errFor('version')" class="field-error" data-testid="adm-wf-field-error">{{ errFor('version') }}</div>
              </el-form-item>
              <el-form-item label="最低 ComfyUI 版本（可空，仅展示）">
                <el-input v-model="form.manifest.comfyui_min" maxlength="16" placeholder="0.3.60" data-testid="adm-wf-comfyui-min" />
                <div v-if="errFor('manifest.comfyui_min')" class="field-error" data-testid="adm-wf-field-error">{{ errFor('manifest.comfyui_min') }}</div>
              </el-form-item>
            </div>
          </el-form>
          <!-- 更新说明单独一个表单：已发布版本也可改（el-form 的 disabled 会压过子项） -->
          <el-form label-position="top" @submit.prevent>
            <el-form-item :label="`更新说明（≤ ${LIMITS.changelog} 字）`">
              <el-input v-model="form.changelog" type="textarea" :rows="2" :maxlength="LIMITS.changelog" :disabled="!changelogEditable" data-testid="adm-wf-changelog" />
              <div v-if="errFor('changelog')" class="field-error" data-testid="adm-wf-field-error">{{ errFor('changelog') }}</div>
            </el-form-item>
          </el-form>

          <div class="file-head">
            <h4>graph.json（ComfyUI「保存」的 UI 格式，必填）</h4>
            <template v-if="editable">
              <input ref="graphFile" type="file" accept=".json,application/json" class="hidden" data-testid="adm-wf-graph-file" @change="readFile($event, 'graph')" />
              <el-button size="small" @click="graphFile?.click()">选择文件</el-button>
            </template>
          </div>
          <textarea
            v-model="form.graph"
            class="code"
            :class="{ 'has-error': !!errFor('graph') }"
            :readonly="!editable"
            spellcheck="false"
            wrap="off"
            placeholder="粘贴或选择 ComfyUI 菜单「工作流 → 保存」得到的 JSON"
            data-testid="adm-wf-graph"
          />
          <div class="file-info" data-testid="adm-wf-graph-info">
            <template v-if="graphInfo.ok">{{ graphInfo.nodes }} 个节点 · {{ formatBytes(graphInfo.bytes) }}（上限 256 KB）</template>
            <span v-else-if="!graphInfo.empty" class="bad">{{ graphInfo.error }}</span>
            <span v-else class="muted">未提供</span>
          </div>
          <div v-if="errFor('graph')" class="field-error" data-testid="adm-wf-field-error">{{ errFor('graph') }}</div>

          <div class="file-head">
            <h4>api.json（「导出 (API)」格式，可空 = 不支持快速出图）</h4>
            <template v-if="editable">
              <input ref="apiFile" type="file" accept=".json,application/json" class="hidden" data-testid="adm-wf-api-file" @change="readFile($event, 'api')" />
              <el-button size="small" @click="apiFile?.click()">选择文件</el-button>
            </template>
          </div>
          <textarea
            v-model="form.api"
            class="code"
            :class="{ 'has-error': !!errFor('api_prompt') }"
            :readonly="!editable"
            spellcheck="false"
            wrap="off"
            placeholder="可空。ComfyUI 设置里打开开发者模式后，菜单「工作流 → 导出 (API)」"
            data-testid="adm-wf-api"
          />
          <div class="file-info" data-testid="adm-wf-api-info">
            <template v-if="apiInfo.empty"><span class="muted">未提供：该版本不支持快速出图</span></template>
            <template v-else-if="apiInfo.ok">{{ apiInfo.nodes.length }} 个节点 · {{ formatBytes(apiInfo.bytes) }}（上限 256 KB）</template>
            <span v-else class="bad">{{ apiInfo.error }}</span>
          </div>
          <div v-if="errFor('api_prompt')" class="field-error" data-testid="adm-wf-field-error">{{ errFor('api_prompt') }}</div>
        </div>

        <div class="right">
          <el-form label-position="top" :disabled="!editable" @submit.prevent>
            <h4>模型（≤ {{ LIMITS.models }} 个；只允许 ModelScope / hf-mirror，至少给一个来源）</h4>
            <div class="scroll">
              <table class="grid" data-testid="adm-wf-models">
                <thead><tr><th>文件名</th><th>目录</th><th>大小 GB</th><th>ModelScope 仓库 / 文件</th><th>HF 仓库 / 文件</th><th>sha256（可空）</th><th v-if="editable"></th></tr></thead>
                <tbody>
                  <tr v-for="(m, i) in form.manifest.models" :key="i" :class="{ 'err-row': rowErr('models', i) }" data-testid="adm-wf-model-row">
                    <td :class="{ 'err-cell': cellErr('models', i, 'name') }"><el-input v-model="m.name" size="small" class="mono" style="width: 200px" data-testid="adm-wf-model-name" /></td>
                    <td :class="{ 'err-cell': cellErr('models', i, 'dir') }">
                      <el-select v-model="m.dir" size="small" style="width: 140px" data-testid="adm-wf-model-dir">
                        <el-option v-for="d in MODEL_DIRS" :key="d" :label="d" :value="d" />
                      </el-select>
                    </td>
                    <td :class="{ 'err-cell': cellErr('models', i, 'size_gb') }"><el-input v-model="m.size_gb" size="small" placeholder="6.94" style="width: 70px" data-testid="adm-wf-model-size" /></td>
                    <td :class="{ 'err-cell': cellErr('models', i, 'ms_repo', 'ms_file') }">
                      <div class="stack">
                        <el-input v-model="m.ms_repo" size="small" class="mono" placeholder="owner/repo" style="width: 220px" data-testid="adm-wf-model-ms-repo" />
                        <el-input v-model="m.ms_file" size="small" class="mono" placeholder="仓库内路径" style="width: 220px" data-testid="adm-wf-model-ms-file" />
                      </div>
                    </td>
                    <td :class="{ 'err-cell': cellErr('models', i, 'hf_repo', 'hf_file') }">
                      <div class="stack">
                        <el-input v-model="m.hf_repo" size="small" class="mono" placeholder="owner/repo" style="width: 220px" data-testid="adm-wf-model-hf-repo" />
                        <el-input v-model="m.hf_file" size="small" class="mono" placeholder="仓库内路径" style="width: 220px" data-testid="adm-wf-model-hf-file" />
                      </div>
                    </td>
                    <td :class="{ 'err-cell': cellErr('models', i, 'sha256') }"><el-input v-model="m.sha256" size="small" class="mono" style="width: 160px" data-testid="adm-wf-model-sha256" /></td>
                    <td v-if="editable"><el-button size="small" link type="danger" data-testid="adm-wf-model-remove" @click="removeAt(form.manifest.models, i)">删除</el-button></td>
                  </tr>
                  <tr v-if="!form.manifest.models.length"><td colspan="7" class="muted">没有模型</td></tr>
                </tbody>
              </table>
            </div>
            <el-button v-if="editable && form.manifest.models.length < LIMITS.models" size="small" class="add" data-testid="adm-wf-model-add" @click="addModel">添加模型</el-button>
            <div v-for="t in tableErrors('models')" :key="t" class="field-error" data-testid="adm-wf-field-error">{{ t }}</div>

            <h4>自定义节点（≤ {{ LIMITS.nodes }} 个；Git 只允许 github.com / gitee.com / gitcode.com，钉 40 位 commit）</h4>
            <div class="scroll">
              <table class="grid" data-testid="adm-wf-nodes">
                <thead><tr><th>目录名</th><th>Git 地址</th><th>commit</th><th>节点类型（逗号或换行分隔）</th><th v-if="editable"></th></tr></thead>
                <tbody>
                  <tr v-for="(n, i) in form.manifest.nodes" :key="i" :class="{ 'err-row': rowErr('nodes', i) }" data-testid="adm-wf-node-row">
                    <td :class="{ 'err-cell': cellErr('nodes', i, 'name') }"><el-input v-model="n.name" size="small" class="mono" style="width: 180px" data-testid="adm-wf-node-name" /></td>
                    <td :class="{ 'err-cell': cellErr('nodes', i, 'git') }"><el-input v-model="n.git" size="small" class="mono" :placeholder="GIT_PLACEHOLDER" style="width: 280px" data-testid="adm-wf-node-git" /></td>
                    <td :class="{ 'err-cell': cellErr('nodes', i, 'commit') }"><el-input v-model="n.commit" size="small" class="mono" style="width: 200px" data-testid="adm-wf-node-commit" /></td>
                    <td :class="{ 'err-cell': cellErr('nodes', i, 'node_types') }">
                      <el-input v-model="n.node_types" type="textarea" :autosize="{ minRows: 1, maxRows: 6 }" size="small" class="mono" style="width: 240px" data-testid="adm-wf-node-types" />
                    </td>
                    <td v-if="editable"><el-button size="small" link type="danger" data-testid="adm-wf-node-remove" @click="removeAt(form.manifest.nodes, i)">删除</el-button></td>
                  </tr>
                  <tr v-if="!form.manifest.nodes.length"><td colspan="5" class="muted">没有自定义节点</td></tr>
                </tbody>
              </table>
            </div>
            <el-button v-if="editable && form.manifest.nodes.length < LIMITS.nodes" size="small" class="add" data-testid="adm-wf-node-add" @click="addNode">添加节点</el-button>
            <div v-for="t in tableErrors('nodes')" :key="t" class="field-error" data-testid="adm-wf-field-error">{{ t }}</div>

            <h4>快速出图参数（≤ {{ LIMITS.inputs }} 个；节点 / 输入来自 api.json，只能选非连线的输入）</h4>
            <div class="scroll">
              <table class="grid" data-testid="adm-wf-inputs">
                <thead><tr><th>节点</th><th>输入</th><th>key</th><th>名称</th><th>类型</th><th>默认值</th><th>范围 / 步长</th><th>最长</th><th>选项（一行一个）</th><th v-if="editable"></th></tr></thead>
                <tbody>
                  <tr v-for="(r, i) in form.manifest.inputs" :key="i" :class="{ 'err-row': rowErr('inputs', i) }" data-testid="adm-wf-input-row">
                    <td :class="{ 'err-cell': cellErr('inputs', i, 'node') }">
                      <el-select v-model="r.node" size="small" filterable allow-create style="width: 170px" placeholder="节点" data-testid="adm-wf-input-node" @change="onPickNode(r)">
                        <el-option v-for="n in apiNodes ?? []" :key="n.id" :label="`${n.id} · ${n.class_type}`" :value="n.id" />
                      </el-select>
                    </td>
                    <td :class="{ 'err-cell': cellErr('inputs', i, 'input') }">
                      <el-select v-model="r.input" size="small" filterable allow-create style="width: 140px" placeholder="输入" data-testid="adm-wf-input-input" @change="onPickInput(r)">
                        <el-option v-for="k in nodeInputs(r)" :key="k" :label="k" :value="k" />
                      </el-select>
                      <div class="hint">{{ currentValue(r) }}</div>
                    </td>
                    <td :class="{ 'err-cell': cellErr('inputs', i, 'key') }"><el-input v-model="r.key" size="small" class="mono" style="width: 100px" data-testid="adm-wf-input-key" /></td>
                    <td :class="{ 'err-cell': cellErr('inputs', i, 'label') }"><el-input v-model="r.label" size="small" maxlength="32" style="width: 100px" data-testid="adm-wf-input-label" /></td>
                    <td :class="{ 'err-cell': cellErr('inputs', i, 'type') }">
                      <el-select v-model="r.type" size="small" style="width: 96px" data-testid="adm-wf-input-type">
                        <el-option v-for="t in INPUT_TYPES" :key="t" :label="t" :value="t" />
                      </el-select>
                    </td>
                    <td :class="{ 'err-cell': cellErr('inputs', i, 'default') }">
                      <el-select v-if="r.type === 'bool'" v-model="r.default" size="small" clearable style="width: 100px" data-testid="adm-wf-input-default">
                        <el-option label="true" value="true" /><el-option label="false" value="false" />
                      </el-select>
                      <el-input
                        v-else-if="inputHas.default(r.type)"
                        v-model="r.default"
                        size="small"
                        :placeholder="r.type === 'seed' ? '-1 = 随机' : ''"
                        style="width: 140px"
                        data-testid="adm-wf-input-default"
                      />
                      <span v-else class="muted">—</span>
                    </td>
                    <td :class="{ 'err-cell': cellErr('inputs', i, 'min', 'max', 'step') }">
                      <span v-if="inputHas.range(r.type)" class="row">
                        <el-input v-model="r.min" size="small" placeholder="min" style="width: 64px" data-testid="adm-wf-input-min" />~<el-input v-model="r.max" size="small" placeholder="max" style="width: 64px" data-testid="adm-wf-input-max" />
                        <el-input v-model="r.step" size="small" placeholder="step" style="width: 56px" data-testid="adm-wf-input-step" />
                      </span>
                      <span v-else class="muted">—</span>
                    </td>
                    <td :class="{ 'err-cell': cellErr('inputs', i, 'max_len') }">
                      <el-input v-if="inputHas.maxLen(r.type)" v-model="r.max_len" size="small" placeholder="1000" style="width: 64px" data-testid="adm-wf-input-maxlen" />
                      <span v-else class="muted">—</span>
                    </td>
                    <td :class="{ 'err-cell': cellErr('inputs', i, 'options') }">
                      <el-input v-if="inputHas.options(r.type)" v-model="r.options" type="textarea" :autosize="{ minRows: 1, maxRows: 6 }" size="small" style="width: 160px" data-testid="adm-wf-input-options" />
                      <span v-else class="muted">—</span>
                    </td>
                    <td v-if="editable"><el-button size="small" link type="danger" data-testid="adm-wf-input-remove" @click="removeAt(form.manifest.inputs, i)">删除</el-button></td>
                  </tr>
                  <tr v-if="!form.manifest.inputs.length"><td colspan="10" class="muted">{{ apiNodes ? '没有参数（快速出图表单为空）' : '没有 api.json：不能定义快速出图参数' }}</td></tr>
                </tbody>
              </table>
            </div>
            <el-button v-if="editable && apiNodes && form.manifest.inputs.length < LIMITS.inputs" size="small" class="add" data-testid="adm-wf-input-add" @click="addInput">添加参数</el-button>
            <div v-for="t in tableErrors('inputs')" :key="t" class="field-error" data-testid="adm-wf-field-error">{{ t }}</div>

            <h4>输出节点（≤ {{ LIMITS.outputs }} 个；留空 = 收集全部图片 / 视频输出）</h4>
            <div class="scroll">
              <table class="grid" data-testid="adm-wf-outputs">
                <thead><tr><th>节点</th><th v-if="editable"></th></tr></thead>
                <tbody>
                  <tr v-for="(o, i) in form.manifest.outputs" :key="i" :class="{ 'err-row': rowErr('outputs', i) }" data-testid="adm-wf-output-row">
                    <td :class="{ 'err-cell': cellErr('outputs', i, 'node') }">
                      <el-select v-model="o.node" size="small" filterable allow-create style="width: 220px" placeholder="保存图片 / 视频的节点" data-testid="adm-wf-output-node">
                        <el-option v-for="n in outputNodes" :key="n.id" :label="`${n.id} · ${n.class_type}`" :value="n.id" />
                      </el-select>
                    </td>
                    <td v-if="editable"><el-button size="small" link type="danger" data-testid="adm-wf-output-remove" @click="removeAt(form.manifest.outputs, i)">删除</el-button></td>
                  </tr>
                  <tr v-if="!form.manifest.outputs.length"><td colspan="2" class="muted">未指定</td></tr>
                </tbody>
              </table>
            </div>
            <el-button v-if="editable && apiNodes && form.manifest.outputs.length < LIMITS.outputs" size="small" class="add" data-testid="adm-wf-output-add" @click="addOutput">添加输出节点</el-button>
            <div v-for="t in tableErrors('outputs')" :key="t" class="field-error" data-testid="adm-wf-field-error">{{ t }}</div>
          </el-form>
          <div class="json-toggle">
            <el-button size="small" link type="primary" data-testid="adm-wf-json-toggle" @click="showJson = !showJson">{{ showJson ? '收起 JSON' : '查看 JSON' }}</el-button>
          </div>
          <pre v-if="showJson" class="json" data-testid="adm-wf-json">{{ manifestJson }}</pre>
        </div>
      </div>
    </el-card>

    <!-- 发布确认：先校验并显示下载量，确认后由请求层弹全局动态码框（totp-dialog） -->
    <el-dialog v-model="publish.visible" title="发布工作流版本" width="560" align-center data-testid="adm-wf-totp">
      <p>发布「{{ wf?.name }} {{ version?.version }}」后版本即冻结（只能改更新说明或退役），控制台工作流市场即可安装。</p>
      <p>
        用户安装时需下载约 <b data-testid="adm-wf-publish-download">{{ formatGB(publish.downloadGB) }}</b>
        （{{ form.manifest.models.length }} 个模型、{{ form.manifest.nodes.length }} 个自定义节点）。
      </p>
      <div v-loading="publish.checking" class="publish-check">
        <el-alert v-if="!publish.checking" :type="publish.ok ? 'success' : 'error'" :title="publish.ok ? '校验通过' : publish.text" :closable="false" show-icon data-testid="adm-wf-publish-check" />
      </div>
      <el-alert v-if="publish.warnings.length" type="warning" :closable="false" show-icon class="mt" title="提示" data-testid="adm-wf-publish-warnings">
        <div v-for="w in publish.warnings" :key="w">{{ w }}</div>
      </el-alert>
      <p class="muted">确认后需要输入动态码。</p>
      <template #footer>
        <el-button @click="publish.visible = false">取消</el-button>
        <el-button type="success" :disabled="publish.checking || !publish.ok" :loading="busy" data-testid="adm-wf-publish-ok" @click="confirmPublish">确认发布</el-button>
      </template>
    </el-dialog>

    <el-dialog v-model="retiring" title="退役版本" width="440" align-center data-testid="adm-wf-retire-dialog">
      <p>退役「{{ version?.version }}」？退役后不能再新安装或作为「更新」的目标；已装的照常使用。不可撤销。</p>
      <template #footer>
        <el-button @click="retiring = false">取消</el-button>
        <el-button type="danger" :loading="busy" data-testid="adm-wf-retire-ok" @click="confirmRetire">确定退役</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.page { display: flex; flex-direction: column; gap: 16px; }
.head { display: flex; justify-content: space-between; align-items: center; gap: 12px; flex-wrap: wrap; }
.row { display: inline-flex; align-items: center; gap: 6px; }
.ml { margin-left: 8px; }
.mb { margin-bottom: 12px; }
.mt { margin-top: 12px; }
.back { color: var(--el-color-primary); text-decoration: none; }
.muted { color: #909399; font-size: 13px; }
.bad { color: var(--el-color-danger); }
.hint { color: #909399; font-size: 11px; max-width: 140px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.layout { display: grid; grid-template-columns: minmax(320px, 0.8fr) minmax(0, 2fr); gap: 24px; align-items: start; }
@media (max-width: 1280px) { .layout { grid-template-columns: minmax(0, 1fr); } }
.two { display: grid; grid-template-columns: 1fr 1fr; gap: 12px; }
h4 { margin: 12px 0 8px; }
.file-head { display: flex; justify-content: space-between; align-items: center; gap: 8px; }
.hidden { display: none; }
.code { width: 100%; box-sizing: border-box; height: 220px; resize: vertical; font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 12px; line-height: 18px; padding: 8px; border: 1px solid var(--el-border-color); border-radius: 4px; white-space: pre; overflow: auto; }
.code[readonly] { background: var(--el-fill-color-light); }
.code.has-error { border-color: var(--el-color-danger); }
.file-info { font-size: 12px; margin-top: 4px; }
.scroll { overflow-x: auto; }
.grid { width: 100%; border-collapse: collapse; font-size: 13px; }
.grid th, .grid td { padding: 4px 6px; text-align: left; border-bottom: 1px solid var(--el-border-color-lighter); vertical-align: top; white-space: nowrap; }
.stack { display: flex; flex-direction: column; gap: 4px; }
.err-row { background: var(--el-color-danger-light-9); }
.err-cell :deep(.el-input__wrapper), .err-cell :deep(.el-textarea__inner), .err-cell :deep(.el-select__wrapper) { box-shadow: 0 0 0 1px var(--el-color-danger) inset; }
.add { margin-top: 6px; }
.field-error { color: var(--el-color-danger); font-size: 12px; line-height: 1.5; margin-top: 4px; white-space: pre-wrap; }
.mono, .mono :deep(input), .mono :deep(textarea) { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.json-toggle { margin-top: 12px; }
.json { background: var(--el-fill-color-light); padding: 8px 12px; border-radius: 4px; font-size: 12px; max-height: 360px; overflow: auto; }
.publish-check { min-height: 40px; }
</style>
