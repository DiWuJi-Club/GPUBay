<script setup lang="ts">
import { brand, isApiError, type Admin, type AdminLoginResult } from '@spark/shared'
import { ElMessage } from 'element-plus'
import QRCode from 'qrcode'
import { nextTick, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import { notifyError } from '../logic/notify'
import { safeRedirect } from '../router'
import { setAdmin } from '../store/session'

// 三步登录：密码 →（首次）绑定动态码并展示恢复码 / 动态码或恢复码。
// 密钥与恢复码只放在本页内存里：不写 localStorage / sessionStorage、不进 URL，离开页面即丢弃。
type Step = 'password' | 'setup' | 'totp' | 'recovery-codes'
const route = useRoute()
const router = useRouter()
const step = ref<Step>('password')
const busy = ref(false)
const timeout = ref(false)
const username = ref('')
const password = ref('')
const otpauthUri = ref('')
const secret = ref('')
const setupCode = ref('')
const qrCanvas = ref<HTMLCanvasElement | null>(null)
const recoveryCodes = ref<string[]>([])
const saved = ref(false)
const totpCode = ref('')
const useRecovery = ref(false)
const recoveryInput = ref('')
const logo = import.meta.env.BASE_URL + brand.logoUrl

function fail(e: unknown) {
  // 临时会话（密码通过后 5 分钟）过期：回到步骤一
  if (isApiError(e) && e.status === 401 && e.code === 'unauthorized') {
    step.value = 'password'
    timeout.value = true
    password.value = ''
    return
  }
  notifyError(e)
}

const target = () => safeRedirect(route.query.redirect) ?? '/'

async function enter(a: Admin) {
  setAdmin(a)
  await router.replace(target())
}

async function submitPassword() {
  if (busy.value) return
  busy.value = true
  timeout.value = false
  try {
    const out = await api.post<AdminLoginResult>('/auth/login', { username: username.value.trim(), password: password.value })
    if (out.next === 'totp_setup') {
      otpauthUri.value = out.otpauth_uri
      secret.value = new URL(out.otpauth_uri).searchParams.get('secret') ?? ''
      step.value = 'setup'
    } else step.value = 'totp'
  } catch (e) {
    notifyError(e) // invalid_credentials / account_locked / rate_limited 原文
  } finally {
    busy.value = false
  }
}

watch([step, otpauthUri], async ([s, uri]) => {
  if (s !== 'setup' || !uri) return
  await nextTick()
  if (qrCanvas.value) await QRCode.toCanvas(qrCanvas.value, uri, { width: 200, margin: 1 })
})

async function submitSetup() {
  if (busy.value) return
  busy.value = true
  try {
    const out = await api.post<{ admin: Admin; recovery_codes: string[] }>('/auth/totp/setup', { code: setupCode.value.replace(/\s+/g, '') })
    recoveryCodes.value = out.recovery_codes
    setAdmin(out.admin)
    secret.value = otpauthUri.value = '' // 绑定完成，密钥不再需要留在内存里
    step.value = 'recovery-codes' // 恢复码只展示这一次
  } catch (e) {
    fail(e)
  } finally {
    busy.value = false
  }
}

async function submitTotp() {
  if (busy.value) return
  busy.value = true
  try {
    const body = useRecovery.value ? { recovery_code: recoveryInput.value.trim() } : { code: totpCode.value.replace(/\s+/g, '') }
    const out = await api.post<{ admin: Admin }>('/auth/totp', body)
    await enter(out.admin)
  } catch (e) {
    fail(e)
  } finally {
    busy.value = false
  }
}

async function copy(text: string, tip: string) {
  try {
    await navigator.clipboard.writeText(text)
    ElMessage.success(tip)
  } catch {
    ElMessage.error('复制失败，请手动选中复制')
  }
}
</script>

<template>
  <div class="login-page">
    <el-card class="card">
      <h1 class="brand"><img :src="logo" alt="" width="28" height="28" /> {{ brand.name }} 管理后台</h1>
      <el-alert v-if="timeout" type="warning" title="登录已超时，请重新输入密码" :closable="false" show-icon data-testid="login-timeout" />

      <el-form v-if="step === 'password'" label-position="top" @submit.prevent="submitPassword">
        <el-form-item label="账号"><el-input v-model="username" data-testid="login-username" autocomplete="username" /></el-form-item>
        <el-form-item label="密码"><el-input v-model="password" type="password" show-password data-testid="login-password" autocomplete="current-password" /></el-form-item>
        <el-button type="primary" native-type="submit" class="full" :loading="busy" data-testid="login-submit">下一步</el-button>
      </el-form>

      <div v-else-if="step === 'setup'" data-testid="login-setup">
        <p>首次登录需要绑定动态码：用验证器 App（如 Google Authenticator）扫描二维码，或手动输入密钥。</p>
        <canvas ref="qrCanvas" width="200" height="200" data-testid="login-setup-qr" />
        <div class="secret-row">
          <code data-testid="login-setup-secret">{{ secret }}</code>
          <el-button size="small" data-testid="login-setup-copy-secret" @click="copy(secret, '密钥已复制')">复制</el-button>
        </div>
        <el-form label-position="top" @submit.prevent="submitSetup">
          <el-form-item label="输入 App 显示的 6 位动态码"><el-input v-model="setupCode" data-testid="login-setup-code" inputmode="numeric" autocomplete="one-time-code" maxlength="7" /></el-form-item>
          <el-button type="primary" native-type="submit" class="full" :loading="busy" data-testid="login-setup-submit">绑定</el-button>
        </el-form>
      </div>

      <div v-else-if="step === 'recovery-codes'">
        <el-alert type="warning" title="请妥善保存以下 8 个恢复码，每个只能使用一次；此页面关闭后不再显示。" :closable="false" show-icon />
        <ul class="codes" data-testid="login-recovery-codes">
          <li v-for="c in recoveryCodes" :key="c" data-testid="login-recovery-code">{{ c }}</li>
        </ul>
        <el-button data-testid="login-recovery-copy" @click="copy(recoveryCodes.join('\n'), '恢复码已复制')">复制全部</el-button>
        <el-checkbox v-model="saved" data-testid="login-recovery-saved">我已妥善保存这些恢复码</el-checkbox>
        <el-button type="primary" class="full" :disabled="!saved" data-testid="login-enter" @click="router.replace(target())">进入后台</el-button>
      </div>

      <el-form v-else label-position="top" @submit.prevent="submitTotp">
        <template v-if="!useRecovery">
          <el-form-item label="动态码"><el-input v-model="totpCode" data-testid="login-totp-code" inputmode="numeric" autocomplete="one-time-code" maxlength="7" /></el-form-item>
          <p class="hint" data-testid="login-totp-hint">如刚用过该动态码，请等待下一个（同一动态码 30 秒内不能重复使用）</p>
          <el-button type="primary" native-type="submit" class="full" :loading="busy" data-testid="login-totp-submit">登录</el-button>
          <el-button link data-testid="login-use-recovery" @click="useRecovery = true">改用恢复码</el-button>
        </template>
        <template v-else>
          <el-form-item label="恢复码（格式 xxxxx-xxxxx）"><el-input v-model="recoveryInput" data-testid="login-recovery-input" autocomplete="off" /></el-form-item>
          <el-button type="primary" native-type="submit" class="full" :loading="busy" data-testid="login-recovery-submit">登录</el-button>
          <el-button link data-testid="login-use-totp" @click="useRecovery = false">改用动态码</el-button>
        </template>
      </el-form>
    </el-card>
  </div>
</template>

<style scoped>
.login-page { min-height: 100vh; display: flex; align-items: center; justify-content: center; background: #f5f7fa; }
.card { width: 440px; }
.brand { display: flex; align-items: center; gap: 8px; font-size: 18px; margin: 0 0 16px; }
.full { width: 100%; margin-top: 8px; }
.secret-row { display: flex; align-items: center; gap: 8px; margin: 8px 0 16px; }
.codes { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; columns: 2; margin: 16px 0; padding-left: 20px; }
.hint { color: #909399; font-size: 12px; }
</style>
