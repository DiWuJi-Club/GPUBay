<script setup lang="ts">
import { messageOf, type LLMModelPublic, type LLMModelsResponse } from '@spark/shared'
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import KeysTab from '../components/tokens/KeysTab.vue'
import LoginPrompt from '../components/tokens/LoginPrompt.vue'
import ModelsTab from '../components/tokens/ModelsTab.vue'
import QuickStartTab from '../components/tokens/QuickStartTab.vue'
import { notifyError } from '../logic/notify'
import { session } from '../store/session'

const route = useRoute()
const router = useRouter()
const tabs = ['models', 'keys', 'usage', 'logs', 'quickstart'] as const
type Tab = (typeof tabs)[number]
const tab = computed<Tab>({
  get: () => (tabs.includes(route.query.tab as Tab) ? (route.query.tab as Tab) : 'models'),
  set: (v) => void router.replace({ query: { ...route.query, tab: v } }),
})
const authed = computed(() => session.status === 'authed')

// ---- 模型与 base_url：GET /llm/models（匿名可访问），模型 Tab 与快速开始共用 ----
const models = ref<LLMModelPublic[]>([])
const baseUrl = ref('')
const loading = ref(true)
const loadError = ref('')
async function loadModels() {
  loading.value = true
  loadError.value = ''
  try {
    const r = await api.get<LLMModelsResponse>('/llm/models')
    models.value = r.items
    baseUrl.value = r.base_url || `${window.location.origin}/v1`
  } catch (e) {
    notifyError(e)
    loadError.value = messageOf(e)
  } finally {
    loading.value = false
  }
}
onMounted(loadModels)
</script>

<template>
  <div class="tokens">
    <el-tabs v-model="tab">
      <el-tab-pane name="models">
        <template #label><span data-testid="tokens-tab-models">模型与价格</span></template>
        <el-alert v-if="loadError" type="error" :title="loadError" show-icon :closable="false" data-testid="tokens-error">
          <el-button size="small" data-testid="tokens-retry" @click="loadModels">重试</el-button>
        </el-alert>
        <el-skeleton v-else-if="loading" :rows="5" animated />
        <ModelsTab v-else :models="models" />
      </el-tab-pane>

      <!-- 已登录时 lazy：第一次打开该 Tab 才挂载 KeysTab、请求 /llm/keys -->
      <el-tab-pane name="keys" :lazy="authed">
        <template #label><span data-testid="tokens-tab-keys">API Keys</span></template>
        <KeysTab v-if="authed" :models="models" data-testid="tokens-keys-pane" />
        <LoginPrompt v-else />
      </el-tab-pane>

      <el-tab-pane name="usage">
        <template #label><span data-testid="tokens-tab-usage">用量</span></template>
        <!-- 已登录内容：计划 9 Task 19（UsageTab） -->
        <el-empty v-if="authed" description="即将开放" data-testid="tokens-usage-pane" />
        <LoginPrompt v-else />
      </el-tab-pane>

      <el-tab-pane name="logs">
        <template #label><span data-testid="tokens-tab-logs">请求日志</span></template>
        <!-- 已登录内容：计划 9 Task 19（LogsTab） -->
        <el-empty v-if="authed" description="即将开放" data-testid="tokens-logs-pane" />
        <LoginPrompt v-else />
      </el-tab-pane>

      <el-tab-pane name="quickstart">
        <template #label><span data-testid="tokens-tab-quickstart">快速开始</span></template>
        <el-alert v-if="loadError" type="error" :title="loadError" show-icon :closable="false">
          <el-button size="small" @click="loadModels">重试</el-button>
        </el-alert>
        <el-skeleton v-else-if="loading" :rows="5" animated />
        <QuickStartTab v-else :base-url="baseUrl" :models="models" />
      </el-tab-pane>
    </el-tabs>
  </div>
</template>
