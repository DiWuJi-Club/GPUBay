<script setup lang="ts">
import { fmtYuan, labelOf, llmModelStatusLabel, llmModelStatusType, messageOf, type LLMModelAdmin, type LLMSummary, type Listed } from '@spark/shared'
import { computed, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import AccountsTab from '../components/llm/AccountsTab.vue'
import ModelsTab from '../components/llm/ModelsTab.vue'
import NodesTab from '../components/llm/NodesTab.vue'
import UsageTab from '../components/llm/UsageTab.vue'
import { tabFromQuery, visibleTabs, wantedReplicas as wanted, type LlmTabKey } from '../logic/llm'
import { notifyError } from '../logic/notify'
import { session } from '../store/session'

// Token 商城（规格 §11）：路由对所有角色开放，Tab 按角色显示（logic/llm.ts 的 LLM_TABS）；?tab= 选中 Tab，其它查询参数留给各 Tab。
const route = useRoute()
const router = useRouter()
const role = computed(() => session.admin?.role ?? 'finance')
const tabs = computed(() => visibleTabs(role.value))
const tab = ref<LlmTabKey>(tabFromQuery(route.query.tab, role.value))
watch(
  () => route.query.tab,
  (q) => {
    tab.value = tabFromQuery(q, role.value)
  },
)
function onTab(name: string | number) {
  if (name === route.query.tab) return
  void router.replace({ path: route.path, query: { tab: String(name) } }) // 换 Tab 时丢掉上一个 Tab 的筛选参数
}

// ---- 总览：GET /llm/summary + 按模型的节点卡片（GET /llm/models 的节点计数）----
const overview = reactive({ loading: false, loaded: false, error: '', summary: null as LLMSummary | null, models: [] as LLMModelAdmin[] })
async function loadOverview() {
  overview.loading = true
  try {
    const [s, m] = await Promise.all([api.get<LLMSummary>('/llm/summary'), api.get<Listed<LLMModelAdmin>>('/llm/models')])
    overview.summary = s
    overview.models = [...m.items].sort((a, b) => a.sort - b.sort || a.id - b.id)
    overview.error = ''
  } catch (e) {
    notifyError(e)
    overview.error = messageOf(e)
  } finally {
    overview.loading = false
    overview.loaded = true
  }
}
// 每次切到总览都重新加载（模型 Tab 里的改动、节点状态随时在变）
watch(
  tab,
  (t) => {
    if (t === 'overview') void loadOverview()
  },
  { immediate: true },
)
const fmtNum = (n: number) => n.toLocaleString('en-US')
</script>

<template>
  <el-card class="llm">
    <el-tabs :model-value="tab" data-testid="llm-tabs" @tab-change="onTab">
      <el-tab-pane v-for="t in tabs" :key="t.key" :name="t.key" lazy>
        <template #label><span :data-testid="`llm-tab-${t.key}`">{{ t.label }}</span></template>

        <div v-if="t.key === 'overview'" class="overview" data-testid="llm-overview">
          <el-alert v-if="overview.error" type="error" :title="overview.error" show-icon :closable="false" data-testid="llm-overview-error">
            <el-button size="small" :loading="overview.loading" data-testid="llm-overview-retry" @click="loadOverview">重试</el-button>
          </el-alert>
          <div class="bar"><el-button size="small" :loading="overview.loading" data-testid="llm-overview-refresh" @click="loadOverview">刷新</el-button></div>
          <el-skeleton v-if="!overview.summary && overview.loading" :rows="4" animated />
          <div v-if="overview.summary" class="grid">
            <el-card header="节点">
              <div class="big"><span data-testid="llm-sum-nodes-ready">{{ overview.summary.nodes.ready }}</span> / <span data-testid="llm-sum-nodes-total">{{ overview.summary.nodes.total }}</span> 就绪</div>
              <ul class="kv"><li><span>不健康</span><b data-testid="llm-sum-nodes-unhealthy" :class="{ bad: overview.summary.nodes.unhealthy > 0 }">{{ overview.summary.nodes.unhealthy }}</b></li></ul>
            </el-card>
            <el-card header="今日请求">
              <div class="big" data-testid="llm-sum-requests">{{ fmtNum(overview.summary.today.requests) }}</div>
              <ul class="kv">
                <li><span>输入 token</span><b data-testid="llm-sum-prompt-tokens">{{ fmtNum(overview.summary.today.prompt_tokens) }}</b></li>
                <li><span>输出 token</span><b data-testid="llm-sum-completion-tokens">{{ fmtNum(overview.summary.today.completion_tokens) }}</b></li>
              </ul>
            </el-card>
            <el-card header="今日收入">
              <div class="big" data-testid="llm-sum-settled">{{ fmtYuan(overview.summary.today.settled_amount) }}</div>
              <ul class="kv"><li><span>未结算（估算）</span><b data-testid="llm-sum-unsettled">{{ fmtYuan(overview.summary.today.unsettled_estimate) }}</b></li></ul>
            </el-card>
            <el-card header="欠费">
              <div class="big"><span data-testid="llm-sum-debt-users" :class="{ bad: overview.summary.debt_users > 0 }">{{ overview.summary.debt_users }}</span> 位用户</div>
              <ul class="kv"><li><span>欠费合计</span><b data-testid="llm-sum-debt-total">{{ fmtYuan(overview.summary.debt_total) }}</b></li></ul>
            </el-card>
          </div>
          <h4 v-if="overview.loaded">按模型</h4>
          <el-empty v-if="overview.loaded && !overview.error && overview.models.length === 0" description="还没有模型" :image-size="60" data-testid="llm-overview-models-empty" />
          <div class="models">
            <el-card v-for="m in overview.models" :key="m.id" shadow="never" data-testid="llm-overview-model" :data-slug="m.slug">
              <div class="mhead">
                <b>{{ m.name }}</b>
                <el-tag size="small" :type="labelOf(llmModelStatusType, m.status) as 'primary'">{{ labelOf(llmModelStatusLabel, m.status) }}</el-tag>
              </div>
              <div class="mono sub">{{ m.slug }}</div>
              <ul class="kv">
                <li><span>就绪 / 节点</span><b data-testid="llm-overview-model-nodes">{{ m.nodes.ready }} / {{ m.nodes.total }}</b></li>
                <li><span>期望副本</span><b data-testid="llm-overview-model-wanted" :class="{ bad: m.nodes.ready < wanted(m) }">{{ wanted(m) }}</b></li>
                <li v-if="(m.nodes_per_replica ?? 1) > 1"><span>每副本</span><b data-testid="llm-overview-model-npr">{{ m.nodes_per_replica }} 台 · TP {{ m.tensor_parallel ?? m.nodes_per_replica }}</b></li>
              </ul>
            </el-card>
          </div>
        </div>

        <ModelsTab v-else-if="t.key === 'models'" />

        <!-- active：只在 Tab 可见时自动刷新（el-tabs 切走后面板仍挂载） -->
        <NodesTab v-else-if="t.key === 'nodes'" :active="tab === 'nodes'" />

        <AccountsTab v-else-if="t.key === 'accounts'" />

        <UsageTab v-else-if="t.key === 'usage'" />
      </el-tab-pane>
    </el-tabs>
  </el-card>
</template>

<style scoped>
.overview { display: grid; gap: 16px; }
.bar { display: flex; justify-content: flex-end; }
.grid { display: grid; grid-template-columns: repeat(4, 1fr); gap: 16px; }
.models { display: grid; grid-template-columns: repeat(auto-fill, minmax(220px, 1fr)); gap: 12px; }
.big { font-size: 20px; font-weight: 600; margin-bottom: 8px; }
.kv { list-style: none; margin: 0; padding: 0; display: grid; gap: 6px; }
.kv li { display: flex; justify-content: space-between; }
.bad { color: var(--el-color-danger); }
.mhead { display: flex; justify-content: space-between; align-items: center; gap: 8px; }
.sub { color: #909399; font-size: 12px; margin: 4px 0 8px; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
h4 { margin: 0; font-size: 14px; }
</style>
