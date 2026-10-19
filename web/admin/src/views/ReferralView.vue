<script setup lang="ts">
import {
  bindingStatusLabel, centsToYuanInput, digitsOnly, fmtTime, fmtYuan, isApiError, labelOf, messageOf, rewardStatusLabel,
  type AdminBinding, type AdminInviterDetail, type AdminInviterRow, type AdminRateVersion, type AdminReferralReward, type AdminReferralSummary, type Paged,
} from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, reactive, ref, watch, type Directive } from 'vue'
import { onBeforeRouteUpdate, useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import DateRange from '../components/DateRange.vue'
import IdLink from '../components/IdLink.vue'
import { usePaged } from '../composables/usePaged'
import { downloadBlob } from '../logic/csv'
import { notifyError } from '../logic/notify'
import { accountStatusLabel, eventKindLabel, flagLabel, fmtRate, maskPhone, parseRateForm, parseRescanDays, rescanMessage, riskFlagLabel, sortOptions } from '../logic/referral'
import { session } from '../store/session'

// 邀请返利（计划 8）。finance 与 super 可进；配置与重扫只给 super（按钮与 Tab 都不渲染，后端同样拒绝）。
// 撤销、改配置、重扫需要动态码：后端返回 totp_required 时由请求层弹框并带 totp 重发（取消静默，totp_used 带提示重新弹框）。
// 手机号一律脱敏显示；绑定的 IP / 设备 / UA 只在导出 CSV 中供核查，页面不展示，也从不显示实名指纹。
const route = useRoute()
const router = useRouter()
const isSuper = computed(() => session.admin?.role === 'super')

const tabs = ['overview', 'held', 'inviters', 'rewards', 'config'] as const
type Tab = (typeof tabs)[number]
const tab = computed<Tab>({
  get: () => {
    const t = route.query.tab as Tab
    return tabs.includes(t) && (t !== 'config' || isSuper.value) ? t : 'overview'
  },
  set: (v) => void router.replace({ query: { ...route.query, tab: v } }),
})
const flagOptions = Object.entries(riskFlagLabel).map(([value, label]) => ({ value, label }))
const accountStatusOptions = Object.entries(accountStatusLabel).map(([value, label]) => ({ value, label }))
const rewardStatusOptions = Object.entries(rewardStatusLabel).map(([value, label]) => ({ value, label }))

// ---- 概览 ----
const summary = ref<AdminReferralSummary | null>(null)
const summaryError = ref('')
const summaryLoading = ref(false)
let summarySeq = 0
async function loadSummary() {
  const mine = ++summarySeq
  summaryLoading.value = true
  try {
    const out = await api.get<AdminReferralSummary>('/referral/summary')
    if (mine !== summarySeq) return
    summary.value = out
    summaryError.value = ''
  } catch (e) {
    if (mine !== summarySeq) return
    notifyError(e)
    if (!summary.value) summaryError.value = messageOf(e)
  } finally {
    if (mine === summarySeq) summaryLoading.value = false
  }
}

// ---- 待审核（held 队列：标记多的在前、同数量先到先审） ----
const held = usePaged<AdminBinding>((page, size) => api.get<Paged<AdminBinding>>('/referral/bindings', { status: 'held', page, page_size: size }))
const approveDlg = reactive({ visible: false, target: null as AdminBinding | null, note: '', kycAck: false, busy: false })
function openApprove(b: AdminBinding) {
  Object.assign(approveDlg, { visible: true, target: b, note: '', kycAck: false })
}
/** 有未解决的实名核查失败时必须勾选「已人工核查」才能通过 */
const approveBlocked = computed(() => !!approveDlg.target?.kyc_check_unresolved && !approveDlg.kycAck)
async function submitApprove() {
  const b = approveDlg.target
  if (approveDlg.busy || !b || approveBlocked.value) return // 同步守卫：连点只发一次
  approveDlg.busy = true
  try {
    await api.post(`/referral/bindings/${b.id}/approve`, { note: approveDlg.note.trim() })
    ElMessage.success('已通过')
    approveDlg.visible = false
    await Promise.all([held.load(), loadSummary()])
  } catch (e) {
    notifyError(e) // 弹窗保留，输入不丢
  } finally {
    approveDlg.busy = false
  }
}
const revokeDlg = reactive({ visible: false, target: null as AdminBinding | null, reason: '', error: '', busy: false })
function openRevoke(b: AdminBinding) {
  Object.assign(revokeDlg, { visible: true, target: b, reason: '', error: '' })
}
async function submitRevoke() {
  const b = revokeDlg.target
  if (revokeDlg.busy || !b) return // 同步守卫：撤销会作废待发放返利
  revokeDlg.error = revokeDlg.reason.trim() ? '' : '请填写撤销原因'
  if (revokeDlg.error) return
  revokeDlg.busy = true
  try {
    const out = await api.post<{ voided_amount: number }>(`/referral/bindings/${b.id}/revoke`, { reason: revokeDlg.reason.trim() })
    ElMessage.success(`已撤销，作废待发放 ${fmtYuan(out.voided_amount)}`)
    revokeDlg.visible = false
    await Promise.all([held.load(), loadSummary()])
  } catch (e) {
    notifyError(e) // 动态码取消静默；弹窗保留
  } finally {
    revokeDlg.busy = false
  }
}

// ---- 邀请人 ----
const invFilter = reactive({ q: digitsOnly(String(route.query.q ?? '')), status: '', flag: '', sort: 'settled' })
// 已生效的筛选：翻页与导出都用它（表单改了没点查询时仍与屏幕上的表格一致）
let invApplied = { ...invFilter }
const inviters = usePaged<AdminInviterRow>((page, size) => api.get<Paged<AdminInviterRow>>('/referral/inviters', { ...invApplied, page, page_size: size }))
function invSearch() {
  invApplied = { ...invFilter }
  inviters.reset()
}
// 从用户详情的链接（?tab=inviters&q=ID）进入已打开的本页：按新的 q 重新预填
onBeforeRouteUpdate((to) => {
  const q = digitsOnly(String(to.query.q ?? ''))
  if (q === invApplied.q) return
  invFilter.q = q
  invApplied = { ...invFilter }
  if (to.query.tab === 'inviters' && route.query.tab === 'inviters') inviters.reset()
})
const invExporting = ref(false)
async function exportBindings() {
  if (invExporting.value) return
  invExporting.value = true
  try {
    const { blob, filename } = await api.download('/referral/bindings/export', { flag: invApplied.flag })
    downloadBlob(filename, blob)
  } catch (e) {
    notifyError(e) // export_too_large 等原文
  } finally {
    invExporting.value = false
  }
}

const drawer = reactive({ visible: false, userId: 0, data: null as AdminInviterDetail | null, loading: false })
let drawerSeq = 0
async function openDrawer(userId: number) {
  const mine = ++drawerSeq
  if (drawer.userId !== userId) drawer.data = null
  Object.assign(drawer, { visible: true, userId, loading: true })
  try {
    const d = await api.get<AdminInviterDetail>(`/referral/inviters/${userId}`)
    if (mine === drawerSeq) drawer.data = d
  } catch (e) {
    if (mine === drawerSeq) notifyError(e)
  } finally {
    if (mine === drawerSeq) drawer.loading = false
  }
}
const freezeDlg = reactive({ visible: false, reason: '', error: '', busy: false })
function openFreeze() {
  Object.assign(freezeDlg, { visible: true, reason: '', error: '' })
}
async function submitFreeze() {
  if (freezeDlg.busy || !drawer.userId) return
  freezeDlg.error = freezeDlg.reason.trim() ? '' : '请填写冻结原因'
  if (freezeDlg.error) return
  freezeDlg.busy = true
  const userId = drawer.userId
  try {
    await api.post(`/referral/inviters/${userId}/freeze`, { reason: freezeDlg.reason.trim() })
    ElMessage.success('已冻结')
    freezeDlg.visible = false
    await Promise.all([openDrawer(userId), inviters.load()])
  } catch (e) {
    notifyError(e)
  } finally {
    freezeDlg.busy = false
  }
}
const unfreezing = ref(false)
async function unfreeze() {
  if (unfreezing.value || !drawer.userId) return
  unfreezing.value = true
  const userId = drawer.userId
  try {
    await api.post(`/referral/inviters/${userId}/unfreeze`, {})
    ElMessage.success('已解冻')
    await Promise.all([openDrawer(userId), inviters.load()])
  } catch (e) {
    notifyError(e)
  } finally {
    unfreezing.value = false
  }
}

// ---- 返利明细 ----
const rwFilter = reactive({ inviter_id: '', invitee_id: '', status: '', range: { from: '', to: '' } })
// 空值由 buildQuery 省略；日期按 YYYY-MM-DD 原样发出（后端按北京时间解释，to 含当天；按 paid_at 筛选）
const rwQuery = () => ({ inviter_id: rwFilter.inviter_id, invitee_id: rwFilter.invitee_id, status: rwFilter.status, from: rwFilter.range.from, to: rwFilter.range.to })
let rwApplied = rwQuery()
const rewards = usePaged<AdminReferralReward>((page, size) => api.get<Paged<AdminReferralReward>>('/referral/rewards', { ...rwApplied, page, page_size: size }))
function rwSearch() {
  rwApplied = rwQuery()
  rewards.reset()
}
function rwReset() {
  Object.assign(rwFilter, { inviter_id: '', invitee_id: '', status: '', range: { from: '', to: '' } })
  rwSearch()
}
const rwExporting = ref(false)
async function exportRewards() {
  if (rwExporting.value) return
  rwExporting.value = true
  try {
    const { blob, filename } = await api.download('/referral/rewards/export', rwApplied)
    downloadBlob(filename, blob)
  } catch (e) {
    notifyError(e)
  } finally {
    rwExporting.value = false
  }
}

// ---- 配置（super）----
const config = ref<{ current: AdminRateVersion; versions: AdminRateVersion[] } | null>(null)
const configError = ref('')
let configSeq = 0
let prefilled = false
const cfg = reactive({ rate: '', invitee: '', inviter: '', hold: '', period: '', effective: null as Date | null, note: '', error: '', busy: false })
async function loadConfig() {
  const mine = ++configSeq
  try {
    const out = await api.get<{ current: AdminRateVersion; versions: AdminRateVersion[] }>('/referral/config')
    if (mine !== configSeq) return
    config.value = out
    configError.value = ''
    if (!prefilled) {
      // 表单以当前版本预填一次（之后保留管理员的输入）
      prefilled = true
      const c = out.current
      Object.assign(cfg, {
        rate: fmtRate(c.rate_bp).slice(0, -1),
        invitee: centsToYuanInput(c.max_per_invitee),
        inviter: centsToYuanInput(c.max_per_inviter_30d),
        hold: String(c.hold_days),
        period: String(c.period_days),
      })
    }
  } catch (e) {
    if (mine !== configSeq) return
    notifyError(e)
    if (!config.value) configError.value = messageOf(e)
  }
}
async function submitConfig() {
  if (cfg.busy) return // 同步守卫
  const v = parseRateForm(cfg)
  if ('error' in v) {
    cfg.error = v.error
    return
  }
  cfg.error = ''
  cfg.busy = true
  try {
    await api.post<AdminRateVersion>('/referral/config', v.body)
    ElMessage.success('已新增版本')
    await loadConfig()
  } catch (e) {
    notifyError(e) // referral_config_invalid（如「生效时间不能早于当前时间」）按原文提示
  } finally {
    cfg.busy = false
  }
}
const rescan = reactive({ days: '3', error: '', busy: false })
async function submitRescan() {
  if (rescan.busy) return
  const days = parseRescanDays(rescan.days)
  rescan.error = days == null ? '天数需为 1～30 的整数' : ''
  if (days == null) return
  rescan.busy = true
  try {
    const m = rescanMessage(await api.post<{ failed?: number }>('/referral/rescan', { days }))
    ElMessage({ type: m.type, message: m.text })
  } catch (e) {
    // 旧接口在重扫已生效、但仍有行处理失败时返回 5xx：不当作「没执行」提示
    if (isApiError(e) && e.status >= 500) ElMessage.warning(`重扫可能未全部完成：${messageOf(e)}`)
    else notifyError(e)
  } finally {
    rescan.busy = false
  }
}
/** el-date-picker 不把 data-testid 透传到输入框：挂在外层后转给内部 input */
const vInnerTestid: Directive<HTMLElement, string> = {
  mounted: (el, b) => el.querySelector('input')?.setAttribute('data-testid', b.value),
}

// 切到哪个 Tab 就（重新）加载哪个
watch(
  tab,
  (t) => {
    if (t === 'overview') void loadSummary()
    else if (t === 'held') void held.load()
    else if (t === 'inviters') void inviters.load()
    else if (t === 'rewards') void rewards.load()
    else void loadConfig()
  },
  { immediate: true },
)

const days = (n: number) => `${n} 天`
</script>

<template>
  <el-card>
    <el-tabs v-model="tab">
      <!-- 概览 -->
      <el-tab-pane label="概览" name="overview" data-testid="ref-tab-overview">
        <el-alert v-if="summaryError && !summary" type="error" :title="summaryError" show-icon :closable="false" data-testid="ref-sum-error">
          <el-button size="small" :loading="summaryLoading" data-testid="ref-sum-retry" @click="loadSummary()">重试</el-button>
        </el-alert>
        <div v-if="summary" class="stats">
          <div class="stat"><span>待发放</span><b data-testid="ref-sum-pending">{{ fmtYuan(summary.pending) }}</b></div>
          <div class="stat"><span>本月已发放</span><b data-testid="ref-sum-month">{{ fmtYuan(summary.month_settled) }}</b></div>
          <div class="stat"><span>累计净发放（发放 − 扣回）</span><b data-testid="ref-sum-net">{{ fmtYuan(summary.settled_net) }}</b></div>
          <div class="stat"><span>欠账合计</span><b data-testid="ref-sum-debt">{{ fmtYuan(summary.debt) }}</b></div>
          <div class="stat clickable" @click="tab = 'held'"><span>待审核绑定</span><b data-testid="ref-sum-held">{{ summary.held_count }}</b></div>
        </div>
      </el-tab-pane>

      <!-- 待审核 -->
      <el-tab-pane label="待审核" name="held" data-testid="ref-tab-held">
        <el-alert v-if="held.error.value" type="error" :title="held.error.value" show-icon :closable="false" class="error-bar" data-testid="held-error">
          <el-button size="small" :loading="held.loading.value" data-testid="held-retry" @click="held.load()">重试</el-button>
        </el-alert>
        <div v-loading="held.loading.value">
          <table class="grid">
            <thead><tr><th>邀请人</th><th>被邀请人</th><th>标记</th><th>绑定时间</th><th>待发放</th><th>操作</th></tr></thead>
            <tbody>
              <tr v-for="b in held.items.value" :key="b.id" data-testid="held-row">
                <!-- ID 与脱敏手机号分开放：testid 元素里只有 ID，按 ID 过滤行时不会被手机号里的数字误匹配 -->
                <td><IdLink kind="user" :id="b.inviter_id" data-testid="held-inviter" /> <span class="muted mono" data-testid="held-inviter-phone">{{ maskPhone(b.inviter_phone) }}</span></td>
                <td><IdLink kind="user" :id="b.invitee_id" data-testid="held-invitee" /> <span class="muted mono" data-testid="held-invitee-phone">{{ maskPhone(b.invitee_phone) }}</span></td>
                <td data-testid="held-flags">
                  <el-tag v-for="f in b.risk_flags" :key="f" type="warning" size="small" class="tag">{{ flagLabel(f) }}</el-tag>
                  <el-tag v-if="b.kyc_check_unresolved" type="danger" effect="dark" size="small" class="tag" data-testid="held-kyc-unresolved">实名核查未解决</el-tag>
                </td>
                <td data-testid="held-bound">{{ fmtTime(b.bound_at) }}</td>
                <td data-testid="held-pending">{{ fmtYuan(b.pending) }}</td>
                <td class="ops">
                  <el-button size="small" type="primary" data-testid="held-approve" @click="openApprove(b)">通过</el-button>
                  <el-button size="small" type="danger" plain data-testid="held-revoke" @click="openRevoke(b)">撤销</el-button>
                </td>
              </tr>
              <tr v-if="!held.items.value.length && !held.loading.value"><td colspan="6" class="muted empty" data-testid="held-empty">没有待审核的绑定</td></tr>
            </tbody>
          </table>
        </div>
        <el-pagination v-model:current-page="held.page.value" v-model:page-size="held.size.value" :total="held.total.value" layout="total, prev, pager, next" class="pager" data-testid="held-pager" />
      </el-tab-pane>

      <!-- 邀请人 -->
      <el-tab-pane label="邀请人" name="inviters" data-testid="ref-tab-inviters">
        <el-form inline class="filters" @submit.prevent="invSearch">
          <el-form-item label="手机号 / ID">
            <el-input :model-value="invFilter.q" clearable placeholder="手机尾号或用户 ID" data-testid="inv-q" style="width: 170px" @update:model-value="(v: string) => (invFilter.q = digitsOnly(v))" />
          </el-form-item>
          <el-form-item label="状态">
            <el-select v-model="invFilter.status" clearable placeholder="全部" data-testid="inv-status" style="width: 110px" @change="invSearch">
              <el-option v-for="o in accountStatusOptions" :key="o.value" :label="o.label" :value="o.value" />
            </el-select>
          </el-form-item>
          <el-form-item label="标记">
            <el-select v-model="invFilter.flag" clearable placeholder="全部" data-testid="inv-flag" style="width: 110px" @change="invSearch">
              <el-option v-for="o in flagOptions" :key="o.value" :label="o.label" :value="o.value" />
            </el-select>
          </el-form-item>
          <el-form-item label="排序">
            <el-select v-model="invFilter.sort" data-testid="inv-sort" style="width: 130px" @change="invSearch">
              <el-option v-for="o in sortOptions" :key="o.value" :label="o.label" :value="o.value" />
            </el-select>
          </el-form-item>
          <el-button type="primary" native-type="submit" data-testid="inv-search">查询</el-button>
          <span class="spacer" />
          <el-button :loading="invExporting" data-testid="inv-export" @click="exportBindings">导出绑定 CSV</el-button>
        </el-form>
        <el-alert v-if="inviters.error.value" type="error" :title="inviters.error.value" show-icon :closable="false" class="error-bar" data-testid="inv-error">
          <el-button size="small" :loading="inviters.loading.value" data-testid="inv-retry" @click="inviters.load()">重试</el-button>
        </el-alert>
        <div v-loading="inviters.loading.value">
          <table class="grid">
            <thead><tr><th>邀请人</th><th>邀请码</th><th>状态</th><th>被邀请人</th><th>待审核</th><th>待发放</th><th>累计净发放</th><th>欠账</th></tr></thead>
            <tbody>
              <!-- 整行点击打开详情抽屉（行内不放链接，避免误跳转；抽屉里有用户详情链接） -->
              <tr v-for="r in inviters.items.value" :key="r.user_id" class="clickable" data-testid="inv-row" @click="openDrawer(r.user_id)">
                <td><span class="mono" data-testid="inv-user">{{ r.user_id }}</span> <span class="muted mono" data-testid="inv-phone">{{ maskPhone(r.phone) }}</span></td>
                <td class="mono" data-testid="inv-code">{{ r.code }}</td>
                <td><el-tag :type="r.status === 'frozen' ? 'danger' : 'success'" size="small" data-testid="inv-status">{{ labelOf(accountStatusLabel, r.status) }}</el-tag></td>
                <td data-testid="inv-invitees">{{ r.invitees }}</td>
                <td data-testid="inv-held">{{ r.held_count }}</td>
                <td data-testid="inv-pending">{{ fmtYuan(r.pending) }}</td>
                <td data-testid="inv-net">{{ fmtYuan(r.settled_net) }}</td>
                <td data-testid="inv-debt" :class="{ 'money-minus': r.debt > 0 }">{{ fmtYuan(r.debt) }}</td>
              </tr>
              <tr v-if="!inviters.items.value.length && !inviters.loading.value"><td colspan="8" class="muted empty">没有邀请人</td></tr>
            </tbody>
          </table>
        </div>
        <el-pagination v-model:current-page="inviters.page.value" v-model:page-size="inviters.size.value" :total="inviters.total.value" layout="total, prev, pager, next" class="pager" data-testid="inv-pager" />
      </el-tab-pane>

      <!-- 返利明细 -->
      <el-tab-pane label="返利明细" name="rewards" data-testid="ref-tab-rewards">
        <el-form inline class="filters" @submit.prevent="rwSearch">
          <el-form-item label="邀请人 ID">
            <el-input :model-value="rwFilter.inviter_id" clearable data-testid="rw-inviter" style="width: 110px" @update:model-value="(v: string) => (rwFilter.inviter_id = digitsOnly(v))" />
          </el-form-item>
          <el-form-item label="被邀请人 ID">
            <el-input :model-value="rwFilter.invitee_id" clearable data-testid="rw-invitee" style="width: 110px" @update:model-value="(v: string) => (rwFilter.invitee_id = digitsOnly(v))" />
          </el-form-item>
          <el-form-item label="状态">
            <el-select v-model="rwFilter.status" clearable placeholder="全部" data-testid="rw-status" style="width: 110px" @change="rwSearch">
              <el-option v-for="o in rewardStatusOptions" :key="o.value" :label="o.label" :value="o.value" />
            </el-select>
          </el-form-item>
          <el-form-item label="消费日期"><DateRange v-model="rwFilter.range" prefix="rw-range" /></el-form-item>
          <el-button type="primary" native-type="submit" data-testid="rw-search">查询</el-button>
          <el-button data-testid="rw-reset" @click="rwReset">重置</el-button>
          <span class="spacer" />
          <el-button :loading="rwExporting" data-testid="rw-export" @click="exportRewards">导出 CSV</el-button>
        </el-form>
        <el-alert v-if="rewards.error.value" type="error" :title="rewards.error.value" show-icon :closable="false" class="error-bar" data-testid="rw-error">
          <el-button size="small" :loading="rewards.loading.value" data-testid="rw-retry" @click="rewards.load()">重试</el-button>
        </el-alert>
        <div v-loading="rewards.loading.value">
          <table class="grid">
            <thead><tr><th>订单号</th><th>邀请人</th><th>被邀请人</th><th>消费</th><th>退款</th><th>费率</th><th>返利</th><th>状态</th><th>发放时间</th><th>已扣回</th></tr></thead>
            <tbody>
              <tr v-for="r in rewards.items.value" :key="r.order_id" data-testid="rw-row">
                <td class="mono" data-testid="rw-order">{{ r.order_id }}</td>
                <td><IdLink kind="user" :id="r.inviter_id" data-testid="rw-inviter-id" /> <span class="muted mono" data-testid="rw-inviter-phone">{{ maskPhone(r.inviter_phone) }}</span></td>
                <td><IdLink kind="user" :id="r.invitee_id" data-testid="rw-invitee-id" /> <span class="muted mono" data-testid="rw-invitee-phone">{{ maskPhone(r.invitee_phone) }}</span></td>
                <td data-testid="rw-amount">{{ fmtYuan(r.order_amount) }}</td>
                <td data-testid="rw-refunded">{{ fmtYuan(r.refunded_amount) }}</td>
                <td data-testid="rw-rate">{{ fmtRate(r.rate_bp) }}</td>
                <td data-testid="rw-reward">{{ fmtYuan(r.reward) }}</td>
                <td data-testid="rw-status">{{ labelOf(rewardStatusLabel, r.status) }}</td>
                <td data-testid="rw-settled-at">{{ fmtTime(r.settled_at) || '—' }}</td>
                <td data-testid="rw-clawed" :class="{ 'money-minus': r.clawed_amount > 0 }">{{ fmtYuan(r.clawed_amount) }}</td>
              </tr>
              <tr v-if="!rewards.items.value.length && !rewards.loading.value"><td colspan="10" class="muted empty">没有返利记录</td></tr>
            </tbody>
          </table>
        </div>
        <el-pagination v-model:current-page="rewards.page.value" v-model:page-size="rewards.size.value" :total="rewards.total.value" layout="total, prev, pager, next" class="pager" data-testid="rw-pager" />
      </el-tab-pane>

      <!-- 配置（super） -->
      <el-tab-pane v-if="isSuper" label="配置" name="config" data-testid="ref-tab-config">
        <el-alert v-if="configError && !config" type="error" :title="configError" show-icon :closable="false" data-testid="cfg-error-load">
          <el-button size="small" data-testid="cfg-retry" @click="loadConfig()">重试</el-button>
        </el-alert>
        <template v-if="config">
          <h4>当前生效</h4>
          <el-descriptions :column="3" border>
            <el-descriptions-item label="返利比例"><span data-testid="cfg-current-rate">{{ fmtRate(config.current.rate_bp) }}</span></el-descriptions-item>
            <el-descriptions-item label="单个被邀请人上限"><span data-testid="cfg-current-invitee">{{ fmtYuan(config.current.max_per_invitee) }}</span></el-descriptions-item>
            <el-descriptions-item label="单个邀请人 30 天上限"><span data-testid="cfg-current-inviter">{{ fmtYuan(config.current.max_per_inviter_30d) }}</span></el-descriptions-item>
            <el-descriptions-item label="发放延迟"><span data-testid="cfg-current-hold">{{ days(config.current.hold_days) }}</span></el-descriptions-item>
            <el-descriptions-item label="返利期"><span data-testid="cfg-current-period">{{ days(config.current.period_days) }}</span></el-descriptions-item>
            <el-descriptions-item label="生效时间"><span data-testid="cfg-current-effective">{{ fmtTime(config.current.effective_from) }}</span></el-descriptions-item>
          </el-descriptions>

          <h4>历史版本</h4>
          <table class="grid">
            <thead><tr><th>版本</th><th>比例</th><th>被邀请人上限</th><th>邀请人 30 天上限</th><th>延迟</th><th>返利期</th><th>生效时间</th><th>备注</th><th>创建人</th><th>创建时间</th></tr></thead>
            <tbody>
              <tr v-for="v in config.versions" :key="v.id" data-testid="cfg-version-row">
                <td class="mono">{{ v.id }}<el-tag v-if="v.id === config.current.id" size="small" type="success" class="tag">当前</el-tag></td>
                <td>{{ fmtRate(v.rate_bp) }}</td>
                <td>{{ fmtYuan(v.max_per_invitee) }}</td>
                <td>{{ fmtYuan(v.max_per_inviter_30d) }}</td>
                <td>{{ days(v.hold_days) }}</td>
                <td>{{ days(v.period_days) }}</td>
                <td>{{ fmtTime(v.effective_from) }}</td>
                <td>{{ v.note }}</td>
                <td>{{ v.created_by ?? '系统' }}</td>
                <td>{{ fmtTime(v.created_at) }}</td>
              </tr>
            </tbody>
          </table>
        </template>

        <h4>新增版本</h4>
        <p class="muted">新版本只影响生效时间之后支付的订单，不能追溯；保存需要输入动态码。</p>
        <el-form label-width="200px" class="cfg-form" @submit.prevent="submitConfig">
          <el-form-item label="返利比例（%）"><el-input v-model="cfg.rate" inputmode="decimal" placeholder="如 10 或 12.5" data-testid="cfg-rate" style="width: 200px" /></el-form-item>
          <el-form-item label="单个被邀请人上限（元）"><el-input v-model="cfg.invitee" inputmode="decimal" data-testid="cfg-invitee" style="width: 200px" /></el-form-item>
          <el-form-item label="单个邀请人 30 天上限（元）"><el-input v-model="cfg.inviter" inputmode="decimal" data-testid="cfg-inviter" style="width: 200px" /></el-form-item>
          <el-form-item label="发放延迟（天）"><el-input v-model="cfg.hold" inputmode="numeric" data-testid="cfg-hold" style="width: 200px" /></el-form-item>
          <el-form-item label="返利期（天）"><el-input v-model="cfg.period" inputmode="numeric" data-testid="cfg-period" style="width: 200px" /></el-form-item>
          <el-form-item label="生效时间">
            <span v-inner-testid="'cfg-effective'">
              <el-date-picker v-model="cfg.effective" type="datetime" format="YYYY-MM-DD HH:mm:ss" placeholder="留空即立即生效" clearable />
            </span>
          </el-form-item>
          <el-form-item label="备注"><el-input v-model="cfg.note" data-testid="cfg-note" style="width: 360px" /></el-form-item>
          <el-form-item>
            <p v-if="cfg.error" class="error" data-testid="cfg-error">{{ cfg.error }}</p>
            <el-button type="primary" native-type="submit" :loading="cfg.busy" data-testid="cfg-submit">新增版本</el-button>
          </el-form-item>
        </el-form>

        <h4>重扫</h4>
        <p class="muted">把扫描水位拨回最近 N 天并立即跑一轮计提与发放，用于补处理漏掉的订单（已处理的订单不会重复计提）。需要输入动态码。</p>
        <el-form inline @submit.prevent="submitRescan">
          <el-form-item label="天数（1～30）"><el-input v-model="rescan.days" inputmode="numeric" data-testid="cfg-rescan-days" style="width: 100px" /></el-form-item>
          <el-button native-type="submit" :loading="rescan.busy" data-testid="cfg-rescan">重扫</el-button>
          <span v-if="rescan.error" class="error inline" data-testid="cfg-rescan-error">{{ rescan.error }}</span>
        </el-form>
      </el-tab-pane>
    </el-tabs>

    <!-- 通过 -->
    <el-dialog v-model="approveDlg.visible" title="通过绑定" width="480" align-center data-testid="held-approve-dialog">
      <template v-if="approveDlg.target">
        <p>通过后绑定恢复有效，待发放的 {{ fmtYuan(approveDlg.target.pending) }} 到期后照常发放。</p>
        <el-alert v-if="approveDlg.target.kyc_check_unresolved" type="error" :closable="false" show-icon title="实名核查未解决" class="kyc-warn" data-testid="held-approve-kyc-warning">
          系统未能完成邀请人与被邀请人的实名同身份判定。请先在两人的用户详情核对实名信息，确认不是同一人后再通过。
        </el-alert>
        <el-checkbox v-if="approveDlg.target.kyc_check_unresolved" v-model="approveDlg.kycAck" data-testid="held-approve-kyc-ack">我已人工核查，确认不是同一身份</el-checkbox>
        <el-form label-position="top" @submit.prevent="submitApprove">
          <el-form-item label="备注（选填）"><el-input v-model="approveDlg.note" data-testid="held-approve-note" /></el-form-item>
        </el-form>
      </template>
      <template #footer>
        <el-button data-testid="held-approve-cancel" @click="approveDlg.visible = false">取消</el-button>
        <el-button type="primary" :loading="approveDlg.busy" :disabled="approveBlocked" data-testid="held-approve-ok" @click="submitApprove">通过</el-button>
      </template>
    </el-dialog>

    <!-- 撤销 -->
    <el-dialog v-model="revokeDlg.visible" title="撤销绑定" width="480" align-center data-testid="held-revoke-dialog">
      <p v-if="revokeDlg.target">撤销后该绑定的待发放返利 {{ fmtYuan(revokeDlg.target.pending) }} 全部作废，已发放的不自动扣回。需要输入动态码。</p>
      <el-form label-position="top" @submit.prevent="submitRevoke">
        <el-form-item label="原因（必填）"><el-input v-model="revokeDlg.reason" data-testid="held-revoke-reason" /></el-form-item>
        <p v-if="revokeDlg.error" class="error" data-testid="held-revoke-error">{{ revokeDlg.error }}</p>
      </el-form>
      <template #footer>
        <el-button data-testid="held-revoke-cancel" @click="revokeDlg.visible = false">取消</el-button>
        <el-button type="danger" :loading="revokeDlg.busy" data-testid="held-revoke-ok" @click="submitRevoke">撤销</el-button>
      </template>
    </el-dialog>

    <!-- 邀请人详情 -->
    <el-drawer v-model="drawer.visible" :title="`邀请人 ${drawer.userId}`" size="860px" data-testid="inv-drawer">
      <div v-loading="drawer.loading">
        <template v-if="drawer.data">
          <el-descriptions :column="3" border>
            <el-descriptions-item label="用户"><IdLink kind="user" :id="drawer.data.account.user_id" data-testid="inv-d-user" /> <span class="muted mono">{{ maskPhone(drawer.data.phone) }}</span></el-descriptions-item>
            <el-descriptions-item label="邀请码"><span class="mono" data-testid="inv-d-code">{{ drawer.data.account.code }}</span></el-descriptions-item>
            <el-descriptions-item label="状态">
              <el-tag :type="drawer.data.account.status === 'frozen' ? 'danger' : 'success'" size="small" data-testid="inv-d-status">{{ labelOf(accountStatusLabel, drawer.data.account.status) }}</el-tag>
              <span v-if="drawer.data.account.frozen_reason" class="muted" data-testid="inv-d-frozen-reason">（{{ drawer.data.account.frozen_reason }}）</span>
            </el-descriptions-item>
            <el-descriptions-item label="欠账"><span data-testid="inv-d-debt">{{ fmtYuan(drawer.data.account.debt) }}</span></el-descriptions-item>
          </el-descriptions>
          <div class="actions">
            <el-button v-if="drawer.data.account.status === 'active'" type="danger" plain data-testid="inv-freeze" @click="openFreeze">冻结</el-button>
            <el-button v-else :loading="unfreezing" data-testid="inv-unfreeze" @click="unfreeze">解冻</el-button>
            <span class="muted">冻结后邀请码不能再绑定新用户，返利继续计提但暂停发放；解冻后恢复。</span>
          </div>

          <h4>被邀请人（最近 100 个）</h4>
          <table class="grid">
            <thead><tr><th>被邀请人</th><th>状态</th><th>标记</th><th>绑定时间</th><th>返利截止</th><th>累计贡献</th><th>待发放</th></tr></thead>
            <tbody>
              <tr v-for="b in drawer.data.bindings" :key="b.id" data-testid="inv-binding-row">
                <td><IdLink kind="user" :id="b.invitee_id" /> <span class="muted mono">{{ maskPhone(b.invitee_phone) }}</span></td>
                <td>{{ labelOf(bindingStatusLabel, b.status) }}</td>
                <td>
                  <el-tag v-for="f in b.risk_flags" :key="f" type="warning" size="small" class="tag">{{ flagLabel(f) }}</el-tag>
                  <el-tag v-if="b.kyc_check_unresolved" type="danger" size="small" class="tag">实名核查未解决</el-tag>
                </td>
                <td>{{ fmtTime(b.bound_at) }}</td>
                <td>{{ fmtTime(b.rebate_until) }}</td>
                <td>{{ fmtYuan(b.contributed) }}</td>
                <td>{{ fmtYuan(b.pending) }}</td>
              </tr>
              <tr v-if="!drawer.data.bindings.length"><td colspan="7" class="muted empty">无</td></tr>
            </tbody>
          </table>

          <h4>返利（最近 100 条）</h4>
          <table class="grid">
            <thead><tr><th>订单号</th><th>被邀请人</th><th>消费</th><th>返利</th><th>状态</th><th>发放时间</th><th>已扣回</th></tr></thead>
            <tbody>
              <tr v-for="r in drawer.data.rewards" :key="r.order_id" data-testid="inv-reward-row">
                <td class="mono">{{ r.order_id }}</td>
                <td><span class="mono">{{ r.invitee_id }}</span> <span class="muted mono">{{ maskPhone(r.invitee_phone) }}</span></td>
                <td>{{ fmtYuan(r.order_amount) }}</td>
                <td>{{ fmtYuan(r.reward) }}</td>
                <td>{{ labelOf(rewardStatusLabel, r.status) }}</td>
                <td>{{ fmtTime(r.settled_at) || '—' }}</td>
                <td>{{ fmtYuan(r.clawed_amount) }}</td>
              </tr>
              <tr v-if="!drawer.data.rewards.length"><td colspan="7" class="muted empty">无</td></tr>
            </tbody>
          </table>

          <h4>事件（最近 50 条）</h4>
          <table class="grid">
            <thead><tr><th>时间</th><th>类型</th><th>订单号</th><th>金额</th><th>说明</th></tr></thead>
            <tbody>
              <tr v-for="e in drawer.data.events" :key="e.id" data-testid="inv-event-row">
                <td>{{ fmtTime(e.created_at) }}</td>
                <td>{{ labelOf(eventKindLabel, e.kind) }}</td>
                <td class="mono">{{ e.order_id }}</td>
                <td>{{ fmtYuan(e.amount) }}</td>
                <td>{{ e.detail }}</td>
              </tr>
              <tr v-if="!drawer.data.events.length"><td colspan="5" class="muted empty">无</td></tr>
            </tbody>
          </table>
        </template>
      </div>
    </el-drawer>

    <el-dialog v-model="freezeDlg.visible" title="冻结邀请人" width="420" align-center append-to-body data-testid="inv-freeze-dialog">
      <el-form label-position="top" @submit.prevent="submitFreeze">
        <el-form-item label="原因（必填）"><el-input v-model="freezeDlg.reason" data-testid="inv-freeze-reason" /></el-form-item>
        <p v-if="freezeDlg.error" class="error" data-testid="inv-freeze-error">{{ freezeDlg.error }}</p>
      </el-form>
      <template #footer>
        <el-button data-testid="inv-freeze-cancel" @click="freezeDlg.visible = false">取消</el-button>
        <el-button type="danger" :loading="freezeDlg.busy" data-testid="inv-freeze-ok" @click="submitFreeze">冻结</el-button>
      </template>
    </el-dialog>
  </el-card>
</template>

<style scoped>
.stats { display: grid; grid-template-columns: repeat(auto-fill, minmax(200px, 1fr)); gap: 12px; }
.stat { display: flex; flex-direction: column; gap: 6px; padding: 16px; border: 1px solid var(--el-border-color-lighter); border-radius: 6px; }
.stat span { color: #909399; font-size: 13px; }
.stat b { font-size: 22px; }
.clickable { cursor: pointer; }
.filters { display: flex; flex-wrap: wrap; gap: 8px 16px; align-items: center; margin-bottom: 12px; }
.spacer { flex: 1; }
.error-bar { margin-bottom: 12px; }
.pager { margin-top: 12px; justify-content: flex-end; }
.grid { width: 100%; border-collapse: collapse; font-size: 13px; }
.grid th, .grid td { padding: 8px; text-align: left; border-bottom: 1px solid var(--el-border-color-lighter); }
.grid th { color: #909399; font-weight: 500; }
tbody tr.clickable:hover { background: var(--el-fill-color-light); }
.empty { text-align: center; }
.ops { white-space: nowrap; }
.tag { margin-right: 4px; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.muted { color: #909399; font-size: 13px; }
.error { color: var(--el-color-danger); font-size: 13px; margin: 0 12px 0 0; }
.error.inline { margin-left: 12px; }
.money-minus { color: var(--el-color-danger); }
.actions { display: flex; gap: 12px; align-items: center; margin: 12px 0; }
.kyc-warn { margin: 8px 0; }
.cfg-form { max-width: 640px; }
h4 { margin: 20px 0 10px; }
</style>
