<script setup lang="ts">
import { brand, isApiError, useCountdown, type User } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import { postLoginTarget } from '../logic/access'
import { clearInvite, inviteToast, parseInviteParam, readInvite } from '../logic/invite'
import { isValidPhone, normalizeCode, normalizePhone } from '../logic/login'
import { notifyError } from '../logic/notify'
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

// 邀请：本地存下的邀请码（App.vue 落地时写入；直接打开 /login?ic= 时它可能还没写完，所以也看地址栏）。
// inviteCode 是登录时要带上的码；inviter 非空才显示提示条。查询接口临时失败（限流 / 网络）时不显示提示条但仍带码登录，
// 由后端判定；只有查询明确说无效、或登录已用掉它时才丢弃。
const inviteCode = ref<string | null>(null)
const inviter = ref('')
onMounted(async () => {
  const code = readInvite() ?? parseInviteParam(window.location.search)
  if (!code) return
  inviteCode.value = code
  try {
    const r = await api.get<{ valid: boolean; inviter: string }>(`/referral/codes/${code}`)
    if (inviteCode.value !== code) return // 查询期间用户已点 ×
    if (r.valid) inviter.value = r.inviter
    else dropInvite() // 明确无效：静默丢弃
  } catch {
    /* 临时失败：不显示提示条，提交时仍带码 */
  }
})
function dropInvite() {
  inviteCode.value = null
  inviter.value = ''
  clearInvite()
}

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
    const u = await api.post<User>('/auth/login', { phone: p, code: c, ...(inviteCode.value ? { invite_code: inviteCode.value } : {}) })
    if (u.invite) {
      const t = inviteToast(u.invite)
      if (t.type === 'success') ElMessage.success(t.message)
      else if (t.type === 'warning') ElMessage.warning(t.message)
    }
    clearInvite() // 绑定只在注册时：登录成功后邀请码已无用
    setUser(u)
    const next = postLoginTarget(route.query.redirect)
    if (next.kind === 'assign') {
      window.location.assign(next.url) // Web 访问授权端点是后端路径，router.replace 到不了
      return
    }
    await router.replace(next.path)
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
      <el-alert v-if="inviteCode && inviter" type="info" :closable="false" show-icon class="invite" data-testid="login-invite">
        <span>通过 <b>{{ inviter }}</b> 的邀请注册</span>
        <el-button link size="small" class="invite-clear" aria-label="不使用邀请码" data-testid="login-invite-clear" @click="dropInvite">×</el-button>
      </el-alert>
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
.invite { margin-bottom: 12px; }
.invite-clear { margin-left: 8px; }
.hint { color: #909399; font-size: 12px; text-align: center; margin: 12px 0 0; }
</style>
