<script setup lang="ts">
import { labelOf, messageOf, workflowCategoryLabel, type WorkflowCatalogItem } from '@spark/shared'
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { fetchWorkflows } from '../api'
import WorkflowCard from '../components/WorkflowCard.vue'
import WorkflowInstallFlow from '../components/WorkflowInstallFlow.vue'
import { filterWorkflows, sortWorkflows, WORKFLOW_CATEGORIES, type CategoryFilter } from '../logic/workflows'
import { session } from '../store/session'

const route = useRoute()
const router = useRouter()
const items = ref<WorkflowCatalogItem[]>([])
const loading = ref(true)
const loadError = ref('')
const category = ref<CategoryFilter>('all')
const query = ref('')
const flow = ref<InstanceType<typeof WorkflowInstallFlow> | null>(null)

const tabs = computed(() => [{ key: 'all' as CategoryFilter, label: '全部' }, ...WORKFLOW_CATEGORIES.map((c) => ({ key: c as CategoryFilter, label: labelOf(workflowCategoryLabel, c) }))])
const shown = computed(() => sortWorkflows(filterWorkflows(items.value, { category: category.value, query: query.value })))

async function load() {
  loading.value = true
  loadError.value = ''
  try {
    items.value = await fetchWorkflows()
    resumeInstall()
  } catch (e) {
    loadError.value = messageOf(e)
  } finally {
    loading.value = false
  }
}
onMounted(load)

/** 登录回来（?install=<slug>）：自动打开安装弹窗，并去掉查询参数。 */
function resumeInstall() {
  const slug = route.query.install
  if (typeof slug !== 'string') return
  void router.replace({ path: route.path })
  const w = items.value.find((x) => x.slug === slug)
  if (w && session.status === 'authed') void flow.value?.open(w)
}
</script>

<template>
  <div class="workflows" data-testid="wf-market">
    <div class="toolbar">
      <div class="cats" role="tablist">
        <button
          v-for="t in tabs" :key="t.key" type="button" role="tab" class="cat" :class="{ active: category === t.key }" :aria-selected="category === t.key"
          :data-testid="`wf-category-${t.key}`" @click="category = t.key"
        >{{ t.label }}</button>
      </div>
      <el-input v-model="query" clearable placeholder="搜索名称或标签" class="search" data-testid="wf-search" />
    </div>
    <el-alert v-if="loadError" type="error" :title="loadError" show-icon :closable="false" data-testid="wf-market-error">
      <el-button size="small" data-testid="wf-market-retry" @click="load">重试</el-button>
    </el-alert>
    <el-skeleton v-else-if="loading" :rows="5" animated />
    <el-empty v-else-if="!shown.length" :description="items.length ? '没有符合条件的工作流' : '暂无上架的工作流'" data-testid="wf-market-empty" />
    <div v-else class="grid">
      <WorkflowCard v-for="w in shown" :key="w.id" :item="w" @install="(x) => flow?.open(x)" />
    </div>
    <p class="muted">工作流装在您实例上的 ComfyUI 里：模型与自定义节点在实例内下载，安装会重启 ComfyUI。<router-link to="/help">了解更多</router-link></p>
    <WorkflowInstallFlow ref="flow" />
  </div>
</template>

<style scoped>
.toolbar { display: flex; flex-wrap: wrap; gap: 12px; align-items: center; justify-content: space-between; margin-bottom: 16px; }
.cats { display: flex; flex-wrap: wrap; gap: 6px; }
.cat { border: 1px solid #dcdfe6; background: #fff; border-radius: 16px; padding: 4px 14px; font: inherit; font-size: 13px; color: #606266; cursor: pointer; min-height: 32px; }
.cat.active { border-color: var(--el-color-primary); color: #fff; background: var(--el-color-primary); }
.search { width: 240px; max-width: 100%; }
.grid { display: grid; gap: 16px; grid-template-columns: repeat(auto-fill, minmax(260px, 1fr)); }
.muted { color: #909399; font-size: 13px; margin-top: 16px; }
</style>
