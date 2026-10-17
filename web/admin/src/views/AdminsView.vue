<script setup lang="ts">
import { adminStatusLabel, fmtTime, labelOf, messageOf, roleLabel, type Admin, type Listed, type Role } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { onMounted, reactive, ref } from 'vue'
import { api } from '../api'
import { notifyError } from '../logic/notify'

const items = ref<Admin[]>([])
const loading = ref(false)
const error = ref('')

/** 加载失败清空表格并显示页内错误与重试（不自动重试）。 */
async function load() {
  loading.value = true
  try {
    items.value = (await api.get<Listed<Admin>>('/admins')).items
    error.value = ''
  } catch (e) {
    notifyError(e)
    items.value = []
    error.value = messageOf(e)
  } finally {
    loading.value = false
  }
}
onMounted(load)
const roles: Role[] = ['super', 'ops', 'finance']

// 以下写操作后端都要求动态码：请求层弹出全局动态码框，带 totp 重发一次；取消（cancelled）静默，弹窗与输入保留
const create = reactive({ visible: false, username: '', password: '', role: 'ops' as Role, error: '', busy: false })
function openCreate() {
  Object.assign(create, { visible: true, username: '', password: '', role: 'ops', error: '' })
}
async function submitCreate() {
  if (create.busy) return // 同步守卫：连点只发一次
  const username = create.username.trim()
  create.error = !/^[a-z][a-z0-9_.-]{2,31}$/.test(username)
    ? '用户名需为 3～32 位小写字母、数字或 _ . -，并以字母开头'
    : create.password.length < 12 || create.password.length > 128
      ? '密码长度需为 12～128 个字符'
      : ''
  if (create.error) return
  create.busy = true
  try {
    await api.post('/admins', { username, password: create.password, role: create.role })
    ElMessage.success('管理员已创建，首次登录时绑定动态码')
    create.visible = false
    await load()
  } catch (e) {
    notifyError(e) // duplicate_username → 「用户名已存在」
  } finally {
    create.busy = false
  }
}

const edit = reactive({ visible: false, target: null as Admin | null, role: 'ops' as Role, status: 'active' as Admin['status'], busy: false })
function openEdit(a: Admin) {
  Object.assign(edit, { visible: true, target: a, role: a.role, status: a.status })
}
async function submitEdit() {
  if (!edit.target || edit.busy) return
  edit.busy = true
  try {
    await api.put(`/admins/${edit.target.id}`, { role: edit.role, status: edit.status })
    ElMessage.success('已保存')
    edit.visible = false
    await load()
  } catch (e) {
    notifyError(e) // last_super →「不能停用或降级最后一个启用中的超级管理员」
  } finally {
    edit.busy = false
  }
}

const reset = reactive({ visible: false, target: null as Admin | null, busy: false })
function openReset(a: Admin) {
  Object.assign(reset, { visible: true, target: a })
}
async function submitReset() {
  if (!reset.target || reset.busy) return
  reset.busy = true
  try {
    await api.post(`/admins/${reset.target.id}/reset-totp`, {})
    ElMessage.success('已重置动态码')
    reset.visible = false
    await load()
  } catch (e) {
    notifyError(e)
  } finally {
    reset.busy = false
  }
}
</script>

<template>
  <el-card>
    <div class="toolbar"><el-button type="primary" data-testid="admins-create-btn" @click="openCreate">新增管理员</el-button></div>
    <el-alert v-if="error" type="error" :title="error" show-icon :closable="false" class="load-error" data-testid="admins-error">
      <el-button size="small" :loading="loading" data-testid="admins-retry" @click="load">重试</el-button>
    </el-alert>
    <el-table :data="items" v-loading="loading" row-key="id">
      <el-table-column label="用户名" min-width="140"><template #default="{ row }"><span data-testid="adm-username">{{ row.username }}</span></template></el-table-column>
      <el-table-column label="角色" width="120"><template #default="{ row }"><span data-testid="adm-role">{{ labelOf(roleLabel, row.role) }}</span></template></el-table-column>
      <el-table-column label="状态" width="100"><template #default="{ row }"><el-tag :type="row.status === 'active' ? 'success' : 'info'" data-testid="adm-status">{{ labelOf(adminStatusLabel, row.status) }}</el-tag></template></el-table-column>
      <el-table-column label="动态码" width="100"><template #default="{ row }"><span data-testid="adm-totp">{{ row.totp_confirmed ? '已绑定' : '未绑定' }}</span></template></el-table-column>
      <el-table-column label="最后登录" width="160"><template #default="{ row }"><span data-testid="adm-last-login">{{ row.last_login_at ? fmtTime(row.last_login_at) : '—' }}</span></template></el-table-column>
      <el-table-column label="IP" width="140"><template #default="{ row }"><span data-testid="adm-last-ip">{{ row.last_login_ip || '—' }}</span></template></el-table-column>
      <el-table-column label="操作" width="240">
        <template #default="{ row }">
          <el-button size="small" data-testid="adm-edit" @click="openEdit(row as Admin)">修改角色 / 状态</el-button>
          <el-button size="small" type="danger" plain data-testid="adm-reset-totp" @click="openReset(row as Admin)">重置动态码</el-button>
        </template>
      </el-table-column>
    </el-table>

    <el-dialog v-model="create.visible" title="新增管理员" width="460" align-center destroy-on-close data-testid="adm-create-dialog">
      <el-form label-position="top" @submit.prevent="submitCreate">
        <el-form-item label="用户名"><el-input v-model="create.username" data-testid="adm-create-username" autocomplete="off" /></el-form-item>
        <el-form-item label="初始密码（12～128 位）"><el-input v-model="create.password" type="password" show-password data-testid="adm-create-password" autocomplete="new-password" /></el-form-item>
        <el-form-item label="角色"><el-select v-model="create.role" data-testid="adm-create-role"><el-option v-for="r in roles" :key="r" :label="labelOf(roleLabel, r)" :value="r" /></el-select></el-form-item>
        <p v-if="create.error" class="error" data-testid="adm-create-error">{{ create.error }}</p>
        <el-button type="primary" native-type="submit" :loading="create.busy" data-testid="adm-create-submit">创建</el-button>
      </el-form>
    </el-dialog>

    <el-dialog v-model="edit.visible" :title="`修改 ${edit.target?.username}`" width="420" align-center destroy-on-close data-testid="adm-edit-dialog">
      <el-form label-position="top" @submit.prevent="submitEdit">
        <el-form-item label="角色"><el-select v-model="edit.role" data-testid="adm-form-role"><el-option v-for="r in roles" :key="r" :label="labelOf(roleLabel, r)" :value="r" /></el-select></el-form-item>
        <el-form-item label="状态"><el-select v-model="edit.status" data-testid="adm-form-status"><el-option label="正常" value="active" /><el-option label="已停用" value="disabled" /></el-select></el-form-item>
        <el-button type="primary" native-type="submit" :loading="edit.busy" data-testid="adm-form-submit">保存</el-button>
      </el-form>
    </el-dialog>

    <el-dialog v-model="reset.visible" title="重置动态码" width="420" align-center data-testid="adm-reset-dialog">
      <p data-testid="adm-reset-text">该管理员将被强制下线并在下次登录时重新绑定。</p>
      <template #footer><el-button data-testid="adm-reset-cancel" @click="reset.visible = false">取消</el-button><el-button type="danger" :loading="reset.busy" data-testid="adm-reset-ok" @click="submitReset">确定重置</el-button></template>
    </el-dialog>
  </el-card>
</template>

<style scoped>
.toolbar { display: flex; justify-content: flex-end; margin-bottom: 12px; }
.load-error { margin-bottom: 12px; }
.error { color: var(--el-color-danger); font-size: 13px; }
</style>
