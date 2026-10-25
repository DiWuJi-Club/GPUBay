<script setup lang="ts">
import { fmtTime, fmtYuan, kycStatusLabel, labelOf, type User } from '@spark/shared'
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { api } from '../api'
import { notifyError } from '../logic/notify'
import { clearSession, session, setUser } from '../store/session'

const router = useRouter()
const loggingOut = ref(false)
/** 本月返利到账（分）：/me 的 referral_month_settled，0 时后端省略 */
const month = computed(() => session.user?.referral_month_settled ?? 0)

// 会话里的 /me 是应用启动时取的；进入「我的」页刷新一次，让本月返利、本月 Token 消费、实名状态是最新的。
// 不加 quiet：会话过期时与其它需登录页面一样提示并跳登录页；其它失败沿用会话里的值
onMounted(async () => {
  try {
    const u = await api.get<User>('/me')
    if (session.status === 'authed') setUser(u)
  } catch {
    /* 用缓存 */
  }
})
/** 本月 Token 消费（分）；后端为 0 时省略字段 → 不显示 */
const llmMonth = computed(() => (session.user?.llm_month_cost ? fmtYuan(session.user.llm_month_cost) : ''))

async function logout() {
  loggingOut.value = true
  try {
    await api.post('/auth/logout')
  } catch (e) {
    notifyError(e) // 失败也在本地清会话并回登录页
  } finally {
    clearSession()
    loggingOut.value = false
    await router.replace('/login')
  }
}
</script>

<template>
  <el-card data-testid="account-card">
    <el-descriptions :column="1" border>
      <el-descriptions-item label="手机号"><span data-testid="account-phone">{{ session.user?.phone }}</span></el-descriptions-item>
      <el-descriptions-item label="实名状态">
        <router-link to="/kyc" data-testid="account-kyc">{{ labelOf(kycStatusLabel, session.user?.kyc_status) }}</router-link>
      </el-descriptions-item>
      <el-descriptions-item label="注册时间"><span data-testid="account-created">{{ fmtTime(session.user?.created_at) }}</span></el-descriptions-item>
    </el-descriptions>
    <router-link to="/tokens" class="entry" data-testid="account-tokens">
      <span>Token 商城</span>
      <span v-if="llmMonth" class="muted" data-testid="account-tokens-month">本月 {{ llmMonth }}</span>
    </router-link>
    <div class="actions">
      <router-link to="/invite" data-testid="account-invite">
        邀请返利<span v-if="month > 0" class="muted month" data-testid="account-invite-month">本月返利 {{ fmtYuan(month) }}</span>
      </router-link>
      <router-link to="/account/ssh-keys" data-testid="account-ssh-keys">SSH 公钥</router-link>
      <router-link to="/help" data-testid="account-help">使用帮助</router-link>
      <el-button data-testid="account-logout" :loading="loggingOut" @click="logout">退出登录</el-button>
    </div>
  </el-card>
</template>

<style scoped>
.actions { display: flex; justify-content: space-between; align-items: center; gap: 8px; flex-wrap: wrap; margin-top: 16px; }
.entry { display: flex; justify-content: space-between; align-items: center; margin-top: 16px; padding: 10px 12px; border: 1px solid #ebeef5; border-radius: 4px; text-decoration: none; }
.muted { color: #909399; font-size: 12px; }
.month { margin-left: 6px; }
</style>
