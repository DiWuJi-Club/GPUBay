<script setup lang="ts">
import { brand, isApiError, useCountdown, type User } from '@spark/shared'
import { computed, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import { isValidPhone, normalizeCode, normalizePhone } from '../logic/login'
import { notifyError } from '../logic/notify'
import { safeRedirect } from '../router'
import { setUser } from '../store/session'

const route = useRoute()
const router = useRouter()
const phone = ref('')
const code = ref('')
const phoneError = ref('')
const codeError = ref('')
const sending = ref(false)
const submitting = ref(false)
// 只在内存，刷新即重置；服务端仍有 60 秒限制。按真实流逝时间计算，后台标签页定时器被节流也不会多等
const { remaining: countdown, start: startCountdown } = useCountdown()
const sendLabel = computed(() => (countdown.value > 0 ? `${countdown.value} 秒后重发` : '发送验证码'))
const logo = import.meta.env.BASE_URL + brand.logoUrl

async function send() {
  const p = normalizePhone(phone.value)
  if (!isValidPhone(p)) {
    phoneError.value = '手机号格式不正确'
    return
  }
  phoneError.value = ''
  sending.value = true
  try {
    await api.post('/auth/sms-code', { phone: p })
    startCountdown(60)
  } catch (e) {
    notifyError(e) // rate_limited 等原文提示
  } finally {
    sending.value = false
  }
}

async function submit() {
  const p = normalizePhone(phone.value)
  const c = normalizeCode(code.value)
  phoneError.value = isValidPhone(p) ? '' : '手机号格式不正确'
  codeError.value = c.length === 6 ? '' : '请输入 6 位验证码'
  if (phoneError.value || codeError.value) return
  submitting.value = true
  try {
    const u = await api.post<User>('/auth/login', { phone: p, code: c })
    setUser(u)
    await router.replace(safeRedirect(route.query.redirect) ?? '/market')
  } catch (e) {
    if (isApiError(e) && e.code === 'invalid_code') codeError.value = e.message
    else notifyError(e) // banned → 「账号已被封禁」
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <div class="login-page">
    <el-card class="login-card" data-testid="login-card">
      <h1 class="brand"><img :src="logo" alt="" width="32" height="32" /> {{ brand.name }}</h1>
      <el-form label-position="top" @submit.prevent="submit">
        <el-form-item label="手机号" :error="phoneError" data-testid="login-phone-item">
          <el-input v-model="phone" data-testid="login-phone" inputmode="tel" autocomplete="tel" maxlength="13" placeholder="请输入手机号" />
        </el-form-item>
        <el-form-item label="验证码" :error="codeError" data-testid="login-code-item">
          <div class="code-row">
            <el-input v-model="code" data-testid="login-code" inputmode="numeric" autocomplete="one-time-code" placeholder="6 位验证码" />
            <el-button data-testid="login-send" :disabled="countdown > 0 || sending" :loading="sending" @click="send">{{ sendLabel }}</el-button>
          </div>
        </el-form-item>
        <el-button type="primary" native-type="submit" class="submit" data-testid="login-submit" :loading="submitting">登录 / 注册</el-button>
      </el-form>
      <p class="hint">未注册的手机号将自动注册；登录即表示同意服务条款</p>
    </el-card>
  </div>
</template>

<style scoped>
.login-page { min-height: 100vh; display: flex; align-items: center; justify-content: center; padding: 16px; box-sizing: border-box; }
.login-card { width: 100%; max-width: 420px; }
.brand { display: flex; align-items: center; gap: 8px; font-size: 20px; margin: 0 0 16px; }
.code-row { display: flex; gap: 8px; width: 100%; }
.code-row .el-input { flex: 1; }
.submit { width: 100%; }
.hint { color: #909399; font-size: 12px; text-align: center; margin: 12px 0 0; }
</style>
