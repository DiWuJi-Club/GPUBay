<script setup lang="ts">
import {
  appStatusLabel, fmtTime, isApiError, labelOf, messageOf, type AdminApp, type AdminAppVersion, type AdminImage, type AppCoverage, type Listed,
} from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import {
  coverageLow, coverageText, emptyEnvRow, emptyLinkRow, emptyPortRow, emptySpec, ENV_TYPES, envHas, fieldPathToRow, formIssues, formToSpec,
  scriptErrorLine, specToForm, splitFieldError, type FieldIssue, type SpecForm,
} from '../logic/appSpec'
import { notifyError } from '../logic/notify'
import { OPS_ROLES } from '../menu'
import { hasRole, session } from '../store/session'

// 版本编辑页（规格 §12）：/apps/:id/versions/:vid 与 /apps/:id/versions/new?copy_from=<版本 id>。
// 草稿（super）可改全部；已发布只能改 changelog 与退役；已退役只读。ops / finance 只读（ops 可重新预拉取）。
const route = useRoute()
const router = useRouter()
const appId = computed(() => Number(route.params.id))
const vid = computed(() => String(route.params.vid))
const isNew = computed(() => vid.value === 'new')

const app = ref<AdminApp | null>(null)
const version = ref<AdminAppVersion | null>(null)
const images = ref<AdminImage[]>([])
const coverage = ref<AppCoverage[]>([])
const loading = ref(false)
const loadError = ref('')
const notFound = ref(false)
const busy = ref(false)

const isSuper = computed(() => session.admin?.role === 'super')
const canPrepull = computed(() => hasRole(OPS_ROLES))
const status = computed(() => version.value?.status ?? 'draft')
/** 可改全部字段：新建或草稿，且是 super。 */
const editable = computed(() => isSuper.value && (isNew.value || status.value === 'draft'))
const changelogEditable = computed(() => isSuper.value && status.value !== 'retired')
const statusType: Record<string, 'info' | 'success' | 'warning'> = { draft: 'info', published: 'success', retired: 'warning' }

interface VersionForm {
  version: string
  image_ids: number[]
  changelog: string
  script: string
  spec: SpecForm
}
const blankForm = (): VersionForm => ({ version: '', image_ids: [], changelog: '', script: '', spec: specToForm(emptySpec()) })
const form = reactive<VersionForm>(blankForm())
const savedSnapshot = ref('')
const snapshot = () => JSON.stringify(form)
const dirty = computed(() => snapshot() !== savedSnapshot.value)

const imageName = computed(() => new Map(images.value.map((i) => [i.id, i.name])))
/** 可选镜像：启用的；已选中但已停用的也列出（否则下拉里显示成裸 id）。 */
const imageOptions = computed(() => images.value.filter((i) => i.enabled || form.image_ids.includes(i.id)))

// ---- 加载 ----
async function load() {
  if (route.matched[0]?.path !== '/apps/:id/versions/:vid') return // 离开本页时 route 先变，不再加载
  loading.value = true
  loadError.value = ''
  notFound.value = false
  clearResult()
  try {
    const copyFrom = isNew.value && route.query.copy_from ? Number(route.query.copy_from) : null
    const [apps, imgs, vs] = await Promise.all([
      api.get<Listed<AdminApp>>('/apps'),
      api.get<Listed<AdminImage>>('/images'),
      api.get<Listed<AdminAppVersion>>(`/apps/${appId.value}/versions`),
    ])
    app.value = apps.items.find((a) => a.id === appId.value) ?? null
    images.value = imgs.items
    const srcId = isNew.value ? copyFrom : Number(vid.value)
    const src = srcId ? (vs.items.find((v) => v.id === srcId) ?? null) : null
    if (!app.value || (isNew.value ? copyFrom !== null && !src : !src)) {
      notFound.value = true
      return
    }
    version.value = isNew.value ? null : src
    coverage.value = isNew.value ? [] : (src?.coverage ?? [])
    const next = blankForm()
    if (src) {
      const { script } = await api.get<{ script: string }>(`/apps/${appId.value}/versions/${src.id}/script`)
      Object.assign(next, { image_ids: [...src.image_ids], spec: specToForm(src.spec), script })
      if (!isNew.value) Object.assign(next, { version: src.version, changelog: src.changelog }) // 复制不带版本号与更新说明
    }
    Object.assign(form, next)
    savedSnapshot.value = isNew.value ? JSON.stringify(blankForm()) : snapshot()
  } catch (e) {
    notifyError(e)
    loadError.value = messageOf(e)
  } finally {
    loading.value = false
  }
}

// ---- 请求体 ----
const fullBody = () => ({ version: form.version.trim(), image_ids: form.image_ids, spec: formToSpec(form.spec), script: form.script, changelog: form.changelog })
/** 已发布版本只发 changelog（省略的字段后端沿用当前值）；改其它字段会 409 app_version_frozen。 */
const body = () => (editable.value ? fullBody() : { changelog: form.changelog })

// ---- 校验结果与字段高亮 ----
const result = ref<{ ok: boolean; text: string } | null>(null)
const fieldError = ref<FieldIssue | null>(null)
const errRow = computed(() => (fieldError.value ? fieldPathToRow(fieldError.value.path) : null))
const errFor = (path: string) => (fieldError.value?.path === path ? fieldError.value.reason : '')
const rowErr = (table: 'ports' | 'env' | 'links', i: number) => errRow.value?.table === table && errRow.value.index === i
const cellErr = (table: 'ports' | 'env' | 'links', i: number, field: string) => rowErr(table, i) && errRow.value?.field === field
const COLUMN: Record<string, string> = {
  port: '端口', label: '名称', path: '路径', health_path: '健康检查路径', name: '名', type: '类型', default: '默认', options: '选项', pattern: '正则',
  min: '范围', max: '范围', max_len: '最长', help: '帮助',
}
const tableErr = (table: 'ports' | 'env' | 'links') =>
  errRow.value?.table === table ? `第 ${errRow.value.index + 1} 行${errRow.value.field ? ` ${COLUMN[errRow.value.field] ?? errRow.value.field}` : ''}：${fieldError.value!.reason}` : errFor(table)
const scriptErrLine = computed(() => (fieldError.value?.path === 'script' ? scriptErrorLine(fieldError.value.reason) : null))

function clearResult() {
  result.value = null
  fieldError.value = null
}
function showIssue(issue: FieldIssue) {
  fieldError.value = issue
  result.value = { ok: false, text: `${issue.path}：${issue.reason}` }
}
/** 后端错误：app_version_invalid 的「路径：原因」定位到字段；其余（含 app_version_frozen）原文显示。 */
function showError(e: unknown) {
  const split = isApiError(e) && e.code === 'app_version_invalid' ? splitFieldError(e.message) : null
  fieldError.value = split
  result.value = { ok: false, text: messageOf(e) }
}
/** 本地先查数值列，避免把 NaN 发给后端。 */
function localCheck(): boolean {
  const issues = editable.value ? formIssues(form.spec) : []
  if (issues.length) showIssue(issues[0])
  return !issues.length
}

/** ?validate=1：只校验不保存（新建走 POST，已有版本走 PUT，同一套冻结规则）。 */
async function validate(): Promise<boolean> {
  if (!localCheck()) return false
  try {
    const path = isNew.value ? `/apps/${appId.value}/versions?validate=1` : `/apps/${appId.value}/versions/${vid.value}?validate=1`
    const out = isNew.value ? await api.post<{ ok: boolean; spec_hash: string }>(path, body()) : await api.put<{ ok: boolean; spec_hash: string }>(path, body())
    fieldError.value = null
    result.value = { ok: true, text: `校验通过（spec_hash ${out.spec_hash.slice(0, 12)}）` }
    return true
  } catch (e) {
    if (isApiError(e) && (e.code === 'unauthorized' || e.code === 'cancelled')) return false
    showError(e)
    return false
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
      const v = await api.post<AdminAppVersion>(`/apps/${appId.value}/versions`, fullBody())
      ElMessage.success('版本已创建（草稿）')
      savedSnapshot.value = snapshot()
      await router.replace(`/apps/${appId.value}/versions/${v.id}`) // 路由变化触发重新加载
    } else {
      await api.put(`/apps/${appId.value}/versions/${vid.value}`, body())
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

// ---- 发布（super + 动态码）----
const publish = reactive({ visible: false, checking: false, ok: false, text: '', coverage: [] as AppCoverage[] })
const lowCoverage = computed(() => publish.coverage.filter(coverageLow))
async function openPublish() {
  if (dirty.value) {
    ElMessage.warning('有未保存的修改，请先保存再发布')
    return
  }
  Object.assign(publish, { visible: true, checking: true, ok: false, text: '', coverage: coverage.value })
  // 先校验：注定失败的发布不弹动态码、不消耗动态码；覆盖率取最新
  const [ok] = await Promise.all([
    validate(),
    api.get<Listed<AdminAppVersion>>(`/apps/${appId.value}/versions`).then(
      (vs) => (publish.coverage = vs.items.find((v) => v.id === version.value?.id)?.coverage ?? []),
      (e) => notifyError(e),
    ),
  ])
  Object.assign(publish, { checking: false, ok, text: result.value?.text ?? '' })
}
async function confirmPublish() {
  if (busy.value || !publish.ok) return
  busy.value = true
  try {
    // totp_required 由请求层弹全局动态码框并带码重发；totp_used 在框内提示「请等待下一个动态码」
    const out = await api.post<{ version: AdminAppVersion; prepull_machines: number }>(`/apps/${appId.value}/versions/${vid.value}/publish`, {})
    ElMessage.success(`已发布，已向 ${out.prepull_machines} 台在线机器下发预拉取`)
    publish.visible = false
    await load()
  } catch (e) {
    if (isApiError(e) && e.code === 'app_version_invalid') {
      showError(e)
      Object.assign(publish, { ok: false, text: e.message })
    }
    notifyError(e) // 取消动态码时静默
  } finally {
    busy.value = false
  }
}

// ---- 退役（super）/ 重新预拉取（ops）----
const retiring = ref(false)
async function confirmRetire() {
  if (busy.value) return
  busy.value = true
  try {
    await api.put(`/apps/${appId.value}/versions/${vid.value}`, { status: 'retired' })
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
async function prepull() {
  if (busy.value) return
  busy.value = true
  try {
    const out = await api.post<{ machines: number }>(`/apps/${appId.value}/versions/${vid.value}/prepull`, {})
    ElMessage.success(`已向 ${out.machines} 台在线机器下发预拉取`)
  } catch (e) {
    notifyError(e)
  } finally {
    busy.value = false
  }
}

// ---- spec 表格 ----
const showJson = ref(false)
const specJson = computed(() => JSON.stringify(formToSpec(form.spec), null, 2))
const addPort = () => form.spec.ports.push(emptyPortRow())
const addEnv = () => form.spec.env.push(emptyEnvRow())
const addLink = () => form.spec.links.push(emptyLinkRow())
const removeAt = <T,>(list: T[], i: number) => list.splice(i, 1)

// ---- 脚本编辑框：左侧行号列与 textarea 同步滚动 ----
const gutter = ref<HTMLElement | null>(null)
const lineCount = computed(() => Math.max(1, form.script.split('\n').length))
function syncScroll(e: Event) {
  if (gutter.value) gutter.value.scrollTop = (e.target as HTMLTextAreaElement).scrollTop
}
// 放在最后：immediate 加载会用到上面声明的 ref
watch(() => [route.params.id, route.params.vid, route.query.copy_from], load, { immediate: true })

const SCRIPT_PLACEHOLDER = '#!/bin/sh\n# 由平台 run.sh 以 `sh app.sh install|start|stop` 调用；env 里的变量已导出\nset -eu\ncase "${1:-}" in\n  install) ;;\n  start) ;;\nesac'
</script>

<template>
  <div class="page">
    <el-alert v-if="loadError" type="error" :title="loadError" show-icon :closable="false" data-testid="app-version-error">
      <el-button size="small" :loading="loading" @click="load">重试</el-button>
    </el-alert>
    <el-empty v-if="notFound" description="应用或版本不存在" data-testid="app-version-missing" />

    <el-card v-if="app && !notFound" v-loading="loading">
      <template #header>
        <div class="head">
          <span>
            <router-link to="/apps" class="back">应用</router-link> /
            <router-link :to="`/apps/${appId}`" class="back" data-testid="app-version-back">{{ app.name }}</router-link> /
            <b>{{ isNew ? '新建版本' : version?.version }}</b>
            <el-tag v-if="!isNew" size="small" :type="statusType[status]" class="ml" data-testid="app-version-status">{{ labelOf(appStatusLabel, status) }}</el-tag>
            <span v-if="version?.published_at" class="muted ml">发布于 {{ fmtTime(version.published_at) }}</span>
            <span v-if="isNew && route.query.copy_from" class="muted ml">从版本 #{{ route.query.copy_from }} 复制</span>
          </span>
          <span class="row">
            <el-button v-if="editable" :loading="validating" data-testid="app-version-validate" @click="onValidate">校验</el-button>
            <el-button v-if="changelogEditable" type="primary" :loading="busy" data-testid="app-version-save" @click="save">保存</el-button>
            <el-button v-if="isSuper && !isNew && status === 'draft'" type="success" data-testid="app-version-publish" @click="openPublish">发布</el-button>
            <el-button v-if="canPrepull && !isNew && status !== 'retired'" :loading="busy" data-testid="app-version-prepull" @click="prepull">重新预拉取</el-button>
            <el-button v-if="isSuper && status === 'published'" type="danger" plain data-testid="app-version-retire" @click="retiring = true">退役</el-button>
          </span>
        </div>
      </template>

      <el-alert v-if="status === 'published'" type="info" :closable="false" show-icon class="mb" title="已发布的版本不能修改，只能改更新说明或退役；要改脚本 / spec 请在应用页「从版本复制」新建版本。" />
      <el-alert v-else-if="status === 'retired'" type="info" :closable="false" show-icon class="mb" title="已退役的版本不能修改，也不能新部署；已有部署照常运行。" />
      <el-alert
        v-if="result"
        :type="result.ok ? 'success' : 'error'"
        :title="result.text"
        :closable="false"
        show-icon
        class="mb"
        data-testid="app-version-validate-result"
      />

      <div class="layout">
        <div class="left">
          <el-form label-position="top" :disabled="!editable" @submit.prevent>
            <div class="two">
              <el-form-item label="版本号（字母或数字开头，最多 32 位 [0-9A-Za-z.-]）">
                <el-input v-model="form.version" maxlength="32" data-testid="app-version-number" />
                <div v-if="errFor('version')" class="field-error" data-testid="app-version-field-error">{{ errFor('version') }}</div>
              </el-form-item>
              <el-form-item label="兼容镜像（第一个为默认）">
                <el-select v-model="form.image_ids" multiple placeholder="选择镜像" style="width: 100%" data-testid="app-version-images">
                  <el-option v-for="i in imageOptions" :key="i.id" :label="i.enabled ? i.name : `${i.name}（已停用）`" :value="i.id" />
                </el-select>
                <div v-if="errFor('image_ids')" class="field-error" data-testid="app-version-field-error">{{ errFor('image_ids') }}</div>
              </el-form-item>
            </div>
          </el-form>
          <!-- 更新说明单独一个表单：已发布版本也可改（el-form 的 disabled 会压过子项） -->
          <el-form label-position="top" @submit.prevent>
            <el-form-item label="更新说明（≤ 1000 字）">
              <el-input v-model="form.changelog" type="textarea" :rows="2" maxlength="1000" :disabled="!changelogEditable" data-testid="app-version-changelog" />
              <div v-if="errFor('changelog')" class="field-error" data-testid="app-version-field-error">{{ errFor('changelog') }}</div>
            </el-form-item>
          </el-form>
          <el-form label-position="top" :disabled="!editable" @submit.prevent>
            <h4>端口（1～2 个；第一个为主端口，只有它做健康检查）</h4>
            <div class="scroll">
              <table class="grid" data-testid="app-spec-ports">
                <thead><tr><th>端口</th><th>名称</th><th>路径</th><th>健康检查路径</th><th v-if="editable"></th></tr></thead>
                <tbody>
                  <tr v-for="(p, i) in form.spec.ports" :key="i" :class="{ 'err-row': rowErr('ports', i) }" data-testid="app-spec-port-row">
                    <td :class="{ 'err-cell': cellErr('ports', i, 'port') }"><el-input-number v-model="p.port" :min="1" :max="65535" :controls="false" size="small" style="width: 90px" /></td>
                    <td :class="{ 'err-cell': cellErr('ports', i, 'label') }"><el-input v-model="p.label" size="small" maxlength="32" /></td>
                    <td :class="{ 'err-cell': cellErr('ports', i, 'path') }"><el-input v-model="p.path" size="small" placeholder="/" /></td>
                    <td :class="{ 'err-cell': cellErr('ports', i, 'health_path') }">
                      <el-input v-if="i === 0" v-model="p.health_path" size="small" placeholder="空 = 只检查端口连通" />
                      <span v-else class="muted">—</span>
                    </td>
                    <td v-if="editable"><el-button size="small" link type="danger" :disabled="form.spec.ports.length <= 1" @click="removeAt(form.spec.ports, i)">删除</el-button></td>
                  </tr>
                </tbody>
              </table>
            </div>
            <el-button v-if="editable && form.spec.ports.length < 2" size="small" class="add" @click="addPort">添加端口</el-button>
            <div v-if="tableErr('ports')" class="field-error" data-testid="app-version-field-error">{{ tableErr('ports') }}</div>

            <div class="two">
              <el-form-item label="部署超时（分钟，1～180）">
                <el-input-number v-model="form.spec.timeout_minutes" :min="1" :max="180" :controls="false" data-testid="app-spec-timeout" />
                <div v-if="errFor('timeout_minutes')" class="field-error" data-testid="app-version-field-error">{{ errFor('timeout_minutes') }}</div>
              </el-form-item>
              <el-form-item label="数据盘最少剩余（GiB，0～2048）">
                <el-input-number v-model="form.spec.min_data_free_gib" :min="0" :max="2048" :controls="false" data-testid="app-spec-disk" />
                <div v-if="errFor('min_data_free_gib')" class="field-error" data-testid="app-version-field-error">{{ errFor('min_data_free_gib') }}</div>
              </el-form-item>
            </div>
          </el-form>
        </div>

        <div class="right">
          <h4>安装脚本 app.sh（POSIX sh，≤ 64 KB）</h4>
          <div class="editor" :class="{ readonly: !editable, 'has-error': !!errFor('script') }">
            <pre ref="gutter" class="gutter" aria-hidden="true"><span v-for="n in lineCount" :key="n" :class="{ err: n === scriptErrLine }">{{ n }}</span></pre>
            <textarea
              v-model="form.script"
              data-testid="app-version-script"
              :readonly="!editable"
              :placeholder="editable ? SCRIPT_PLACEHOLDER : ''"
              spellcheck="false"
              autocomplete="off"
              wrap="off"
              @scroll="syncScroll"
            />
          </div>
          <div v-if="errFor('script')" class="field-error" data-testid="app-version-field-error">{{ errFor('script') }}</div>
          <p class="muted">保存与校验都会做 POSIX sh 语法检查；脚本里不要写死密钥，用 secret 变量。</p>

          <template v-if="coverage.length">
            <h4>镜像覆盖率</h4>
            <div v-for="c in coverage" :key="c.image_id" :class="{ low: coverageLow(c) }">{{ c.image_name }}：{{ coverageText(c) }}</div>
          </template>
        </div>
      </div>

      <!-- 变量表与链接表列多，放在两栏下方占满宽度 -->
      <el-form label-position="top" :disabled="!editable" @submit.prevent>
        <h4>用户变量（≤ 16 个；secret 不能设默认值）</h4>
        <div class="scroll">
          <table class="grid env" data-testid="app-spec-env">
            <thead>
              <tr><th>名</th><th>标签</th><th>类型</th><th>必填</th><th>默认</th><th>选项（一行一个）</th><th>正则</th><th>范围</th><th>帮助</th><th v-if="editable"></th></tr>
            </thead>
            <tbody>
              <tr v-for="(e, i) in form.spec.env" :key="i" :class="{ 'err-row': rowErr('env', i) }" data-testid="app-spec-env-row">
                <td :class="{ 'err-cell': cellErr('env', i, 'name') }"><el-input v-model="e.name" size="small" placeholder="MODEL_ID" class="mono" style="width: 130px" /></td>
                <td :class="{ 'err-cell': cellErr('env', i, 'label') }"><el-input v-model="e.label" size="small" maxlength="32" style="width: 100px" /></td>
                <td :class="{ 'err-cell': cellErr('env', i, 'type') }">
                  <el-select v-model="e.type" size="small" style="width: 90px">
                    <el-option v-for="t in ENV_TYPES" :key="t" :label="t" :value="t" />
                  </el-select>
                </td>
                <td><el-checkbox v-model="e.required" /></td>
                <td :class="{ 'err-cell': cellErr('env', i, 'default') }">
                  <el-input v-if="envHas.default(e.type)" v-model="e.default" size="small" style="width: 120px" />
                  <span v-else class="muted">—</span>
                </td>
                <td :class="{ 'err-cell': cellErr('env', i, 'options') }">
                  <el-input v-if="envHas.options(e.type)" v-model="e.options" type="textarea" :autosize="{ minRows: 1, maxRows: 6 }" size="small" style="width: 160px" />
                  <span v-else class="muted">—</span>
                </td>
                <td :class="{ 'err-cell': cellErr('env', i, 'pattern') }">
                  <el-input v-if="envHas.pattern(e.type)" v-model="e.pattern" size="small" class="mono" placeholder="RE2，全匹配" style="width: 140px" />
                  <span v-else class="muted">—</span>
                </td>
                <td :class="{ 'err-cell': cellErr('env', i, 'min') || cellErr('env', i, 'max') || cellErr('env', i, 'max_len') }">
                  <span v-if="envHas.range(e.type)" class="row">
                    <el-input v-model="e.min" size="small" placeholder="min" style="width: 80px" />~<el-input v-model="e.max" size="small" placeholder="max" style="width: 80px" />
                  </span>
                  <span v-else-if="e.type !== 'bool'" class="row">最长 <el-input v-model="e.max_len" size="small" placeholder="256" style="width: 70px" /></span>
                  <span v-else class="muted">—</span>
                </td>
                <td :class="{ 'err-cell': cellErr('env', i, 'help') }"><el-input v-model="e.help" size="small" maxlength="200" style="width: 160px" /></td>
                <td v-if="editable"><el-button size="small" link type="danger" @click="removeAt(form.spec.env, i)">删除</el-button></td>
              </tr>
              <tr v-if="!form.spec.env.length"><td colspan="10" class="muted">没有变量</td></tr>
            </tbody>
          </table>
        </div>
        <el-button v-if="editable && form.spec.env.length < 16" size="small" class="add" @click="addEnv">添加变量</el-button>
        <div v-if="tableErr('env')" class="field-error" data-testid="app-version-field-error">{{ tableErr('env') }}</div>

        <h4>链接（≤ 4 个，拼在主端口地址后面）</h4>
        <div class="scroll">
          <table class="grid" data-testid="app-spec-links">
            <thead><tr><th>名称</th><th>路径</th><th v-if="editable"></th></tr></thead>
            <tbody>
              <tr v-for="(l, i) in form.spec.links" :key="i" :class="{ 'err-row': rowErr('links', i) }" data-testid="app-spec-link-row">
                <td :class="{ 'err-cell': cellErr('links', i, 'label') }"><el-input v-model="l.label" size="small" maxlength="32" /></td>
                <td :class="{ 'err-cell': cellErr('links', i, 'path') }"><el-input v-model="l.path" size="small" placeholder="/docs" /></td>
                <td v-if="editable"><el-button size="small" link type="danger" @click="removeAt(form.spec.links, i)">删除</el-button></td>
              </tr>
              <tr v-if="!form.spec.links.length"><td colspan="3" class="muted">没有链接</td></tr>
            </tbody>
          </table>
        </div>
        <el-button v-if="editable && form.spec.links.length < 4" size="small" class="add" @click="addLink">添加链接</el-button>
        <div v-if="tableErr('links')" class="field-error" data-testid="app-version-field-error">{{ tableErr('links') }}</div>
      </el-form>
      <div class="json-toggle">
        <el-button size="small" link type="primary" data-testid="app-spec-json-toggle" @click="showJson = !showJson">{{ showJson ? '收起 JSON' : '查看 JSON' }}</el-button>
      </div>
      <pre v-if="showJson" class="json" data-testid="app-spec-json">{{ specJson }}</pre>
    </el-card>

    <el-dialog v-model="publish.visible" title="发布版本" width="560" align-center data-testid="app-version-publish-dialog">
      <p>发布「{{ app?.name }} {{ version?.version }}」后版本即冻结（只能改更新说明或退役），控制台可部署，并向所有在线机器下发镜像预拉取。</p>
      <div v-loading="publish.checking" class="publish-check">
        <el-alert
          v-if="!publish.checking"
          :type="publish.ok ? 'success' : 'error'"
          :title="publish.ok ? '校验通过' : publish.text"
          :closable="false"
          show-icon
          data-testid="app-version-publish-check"
        />
      </div>
      <el-alert v-if="lowCoverage.length" type="warning" :closable="false" show-icon class="mt" data-testid="app-version-publish-coverage" title="部分在线机器还没有缓存兼容镜像">
        <div v-for="c in lowCoverage" :key="c.image_id">{{ c.image_name }}：{{ coverageText(c) }}</div>
        <div>发布不受覆盖率限制；未缓存的机器上首次部署要现场拉取镜像，会明显变慢。</div>
      </el-alert>
      <p class="muted">确认后需要输入动态码。</p>
      <template #footer>
        <el-button @click="publish.visible = false">取消</el-button>
        <el-button type="success" :disabled="publish.checking || !publish.ok" :loading="busy" data-testid="app-version-publish-ok" @click="confirmPublish">确认发布</el-button>
      </template>
    </el-dialog>

    <el-dialog v-model="retiring" title="退役版本" width="440" align-center data-testid="app-version-retire-dialog">
      <p>退役「{{ version?.version }}」？退役后不能再新部署或作为重新部署的目标；已有部署照常运行。不可撤销。</p>
      <template #footer>
        <el-button @click="retiring = false">取消</el-button>
        <el-button type="danger" :loading="busy" data-testid="app-version-retire-ok" @click="confirmRetire">确定退役</el-button>
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
.low { color: var(--el-color-warning); }
.layout { display: grid; grid-template-columns: minmax(0, 1.2fr) minmax(0, 1fr); gap: 24px; align-items: start; }
@media (max-width: 1200px) { .layout { grid-template-columns: minmax(0, 1fr); } }
.two { display: grid; grid-template-columns: 1fr 1fr; gap: 12px; }
h4 { margin: 12px 0 8px; }
.scroll { overflow-x: auto; }
.grid { width: 100%; border-collapse: collapse; font-size: 13px; }
.grid th, .grid td { padding: 4px 6px; text-align: left; border-bottom: 1px solid var(--el-border-color-lighter); vertical-align: top; white-space: nowrap; }
.err-row { background: var(--el-color-danger-light-9); }
.err-cell :deep(.el-input__wrapper), .err-cell :deep(.el-textarea__inner), .err-cell :deep(.el-select__wrapper) { box-shadow: 0 0 0 1px var(--el-color-danger) inset; }
.add { margin-top: 6px; }
.field-error { color: var(--el-color-danger); font-size: 12px; line-height: 1.5; margin-top: 4px; white-space: pre-wrap; }
.mono, .mono :deep(input) { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.json-toggle { margin-top: 12px; }
.json { background: var(--el-fill-color-light); padding: 8px 12px; border-radius: 4px; font-size: 12px; max-height: 360px; overflow: auto; }
.editor { display: flex; border: 1px solid var(--el-border-color); border-radius: 4px; height: 460px; overflow: hidden; font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 13px; line-height: 20px; }
.editor.has-error { border-color: var(--el-color-danger); }
.editor.readonly textarea { background: var(--el-fill-color-light); }
.gutter { margin: 0; padding: 8px 6px; min-width: 40px; text-align: right; color: #909399; background: var(--el-fill-color-lighter); overflow: hidden; user-select: none; font: inherit; }
.gutter span { display: block; }
.gutter span.err { color: #fff; background: var(--el-color-danger); }
.editor textarea { flex: 1; margin: 0; padding: 8px; border: 0; outline: none; resize: none; font: inherit; tab-size: 4; white-space: pre; overflow: auto; }
.publish-check { min-height: 40px; }
</style>
