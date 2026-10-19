<script setup lang="ts">
import { fmtTime, fmtYuan, isMobileUA, labelOf, messageOf, rechargeChannelLabel, useCountdown, type Recharge } from '@spark/shared'
import { ElMessage } from 'element-plus'
import QRCode from 'qrcode'
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import { usePolling } from '../composables/usePolling'
import { alipayLink, isMockQR, mmss } from '../logic/recharge'
import { notifyError } from '../logic/notify'
import { safeRedirect } from '../router'
import { refreshBalance } from '../store/session'

const route = useRoute()
const router = useRouter()
const id = computed(() => String(route.params.id))
const mobileUA = isMobileUA() // 拉起支付宝还是展示二维码，按设备类型而不是屏幕宽度
const rch = ref<Recharge | null>(null)
const loading = ref(true)
const loadError = ref('')
const expired = ref(false)
const paying = ref(false)
const canvas = ref<HTMLCanvasElement | null>(null)
let announcedPaid = false
let failing = false
let seq = 0 // 查询序号：只采用最后发出的那次查询的结果

const countdown = useCountdown({
  onZero: async () => {
    // 归零后再查一次：成功会按服务器剩余秒重新校准（剩 0 秒时 load 已标记过期）；查询失败时本地已归零，同样按过期处理
    await load()
    if (rch.value?.status === 'pending' && countdown.remaining.value === 0) expired.value = true
  },
})

async function load() {
  const mine = ++seq
  try {
    const r = await api.get<Recharge>(`/recharges/${id.value}`)
    if (mine !== seq) return // 之后又发出过查询：丢弃这次迟到的响应，避免旧的「待支付」覆盖「已支付」
    const wasPending = rch.value?.status === 'pending'
    rch.value = r
    loadError.value = ''
    failing = false
    if (r.status === 'pending') {
      countdown.start(r.remaining_seconds) // 每次查询都按服务器剩余秒重新校准
      if (r.remaining_seconds === 0) expired.value = true
    } else {
      countdown.stop()
    }
    if (r.status === 'paid' && wasPending && !announcedPaid) {
      // 只在本页看到「待支付 → 已支付」时提示；直接打开已支付的单只显示成功页
      announcedPaid = true
      ElMessage.success('充值成功')
      // 余额刷新失败不影响到账结果（钱包页会自己重新加载），不能变成未处理的 Promise 拒绝
      refreshBalance().catch(() => {})
    }
  } catch (e) {
    if (mine !== seq) return
    if (!rch.value) loadError.value = messageOf(e) // 首次加载失败：占位 + 重试
    else if (!failing) notifyError(e) // 轮询失败：同一次故障只提示一次，成功后复位
    failing = true
  } finally {
    if (mine === seq) loading.value = false
  }
}

async function retry() {
  loading.value = true
  loadError.value = ''
  await load()
}

onMounted(load)
usePolling(load, 3_000, { enabled: () => rch.value?.status === 'pending' && !expired.value })

const qr = computed(() => rch.value?.qr_code ?? '')
const mock = computed(() => isMockQR(qr.value))
const showCanvas = computed(() => !mobileUA && rch.value?.status === 'pending' && !expired.value && !!qr.value)
watch(
  [showCanvas, qr],
  async ([show, text]) => {
    if (!show || !text) return
    await nextTick()
    if (canvas.value) await QRCode.toCanvas(canvas.value, text, { width: 240, margin: 1 })
  },
  { immediate: true },
)

async function mockPay() {
  if (paying.value) return
  paying.value = true
  try {
    await api.post(`/dev/recharges/${id.value}/pay`, {})
    await load()
  } catch (e) {
    notifyError(e)
  } finally {
    paying.value = false
  }
}

const redirectTo = computed(() => safeRedirect(route.query.redirect))
const backLabel = computed(() => (redirectTo.value ? '返回' : '返回钱包'))
function back() {
  void router.replace(redirectTo.value ?? '/wallet')
}
function again() {
  void router.replace('/wallet/recharge')
}
</script>

<template>
  <div class="order">
    <el-alert v-if="loadError" type="error" :title="loadError" :closable="false" show-icon data-testid="rchorder-error">
      <el-button size="small" data-testid="rchorder-retry" @click="retry">重试</el-button>
    </el-alert>
    <el-skeleton v-else-if="loading" :rows="4" animated />
    <el-card v-else-if="rch">
      <template v-if="rch.status === 'paid'">
        <el-result icon="success" title="充值成功" data-testid="rchorder-paid">
          <template #sub-title>
            <p>到账 {{ fmtYuan(rch.amount) }}</p>
            <p data-testid="rchorder-paid-at">到账时间 {{ fmtTime(rch.paid_at) }}</p>
          </template>
          <template #extra><el-button type="primary" data-testid="rchorder-back" @click="back">{{ backLabel }}</el-button></template>
        </el-result>
      </template>

      <template v-else-if="rch.status === 'closed'">
        <el-result icon="warning" title="充值单已关闭，请重新发起" data-testid="rchorder-closed">
          <template #extra><el-button type="primary" data-testid="rchorder-again" @click="again">重新发起</el-button></template>
        </el-result>
      </template>

      <template v-else>
        <div class="pending" data-testid="rchorder-pending">
          <div class="amount" data-testid="rchorder-amount">{{ fmtYuan(rch.amount) }}</div>
          <div class="muted"><span data-testid="rchorder-channel">{{ labelOf(rechargeChannelLabel, rch.channel) }}</span> · 剩余 <span data-testid="rchorder-countdown">{{ mmss(countdown.remaining.value) }}</span></div>

          <el-alert v-if="expired" type="warning" title="二维码已过期，请重新发起" :closable="false" show-icon data-testid="rchorder-expired">
            <el-button size="small" data-testid="rchorder-again" @click="again">重新发起</el-button>
          </el-alert>

          <template v-else>
            <canvas v-if="showCanvas" ref="canvas" class="qr" data-testid="rchorder-qr" width="240" height="240" />
            <template v-if="mobileUA">
              <a :href="alipayLink(qr)" class="el-button el-button--primary el-button--large alipay" data-testid="rchorder-alipay-link">打开支付宝付款</a>
              <a :href="qr" class="muted small" target="_blank" rel="noopener" data-testid="rchorder-open-browser">没有自动打开？点此在浏览器中打开</a>
            </template>
            <div v-if="mock" class="mock" data-testid="rchorder-mock">
              <p>开发环境模拟支付</p>
              <el-button type="success" :loading="paying" data-testid="rchorder-mock-pay" @click="mockPay">模拟支付成功</el-button>
            </div>
            <p class="muted small">支付完成后页面会自动刷新</p>
          </template>
        </div>
      </template>
    </el-card>
  </div>
</template>

<style scoped>
.order { max-width: 560px; }
.pending { display: flex; flex-direction: column; align-items: center; gap: 12px; text-align: center; }
.amount { font-size: 28px; font-weight: 600; }
.muted { color: #909399; }
.small { font-size: 12px; }
.qr { width: 240px; height: 240px; }
.alipay { width: 100%; max-width: 320px; text-decoration: none; }
.mock { border: 1px dashed var(--el-color-warning); padding: 12px; border-radius: 8px; }
</style>
