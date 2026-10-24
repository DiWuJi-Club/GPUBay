<script setup lang="ts">
import {
  digitsOnly, fmtTime, fmtYuan, isApiError, labelOf, messageOf,
  type AdminButlerConversation, type AdminButlerConversationDetail, type ButlerSettingsView, type ButlerUsageReport, type ButlerUserUsage,
  type ButlerUserUsagePage, type Paged,
} from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, reactive, ref, watch } from 'vue'
import { onBeforeRouteUpdate, useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import DateRange from '../components/DateRange.vue'
import IdLink from '../components/IdLink.vue'
import { usePaged } from '../composables/usePaged'
import {
  argsText, convStatusLabel, defaultUsageRange, fmtCount, roleLabel, tabOf, todayCST, toolCallStatusLabel, toolCallStatusType, userIdOf,
  validateSettingsForm, type ButlerTab, type SettingsForm,
} from '../logic/butler'
import { notifyError } from '../logic/notify'
import { maskPhone } from '../logic/referral'
import { hasRole, session } from '../store/session'
import { OPS_ROLES } from '../menu'

// AI 管家（计划 12，规格 §12 / D8）：用量所有角色可看；对话只给 ops / super（tab 不渲染，后端同样拒绝）；
// 设置所有角色可看、只有 super 可改。对话内容是用户数据：只读展示，文本一律按纯文本渲染（不用 v-html）。
const route = useRoute()
const router = useRouter()
const canConversations = computed(() => hasRole(OPS_ROLES))
const isSuper = computed(() => session.admin?.role === 'super')

const tab = computed<ButlerTab>({
  get: () => tabOf(route.query, canConversations.value),
  set: (v) => void router.replace({ query: { ...route.query, tab: v } }),
})

/** 平台没启用管家时后台路由不注册（404）：页内统一提示 */
const notEnabled = (e: unknown) => isApiError(e) && e.status === 404
const errorText = (e: unknown) => (notEnabled(e) ? '平台未启用 AI 管家（SPARK_BUTLER=off）' : messageOf(e))

// ---- 用量：按日合计 ----
const usageQ = reactive({ range: defaultUsageRange() })
const usage = ref<ButlerUsageReport | null>(null)
const usageError = ref('')
const usageLoading = ref(false)
let usageSeq = 0
/** 只采用最后一次请求的结果；失败清空表格（与当前范围对不上），显示错误与重试。 */
async function loadUsage() {
  const mine = ++usageSeq
  usageLoading.value = true
  try {
    const out = await api.get<ButlerUsageReport>('/butler/usage', { from: usageQ.range.from, to: usageQ.range.to })
    if (mine !== usageSeq) return
    usage.value = out
    usageError.value = ''
  } catch (e) {
    if (mine !== usageSeq) return
    if (!notEnabled(e)) notifyError(e)
    usage.value = null
    usageError.value = errorText(e)
  } finally {
    if (mine === usageSeq) usageLoading.value = false
  }
}
/** 表格按日期倒序（最近的在上） */
const usageRows = computed(() => [...(usage.value?.items ?? [])].reverse())

// ---- 用量：某日按用户 ----
const usersDay = ref(todayCST())
const users = usePaged<ButlerUserUsage>((page, size) =>
  api.get<ButlerUserUsagePage>('/butler/usage/users', { day: usersDay.value, page, page_size: size }) as Promise<Paged<ButlerUserUsage>>,
)
function pickDay(day: string) {
  usersDay.value = day
}
watch(usersDay, (d) => {
  if (d) users.reset()
})

// ---- 对话（ops） ----
const conv = reactive({ user_id: userIdOf(route.query.user_id), range: { from: '', to: '' } })
const convs = usePaged<AdminButlerConversation>((page, size) =>
  api.get<Paged<AdminButlerConversation>>('/butler/conversations', {
    user_id: conv.user_id, from: conv.range.from, to: conv.range.to, page, page_size: size,
  }),
)
function resetConv() {
  Object.assign(conv, { user_id: '', range: { from: '', to: '' } })
  convs.reset()
}

const detail = reactive({ open: false, id: '', loading: false, error: '', data: null as AdminButlerConversationDetail | null })
let detailSeq = 0
async function loadDetail() {
  const mine = ++detailSeq
  detail.loading = true
  try {
    const d = await api.get<AdminButlerConversationDetail>(`/butler/conversations/${encodeURIComponent(detail.id)}`)
    if (mine !== detailSeq) return
    detail.data = d
    detail.error = ''
  } catch (e) {
    if (mine !== detailSeq) return
    notifyError(e)
    if (!detail.data) detail.error = messageOf(e)
  } finally {
    if (mine === detailSeq) detail.loading = false
  }
}
function openDetail(id: string) {
  Object.assign(detail, { open: true, id, error: '', data: null })
  void loadDetail()
}
function closeDetail() {
  detail.open = false
  detailSeq++ // 关掉后迟到的响应不再写入
}

// ---- 设置 ----
const settings = ref<ButlerSettingsView | null>(null)
const settingsError = ref('')
const settingsLoading = ref(false)
const form = reactive<SettingsForm>({ enabled: true, daily: '', turns: '', notice: '' })
const formError = ref('')
const saving = ref(false)
function fill(v: ButlerSettingsView) {
  settings.value = v
  Object.assign(form, { enabled: v.enabled, daily: String(v.daily_token_limit), turns: String(v.max_turns), notice: v.notice })
  formError.value = ''
}
let settingsSeq = 0
async function loadSettings() {
  const mine = ++settingsSeq
  settingsLoading.value = true
  try {
    const v = await api.get<ButlerSettingsView>('/butler/settings')
    if (mine !== settingsSeq) return
    fill(v)
    settingsError.value = ''
  } catch (e) {
    if (mine !== settingsSeq) return
    if (!notEnabled(e)) notifyError(e)
    if (!settings.value) settingsError.value = errorText(e)
  } finally {
    if (mine === settingsSeq) settingsLoading.value = false
  }
}
async function saveSettings() {
  if (!isSuper.value || saving.value) return // 同步守卫：连点只发一次
  const v = validateSettingsForm(form)
  if ('error' in v) {
    formError.value = v.error
    return
  }
  formError.value = ''
  saving.value = true
  try {
    const out = await api.put<ButlerSettingsView>('/butler/settings', v.input)
    settingsSeq++ // 保存结果比在途的读取更新
    fill(out)
    ElMessage.success('已保存，最多 10 秒后对所有用户生效')
  } catch (e) {
    notifyError(e) // 输入保留
  } finally {
    saving.value = false
  }
}

// ---- 各 tab 第一次打开时加载 ----
const loaded = reactive({ usage: false, conversations: false, settings: false })
function ensure(t: ButlerTab) {
  if (loaded[t]) return
  loaded[t] = true
  if (t === 'usage') {
    void loadUsage()
    void users.load()
  } else if (t === 'conversations') void convs.load()
  else void loadSettings()
}
watch(tab, ensure, { immediate: true })

// 已在本页时又从用户详情点「查看对话」（?user_id= 变了）：按新用户重新筛选
onBeforeRouteUpdate((to) => {
  const uid = userIdOf(to.query.user_id)
  if (uid && uid !== conv.user_id) {
    conv.user_id = uid
    if (loaded.conversations) convs.reset()
  }
})

const stepMark = (s: string) => (s === 'ok' ? '✓' : '✗')
const callType = (s: string) => labelOf(toolCallStatusType, s) as 'success'
</script>

<template>
  <el-card>
    <el-tabs v-model="tab" data-testid="admin-butler-tabs">
      <!-- 用量（所有角色） -->
      <el-tab-pane name="usage">
        <template #label><span data-testid="admin-butler-tab-usage">用量</span></template>
        <el-form inline class="filters" @submit.prevent="loadUsage">
          <el-form-item label="日期范围"><span data-testid="admin-butler-usage-range"><DateRange v-model="usageQ.range" prefix="admin-butler-usage-range" /></span></el-form-item>
          <el-button type="primary" native-type="submit" :loading="usageLoading" data-testid="admin-butler-usage-search">查询</el-button>
          <span v-if="usage" class="muted" data-testid="admin-butler-usage-price">
            估算单价：输入 {{ fmtYuan(usage.price_in_cents_per_m) }} / 百万 token，输出 {{ fmtYuan(usage.price_out_cents_per_m) }} / 百万 token
          </span>
        </el-form>
        <el-alert v-if="usageError" type="error" :title="usageError" show-icon :closable="false" class="gap-b" data-testid="admin-butler-usage-error">
          <el-button size="small" :loading="usageLoading" data-testid="admin-butler-usage-retry" @click="loadUsage">重试</el-button>
        </el-alert>
        <table v-else class="grid" v-loading="usageLoading">
          <thead><tr><th>日期</th><th>请求</th><th>输入 token</th><th>输出 token</th><th>工具调用</th><th>失败</th><th>活跃用户</th><th>估算费用</th></tr></thead>
          <tbody>
            <tr v-for="d in usageRows" :key="d.day" class="clickable" title="查看当日按用户" data-testid="admin-butler-usage-row" :data-day="d.day" @click="pickDay(d.day)">
              <td class="mono">{{ d.day }}</td>
              <td data-testid="admin-butler-usage-requests">{{ fmtCount(d.requests) }}</td>
              <td data-testid="admin-butler-usage-input">{{ fmtCount(d.input_tokens) }}</td>
              <td data-testid="admin-butler-usage-output">{{ fmtCount(d.output_tokens) }}</td>
              <td>{{ fmtCount(d.tool_calls) }}</td>
              <td :class="{ bad: d.failures > 0 }" data-testid="admin-butler-usage-failures">{{ fmtCount(d.failures) }}</td>
              <td data-testid="admin-butler-usage-users">{{ fmtCount(d.users) }}</td>
              <td data-testid="admin-butler-usage-cost">{{ fmtYuan(d.cost_cents) }}</td>
            </tr>
            <tr v-if="usage && !usageRows.length"><td colspan="8" class="muted empty">该范围内没有用量</td></tr>
          </tbody>
          <tfoot v-if="usage">
            <tr class="total" data-testid="admin-butler-usage-total">
              <td>合计</td>
              <td data-testid="admin-butler-total-requests">{{ fmtCount(usage.totals.requests) }}</td>
              <td data-testid="admin-butler-total-input">{{ fmtCount(usage.totals.input_tokens) }}</td>
              <td data-testid="admin-butler-total-output">{{ fmtCount(usage.totals.output_tokens) }}</td>
              <td>{{ fmtCount(usage.totals.tool_calls) }}</td>
              <td data-testid="admin-butler-total-failures">{{ fmtCount(usage.totals.failures) }}</td>
              <td class="muted">—</td>
              <td data-testid="admin-butler-total-cost">{{ fmtYuan(usage.totals.cost_cents) }}</td>
            </tr>
          </tfoot>
        </table>

        <h4 class="sub">按用户</h4>
        <el-form inline class="filters" @submit.prevent>
          <el-form-item label="日期">
            <input v-model="usersDay" type="date" class="el-input__inner native-date" data-testid="admin-butler-users-day" />
          </el-form-item>
        </el-form>
        <el-alert v-if="users.error.value" type="error" :title="users.error.value" show-icon :closable="false" class="gap-b" data-testid="admin-butler-users-error">
          <el-button size="small" :loading="users.loading.value" data-testid="admin-butler-users-retry" @click="users.load()">重试</el-button>
        </el-alert>
        <table class="grid" v-loading="users.loading.value">
          <thead><tr><th>用户</th><th>手机号</th><th>请求</th><th>输入 token</th><th>输出 token</th><th>工具调用</th><th>失败</th><th>估算费用</th></tr></thead>
          <tbody>
            <tr v-for="u in users.items.value" :key="u.user_id" data-testid="admin-butler-users-row" :data-user="u.user_id">
              <td><IdLink kind="user" :id="u.user_id" /></td>
              <td class="mono">{{ maskPhone(u.phone) }}</td>
              <td>{{ fmtCount(u.requests) }}</td>
              <td>{{ fmtCount(u.input_tokens) }}</td>
              <td>{{ fmtCount(u.output_tokens) }}</td>
              <td>{{ fmtCount(u.tool_calls) }}</td>
              <td :class="{ bad: u.failures > 0 }">{{ fmtCount(u.failures) }}</td>
              <td>{{ fmtYuan(u.cost_cents) }}</td>
            </tr>
            <tr v-if="!users.loading.value && !users.error.value && !users.items.value.length"><td colspan="8" class="muted empty">当日没有用量</td></tr>
          </tbody>
        </table>
        <div class="pager-row">
          <span class="muted">共 {{ users.total.value }} 人</span>
          <el-pagination v-model:current-page="users.page.value" v-model:page-size="users.size.value" :total="users.total.value" layout="prev, pager, next" />
        </div>
      </el-tab-pane>

      <!-- 对话（ops / super；其它角色不渲染） -->
      <el-tab-pane v-if="canConversations" name="conversations">
        <template #label><span data-testid="admin-butler-tab-conversations">对话</span></template>
        <p class="muted gap-b">对话是用户数据，只用于客服与排障；页面只读，不能修改或删除。</p>
        <el-form inline class="filters" @submit.prevent="convs.reset()">
          <el-form-item label="用户 ID">
            <el-input :model-value="conv.user_id" clearable style="width: 120px" data-testid="admin-butler-conv-user" @update:model-value="(v: string) => (conv.user_id = digitsOnly(v))" />
          </el-form-item>
          <el-form-item label="更新时间"><DateRange v-model="conv.range" prefix="admin-butler-conv-range" /></el-form-item>
          <el-button type="primary" native-type="submit" data-testid="admin-butler-conv-search">查询</el-button>
          <el-button data-testid="admin-butler-conv-reset" @click="resetConv">重置</el-button>
        </el-form>
        <el-alert v-if="convs.error.value" type="error" :title="convs.error.value" show-icon :closable="false" class="gap-b" data-testid="admin-butler-conv-error">
          <el-button size="small" :loading="convs.loading.value" data-testid="admin-butler-conv-retry" @click="convs.load()">重试</el-button>
        </el-alert>
        <table class="grid" v-loading="convs.loading.value">
          <thead><tr><th>对话</th><th>用户</th><th>标题</th><th>状态</th><th>消息</th><th>输入 / 输出 token</th><th>写操作</th><th>最后消息</th></tr></thead>
          <tbody>
            <tr
              v-for="c in convs.items.value" :key="c.id" class="clickable" role="button" tabindex="0" data-testid="admin-butler-conv-row" :data-id="c.id"
              @click="openDetail(c.id)" @keydown.enter.prevent="openDetail(c.id)"
            >
              <td class="mono">{{ c.id }}</td>
              <td @click.stop><IdLink kind="user" :id="c.user_id" /> <span class="mono muted">{{ maskPhone(c.phone) }}</span></td>
              <td class="title-cell" data-testid="admin-butler-conv-title">{{ c.title || '（无标题）' }}</td>
              <td>{{ labelOf(convStatusLabel, c.status) }}</td>
              <td>{{ c.messages }}</td>
              <td>{{ fmtCount(c.input_tokens) }} / {{ fmtCount(c.output_tokens) }}</td>
              <td data-testid="admin-butler-conv-writes">{{ c.write_calls }}</td>
              <td>{{ fmtTime(c.last_message_at) || '—' }}</td>
            </tr>
            <tr v-if="!convs.loading.value && !convs.error.value && !convs.items.value.length"><td colspan="8" class="muted empty">没有对话</td></tr>
          </tbody>
        </table>
        <div class="pager-row">
          <span class="muted" data-testid="admin-butler-conv-total">共 {{ convs.total.value }} 个</span>
          <el-pagination v-model:current-page="convs.page.value" v-model:page-size="convs.size.value" :total="convs.total.value" layout="prev, pager, next" />
        </div>
      </el-tab-pane>

      <!-- 设置（所有角色可看，super 可改） -->
      <el-tab-pane name="settings">
        <template #label><span data-testid="admin-butler-tab-settings">设置</span></template>
        <el-alert v-if="settingsError && !settings" type="error" :title="settingsError" show-icon :closable="false" data-testid="admin-butler-settings-error">
          <el-button size="small" :loading="settingsLoading" data-testid="admin-butler-settings-retry" @click="loadSettings">重试</el-button>
        </el-alert>
        <el-skeleton v-else-if="!settings" :rows="4" animated />
        <el-form v-else label-width="150px" class="settings" @submit.prevent="saveSettings">
          <el-alert v-if="!isSuper" type="info" title="只有超级管理员可修改" show-icon :closable="false" class="gap-b" data-testid="admin-butler-readonly" />
          <el-form-item label="总开关">
            <el-switch v-model="form.enabled" :disabled="!isSuper" active-text="开放" inactive-text="关闭" data-testid="admin-butler-enabled" />
          </el-form-item>
          <el-form-item label="关闭说明">
            <el-input v-model="form.notice" type="textarea" :rows="3" maxlength="200" show-word-limit :disabled="!isSuper" placeholder="关闭时展示给用户，例如：系统维护中，预计 18:00 恢复" data-testid="admin-butler-notice" />
          </el-form-item>
          <el-form-item label="每用户每日 token">
            <el-input :model-value="form.daily" :disabled="!isSuper" inputmode="numeric" style="width: 200px" data-testid="admin-butler-daily" @update:model-value="(v: string) => (form.daily = digitsOnly(v))" />
            <span class="muted hint">10,000～100,000,000（输入 + 输出合计）</span>
          </el-form-item>
          <el-form-item label="每对话用户消息数">
            <el-input :model-value="form.turns" :disabled="!isSuper" inputmode="numeric" style="width: 200px" data-testid="admin-butler-turns" @update:model-value="(v: string) => (form.turns = digitsOnly(v))" />
            <span class="muted hint">5～1000</span>
          </el-form-item>
          <p v-if="formError" class="error" data-testid="admin-butler-settings-form-error">{{ formError }}</p>
          <el-form-item>
            <el-button type="primary" native-type="submit" :disabled="!isSuper" :loading="saving" data-testid="admin-butler-save">保存</el-button>
            <span class="muted hint" data-testid="admin-butler-updated">
              最后修改：{{ fmtTime(settings.updated_at) }}<template v-if="settings.updated_by != null">（管理员 #{{ settings.updated_by }}）</template>
            </span>
          </el-form-item>
        </el-form>
      </el-tab-pane>
    </el-tabs>

    <el-drawer :model-value="detail.open" size="760px" title="对话详情（只读）" data-testid="admin-butler-conv-drawer" @close="closeDetail">
      <el-alert v-if="detail.error && !detail.data" type="error" :title="detail.error" show-icon :closable="false" data-testid="admin-butler-conv-detail-error">
        <el-button size="small" :loading="detail.loading" @click="loadDetail">重试</el-button>
      </el-alert>
      <el-skeleton v-else-if="!detail.data" :rows="6" animated />
      <div v-if="detail.data" class="drawer">
        <el-descriptions :column="2" border size="small">
          <el-descriptions-item label="对话 ID"><span class="mono" data-testid="admin-butler-conv-id">{{ detail.data.conversation.id }}</span></el-descriptions-item>
          <el-descriptions-item label="用户"><IdLink kind="user" :id="detail.data.conversation.user_id" /> <span class="mono muted">{{ maskPhone(detail.data.conversation.phone) }}</span></el-descriptions-item>
          <el-descriptions-item label="标题" :span="2">{{ detail.data.conversation.title || '（无标题）' }}</el-descriptions-item>
          <el-descriptions-item label="状态">{{ labelOf(convStatusLabel, detail.data.conversation.status) }}</el-descriptions-item>
          <el-descriptions-item label="模型"><span class="mono">{{ detail.data.conversation.provider }} / {{ detail.data.conversation.model }}</span></el-descriptions-item>
          <el-descriptions-item label="用户消息数">{{ detail.data.conversation.user_turns }}</el-descriptions-item>
          <el-descriptions-item label="输入 / 输出 token">{{ fmtCount(detail.data.conversation.input_tokens) }} / {{ fmtCount(detail.data.conversation.output_tokens) }}</el-descriptions-item>
          <el-descriptions-item label="创建">{{ fmtTime(detail.data.conversation.created_at) }}</el-descriptions-item>
          <el-descriptions-item label="最后消息">{{ fmtTime(detail.data.conversation.last_message_at) || '—' }}</el-descriptions-item>
          <el-descriptions-item v-if="detail.data.conversation.context?.instance_id" label="上下文实例" :span="2"><IdLink kind="instance" :id="detail.data.conversation.context.instance_id" /></el-descriptions-item>
        </el-descriptions>

        <h4>消息</h4>
        <div class="msgs">
          <div v-for="m in detail.data.messages" :key="m.id" class="msg" :class="`role-${m.role}`" data-testid="admin-butler-conv-msg" :data-role="m.role" :data-seq="m.seq">
            <div class="msg-head"><b>{{ labelOf(roleLabel, m.role) }}</b><span class="muted">#{{ m.seq }} · {{ fmtTime(m.created_at) }} · {{ m.tokens }} token</span></div>
            <template v-for="(b, i) in m.content" :key="i">
              <!-- 纯文本：插值自动转义，white-space: pre-wrap 保留换行 -->
              <div v-if="b.type === 'text'" class="text" data-testid="admin-butler-conv-text">{{ b.text }}</div>
              <div v-else-if="b.type === 'tool'" class="step" :class="b.status" data-testid="admin-butler-conv-step">{{ stepMark(b.status) }} {{ b.label }} <span class="mono muted">{{ b.name }}</span></div>
              <details v-else-if="b.type === 'tool_result'" class="result" :class="{ suspicious: b.suspicious }">
                <summary>
                  工具结果<span class="mono muted"> {{ b.tool_call_id }}</span>
                  <el-tag v-if="!b.ok" size="small" type="danger" class="gap">失败</el-tag>
                  <el-tag v-if="b.suspicious" size="small" type="danger" effect="dark" class="gap" data-testid="admin-butler-suspicious">可疑：疑似提示注入</el-tag>
                </summary>
                <pre class="pre">{{ b.summary }}</pre>
              </details>
              <div v-else-if="b.type === 'context'" class="muted ctx">上下文：实例 <IdLink kind="instance" :id="b.instance_id" /></div>
            </template>
          </div>
          <p v-if="!detail.data.messages.length" class="muted">没有消息</p>
        </div>

        <h4>工具调用</h4>
        <table class="grid calls">
          <thead><tr><th>时间</th><th>工具</th><th>状态</th><th>报价</th><th>参数 / 确认卡</th><th>结果</th></tr></thead>
          <tbody>
            <tr v-for="c in detail.data.tool_calls" :key="c.id" data-testid="admin-butler-conv-call" :data-id="c.id" :data-tool="c.tool" :data-status="c.status">
              <td>
                {{ fmtTime(c.created_at) }}
                <div v-if="c.decided_at" class="muted">确认 {{ fmtTime(c.decided_at) }}</div>
                <div v-if="c.executed_at" class="muted">执行 {{ fmtTime(c.executed_at) }}</div>
              </td>
              <td>{{ c.label }}<div class="mono muted">{{ c.tool }} · {{ c.kind === 'write' ? '写' : '读' }}</div><div class="mono muted">{{ c.id }}</div></td>
              <td><el-tag size="small" :type="callType(c.status)" data-testid="admin-butler-call-status">{{ labelOf(toolCallStatusLabel, c.status) }}</el-tag></td>
              <td>{{ c.quote_amount != null ? fmtYuan(c.quote_amount) : '—' }}</td>
              <td>
                <pre class="pre" data-testid="admin-butler-call-args">{{ argsText(c.args) }}</pre>
                <div v-if="c.summary" class="text summary">{{ c.summary }}</div>
              </td>
              <td>
                <div v-if="c.error_code" class="bad mono">{{ c.error_code }}</div>
                <div v-if="c.result_ref" class="mono">产生：{{ c.result_ref }}</div>
                <details v-if="c.result_summary"><summary>结果摘要</summary><pre class="pre">{{ c.result_summary }}</pre></details>
                <span v-if="!c.error_code && !c.result_ref && !c.result_summary" class="muted">—</span>
                <div v-if="c.kind === 'write'" class="muted">审计：butler_tool_call / {{ c.id }}</div>
              </td>
            </tr>
            <tr v-if="!detail.data.tool_calls.length"><td colspan="6" class="muted empty">没有工具调用</td></tr>
          </tbody>
        </table>
      </div>
    </el-drawer>
  </el-card>
</template>

<style scoped>
.filters { margin-bottom: 8px; }
.muted { color: #909399; font-size: 13px; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.gap { margin-left: 6px; }
.gap-b { margin-bottom: 12px; }
.bad { color: var(--el-color-danger); }
.error { color: var(--el-color-danger); font-size: 13px; margin: 0 0 12px 150px; }
.hint { margin-left: 12px; }
.sub { margin: 24px 0 8px; }
.grid { width: 100%; border-collapse: collapse; font-size: 13px; }
.grid th, .grid td { padding: 8px; text-align: left; border-bottom: 1px solid var(--el-border-color-lighter); vertical-align: top; }
.grid th { color: #909399; font-weight: 500; }
.total td { font-weight: 600; background: var(--el-fill-color-light); }
.empty { text-align: center; }
.clickable { cursor: pointer; }
.clickable:hover { background: var(--el-fill-color-lighter); }
.title-cell { max-width: 260px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.pager-row { display: flex; justify-content: space-between; align-items: center; margin-top: 12px; }
.native-date { height: 32px; border: 1px solid #dcdfe6; border-radius: 4px; padding: 0 8px; font-size: 14px; background: #fff; }
.settings { max-width: 720px; }
.drawer { display: grid; gap: 12px; }
.drawer h4 { margin: 8px 0 0; }
.msgs { display: grid; gap: 8px; }
.msg { border: 1px solid var(--el-border-color-lighter); border-radius: 6px; padding: 8px 10px; }
.role-user { background: var(--el-color-primary-light-9); }
.role-tool { background: var(--el-fill-color-lighter); }
.msg-head { display: flex; justify-content: space-between; margin-bottom: 4px; font-size: 13px; }
.text { white-space: pre-wrap; word-break: break-word; font-size: 14px; }
.summary { margin-top: 6px; font-size: 13px; }
.step { font-size: 13px; }
.step.ok { color: var(--el-color-success); }
.step.error { color: var(--el-color-danger); }
.result { font-size: 13px; }
.result.suspicious { border-left: 3px solid var(--el-color-danger); padding-left: 6px; }
.pre { white-space: pre-wrap; word-break: break-all; margin: 4px 0 0; font-size: 12px; max-height: 240px; overflow: auto; background: var(--el-fill-color-lighter); padding: 6px; border-radius: 4px; font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.calls td { font-size: 12px; }
.ctx { font-size: 12px; }
</style>
