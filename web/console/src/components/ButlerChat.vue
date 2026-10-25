<script setup lang="ts">
import { ApiError, isApiError, messageOf, streamPost, type ButlerStatus, type ConversationDetail, type PendingToolCall, type SSEEvent } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { api } from '../api'
import { usePolling } from '../composables/usePolling'
import {
  SUGGESTIONS, appendError, appendUser, applyEvent, bannerOfStatus, charCount, clearErrors, emptyState, formatQuota, fromDetail, markConfirm,
  rollbackUser, sendErrorKind, stepText, withoutRetry, type ChatItem, type ChatState,
} from '../logic/butler'
import { renderMarkdown } from '../logic/markdown'
import { notifyError } from '../logic/notify'
import { handleUnauthorized } from '../store/session'
import ButlerConfirmCard from './ButlerConfirmCard.vue'

/**
 * AI 管家聊天区（抽屉与 /butler 全页共用，规格 §11.2）。
 * - 发送 / 确认都是 SSE（streamPost），事件经 logic/butler 的 applyEvent 更新状态；
 * - 流在 done / error 之前断开（网络、看门狗、服务端重启）→ 拉对话详情刷新，status 仍是 running 时每 3 秒再拉，直到 idle / awaiting_confirm（服务端轮次脱离请求继续跑）；
 * - 建立流之前的错误：额度 / 关闭 / 轮数上限 → 横幅；401 → 跳登录；其它 → 可重试的错误行。
 * 对话 id、草稿、实例上下文由父组件（store/butler）保管：本组件只 emit。
 */
const props = withDefaults(
  defineProps<{ conversationId?: string | null; context?: { instance_id: string } | null; initialDraft?: string }>(),
  { conversationId: null, context: null, initialDraft: '' },
)
const emit = defineEmits<{
  'update:conversationId': [id: string | null]
  'update:draft': [text: string]
  /** 实例上下文已随一条消息被服务端收下（父组件 consumeContext） */
  'context-used': []
  /** 一轮结束（done / error / 断线后轮询到结束）：父组件可刷新历史列表 */
  settled: []
}>()

const MAX_CHARS = 2000
/** 距底部小于这个像素时新内容自动滚到底；用户往上翻看历史时不打扰 */
const NEAR_BOTTOM_PX = 80
const router = useRouter()

const state = ref<ChatState>(emptyState())
const title = ref('')
const status = ref<ButlerStatus | null>(null)
const banner = ref<{ code: string; text: string } | null>(null)
const text = ref(props.initialDraft)
const streaming = ref(false)
const polling = ref(false)
const loadingDetail = ref(false)
const opened = reactive(new Set<string>())
const listEl = ref<HTMLElement | null>(null)
let ctrl: AbortController | null = null
let loadSeq = 0

const disabled = computed(() => status.value?.enabled === false)
const busy = computed(() => streaming.value || polling.value || loadingDetail.value || state.value.status === 'running')
const quota = computed(() => formatQuota(state.value.dayUsed, state.value.dayLimit))
const ingressDomain = computed(() => status.value?.ingress_domain ?? '')
const count = computed(() => charCount(text.value))
const canSend = computed(() => !disabled.value && !busy.value && text.value.trim().length > 0 && count.value <= MAX_CHARS)
const empty = computed(() => !state.value.items.length && !loadingDetail.value)

// 渲染结果按「域名 + 文本」缓存：流式时只有最后一个气泡在变，其余气泡不必每个 delta 重新渲染
const mdCache = new Map<string, string>()
function md(src: string): string {
  const key = `${ingressDomain.value}\n${src}`
  let html = mdCache.get(key)
  if (html === undefined) {
    if (mdCache.size > 200) mdCache.clear()
    html = renderMarkdown(src, { ingressDomain: ingressDomain.value })
    mdCache.set(key, html)
  }
  return html
}

/** 站内链接（/instances/… 等，无 target）走前端路由，不整页刷新；实例域名链接（target=_blank）照常打开新标签。 */
function onMessagesClick(e: MouseEvent) {
  if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return
  const a = (e.target as HTMLElement | null)?.closest?.('a')
  if (!a || a.getAttribute('target')) return
  const href = a.getAttribute('href') ?? ''
  if (!href.startsWith('/') || href.startsWith('//') || !router) return
  e.preventDefault()
  void router.push(href)
}

function setState(next: ChatState) {
  const prevId = state.value.conversationId
  state.value = next
  if (next.conversationId !== prevId && next.conversationId !== props.conversationId) emit('update:conversationId', next.conversationId)
}

function keepQuota(s: ChatState): ChatState {
  return { ...s, dayUsed: state.value.dayUsed, dayLimit: state.value.dayLimit }
}

// ---- 状态 / 详情 ----

async function loadStatus() {
  try {
    const st = await api.get<ButlerStatus>('/butler/status')
    status.value = st
    state.value = { ...state.value, dayUsed: Number(st.day_used) || 0, dayLimit: Number(st.day_limit) || 0 }
    if (!st.enabled) banner.value = { code: 'butler_disabled', text: bannerOfStatus(st) }
    else if (banner.value?.code === 'butler_disabled') banner.value = null
  } catch (e) {
    if (isApiError(e) && e.status === 404) {
      // 服务端未启用管家（路由未注册）
      status.value = { enabled: false, notice: '', day_used: 0, day_limit: 0, provider_label: '', ingress_domain: '' }
      banner.value = { code: 'butler_disabled', text: bannerOfStatus(status.value) }
    }
  }
}

async function loadDetail(id: string) {
  const mine = ++loadSeq
  if (!state.value.items.length) loadingDetail.value = true
  try {
    const d = await api.get<ConversationDetail>(`/butler/conversations/${id}`)
    if (mine !== loadSeq || state.value.conversationId !== id) return
    const wasPolling = polling.value
    state.value = fromDetail(d, state.value)
    title.value = d.conversation.title
    polling.value = state.value.status === 'running'
    if (wasPolling && !polling.value) {
      emit('settled')
      void loadStatus() // 轮次结束：刷新今日额度
    }
    scrollToBottom(!wasPolling)
  } catch (e) {
    if (mine !== loadSeq) return
    if (isApiError(e) && e.status === 404) {
      // 对话已被删除（别的标签页）或不属于当前用户：回到新对话
      polling.value = false
      state.value = keepQuota(emptyState())
      title.value = ''
      emit('update:conversationId', null)
      return
    }
    // 断线后的轮询：暂时失败就等下一次；其它情况提示
    if (!polling.value) notifyError(e)
  } finally {
    if (mine === loadSeq) loadingDetail.value = false
  }
}

usePolling(
  () => {
    const id = state.value.conversationId
    if (id) return loadDetail(id)
  },
  3_000,
  { enabled: () => polling.value && !streaming.value },
)

function abortStream() {
  if (ctrl) {
    ctrl.abort()
    ctrl = null
  }
  streaming.value = false
}

watch(
  () => props.conversationId,
  (id) => {
    if (id === state.value.conversationId) return
    abortStream()
    polling.value = false
    loadSeq++
    title.value = ''
    if (!id) {
      state.value = keepQuota(emptyState())
      return
    }
    state.value = { ...keepQuota(emptyState()), conversationId: id }
    void loadDetail(id)
  },
)

watch(
  () => props.initialDraft,
  (v) => {
    if ((v ?? '') !== text.value) text.value = v ?? ''
  },
)
watch(text, (v) => emit('update:draft', v))

onMounted(() => {
  void loadStatus()
  if (props.conversationId) {
    state.value = { ...state.value, conversationId: props.conversationId }
    void loadDetail(props.conversationId)
  }
})
onBeforeUnmount(() => abortStream())

/** 在状态更新之后、DOM 更新之前调用：只有原本就在底部附近（或 force）时才滚到底。 */
function scrollToBottom(force = false) {
  const el = listEl.value
  if (!el) return
  const near = el.scrollHeight - el.scrollTop - el.clientHeight < NEAR_BOTTOM_PX
  if (!force && !near) return
  void nextTick(() => {
    el.scrollTop = el.scrollHeight
  })
}

// ---- 发送 / 确认 ----

interface StreamOpts {
  /** 发送消息：重试用的原文 */
  retryText?: string
  /** 确认流：卡片 id */
  confirmId?: string
  /** 发送消息：appendUser 之前的状态，服务端没收下消息时整体恢复（含被本地标为失效的待确认卡） */
  before?: ChatState
  onStart?: () => void
}

/** 服务端没收下这条消息：恢复发送前的状态（保留最新额度）；没有快照时撤回末尾的用户气泡。 */
function restoreBefore(opt: StreamOpts): ChatState {
  if (!opt.before) return rollbackUser(state.value)
  return { ...opt.before, dayUsed: state.value.dayUsed, dayLimit: state.value.dayLimit }
}

/** 流在 done / error 之前结束：服务端仍在跑，拉详情刷新并在 running 时轮询。 */
function interrupted(opt: StreamOpts) {
  const id = state.value.conversationId
  if (!id) {
    setState(appendError(restoreBefore(opt), 'network', '网络异常，请稍后重试', opt.retryText))
    return
  }
  polling.value = true
  void loadDetail(id)
}

function onStreamError(err: unknown, accepted: boolean, opt: StreamOpts) {
  const e = isApiError(err) ? err : new ApiError(0, 'network', '网络异常，请稍后重试', '')
  const kind = sendErrorKind(e.status, e.code)
  if (kind === 'ignore') return
  if (kind === 'auth') {
    setState(opt.confirmId ? state.value : restoreBefore(opt))
    handleUnauthorized()
    return
  }
  // 服务端已收下（收到过事件，或确认请求可能已执行）后断线：以服务端为准
  if (e.code === 'network' && accepted) {
    interrupted(opt)
    return
  }
  if (opt.confirmId) {
    if (e.code === 'butler_confirm_invalid') {
      // 卡片已失效：本地也没有待确认的了，回到 idle 才能继续发消息
      setState({ ...markConfirm(state.value, opt.confirmId, 'expired'), status: 'idle' })
      ElMessage.warning(e.message)
      return
    }
    // 确认请求没有被处理：卡片回到待确认，可再点
    setState({ ...markConfirm(state.value, opt.confirmId, 'pending'), status: 'awaiting_confirm' })
    if (kind === 'banner') showBanner(e)
    else notifyError(e)
    return
  }
  // 建立流之前失败：消息没有被服务端收下，撤回本地气泡
  const s = restoreBefore(opt)
  if (kind === 'banner') {
    setState(s)
    showBanner(e)
    if (!text.value && opt.retryText) text.value = opt.retryText // 横幅场景没有重试按钮：原文放回输入框
    return
  }
  setState(appendError(s, e.code, messageOf(e), opt.retryText))
}

function showBanner(e: ApiError) {
  banner.value = { code: e.code, text: e.message }
  if (e.code === 'butler_disabled' && status.value) status.value = { ...status.value, enabled: false }
  else if (e.code === 'butler_disabled') status.value = { enabled: false, notice: '', day_used: 0, day_limit: 0, provider_label: '', ingress_domain: '' }
}

async function runStream(path: string, body: unknown, opt: StreamOpts) {
  abortStream()
  const c = new AbortController()
  ctrl = c
  streaming.value = true
  polling.value = false
  loadSeq++ // 进行中的详情请求作废：迟到的旧详情不能覆盖流式状态
  loadingDetail.value = false
  let accepted = !!opt.confirmId
  let finished = false
  try {
    await streamPost(
      '/api/v1',
      path,
      body,
      (ev: SSEEvent) => {
        if (ctrl !== c) return
        if (ev.event === 'start') {
          accepted = true
          opt.onStart?.()
        } else if (ev.event === 'done' || ev.event === 'error') finished = true
        else accepted = true
        const wasNew = !state.value.conversationId
        const next = applyEvent(state.value, ev)
        setState(opt.confirmId && ev.event === 'error' ? withoutRetry(next) : next)
        if (wasNew && state.value.conversationId && opt.retryText) title.value = Array.from(opt.retryText).slice(0, 30).join('')
        scrollToBottom()
      },
      { signal: c.signal },
    )
    if (ctrl !== c) return
    if (!finished) interrupted(opt)
  } catch (err) {
    if (ctrl !== c) return
    onStreamError(err, accepted, opt)
  } finally {
    if (ctrl === c) {
      ctrl = null
      streaming.value = false
    }
    if (finished) emit('settled')
  }
}

async function send(raw?: string) {
  const t = (raw ?? text.value).trim()
  if (!t || charCount(t) > MAX_CHARS || disabled.value || busy.value) return
  if (raw === undefined) text.value = ''
  if (banner.value && banner.value.code !== 'butler_disabled') banner.value = null
  const before = clearErrors(state.value)
  setState(appendUser(before, t))
  scrollToBottom(true)
  const ctx = props.context
  const id = state.value.conversationId
  await runStream(`/butler/conversations/${id ?? 'new'}/messages`, ctx ? { text: t, context: ctx } : { text: t }, {
    retryText: t,
    before,
    onStart: () => {
      if (ctx) emit('context-used')
    },
  })
}

function onKeydown(e: Event | KeyboardEvent) {
  if (!(e instanceof KeyboardEvent)) return
  // Enter 发送、Shift+Enter 换行；输入法组字中的 Enter 不发送
  if (e.key !== 'Enter' || e.shiftKey || e.isComposing || e.keyCode === 229) return
  e.preventDefault()
  void send()
}

function retry(t: string) {
  if (busy.value) return
  void send(t)
}

function decide(card: PendingToolCall, approve: boolean) {
  const id = state.value.conversationId
  if (!id || streaming.value) return
  setState(markConfirm(state.value, card.id, approve ? 'approved' : 'rejected'))
  void runStream(`/butler/conversations/${id}/tool-calls/${card.id}/confirm`, { approve }, { confirmId: card.id })
}

function onExpire(card: PendingToolCall) {
  setState(markConfirm(state.value, card.id, 'expired'))
}

function newConversation() {
  abortStream()
  polling.value = false
  loadSeq++
  title.value = ''
  if (banner.value?.code === 'butler_turn_limit') banner.value = null
  state.value = keepQuota(emptyState())
  emit('update:conversationId', null)
}

function toggleStep(id: string) {
  if (opened.has(id)) opened.delete(id)
  else opened.add(id)
}

function keyOf(it: ChatItem, i: number): string {
  if (it.kind === 'confirm') return `c-${it.card.id}`
  if (it.kind === 'tool') return `t-${it.id}`
  return `${it.kind}-${i}`
}
</script>

<template>
  <div class="chat" data-testid="butler-chat">
    <div class="head">
      <span class="title" data-testid="butler-title">{{ title || '新对话' }}</span>
      <span class="quota" :class="quota.level" data-testid="butler-quota">{{ quota.text }}</span>
      <el-button size="small" data-testid="butler-new" @click="newConversation">新对话</el-button>
    </div>

    <div ref="listEl" class="messages" role="log" aria-live="polite" data-testid="butler-messages" @click="onMessagesClick">
      <div v-if="empty" class="welcome" data-testid="butler-welcome">
        <p>您好，我是 AI 管家，可以帮您挑选机器、部署应用、排查问题、查询余额与计费。任何花钱或删除的操作都会先请您确认。</p>
        <div class="chips">
          <el-button
            v-for="(s, i) in SUGGESTIONS" :key="s" size="small" round :disabled="disabled || busy" :data-testid="`butler-suggest-${i}`"
            @click="send(s)"
          >
            {{ s }}
          </el-button>
        </div>
      </div>
      <el-skeleton v-else-if="loadingDetail" :rows="3" animated />

      <template v-for="(it, i) in state.items" :key="keyOf(it, i)">
        <div v-if="it.kind === 'user'" class="row user">
          <div class="bubble user" data-testid="butler-msg-user">{{ it.text }}</div>
        </div>
        <div v-else-if="it.kind === 'assistant'" class="row">
          <div class="bubble assistant" data-testid="butler-msg-assistant" :data-streaming="String(it.streaming)">
            <!-- eslint-disable-next-line vue/no-v-html -- renderMarkdown 先转义全部文本、链接走白名单（规格 §11.4） -->
            <div class="md" v-html="md(it.text)" />
            <span v-if="it.streaming" class="cursor" aria-hidden="true" />
            <div v-if="it.truncated" class="truncated" data-testid="butler-truncated">回答被截断（超出长度上限），可以让管家继续</div>
          </div>
        </div>
        <div v-else-if="it.kind === 'tool'" class="step" :class="it.status" data-testid="butler-step" :data-name="it.name" :data-status="it.status">
          <button
            v-if="it.summary" type="button" class="step-btn" :aria-expanded="opened.has(it.id)" :data-testid="`butler-step-${it.name}`"
            @click="toggleStep(it.id)"
          >
            {{ stepText(it) }}
          </button>
          <span v-else :data-testid="`butler-step-${it.name}`">{{ stepText(it) }}</span>
          <pre v-if="it.summary && opened.has(it.id)" class="step-summary" data-testid="butler-step-summary">{{ it.summary }}</pre>
        </div>
        <ButlerConfirmCard
          v-else-if="it.kind === 'confirm'" :card="it.card" :state="it.state" :message="it.message" :busy="streaming"
          @approve="decide(it.card, true)" @reject="decide(it.card, false)" @expire="onExpire(it.card)"
        />
        <div v-else-if="it.kind === 'error'" class="err" data-testid="butler-error" :data-code="it.code">
          <span>{{ it.message }}</span>
          <el-button v-if="it.retryText" link type="primary" size="small" :disabled="busy || disabled" data-testid="butler-retry" @click="retry(it.retryText)">重试</el-button>
        </div>
      </template>
      <div v-if="busy && !streaming" class="muted small" data-testid="butler-waiting">管家还在处理，请稍候…</div>
    </div>

    <el-alert v-if="banner" type="warning" :title="banner.text" show-icon :closable="false" class="banner" data-testid="butler-banner" />
    <div class="composer">
      <p v-if="context" class="muted small" data-testid="butler-context">将带上实例 {{ context.instance_id }} 的信息</p>
      <el-input
        v-model="text" type="textarea" :autosize="{ minRows: 2, maxRows: 6 }" resize="none" :disabled="disabled"
        placeholder="问问 AI 管家（Enter 发送，Shift+Enter 换行）" aria-label="给 AI 管家发消息" data-testid="butler-input" @keydown="onKeydown"
      />
      <div class="send-row">
        <span class="muted small">{{ status?.provider_label }}</span>
        <span class="count" :class="{ over: count > MAX_CHARS }" data-testid="butler-count">{{ count }}/{{ MAX_CHARS }}</span>
        <el-button type="primary" size="small" :disabled="!canSend" :loading="busy" data-testid="butler-send" @click="send()">发送</el-button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.chat { display: flex; flex-direction: column; height: 100%; min-height: 0; background: #f5f7fa; }
.head { display: flex; align-items: center; gap: 8px; padding: 8px 12px; background: #fff; border-bottom: 1px solid #ebeef5; }
.title { flex: 1; min-width: 0; font-weight: 600; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.quota { font-size: 12px; color: #909399; }
.quota.warn { color: var(--el-color-warning); }
.quota.exhausted { color: var(--el-color-danger); }
.messages { flex: 1; min-height: 0; overflow-y: auto; padding: 12px; display: flex; flex-direction: column; gap: 10px; }
.welcome p { color: #606266; font-size: 14px; line-height: 1.6; margin: 0 0 12px; }
.chips { display: flex; flex-wrap: wrap; gap: 8px; }
.chips .el-button + .el-button { margin-left: 0; }
.row { display: flex; }
.row.user { justify-content: flex-end; }
.bubble { max-width: 85%; padding: 8px 12px; border-radius: 10px; font-size: 14px; line-height: 1.6; word-break: break-word; }
.bubble.user { background: var(--el-color-primary); color: #fff; white-space: pre-wrap; }
.bubble.assistant { background: #fff; border: 1px solid #ebeef5; }
.md :deep(p) { margin: 0 0 6px; }
.md :deep(p:last-child) { margin-bottom: 0; }
.md :deep(pre) { background: #f5f7fa; padding: 8px; border-radius: 6px; overflow-x: auto; font-size: 12px; }
.md :deep(code) { font-family: ui-monospace, Menlo, monospace; font-size: 12px; }
.md :deep(ul), .md :deep(ol) { margin: 4px 0; padding-left: 20px; }
.md :deep(h1), .md :deep(h2), .md :deep(h3) { font-size: 15px; margin: 6px 0; }
.cursor { display: inline-block; width: 7px; height: 14px; background: #909399; vertical-align: middle; animation: blink 1s steps(1) infinite; }
@keyframes blink { 50% { opacity: 0; } }
.truncated { margin-top: 6px; font-size: 12px; color: var(--el-color-warning); }
.step { font-size: 12px; color: #909399; }
.step.error { color: var(--el-color-danger); }
.step-btn { padding: 0; border: none; background: none; font: inherit; color: inherit; cursor: pointer; text-decoration: underline dotted; }
.step.running span, .step.running .step-btn { animation: blink 1.2s steps(1) infinite; }
.step.rejected, .step.expired, .step.pending { color: #a8abb2; }
.count { margin-left: auto; margin-right: 8px; font-size: 12px; color: #909399; }
.count.over { color: var(--el-color-danger); }
.step-summary { margin: 4px 0 0; padding: 6px 8px; background: #fff; border: 1px solid #ebeef5; border-radius: 6px; white-space: pre-wrap; word-break: break-all; max-height: 200px; overflow-y: auto; font-size: 12px; }
.err { display: flex; align-items: center; gap: 8px; font-size: 13px; color: var(--el-color-danger); background: #fef0f0; padding: 6px 10px; border-radius: 6px; }
.banner { border-radius: 0; }
.composer { padding: 8px 12px 12px; background: #fff; border-top: 1px solid #ebeef5; }
.send-row { display: flex; align-items: center; justify-content: space-between; margin-top: 8px; }
.muted { color: #909399; }
.small { font-size: 12px; margin: 0 0 6px; }
</style>
