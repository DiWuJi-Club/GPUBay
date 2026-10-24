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

// 会话里的 /me 是启动时探测的，进入「我的」页时刷新一次（本月返利、实名状态）；失败就用缓存
onMounted(async () => {
  try {
    const u = await api.get<User>('/me')
    if (session.status === 'authed') setUser(u)
  } catch {
    /* 用缓存 */
  }
})

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
.muted { color: #909399; font-size: 12px; }
.month { margin-left: 6px; }
</style>
