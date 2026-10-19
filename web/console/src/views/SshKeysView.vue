<script setup lang="ts">
import { fmtTime, isApiError, messageOf, type Listed, type SshKey } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { onMounted, ref } from 'vue'
import { api } from '../api'
import AppDialog from '../components/AppDialog.vue'
import { looksLikePublicKey, MAX_KEYS } from '../logic/access'
import { notifyError } from '../logic/notify'

const keys = ref<SshKey[]>([])
const loaded = ref(false)
const loadError = ref('')
const adding = ref(false)
const input = ref('')
const name = ref('')
const error = ref('')
const submitting = ref(false)
const deleting = ref<SshKey | null>(null)
const busyDelete = ref(false)

async function load() {
  try {
    keys.value = (await api.get<Listed<SshKey>>('/ssh-keys')).items
    loadError.value = ''
  } catch (e) {
    loadError.value = messageOf(e)
  } finally {
    loaded.value = true
  }
}
onMounted(load)

function openAdd() {
  input.value = ''
  name.value = ''
  error.value = ''
  adding.value = true
}
async function submit() {
  if (submitting.value) return
  if (!looksLikePublicKey(input.value)) {
    error.value = '请粘贴一行公钥（以 ssh-ed25519 / ecdsa-sha2-… / ssh-rsa 开头）'
    return
  }
  error.value = ''
  submitting.value = true
  try {
    await api.post('/ssh-keys', { name: name.value.trim(), public_key: input.value.trim() })
    adding.value = false
    ElMessage.success('已添加')
    await load()
  } catch (e) {
    if (isApiError(e) && (e.code === 'ssh_key_invalid' || e.code === 'ssh_key_exists' || e.code === 'ssh_key_limit' || e.code === 'bad_request')) error.value = e.message
    else notifyError(e)
  } finally {
    submitting.value = false
  }
}
async function confirmDelete() {
  if (!deleting.value || busyDelete.value) return
  busyDelete.value = true
  try {
    await api.del(`/ssh-keys/${deleting.value.id}`)
    deleting.value = null
    ElMessage.success('已删除')
    await load()
  } catch (e) {
    deleting.value = null
    notifyError(e)
  } finally {
    busyDelete.value = false
  }
}
</script>

<template>
  <div class="keys">
    <el-card>
      <template #header>
        <div class="head">
          <span>SSH 公钥 <span class="muted" data-testid="keys-count">{{ keys.length }} / {{ MAX_KEYS }}</span></span>
          <el-button type="primary" size="small" :disabled="keys.length >= MAX_KEYS" data-testid="keys-add-btn" @click="openAdd">添加公钥</el-button>
        </div>
      </template>
      <el-alert v-if="loadError" type="error" :title="loadError" show-icon :closable="false" />
      <p class="muted">添加后可用公钥登录您的所有实例（<code>ssh -i 私钥 ins-xxxxxxxx@…</code>）；删除公钥不影响已建立的连接。在容器里 <code>ssh-copy-id</code> 不会生效，公钥只在这里管理。</p>
      <p v-if="loaded && !loadError && keys.length === 0" class="muted" data-testid="keys-empty">还没有公钥</p>
      <div v-for="k in keys" :key="k.id" class="row" data-testid="keys-row">
        <div class="info">
          <b data-testid="keys-name">{{ k.name }}</b>
          <code class="mono" data-testid="keys-fp">{{ k.fingerprint }}</code>
          <span class="muted">添加于 {{ fmtTime(k.created_at) }}<template v-if="k.last_used_at"> · 最近使用 {{ fmtTime(k.last_used_at) }}</template></span>
        </div>
        <el-button size="small" type="danger" plain data-testid="keys-delete" @click="deleting = k">删除</el-button>
      </div>
    </el-card>

    <AppDialog v-model="adding" kind="form" title="添加公钥" data-testid="keys-dialog">
      <el-form label-position="top" @submit.prevent="submit">
        <el-form-item label="公钥（一行，支持 ed25519 / ecdsa / rsa≥2048）">
          <el-input v-model="input" type="textarea" :rows="4" placeholder="ssh-ed25519 AAAA… you@laptop" data-testid="keys-input" />
        </el-form-item>
        <el-form-item label="名称（可选，缺省取公钥注释）"><el-input v-model="name" maxlength="64" data-testid="keys-name-input" /></el-form-item>
        <p v-if="error" class="danger" data-testid="keys-error">{{ error }}</p>
      </el-form>
      <template #footer>
        <el-button data-testid="keys-cancel" @click="adding = false">取消</el-button>
        <el-button type="primary" :loading="submitting" data-testid="keys-submit" @click="submit">添加</el-button>
      </template>
    </AppDialog>

    <AppDialog :model-value="!!deleting" kind="confirm" title="删除公钥" data-testid="keys-delete-dialog" @update:model-value="(v: boolean) => { if (!v) deleting = null }">
      <p>删除「{{ deleting?.name }}」后将无法再用它登录，确定删除？</p>
      <template #footer>
        <el-button data-testid="keys-delete-cancel" @click="deleting = null">取消</el-button>
        <el-button type="danger" :loading="busyDelete" data-testid="keys-delete-ok" @click="confirmDelete">确定删除</el-button>
      </template>
    </AppDialog>
  </div>
</template>

<style scoped>
.keys { display: grid; gap: 16px; max-width: 760px; }
.head { display: flex; justify-content: space-between; align-items: center; }
.row { display: flex; justify-content: space-between; align-items: center; gap: 12px; padding: 10px 0; border-bottom: 1px solid var(--el-border-color-lighter); }
.info { display: grid; gap: 4px; min-width: 0; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; word-break: break-all; font-size: 12px; }
.muted { color: #909399; font-size: 12px; }
.danger { color: var(--el-color-danger); font-size: 13px; }
</style>
