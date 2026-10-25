<script setup lang="ts">
import type { PendingToolCall } from '@spark/shared'
import { computed, ref, watch } from 'vue'
import { useTicker } from '../composables/useTicker'
import {
  cardDeadline, confirmAmountText, confirmResult, confirmSummaryLines, confirmTitle, formatCountdown, isDanger, type ConfirmState,
} from '../logic/butler'

/**
 * AI 管家写操作的确认卡（规格 §11.2）：文案与金额都由服务端按真实参数渲染，这里只展示。
 * 危险操作（释放 / 卸载 / 取消预订）红框 + 勾选「我已了解」才能确认；倒计时到 0 → emit('expire')，按钮禁用「已失效」。
 * 确认 / 取消只 emit，由 ButlerChat 发 confirm 请求并继续处理 SSE。
 */
const props = defineProps<{ card: PendingToolCall; state: ConfirmState; message?: string; busy?: boolean }>()
const emit = defineEmits<{ approve: []; reject: []; expire: [] }>()

const now = useTicker()
// 到期时刻按收到卡片时的服务器剩余秒推算（浏览器时钟与服务器不一定一致）。
// 到期时刻与 now 必须取自同一次读钟：两次 Date.now() 之间差 1ms 就会让 10:00 显示成 10:01、expires_in=0 的卡片多挂 1 秒
const deadline = ref(cardDeadline(props.card, now.value))
watch(
  () => props.card,
  (c) => {
    now.value = Date.now()
    deadline.value = cardDeadline(c, now.value)
  },
)
const left = computed(() => Math.max(0, Math.ceil((deadline.value - now.value) / 1000)))
/** 待确认但本地倒计时已到 0：按已失效显示（父组件收到 expire 后也会改成 expired） */
const shown = computed<ConfirmState>(() => (props.state === 'pending' && left.value === 0 ? 'expired' : props.state))
watch(
  shown,
  (s) => {
    if (s === 'expired' && props.state === 'pending') emit('expire')
  },
  { immediate: true },
)

const danger = computed(() => props.card.danger || isDanger(props.card.tool))
const ack = ref(false)
const lines = computed(() => confirmSummaryLines(props.card).join('\n'))
const amount = computed(() => confirmAmountText(props.card))
const result = computed(() => confirmResult(shown.value, props.message))
const okDisabled = computed(() => shown.value !== 'pending' || !!props.busy || (danger.value && !ack.value))
</script>

<template>
  <div class="confirm" :class="{ danger }" data-testid="butler-confirm-card" :data-tool="card.tool" :data-state="shown">
    <div class="title" data-testid="butler-confirm-title">{{ confirmTitle(card) }}</div>
    <div class="summary" data-testid="butler-confirm-summary">{{ lines }}</div>
    <div v-if="amount" class="amount" data-testid="butler-confirm-amount">{{ amount }}</div>
    <div v-if="danger" class="danger-text" data-testid="butler-confirm-danger">此操作不可恢复</div>

    <template v-if="shown === 'pending' || shown === 'expired'">
      <el-checkbox v-if="danger" v-model="ack" :disabled="shown !== 'pending'" data-testid="butler-confirm-ack">我已了解</el-checkbox>
      <div class="foot">
        <span v-if="shown === 'pending'" class="muted" data-testid="butler-confirm-countdown">{{ formatCountdown(left) }} 内有效</span>
        <span v-else class="muted" data-testid="butler-confirm-result">已失效</span>
        <span class="btns">
          <el-button size="small" :disabled="shown !== 'pending' || busy" data-testid="butler-confirm-cancel" @click="emit('reject')">取消</el-button>
          <el-button size="small" :type="danger ? 'danger' : 'primary'" :disabled="okDisabled" data-testid="butler-confirm-ok" @click="emit('approve')">确认执行</el-button>
        </span>
      </div>
    </template>
    <div v-else-if="result" class="result" :class="result.type" data-testid="butler-confirm-result">{{ result.text }}</div>
  </div>
</template>

<style scoped>
.confirm { border: 1px solid var(--el-border-color); border-radius: 8px; padding: 12px; background: #fff; display: grid; gap: 8px; }
.confirm.danger { border-color: var(--el-color-danger); }
.title { font-weight: 600; }
.summary { white-space: pre-line; font-size: 13px; line-height: 1.6; word-break: break-all; }
.amount { font-size: 18px; font-weight: 600; }
.danger-text { color: var(--el-color-danger); font-size: 13px; }
.foot { display: flex; align-items: center; justify-content: space-between; gap: 8px; flex-wrap: wrap; }
.btns { display: flex; gap: 8px; }
.btns .el-button + .el-button { margin-left: 0; }
.muted { color: #909399; font-size: 12px; }
.result { font-size: 13px; }
.result.success { color: var(--el-color-success); }
.result.danger { color: var(--el-color-danger); }
.result.muted, .result.info { color: #909399; }
</style>
