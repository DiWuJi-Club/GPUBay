<script setup lang="ts">
import { cancelTotp, submitTotp, totpState } from '../store/totp'

// 显隐只由 totpState 决定（不用 v-model：el-dialog 在离场动画结束后才回写 false，可能与紧接着弹出的「动态码错误」重试框交错）。
// Esc 关闭走 before-close，一律按「取消」处理：等待中的请求必须得到结果，不能永远挂起。
function onBeforeClose() {
  cancelTotp()
}
function submitIfReady() {
  if (totpState.code.replace(/\s+/g, '').length === 6) submitTotp()
}
</script>

<template>
  <el-dialog
    :model-value="totpState.visible"
    title="安全验证"
    width="420"
    align-center
    :close-on-click-modal="false"
    :show-close="false"
    append-to-body
    data-testid="totp-dialog"
    :before-close="onBeforeClose"
  >
    <p>该操作需要输入验证器 App 中的 6 位动态码。</p>
    <el-alert v-if="totpState.message" type="error" :title="totpState.message" :closable="false" show-icon data-testid="totp-message" />
    <el-input v-model="totpState.code" data-testid="totp-input" inputmode="numeric" autocomplete="one-time-code" maxlength="7" placeholder="动态码" @keyup.enter="submitIfReady" />
    <template #footer>
      <el-button data-testid="totp-cancel" @click="cancelTotp">取消</el-button>
      <el-button type="primary" data-testid="totp-ok" :disabled="totpState.code.replace(/\s+/g, '').length !== 6" @click="submitTotp">确定</el-button>
    </template>
  </el-dialog>
</template>
