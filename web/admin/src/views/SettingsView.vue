<script setup lang="ts">
import { ElMessage } from 'element-plus'
import { reactive, ref } from 'vue'
import { api } from '../api'
import { notifyError } from '../logic/notify'

const form = reactive({ old: '', next: '', confirm: '' })
const errors = reactive({ next: '', confirm: '' })
const busy = ref(false)

async function submit() {
  if (busy.value) return
  errors.next = form.next.length >= 12 ? '' : '新密码至少 12 位'
  errors.confirm = form.next === form.confirm ? '' : '两次输入的新密码不一致'
  if (errors.next || errors.confirm) return
  busy.value = true
  try {
    await api.post('/me/password', { old: form.old, new: form.next })
    ElMessage.success('密码已修改')
    form.old = form.next = form.confirm = ''
  } catch (e) {
    notifyError(e) // wrong_password → 「原密码不正确」；会话失效已由 handleUnauthorized 提示一次
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <el-card class="settings">
    <h2>修改密码</h2>
    <el-form label-position="top" @submit.prevent="submit">
      <el-form-item label="原密码"><el-input v-model="form.old" type="password" show-password data-testid="settings-old" autocomplete="current-password" /></el-form-item>
      <el-form-item label="新密码（至少 12 位）" :error="errors.next" :show-message="false">
        <el-input v-model="form.next" type="password" show-password data-testid="settings-new" autocomplete="new-password" />
        <span v-if="errors.next" class="field-error" data-testid="settings-new-error">{{ errors.next }}</span>
      </el-form-item>
      <el-form-item label="确认新密码" :error="errors.confirm" :show-message="false">
        <el-input v-model="form.confirm" type="password" show-password data-testid="settings-confirm" autocomplete="new-password" />
        <span v-if="errors.confirm" class="field-error" data-testid="settings-confirm-error">{{ errors.confirm }}</span>
      </el-form-item>
      <el-button type="primary" native-type="submit" :loading="busy" data-testid="settings-submit">保存</el-button>
    </el-form>
  </el-card>
</template>

<style scoped>
.settings { max-width: 480px; }
.field-error { color: var(--el-color-danger); font-size: 12px; }
</style>
