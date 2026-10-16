<script setup lang="ts">
import { fmtTime, kycStatusLabel, labelOf } from '@spark/shared'
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import { api } from '../api'
import { clearSession, session } from '../store/session'

const router = useRouter()
const loggingOut = ref(false)

async function logout() {
  loggingOut.value = true
  try {
    await api.post('/auth/logout')
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
      <router-link to="/help" data-testid="account-help">使用帮助</router-link>
      <el-button data-testid="account-logout" :loading="loggingOut" @click="logout">退出登录</el-button>
    </div>
  </el-card>
</template>

<style scoped>
.actions { display: flex; justify-content: space-between; align-items: center; margin-top: 16px; }
</style>
