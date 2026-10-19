<script setup lang="ts">
import {
  bindingStatusLabel, fmtDate, fmtTime, fmtYuan, labelOf, messageOf, rewardStatusLabel,
  type Invitee, type Paged, type ReferralMe, type RewardItem,
} from '@spark/shared'
import { ElMessage } from 'element-plus'
import QRCode from 'qrcode'
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import { notifyError } from '../logic/notify'

const route = useRoute()
const router = useRouter()
const PAGE = 20

const me = ref<ReferralMe | null>(null)
const loadError = ref('')
/** 二维码用 <img>（data URL）而不是 <canvas>：手机上可以长按保存 / 识别 */
const qrUrl = ref('')
const active = computed(() => me.value?.status === 'active')

const rate = computed(() => (me.value ? me.value.rebate.rate_bp / 100 : 0))
const period = computed(() => {
  const d = me.value?.rebate.period_days ?? 0
  return d >= 30 ? `${Math.floor(d / 30)} 个月` : `${d} 天`
})

async function load() {
  try {
    const m = await api.get<ReferralMe>('/referral/me')
    me.value = m
    loadError.value = ''
    qrUrl.value = m.status === 'active' ? await QRCode.toDataURL(m.link, { width: 320, margin: 1 }) : ''
  } catch (e) {
    loadError.value = messageOf(e)
    notifyError(e)
  }
}

const tabs = ['invitees', 'rewards'] as const
type Tab = (typeof tabs)[number]
const tab = computed<Tab>({
  get: () => (tabs.includes(route.query.tab as Tab) ? (route.query.tab as Tab) : 'invitees'),
  set: (v) => void router.replace({ query: { ...route.query, tab: v } }),
})

// 两个列表各自分页；每次请求加一代，翻页过快时旧响应回来直接丢弃
const invitees = ref<Invitee[]>([])
const inviteesTotal = ref(0)
const inviteesPage = ref(1)
let inviteesGen = 0
async function loadInvitees() {
  const gen = ++inviteesGen
  try {
    const out = await api.get<Paged<Invitee>>('/referral/invitees', { page: inviteesPage.value, page_size: PAGE })
    if (gen !== inviteesGen) return
    invitees.value = out.items
    inviteesTotal.value = out.total
  } catch (e) {
    if (gen === inviteesGen) notifyError(e)
  }
}

const rewards = ref<RewardItem[]>([])
const rewardsTotal = ref(0)
const rewardsPage = ref(1)
let rewardsGen = 0
async function loadRewards() {
  const gen = ++rewardsGen
  try {
    const out = await api.get<Paged<RewardItem>>('/referral/rewards', { page: rewardsPage.value, page_size: PAGE })
    if (gen !== rewardsGen) return
    rewards.value = out.items
    rewardsTotal.value = out.total
  } catch (e) {
    if (gen === rewardsGen) notifyError(e)
  }
}

const loadTab = (t: Tab) => (t === 'rewards' ? loadRewards() : loadInvitees())

/** navigator.clipboard 只在安全上下文（https / localhost）里有；局域网 http 访问时退回 execCommand。 */
async function copy(text: string) {
  try {
    if (navigator.clipboard) await navigator.clipboard.writeText(text)
    else {
      const ta = document.createElement('textarea')
      ta.value = text
      ta.setAttribute('readonly', '')
      ta.style.position = 'fixed'
      ta.style.opacity = '0'
      document.body.appendChild(ta)
      ta.select()
      const ok = document.execCommand('copy')
      ta.remove()
      if (!ok) throw new Error('copy failed')
    }
    ElMessage.success('已复制')
  } catch {
    ElMessage.error('复制失败，请手动选择复制')
  }
}

onMounted(() => {
  void load()
  void loadTab(tab.value)
})
watch(tab, (t) => void loadTab(t))
watch(inviteesPage, () => void loadInvitees())
watch(rewardsPage, () => void loadRewards())
</script>

<template>
  <div v-if="me" class="invite">
    <el-alert v-if="!active" type="warning" :closable="false" show-icon title="邀请功能已暂停，如有疑问请联系客服" data-testid="invite-frozen-alert" />
    <el-alert v-else-if="!me.kyc_verified" type="info" :closable="false" show-icon data-testid="invite-kyc-alert">
      实名认证后才能收到返利 <router-link to="/kyc">去实名</router-link>
    </el-alert>

    <!-- 冻结时隐藏码、链接与二维码（接口仍返回码） -->
    <el-card v-if="active">
      <div class="code-row">
        <div>
          <div class="muted">我的邀请码</div>
          <div class="code" data-testid="invite-code">{{ me.code }}</div>
          <el-button size="small" data-testid="invite-copy-code" @click="copy(me.code)">复制邀请码</el-button>
        </div>
        <img v-if="qrUrl" :src="qrUrl" class="qr" alt="邀请二维码" width="160" height="160" data-testid="invite-qr" />
      </div>
      <div class="link-row">
        <span class="mono" data-testid="invite-link">{{ me.link }}</span>
        <el-button size="small" data-testid="invite-copy-link" @click="copy(me.link)">复制链接</el-button>
      </div>
      <p class="muted rules" data-testid="invite-rules">
        好友通过您的链接注册后 {{ period }}内，其每笔实付租用金额的 {{ rate }}% 返到您的余额，扣款 {{ me.rebate.hold_days }} 天后到账（预订单在预订开始后到账），返利不可提现。
        单个好友累计最多 {{ fmtYuan(me.rebate.max_per_invitee) }}，每 30 天最多 {{ fmtYuan(me.rebate.max_per_inviter_30d) }}；好友退款时按比例扣回。
      </p>
    </el-card>

    <el-card>
      <div class="stats">
        <div><span class="muted">已邀请</span><b data-testid="invite-stat-invitees">{{ me.stats.invitees }}</b></div>
        <div><span class="muted">已实名</span><b data-testid="invite-stat-verified">{{ me.stats.verified_invitees }}</b></div>
        <div><span class="muted">待发放</span><b data-testid="invite-stat-pending">{{ fmtYuan(me.stats.pending) }}</b></div>
        <div><span class="muted">累计净返利 *</span><b data-testid="invite-stat-settled">{{ fmtYuan(me.stats.settled_net) }}</b></div>
        <div><span class="muted">本月返利（已到账）</span><b data-testid="invite-stat-month">{{ fmtYuan(me.stats.month_settled) }}</b></div>
      </div>
      <p class="muted note" data-testid="invite-stat-note">
        * 累计净返利 = 已发放返利 − 退款扣回，含用于抵扣欠账、未进入余额的部分；本月返利只计本月实际进入余额的金额。
        <span v-if="me.stats.debt > 0" data-testid="invite-stat-debt">当前欠账 {{ fmtYuan(me.stats.debt) }}，将从之后的返利中抵扣。</span>
      </p>
    </el-card>

    <el-card>
      <el-tabs v-model="tab">
        <el-tab-pane name="invitees">
          <template #label><span data-testid="invite-tab-invitees">被邀请人</span></template>
          <div v-for="i in invitees" :key="i.phone_masked + i.bound_at" class="row-card" data-testid="invitee-row">
            <div class="row-head">
              <span data-testid="invitee-phone">{{ i.phone_masked }}</span>
              <span>
                <span data-testid="invitee-status">{{ labelOf(bindingStatusLabel, i.status) }}</span> ·
                <span data-testid="invitee-kyc">{{ i.kyc_verified ? '已实名' : '未实名' }}</span>
              </span>
            </div>
            <div class="muted">
              注册 {{ fmtTime(i.bound_at) }} · 累计贡献 <span data-testid="invitee-contributed">{{ fmtYuan(i.contributed) }}</span> ·
              返利截止 <span data-testid="invitee-until">{{ fmtDate(i.rebate_until) }}</span>
            </div>
          </div>
          <el-empty v-if="invitees.length === 0" description="还没有被邀请人" />
          <el-pagination v-if="inviteesTotal > PAGE" v-model:current-page="inviteesPage" :page-size="PAGE" :total="inviteesTotal" layout="prev, pager, next" data-testid="invite-invitees-pager" />
        </el-tab-pane>
        <el-tab-pane name="rewards">
          <template #label><span data-testid="invite-tab-rewards">返利明细</span></template>
          <div v-for="r in rewards" :key="r.order_id" class="row-card" data-testid="reward-row">
            <div class="row-head">
              <span><span data-testid="reward-invitee">{{ r.invitee_masked }}</span> · <span class="mono" data-testid="reward-order">{{ r.order_id }}</span></span>
              <b data-testid="reward-amount">{{ fmtYuan(r.reward) }}</b>
            </div>
            <div class="muted">
              消费 {{ fmtYuan(r.order_amount) }}<span v-if="r.refunded_amount > 0">（已退 {{ fmtYuan(r.refunded_amount) }}）</span> · {{ fmtTime(r.paid_at) }} ·
              <span data-testid="reward-status">{{ labelOf(rewardStatusLabel, r.status) }}</span>
              <span v-if="r.status === 'settled' && r.clawed_amount > 0" data-testid="reward-clawed">（已扣回 {{ fmtYuan(r.clawed_amount) }}）</span>
            </div>
          </div>
          <el-empty v-if="rewards.length === 0" description="暂无返利" />
          <el-pagination v-if="rewardsTotal > PAGE" v-model:current-page="rewardsPage" :page-size="PAGE" :total="rewardsTotal" layout="prev, pager, next" data-testid="invite-rewards-pager" />
        </el-tab-pane>
      </el-tabs>
    </el-card>
  </div>
  <el-card v-else-if="loadError" data-testid="invite-load-error">
    <el-alert type="error" :title="loadError" :closable="false" />
    <el-button class="retry" @click="load">重试</el-button>
  </el-card>
</template>

<style scoped>
.invite { display: grid; gap: 16px; max-width: 900px; }
.code-row { display: flex; justify-content: space-between; align-items: center; gap: 16px; flex-wrap: wrap; }
.code { font-size: 32px; font-weight: 700; letter-spacing: 4px; font-family: ui-monospace, SFMono-Regular, Menlo, monospace; margin: 4px 0 8px; }
.qr { width: 160px; height: 160px; }
.link-row { display: flex; gap: 8px; align-items: center; margin-top: 12px; flex-wrap: wrap; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; word-break: break-all; }
.muted { color: #909399; font-size: 13px; }
.rules { line-height: 1.7; margin: 12px 0 0; }
.note { margin: 12px 0 0; line-height: 1.6; }
.stats { display: grid; grid-template-columns: repeat(auto-fit, minmax(120px, 1fr)); gap: 12px; }
.stats div { display: flex; flex-direction: column; gap: 4px; }
.row-card { padding: 12px 0; border-bottom: 1px solid #ebeef5; }
.row-head { display: flex; justify-content: space-between; align-items: center; gap: 8px; flex-wrap: wrap; }
.retry { margin-top: 12px; }
</style>
