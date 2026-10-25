<script setup lang="ts">
import { fmtYuan, messageOf, type LLMAccount, type LLMModelPublic, type LLMModelsResponse } from '@spark/shared'
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import KeysTab from '../components/tokens/KeysTab.vue'
import LoginPrompt from '../components/tokens/LoginPrompt.vue'
import LogsTab from '../components/tokens/LogsTab.vue'
import ModelsTab from '../components/tokens/ModelsTab.vue'
import QuickStartTab from '../components/tokens/QuickStartTab.vue'
import UsageTab from '../components/tokens/UsageTab.vue'
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

// ---- 账户：GET /llm/account（登录后）——欠费 / 停用提示条、用量统计卡、快速开始的实际限额 ----
const account = ref<LLMAccount | null>(null)
const accountError = ref('')
async function loadAccount() {
  accountError.value = ''
  try {
    account.value = await api.get<LLMAccount>('/llm/account')
  } catch (e) {
    notifyError(e)
    accountError.value = messageOf(e)
  }
}
watch(
  authed,
  (v) => {
    if (v) void loadAccount()
    else account.value = null
  },
  { immediate: true },
)
const goRecharge = () => router.push('/wallet/recharge')
</script>

<template>
  <div class="tokens">
    <el-alert v-if="account && account.debt > 0" type="error" show-icon :closable="false" class="top-alert" data-testid="tokens-debt-alert">
      <template #title>
        账户有欠费 <b data-testid="tokens-debt-amount">{{ fmtYuan(account.debt) }}</b>，充值后自动补扣，补扣前 API 调用将被拒绝
      </template>
      <el-button type="danger" size="small" data-testid="tokens-debt-recharge" @click="goRecharge">去充值</el-button>
    </el-alert>
    <el-alert v-if="account?.blocked" type="error" show-icon :closable="false" class="top-alert" data-testid="tokens-blocked-alert">
      <template #title>Token 服务已被平台停用，API 调用将被拒绝</template>
      <span data-testid="tokens-blocked-reason">{{ account.blocked_reason ? `原因：${account.blocked_reason}` : '如有疑问请联系客服' }}</span>
    </el-alert>
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

      <el-tab-pane name="usage" :lazy="authed">
        <template #label><span data-testid="tokens-tab-usage">用量</span></template>
        <UsageTab v-if="authed" :account="account" :account-error="accountError" data-testid="tokens-usage-pane" @retry-account="loadAccount" />
        <LoginPrompt v-else />
      </el-tab-pane>

      <el-tab-pane name="logs" :lazy="authed">
        <template #label><span data-testid="tokens-tab-logs">请求日志</span></template>
        <LogsTab v-if="authed" :models="models" data-testid="tokens-logs-pane" />
        <LoginPrompt v-else />
      </el-tab-pane>

      <el-tab-pane name="quickstart">
        <template #label><span data-testid="tokens-tab-quickstart">快速开始</span></template>
        <el-alert v-if="loadError" type="error" :title="loadError" show-icon :closable="false">
          <el-button size="small" @click="loadModels">重试</el-button>
        </el-alert>
        <el-skeleton v-else-if="loading" :rows="5" animated />
        <QuickStartTab v-else :base-url="baseUrl" :models="models" :limits="account?.limits" />
      </el-tab-pane>
    </el-tabs>
  </div>
</template>

<style scoped>
.top-alert { margin-bottom: 12px; }
</style>
