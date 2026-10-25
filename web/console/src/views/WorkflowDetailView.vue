<script setup lang="ts">
import { brand, fmtTime, formatGB, isApiError, labelOf, messageOf, workflowCategoryLabel, type WorkflowDetail, type WorkflowInput, type WorkflowModel } from '@spark/shared'
import { computed, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { fetchWorkflow } from '../api'
import WorkflowInstallFlow from '../components/WorkflowInstallFlow.vue'
import WorkflowCover from '../components/WorkflowCover.vue'
import { markdownBlocks, modelSource, safeGitUrl, shortCommit } from '../logic/workflows'
import { session } from '../store/session'

const route = useRoute()
const router = useRouter()
const slug = computed(() => String(route.params.slug))
const wf = ref<WorkflowDetail | null>(null)
const loading = ref(true)
const notFound = ref(false)
const loadError = ref('')
const flow = ref<InstanceType<typeof WorkflowInstallFlow> | null>(null)
let seq = 0

async function load() {
  const mine = ++seq
  loading.value = true
  notFound.value = false
  loadError.value = ''
  try {
    const d = await fetchWorkflow(slug.value)
    if (mine !== seq) return
    wf.value = d
    setTitle()
    resumeInstall()
  } catch (e) {
    if (mine !== seq) return
    wf.value = null
    if (isApiError(e) && (e.code === 'workflow_not_found' || e.status === 404)) notFound.value = true
    else loadError.value = messageOf(e)
  } finally {
    if (mine === seq) loading.value = false
  }
}
watch(slug, () => void load(), { immediate: true })

/** 标题用工作流名称（路由 afterEach 设的是通用的「工作流详情」，每次导航后都要再设）。 */
function setTitle() {
  if (wf.value) document.title = `${wf.value.name} - ${brand.name}`
}

/** 登录回来（?install=<slug>）：自动打开安装弹窗，并去掉查询参数（replace 后路由会重设标题：再设一次）。 */
function resumeInstall() {
  if (typeof route.query.install !== 'string') return
  void router.replace({ path: route.path }).then(setTitle)
  if (wf.value && session.status === 'authed') void flow.value?.open(wf.value)
}

const manifest = computed(() => wf.value?.version.manifest ?? null)
const description = computed(() => markdownBlocks(wf.value?.description ?? ''))

const INPUT_TYPE: Record<string, string> = { text: '文本', textarea: '多行文本', int: '整数', float: '小数', bool: '开关', enum: '选项', seed: '种子', image: '图片' }
/** 快速出图参数的范围 / 默认值说明。 */
function inputHint(i: WorkflowInput): string {
  const parts: string[] = []
  if (i.min != null && i.max != null) parts.push(`${i.min}～${i.max}${i.step ? `，步长 ${i.step}` : ''}`)
  if (i.options?.length) parts.push(i.options.join(' / '))
  if (i.max_len) parts.push(`最多 ${i.max_len} 字`)
  if (i.type === 'seed') parts.push('留空随机')
  if (i.default !== undefined && i.default !== '' && i.type !== 'textarea' && i.type !== 'text') parts.push(`默认 ${String(i.default)}`)
  return parts.join('；')
}
</script>

<template>
  <div class="wf-detail" data-testid="wf-detail">
    <router-link to="/workflows" class="back" data-testid="wf-detail-back">← 工作流市场</router-link>
    <el-skeleton v-if="loading && !wf" :rows="6" animated />
    <el-result v-else-if="notFound" icon="warning" title="工作流不存在或未上架" data-testid="wf-detail-not-found">
      <template #extra><router-link to="/workflows">返回工作流市场</router-link></template>
    </el-result>
    <el-alert v-else-if="loadError" type="error" :title="loadError" show-icon :closable="false" data-testid="wf-detail-error">
      <el-button size="small" data-testid="wf-detail-retry" @click="load">重试</el-button>
    </el-alert>
    <template v-else-if="wf && manifest">
      <el-card>
        <div class="head">
          <div class="cover"><WorkflowCover :item="wf" /></div>
          <div class="info">
            <h2 class="name" data-testid="wf-detail-name">{{ wf.name }}</h2>
            <p class="summary">{{ wf.summary }}</p>
            <div class="tags">
              <el-tag size="small">{{ labelOf(workflowCategoryLabel, wf.category) }}</el-tag>
              <el-tag v-for="t in wf.tags" :key="t" size="small" effect="plain">{{ t }}</el-tag>
              <el-tag v-if="wf.version.quick_run" size="small" type="success">支持快速出图</el-tag>
            </div>
            <ul class="facts">
              <li>版本 <b data-testid="wf-detail-version">v{{ wf.version.version }}</b></li>
              <li>需下载约 <b data-testid="wf-detail-size">{{ formatGB(wf.version.download_gb) }}</b>（{{ wf.version.models }} 个模型）</li>
              <li v-if="wf.version.nodes">含 {{ wf.version.nodes }} 个自定义节点</li>
              <li v-if="wf.version.comfyui_min">ComfyUI ≥ {{ wf.version.comfyui_min }}</li>
              <li>{{ wf.installs }} 次在用安装</li>
            </ul>
            <el-button type="primary" data-testid="wf-install-btn" @click="flow?.open(wf)">一键安装</el-button>
          </div>
        </div>
      </el-card>

      <el-card v-if="description.length" header="介绍">
        <div class="md" data-testid="wf-detail-description">
          <template v-for="(b, bi) in description" :key="bi">
            <p v-if="b.type === 'p'"><template v-for="(s, si) in b.inlines" :key="si"><b v-if="s.kind === 'bold'">{{ s.text }}</b><code v-else-if="s.kind === 'code'">{{ s.text }}</code><template v-else>{{ s.text }}</template></template></p>
            <ul v-else>
              <li v-for="(it, ii) in b.items" :key="ii"><template v-for="(s, si) in it" :key="si"><b v-if="s.kind === 'bold'">{{ s.text }}</b><code v-else-if="s.kind === 'code'">{{ s.text }}</code><template v-else>{{ s.text }}</template></template></li>
            </ul>
          </template>
        </div>
      </el-card>

      <el-card header="所需模型">
        <el-table :data="manifest.models" empty-text="无需额外模型" data-testid="wf-detail-models">
          <el-table-column prop="name" label="文件名" min-width="200" />
          <el-table-column prop="dir" label="子目录" width="140"><template #default="{ row }"><code>models/{{ row.dir }}</code></template></el-table-column>
          <el-table-column label="大小" width="100"><template #default="{ row }">{{ formatGB(row.size_gb) }}</template></el-table-column>
          <el-table-column label="来源" min-width="220"><template #default="{ row }">{{ modelSource(row as WorkflowModel) }}</template></el-table-column>
        </el-table>
        <p class="muted">模型下载到实例的 <code>/root/data/comfyui/models</code>，同名模型已存在时不重复下载；移除工作流不删除模型。</p>
      </el-card>

      <el-card header="自定义节点">
        <el-table :data="manifest.nodes" empty-text="只用 ComfyUI 内置节点" data-testid="wf-detail-nodes">
          <el-table-column prop="name" label="名称" min-width="180" />
          <el-table-column label="Git" min-width="260"><template #default="{ row }"><a v-if="safeGitUrl(row.git)" :href="safeGitUrl(row.git)!" target="_blank" rel="noopener noreferrer">{{ row.git }}</a><span v-else>{{ row.git }}</span></template></el-table-column>
          <el-table-column label="commit" width="110"><template #default="{ row }"><code :title="row.commit">{{ shortCommit(row.commit) }}</code></template></el-table-column>
        </el-table>
      </el-card>

      <el-card v-if="wf.version.quick_run" header="快速出图参数">
        <el-table :data="manifest.inputs" empty-text="无可调参数" data-testid="wf-detail-inputs">
          <el-table-column prop="label" label="参数" min-width="120" />
          <el-table-column label="类型" width="100"><template #default="{ row }">{{ INPUT_TYPE[row.type] ?? row.type }}</template></el-table-column>
          <el-table-column label="说明" min-width="200"><template #default="{ row }">{{ inputHint(row as WorkflowInput) }}</template></el-table-column>
        </el-table>
      </el-card>

      <el-card header="版本">
        <div v-if="wf.version.changelog" class="changelog" data-testid="wf-detail-changelog"><b>v{{ wf.version.version }} 更新说明：</b>{{ wf.version.changelog }}</div>
        <ul class="versions" data-testid="wf-detail-versions">
          <li v-for="v in wf.versions" :key="v.id" :class="{ current: v.id === wf.version.id }">v{{ v.version }}<span class="muted"> · {{ v.published_at ? fmtTime(v.published_at) : '—' }}</span><template v-if="v.id === wf.version.id">（当前）</template></li>
        </ul>
      </el-card>
    </template>
    <WorkflowInstallFlow ref="flow" />
  </div>
</template>

<style scoped>
.wf-detail { display: flex; flex-direction: column; gap: 16px; }
.back { color: #606266; text-decoration: none; font-size: 13px; }
.head { display: flex; gap: 20px; flex-wrap: wrap; }
.cover { width: 320px; max-width: 100%; border-radius: 6px; overflow: hidden; flex: none; }
.info { flex: 1; min-width: 240px; display: flex; flex-direction: column; gap: 8px; align-items: flex-start; }
.name { margin: 0; font-size: 20px; }
.summary { margin: 0; color: #606266; }
.tags { display: flex; flex-wrap: wrap; gap: 4px; }
.facts { list-style: none; padding: 0; margin: 0; display: flex; flex-wrap: wrap; gap: 4px 16px; color: #606266; font-size: 13px; }
.md p { margin: 0 0 8px; line-height: 1.7; }
.md ul { margin: 0 0 8px; padding-left: 20px; line-height: 1.7; }
.md code, .muted code, .el-table code { background: #f5f7fa; padding: 0 4px; border-radius: 3px; font-size: 12px; }
.muted { color: #909399; font-size: 13px; }
.changelog { margin-bottom: 8px; font-size: 13px; }
.versions { list-style: none; padding: 0; margin: 0; font-size: 13px; display: flex; flex-direction: column; gap: 4px; }
.versions .current { font-weight: 600; }
</style>
