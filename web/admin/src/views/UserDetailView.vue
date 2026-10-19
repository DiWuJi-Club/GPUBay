<script setup lang="ts">
import {
  durationLabel, fmtTime, fmtYuan, instanceStateLabel, isApiError, kycResultLabel, kycStatusLabel, labelOf, ledgerKindLabel, messageOf, orderKindLabel, orderStatusLabel,
  userKindLabel, userStatusLabel, type Recharge, type UserDetail,
} from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, onMounted, reactive, ref } from 'vue'
import { onBeforeRouteUpdate, useRoute } from 'vue-router'
import { api } from '../api'
import IdLink from '../components/IdLink.vue'
import { validateAdjust, validateManualRecharge } from '../logic/money'
import { notifyError } from '../logic/notify'
import { session } from '../store/session'

const route = useRoute()
const id = computed(() => String(route.params.id))
const data = ref<UserDetail | null>(null)
const loading = ref(false)
const error = ref('')
const tab = ref('instances')
const role = computed(() => session.admin?.role)
/** 封禁 / 解封：运维与超管；调账 / 手动入账与订单、流水、充值单入口：财务与超管（与后端权限一致） */
const canOps = computed(() => role.value === 'ops' || role.value === 'super')
const canFinance = computed(() => role.value === 'finance' || role.value === 'super')

let seq = 0
/**
 * 首次加载失败：页内错误 + 重试按钮（不自动重试）；操作后的刷新失败只提示，保留已显示的数据。
 * 只采用最后一次请求的结果：从一个用户切到另一个时，先发后到的旧响应被丢弃。
 */
async function load(target = id.value) {
  const mine = ++seq
  loading.value = true
  try {
    const d = await api.get<UserDetail>(`/users/${target}`)
    if (mine !== seq) return
    data.value = d
    error.value = ''
  } catch (e) {
    if (mine !== seq) return
    notifyError(e)
    if (!data.value) error.value = messageOf(e)
  } finally {
    if (mine === seq) loading.value = false
  }
}
onMounted(() => void load())
// 同一组件换了用户：清掉旧用户的数据与弹窗，按新 ID 重新加载
onBeforeRouteUpdate((to) => {
  if (to.params.id === route.params.id) return
  data.value = null
  error.value = ''
  tab.value = 'instances'
  ban.visible = false
  unban.visible = false
  adjust.visible = false
  adjust.confirm = false
  mrecharge.visible = false
  mrecharge.confirm = false
  void load(String(to.params.id))
})

// ---- 封禁 / 解封（ops）----
const ban = reactive({ visible: false, reason: '', error: '', busy: false })
const unban = reactive({ visible: false, busy: false })
function openBan() {
  Object.assign(ban, { visible: true, reason: '', error: '' })
}
async function submitBan() {
  if (ban.busy) return // 同步守卫：连点只发一次
  ban.error = ban.reason.trim() ? '' : '请填写封禁原因'
  if (ban.error) return
  ban.busy = true
  try {
    await api.post(`/users/${id.value}/ban`, { reason: ban.reason.trim() })
    ElMessage.success('已封禁')
    ban.visible = false
    await load()
  } catch (e) {
    notifyError(e) // 弹窗保留，输入不丢
  } finally {
    ban.busy = false
  }
}
async function submitUnban() {
  if (unban.busy) return
  unban.busy = true
  try {
    await api.post(`/users/${id.value}/unban`, {})
    ElMessage.success('已解封')
    unban.visible = false
    await load()
  } catch (e) {
    notifyError(e)
  } finally {
    unban.busy = false
  }
}

// ---- 调账（finance）：提交前确认「将把余额从 ¥a 调整为 ¥b」，动态码由请求层弹出 ----
const adjust = reactive({ visible: false, confirm: false, amount: '', remark: '', error: '', busy: false, cents: 0, sendRemark: '' })
const adjustAfter = computed(() => (data.value?.balance ?? 0) + adjust.cents)
function openAdjust() {
  Object.assign(adjust, { visible: true, confirm: false, amount: '', remark: '', error: '' })
}
function openAdjustConfirm() {
  const v = validateAdjust(adjust.amount, adjust.remark)
  if ('error' in v) {
    adjust.error = v.error
    return
  }
  const balance = data.value?.balance ?? 0
  if (balance + v.cents < 0) {
    // 扣减后为负：后端一定拒绝，不进确认（确认框里不出现负余额）
    adjust.error = `扣减金额超过当前余额 ${fmtYuan(balance)}`
    return
  }
  Object.assign(adjust, { error: '', cents: v.cents, sendRemark: v.remark, confirm: true })
}
async function submitAdjust() {
  if (adjust.busy) return // 同步守卫：调账不是幂等操作
  adjust.busy = true
  try {
    const out = await api.post<{ balance: number }>(`/users/${id.value}/adjust`, { amount: adjust.cents, remark: adjust.sendRemark })
    ElMessage.success(`余额已调整为 ${fmtYuan(out.balance)}`)
    adjust.confirm = false
    adjust.visible = false
    await load()
  } catch (e) {
    // 余额不足、60 秒内重复提交等：回到填写弹窗，输入保留；动态码取消静默
    adjust.confirm = false
    if (isApiError(e) && e.code === 'cancelled') return
    // 本地已按显示的余额拦截过扣成负数；仍被拒说明余额在别处变了（如用户刚下单）
    if (isApiError(e) && e.code === 'insufficient_balance') ElMessage.error('扣减后余额将为负，已拒绝；余额可能已变化，已刷新')
    else notifyError(e)
    await load() // 失败后余额可能已变化：刷新，下一次确认按最新余额计算
  } finally {
    adjust.busy = false
  }
}

// ---- 手动入账（finance）：提交前确认「为用户 手机号（ID）入账 ¥x，余额 ¥a → ¥b」，动态码由请求层弹出 ----
const mrecharge = reactive({ visible: false, confirm: false, amount: '', remark: '', error: '', busy: false, cents: 0, sendRemark: '' })
function openManualRecharge() {
  Object.assign(mrecharge, { visible: true, confirm: false, amount: '', remark: '', error: '' })
}
function openManualRechargeConfirm() {
  const v = validateManualRecharge(mrecharge.amount, mrecharge.remark)
  if ('error' in v) {
    mrecharge.error = v.error
    return
  }
  Object.assign(mrecharge, { error: '', cents: v.cents, sendRemark: v.remark, confirm: true })
}
async function submitManualRecharge() {
  if (mrecharge.busy) return // 同步守卫：入账不是幂等操作
  mrecharge.busy = true
  try {
    const out = await api.post<Recharge>(`/users/${id.value}/manual-recharge`, { amount: mrecharge.cents, remark: mrecharge.sendRemark })
    ElMessage.success(`已入账 ${fmtYuan(out.amount)}`)
    mrecharge.confirm = false
    mrecharge.visible = false
    await load()
  } catch (e) {
    mrecharge.confirm = false // 回到填写弹窗，输入不丢
    notifyError(e)
  } finally {
    mrecharge.busy = false
  }
}
const signed = (n: number) => (n >= 0 ? `+${fmtYuan(n)}` : fmtYuan(n))
</script>

<template>
  <div>
    <el-alert v-if="error && !data" type="error" :title="error" show-icon :closable="false" data-testid="udetail-error">
      <el-button size="small" :loading="loading" data-testid="udetail-retry" @click="load()">重试</el-button>
    </el-alert>
    <div v-if="data" class="detail">
      <el-card>
        <el-descriptions :column="3" border>
          <el-descriptions-item label="用户 ID"><span class="mono" data-testid="udetail-id">{{ data.user.id }}</span></el-descriptions-item>
          <el-descriptions-item label="手机号"><span class="mono" data-testid="udetail-phone">{{ data.user.phone }}</span></el-descriptions-item>
          <el-descriptions-item label="类型"><span data-testid="udetail-kind">{{ labelOf(userKindLabel, data.user.kind) }}</span></el-descriptions-item>
          <el-descriptions-item label="实名"><span data-testid="udetail-kyc">{{ labelOf(kycStatusLabel, data.user.kyc_status) }}</span></el-descriptions-item>
          <el-descriptions-item label="账号"><el-tag :type="data.user.status === 'banned' ? 'danger' : 'success'" data-testid="udetail-status">{{ labelOf(userStatusLabel, data.user.status) }}</el-tag></el-descriptions-item>
          <el-descriptions-item label="余额"><b data-testid="udetail-balance">{{ fmtYuan(data.balance) }}</b></el-descriptions-item>
          <el-descriptions-item label="注册时间"><span data-testid="udetail-created">{{ fmtTime(data.user.created_at) }}</span></el-descriptions-item>
        </el-descriptions>
        <div v-if="canOps || canFinance" class="actions">
          <template v-if="canOps">
            <el-button v-if="data.user.status === 'active'" type="danger" plain data-testid="udetail-ban-btn" @click="openBan">封禁</el-button>
            <el-button v-else data-testid="udetail-unban-btn" @click="unban.visible = true">解封</el-button>
          </template>
          <template v-if="canFinance">
            <el-button data-testid="udetail-adjust-btn" @click="openAdjust">调账</el-button>
            <el-button data-testid="udetail-recharge-btn" @click="openManualRecharge">手动入账</el-button>
          </template>
        </div>
      </el-card>

      <el-card>
        <el-tabs v-model="tab">
          <el-tab-pane label="实例" name="instances">
            <table class="grid">
              <thead><tr><th>实例</th><th>状态</th><th>到期时间</th><th>创建时间</th></tr></thead>
              <tbody>
                <tr v-for="i in data.instances" :key="i.id" data-testid="udetail-instance-row">
                  <td data-testid="udetail-instance-id"><IdLink kind="instance" :id="i.id" /></td>
                  <td data-testid="udetail-instance-state">{{ labelOf(instanceStateLabel, i.state) }}</td>
                  <td>{{ fmtTime(i.expires_at) || '—' }}</td>
                  <td>{{ fmtTime(i.created_at) }}</td>
                </tr>
                <tr v-if="!data.instances.length"><td colspan="4" class="muted empty">无实例</td></tr>
              </tbody>
            </table>
            <router-link class="more" :to="{ path: '/instances', query: { user_id: data.user.id } }" data-testid="udetail-more-instances">更多实例</router-link>
          </el-tab-pane>
          <el-tab-pane label="订单" name="orders">
            <table class="grid">
              <thead><tr><th>订单号</th><th>类型</th><th>租期</th><th>金额</th><th>状态</th><th>时间</th></tr></thead>
              <tbody>
                <tr v-for="o in data.orders" :key="o.id" data-testid="udetail-order-row">
                  <td class="mono" data-testid="udetail-order-id">{{ o.id }}</td>
                  <td data-testid="udetail-order-kind">{{ labelOf(orderKindLabel, o.kind) }}</td>
                  <td>{{ labelOf(durationLabel, o.duration) }}</td>
                  <td data-testid="udetail-order-amount">{{ fmtYuan(o.amount) }}</td>
                  <td data-testid="udetail-order-status">{{ labelOf(orderStatusLabel, o.status) }}</td>
                  <td>{{ fmtTime(o.created_at) }}</td>
                </tr>
                <tr v-if="!data.orders.length"><td colspan="6" class="muted empty">无订单</td></tr>
              </tbody>
            </table>
            <router-link v-if="canFinance" class="more" :to="{ path: '/orders', query: { user_id: data.user.id } }" data-testid="udetail-more-orders">更多订单</router-link>
          </el-tab-pane>
          <el-tab-pane label="流水" name="ledger">
            <table class="grid">
              <thead><tr><th>类型</th><th>金额</th><th>变动后余额</th><th>备注</th><th>时间</th></tr></thead>
              <tbody>
                <tr v-for="l in data.ledger" :key="l.id" data-testid="udetail-ledger-row">
                  <td data-testid="udetail-ledger-kind">{{ labelOf(ledgerKindLabel, l.kind) }}</td>
                  <td data-testid="udetail-ledger-amount" :class="l.amount >= 0 ? 'money-plus' : 'money-minus'">{{ signed(l.amount) }}</td>
                  <td data-testid="udetail-ledger-after">{{ fmtYuan(l.balance_after) }}</td>
                  <td data-testid="udetail-ledger-remark">{{ l.remark }}</td>
                  <td>{{ fmtTime(l.created_at) }}</td>
                </tr>
                <tr v-if="!data.ledger.length"><td colspan="5" class="muted empty">无流水</td></tr>
              </tbody>
            </table>
            <div v-if="canFinance" class="more">
              <router-link :to="{ path: '/ledger', query: { user_id: data.user.id } }" data-testid="udetail-more-ledger">更多流水</router-link> ·
              <router-link :to="{ path: '/recharges', query: { user_id: data.user.id } }" data-testid="udetail-more-recharges">充值单</router-link>
            </div>
          </el-tab-pane>
          <el-tab-pane label="实名记录" name="kyc">
            <table class="grid">
              <thead><tr><th>类型</th><th>姓名 / 企业</th><th>身份证 / 信用代码</th><th>结果</th><th>原因</th><th>时间</th></tr></thead>
              <tbody>
                <tr v-for="k in data.kyc" :key="k.id" data-testid="udetail-kyc-row">
                  <td data-testid="udetail-kyc-kind">{{ k.kind === 'personal' ? '个人' : '企业' }}</td>
                  <td data-testid="udetail-kyc-name">{{ k.real_name }}</td>
                  <!-- 身份证号只用后端给的脱敏值（id_number_masked），前端拿不到完整号码 -->
                  <td class="mono" data-testid="udetail-kyc-idno">{{ k.id_number_masked || k.license_no }}</td>
                  <td data-testid="udetail-kyc-result">{{ labelOf(kycResultLabel, k.result) }}</td>
                  <td data-testid="udetail-kyc-reason">{{ k.reason }}</td>
                  <td>{{ fmtTime(k.created_at) }}</td>
                </tr>
                <tr v-if="!data.kyc.length"><td colspan="6" class="muted empty">无实名记录</td></tr>
              </tbody>
            </table>
          </el-tab-pane>
        </el-tabs>
      </el-card>

      <el-dialog v-model="ban.visible" title="封禁用户" width="420" align-center data-testid="ban-dialog">
        <p class="muted">封禁后用户无法登录，已登录的会话立即失效。</p>
        <el-form label-position="top" @submit.prevent="submitBan">
          <el-form-item label="原因（必填）"><el-input v-model="ban.reason" data-testid="ban-reason" /></el-form-item>
          <p v-if="ban.error" class="error" data-testid="ban-error">{{ ban.error }}</p>
          <el-button type="danger" native-type="submit" :loading="ban.busy" data-testid="ban-submit">确定封禁</el-button>
        </el-form>
      </el-dialog>
      <el-dialog v-model="unban.visible" title="解封用户" width="420" align-center data-testid="unban-dialog">
        <p>解封后用户可重新登录。</p>
        <template #footer>
          <el-button data-testid="unban-cancel" @click="unban.visible = false">取消</el-button>
          <el-button type="primary" :loading="unban.busy" data-testid="unban-ok" @click="submitUnban">确定</el-button>
        </template>
      </el-dialog>

      <el-dialog v-model="adjust.visible" title="调账" width="460" align-center data-testid="adjust-dialog">
        <el-form label-position="top" @submit.prevent="openAdjustConfirm">
          <el-form-item label="金额（元，正数为增加，负数为扣减，如 -5）"><el-input v-model="adjust.amount" inputmode="decimal" data-testid="adjust-amount" /></el-form-item>
          <el-form-item label="备注（必填）"><el-input v-model="adjust.remark" data-testid="adjust-remark" /></el-form-item>
          <p v-if="adjust.error" class="error" data-testid="adjust-error">{{ adjust.error }}</p>
          <el-button type="primary" native-type="submit" data-testid="adjust-submit">下一步</el-button>
        </el-form>
      </el-dialog>
      <el-dialog v-model="adjust.confirm" title="确认调账" width="420" align-center data-testid="adjust-confirm-dialog">
        <p data-testid="adjust-confirm-text">将把余额从 {{ fmtYuan(data.balance) }} 调整为 {{ fmtYuan(adjustAfter) }}</p>
        <template #footer>
          <el-button data-testid="adjust-confirm-cancel" @click="adjust.confirm = false">取消</el-button>
          <el-button type="primary" :loading="adjust.busy" data-testid="adjust-confirm-ok" @click="submitAdjust">确定</el-button>
        </template>
      </el-dialog>

      <el-dialog v-model="mrecharge.visible" title="手动入账" width="460" align-center data-testid="mrecharge-dialog">
        <el-form label-position="top" @submit.prevent="openManualRechargeConfirm">
          <el-form-item label="金额（元，1～1,000,000）"><el-input v-model="mrecharge.amount" inputmode="decimal" data-testid="mrecharge-amount" /></el-form-item>
          <el-form-item label="备注（必填，例如转账流水号）"><el-input v-model="mrecharge.remark" data-testid="mrecharge-remark" /></el-form-item>
          <p v-if="mrecharge.error" class="error" data-testid="mrecharge-error">{{ mrecharge.error }}</p>
          <el-button type="primary" native-type="submit" data-testid="mrecharge-submit">下一步</el-button>
        </el-form>
      </el-dialog>
      <el-dialog v-model="mrecharge.confirm" title="确认入账" width="420" align-center data-testid="mrecharge-confirm-dialog">
        <p data-testid="mrecharge-confirm-text">将为用户 {{ data.user.phone }}（ID {{ data.user.id }}）入账 {{ fmtYuan(mrecharge.cents) }}，余额 {{ fmtYuan(data.balance) }} → {{ fmtYuan(data.balance + mrecharge.cents) }}</p>
        <template #footer>
          <el-button data-testid="mrecharge-confirm-cancel" @click="mrecharge.confirm = false">取消</el-button>
          <el-button type="primary" :loading="mrecharge.busy" data-testid="mrecharge-confirm-ok" @click="submitManualRecharge">确定入账</el-button>
        </template>
      </el-dialog>
    </div>
  </div>
</template>

<style scoped>
.detail { display: grid; gap: 16px; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.muted { color: #909399; font-size: 13px; }
.actions { display: flex; gap: 12px; margin-top: 16px; }
.error { color: var(--el-color-danger); font-size: 13px; }
.more { display: inline-block; margin-top: 12px; }
.money-plus { color: var(--el-color-success); }
.money-minus { color: var(--el-color-danger); }
.grid { width: 100%; border-collapse: collapse; font-size: 13px; }
.grid th, .grid td { padding: 8px; text-align: left; border-bottom: 1px solid var(--el-border-color-lighter); }
.grid th { color: #909399; font-weight: 500; }
.empty { text-align: center; }
</style>
