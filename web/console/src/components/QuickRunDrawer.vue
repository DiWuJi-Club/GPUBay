<script setup lang="ts">
import { isApiError, messageOf, useMobile, type Deployment, type InstalledWorkflow, type QuickRun, type WorkflowInput } from '@spark/shared'
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { fetchWorkflow, getQuickRun, startQuickRun, uploadQuickRunImage } from '../api'
import {
  buildRunInputs, defaultValues, fileKind, pollDelayMs, pollExpired, quickRunErrorText, runFinished, runStatusText, safeFileURL, type QuickValues,
} from '../logic/quickrun'
import QuickRunForm from './QuickRunForm.vue'

/**
 * 快速出图抽屉（规格 §4、§6）：左侧参数表单，右侧运行状态与输出。
 * 「运行」→ POST …/runs → 每 2 秒 GET …/runs/{id}，结束（完成 / 失败）或满 30 分钟停止（D10）。
 * 输出文件的 url 是同源代理地址（Cookie 鉴权），直接作 <img> / <video> 的 src 与下载链接。
 *
 * 参数表单取自 GET /workflows/{slug}（市场详情，只有最新已发布版本的 manifest）；控制台没有按已装版本取 inputs 的接口，
 * 已装版本落后时提示先更新；工作流已下线（详情 404）时按默认参数运行（服务器按已装版本校验）；其它加载失败不允许运行。
 */
const props = defineProps<{ instanceId: string; deployment: Pick<Deployment, 'id' | 'url' | 'app_name'>; workflow: InstalledWorkflow | null }>()
const visible = defineModel<boolean>({ required: true })
const mobile = useMobile()

const inputs = ref<WorkflowInput[]>([])
const values = ref<QuickValues>({})
const loading = ref(false)
const loadError = ref('')
/** 参数表单加载失败且不是「已下线」：不允许运行（表单缺项会被服务器拒绝或按默认值误跑） */
const loadBlocking = ref(false)
const versionNote = ref('')
const form = ref<InstanceType<typeof QuickRunForm> | null>(null)

const run = ref<QuickRun | null>(null)
const starting = ref(false)
/** 正在轮询（显式状态：轮询因 4xx / 超时停止时置 false，按钮不会卡在「运行中」） */
const polling = ref(false)
const runError = ref('')
const pollNote = ref('')
const pollWarn = ref('')
let seq = 0 // 每次加载 / 运行 / 关闭递增：丢弃迟到的响应，停止旧的轮询
let timer: ReturnType<typeof setTimeout> | undefined

const wid = () => props.workflow?.workflow_id ?? 0
const inFlight = computed(() => starting.value || polling.value)

/** 作废进行中的加载 / 运行 / 轮询：之后到达的响应因 seq 不符被丢弃。 */
function stop() {
  seq++
  if (timer !== undefined) clearTimeout(timer)
  timer = undefined
  polling.value = false
}

/** 关闭 / 换工作流：清空全部状态（含 POST /runs 未返回时的 starting、详情未返回时的 loading）。 */
function reset() {
  stop()
  starting.value = false
  loading.value = false
  inputs.value = []
  values.value = {}
  loadError.value = ''
  loadBlocking.value = false
  versionNote.value = ''
  run.value = null
  runError.value = ''
  pollNote.value = ''
  pollWarn.value = ''
}

async function load() {
  const w = props.workflow
  if (!w) return
  const mine = seq
  loading.value = true
  try {
    const d = await fetchWorkflow(w.slug)
    if (mine !== seq) return
    inputs.value = d.version.manifest?.inputs ?? []
    values.value = defaultValues(inputs.value)
    if (d.version.id !== w.version_id) versionNote.value = `参数表单按最新版本 v${d.version.version} 显示（已装 v${w.version}）；如运行报参数错误，请先更新工作流`
  } catch (e) {
    if (mine !== seq) return
    const offline = isApiError(e) && e.code === 'workflow_not_found'
    loadError.value = offline ? '该工作流已下线，无法加载参数表单；将按工作流的默认参数运行' : `参数表单加载失败：${messageOf(e)}`
    loadBlocking.value = !offline
  } finally {
    if (mine === seq) loading.value = false
  }
}

watch(
  () => [visible.value, props.workflow?.workflow_id] as const,
  ([v]) => {
    reset()
    if (v) void load()
  },
  { immediate: true },
)
onBeforeUnmount(stop)

const upload = async (file: File) => (await uploadQuickRunImage(props.instanceId, props.deployment.id, wid(), file)).name

async function start() {
  if (inFlight.value || loading.value || loadBlocking.value || !props.workflow) return
  if (form.value && !form.value.validate()) return
  stop()
  const mine = seq
  starting.value = true
  runError.value = ''
  pollNote.value = ''
  pollWarn.value = ''
  run.value = null
  try {
    const r = await startQuickRun(props.instanceId, props.deployment.id, wid(), buildRunInputs(inputs.value, values.value))
    if (mine !== seq) return
    run.value = r
    if (!runFinished(r)) {
      polling.value = true
      schedule(mine, Date.now())
    }
  } catch (e) {
    if (mine === seq) runError.value = quickRunErrorText(e)
  } finally {
    if (mine === seq) starting.value = false
  }
}

function schedule(mine: number, startedAt: number) {
  timer = setTimeout(() => void poll(mine, startedAt), pollDelayMs)
}

async function poll(mine: number, startedAt: number) {
  if (mine !== seq || !run.value) return
  if (pollExpired(startedAt, Date.now())) {
    polling.value = false
    pollWarn.value = ''
    pollNote.value = '已等待 30 分钟，停止刷新；结果可在 ComfyUI 的历史记录（Queue / History）里查看'
    return
  }
  try {
    const r = await getQuickRun(props.instanceId, props.deployment.id, wid(), run.value.id)
    if (mine !== seq) return
    run.value = r
    pollWarn.value = ''
    if (runFinished(r)) {
      polling.value = false
      return
    }
  } catch (e) {
    if (mine !== seq) return
    // 4xx（运行不存在、实例已停等）不再重试；网络错误 / 5xx（ComfyUI 暂时连不上）继续轮询直到上限，并给出提示
    if (isApiError(e) && e.status >= 400 && e.status < 500 && e.status !== 429) {
      polling.value = false
      pollWarn.value = ''
      runError.value = quickRunErrorText(e)
      return
    }
    pollWarn.value = `暂时无法获取运行状态（${quickRunErrorText(e)}），正在重试…`
  }
  schedule(mine, startedAt)
}

function openComfy() {
  if (props.deployment.url) window.open(props.deployment.url, '_blank', 'noopener')
}
</script>

<template>
  <el-drawer
    v-model="visible" :title="`快速出图 · ${workflow?.name ?? ''}`" :size="mobile ? '100%' : '80%'" append-to-body destroy-on-close
    data-testid="qr-drawer"
  >
    <div class="layout">
      <section class="left">
        <el-skeleton v-if="loading" :rows="5" animated />
        <template v-else>
          <el-alert v-if="loadError" type="warning" :title="loadError" show-icon :closable="false" class="mb" data-testid="qr-load-error" />
          <el-alert v-if="versionNote" type="info" :title="versionNote" show-icon :closable="false" class="mb" data-testid="qr-version-note" />
          <QuickRunForm ref="form" v-model="values" :inputs="inputs" :upload="upload" :disabled="inFlight" />
          <el-button type="primary" :loading="inFlight" :disabled="loadBlocking" data-testid="qr-run" @click="start">
            {{ inFlight ? '运行中…' : '运行' }}
          </el-button>
        </template>
      </section>

      <section class="right">
        <el-alert v-if="runError" type="error" :title="runError" show-icon :closable="false" class="mb" data-testid="qr-error" />
        <template v-if="run">
          <div class="bar">
            <span class="status" :class="run.state" role="status" aria-live="polite" data-testid="qr-status" :data-state="run.state">{{ runStatusText(run) }}</span>
            <span v-if="run.seed != null" class="muted" data-testid="qr-seed">本次种子：{{ run.seed }}</span>
          </div>
          <p v-if="pollWarn" class="warn small" data-testid="qr-poll-warn">{{ pollWarn }}</p>
          <p v-if="pollNote" class="muted small" data-testid="qr-poll-stopped">{{ pollNote }}</p>
          <div v-if="run.files.length" class="files">
            <figure v-for="f in run.files" :key="f.index" class="file" :data-testid="`qr-file-${f.index}`" :data-kind="fileKind(f)">
              <template v-if="safeFileURL(f.url)">
                <video v-if="fileKind(f) === 'video'" :src="safeFileURL(f.url)" controls preload="metadata" :aria-label="f.filename" />
                <a v-else :href="safeFileURL(f.url)" target="_blank" rel="noopener" title="查看大图"><img :src="safeFileURL(f.url)" :alt="f.filename" loading="lazy"></a>
              </template>
              <figcaption>
                <span class="name">{{ f.filename }}</span>
                <a v-if="safeFileURL(f.url)" :href="safeFileURL(f.url)" :download="f.filename" :data-testid="`qr-file-${f.index}-download`">下载</a>
              </figcaption>
            </figure>
          </div>
          <p v-else-if="run.state === 'done'" class="muted small" data-testid="qr-no-files">运行完成，但没有可显示的输出文件</p>
        </template>
        <p v-else-if="!runError" class="muted" data-testid="qr-idle">填写参数后点「运行」，生成结果会显示在这里。</p>
        <p class="muted small">输出文件同时保存在实例的 /root/data/comfyui-output；单次运行最长等待 30 分钟。</p>
        <el-button v-if="deployment.url" size="small" data-testid="qr-open-comfy" @click="openComfy">在 ComfyUI 里打开</el-button>
      </section>
    </div>
  </el-drawer>
</template>

<style scoped>
.layout { display: grid; grid-template-columns: minmax(280px, 2fr) 3fr; gap: 24px; align-items: start; }
@media (max-width: 768px) { .layout { grid-template-columns: 1fr; } }
.mb { margin-bottom: 12px; }
.status { padding: 2px 8px; border-radius: 4px; font-size: 13px; line-height: 1.6; word-break: break-word; white-space: pre-wrap; max-width: 100%;
  color: var(--el-color-primary); background: var(--el-color-primary-light-9); }
.status.done { color: var(--el-color-success); background: var(--el-color-success-light-9); }
.status.error { color: var(--el-color-danger); background: var(--el-color-danger-light-9); }
.warn { color: var(--el-color-warning); }
.bar { display: flex; align-items: center; gap: 12px; flex-wrap: wrap; margin-bottom: 12px; }
.files { display: grid; grid-template-columns: repeat(auto-fill, minmax(220px, 1fr)); gap: 12px; margin-bottom: 12px; }
.file { margin: 0; border: 1px solid var(--el-border-color-lighter); border-radius: 6px; overflow: hidden; background: var(--el-fill-color-lighter); }
.file img, .file video { display: block; width: 100%; max-height: 420px; object-fit: contain; background: #000; }
.file figcaption { display: flex; justify-content: space-between; gap: 8px; padding: 6px 8px; font-size: 12px; }
.file .name { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; color: #606266; }
.muted { color: #909399; }
.small { font-size: 12px; margin: 4px 0 8px; }
</style>
