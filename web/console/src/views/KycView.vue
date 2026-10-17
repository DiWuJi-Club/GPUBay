<script setup lang="ts">
import { isApiError, messageOf, type KYCRecord, type Upload, type User } from '@spark/shared'
import { ElMessage, type UploadRequestOptions } from 'element-plus'
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import { validateEnterprise, validatePersonal, type EnterpriseErrors, type PersonalErrors } from '../logic/kyc'
import { notifyError } from '../logic/notify'
import { safeRedirect } from '../router'
import { session, setUser } from '../store/session'

const route = useRoute()
const router = useRouter()
const record = ref<KYCRecord | null>(null)
const loading = ref(true)
const loadError = ref('')
const tab = ref<'personal' | 'enterprise'>('personal')
const status = computed(() => session.user?.kyc_status ?? 'none')
const showForm = computed(() => status.value === 'none' || status.value === 'rejected')

async function loadRecord() {
  record.value = (await api.get<{ record: KYCRecord | null }>('/kyc')).record
}
async function refreshUser() {
  setUser(await api.get<User>('/me'))
}
/** 首次加载：同时刷新用户状态与最近一条记录，失败时占位显示错误与重试（不自动重试）。 */
async function load() {
  loading.value = true
  loadError.value = ''
  try {
    await Promise.all([refreshUser(), loadRecord()])
  } catch (e) {
    notifyError(e)
    loadError.value = messageOf(e)
  } finally {
    loading.value = false
  }
}
/** 提交后的刷新：失败只提示一次，不影响已提交的结果。 */
async function refreshAfterSubmit() {
  try {
    await Promise.all([refreshUser(), loadRecord()])
  } catch (e) {
    notifyError(e)
  }
}
onMounted(load)

const statusText = computed(() => {
  switch (status.value) {
    case 'verified':
      return '已认证'
    case 'pending':
      return '审核中，通常 1 个工作日内完成'
    case 'rejected':
      return '未通过'
    default:
      return '未认证'
  }
})
const statusType = computed(() => ({ verified: 'success', pending: 'warning', rejected: 'error', none: 'info' } as const)[status.value])

// ---- 个人 ----
// 身份证号只放在组件内存里：不写 localStorage / sessionStorage / URL，提交后清空。
const personal = reactive({ name: '', idNumber: '' })
const personalErrors = ref<PersonalErrors>({})
const rejectedMessage = ref('')
const submittingPersonal = ref(false)
async function submitPersonal() {
  if (submittingPersonal.value) return // 双击保护：请求进行中不再发请求
  personalErrors.value = validatePersonal(personal)
  if (Object.keys(personalErrors.value).length) return
  submittingPersonal.value = true
  rejectedMessage.value = ''
  try {
    const u = await api.post<User>('/kyc/personal', { name: personal.name.trim(), id_number: personal.idNumber.trim().toUpperCase() })
    personal.idNumber = ''
    setUser(u)
    ElMessage.success('实名认证成功')
    const back = safeRedirect(route.query.redirect)
    if (back) await router.replace(back)
    else await refreshAfterSubmit()
  } catch (e) {
    if (isApiError(e) && e.code === 'kyc_rejected') {
      rejectedMessage.value = e.message
      await refreshAfterSubmit()
    } else notifyError(e)
  } finally {
    submittingPersonal.value = false
  }
}

// ---- 企业 ----
const enterprise = reactive({ company: '', licenseNo: '', fileKey: '', fileName: '', previewUrl: '', isPdf: false })
const enterpriseErrors = ref<EnterpriseErrors>({})
const uploadError = ref('')
const uploading = ref(false)
const submittingEnterprise = ref(false)

function clearFile() {
  if (enterprise.previewUrl) URL.revokeObjectURL(enterprise.previewUrl)
  enterprise.fileKey = ''
  enterprise.fileName = ''
  enterprise.previewUrl = ''
  enterprise.isPdf = false
}
onBeforeUnmount(clearFile)

async function doUpload(opt: UploadRequestOptions) {
  uploading.value = true
  uploadError.value = ''
  try {
    const up = await api.upload<Upload>('/uploads', opt.file)
    clearFile()
    enterprise.fileKey = up.key
    enterprise.fileName = opt.file.name
    enterprise.isPdf = up.content_type === 'application/pdf'
    enterprise.previewUrl = enterprise.isPdf ? '' : URL.createObjectURL(opt.file)
    enterpriseErrors.value.fileKey = undefined
  } catch (e) {
    clearFile()
    uploadError.value = messageOf(e) // file_too_large / unsupported_file_type 原文；重新选择文件即可重试
    notifyError(e)
  } finally {
    uploading.value = false
  }
}

async function submitEnterprise() {
  if (submittingEnterprise.value) return // 双击保护
  enterpriseErrors.value = validateEnterprise(enterprise)
  if (Object.keys(enterpriseErrors.value).length) return
  submittingEnterprise.value = true
  try {
    await api.post('/kyc/enterprise', { company_name: enterprise.company.trim(), license_no: enterprise.licenseNo.trim().toUpperCase(), license_file: enterprise.fileKey })
    ElMessage.success('已提交，等待审核')
    await refreshAfterSubmit()
  } catch (e) {
    notifyError(e)
  } finally {
    submittingEnterprise.value = false
  }
}
</script>

<template>
  <div class="kyc">
    <el-alert :type="statusType" :closable="false" show-icon data-testid="kyc-status-bar">
      <template #title>
        <span data-testid="kyc-status">{{ statusText }}</span>
        <span v-if="status === 'verified' && record" data-testid="kyc-verified-name">（{{ record.kind === 'enterprise' ? '企业' : '个人' }}：{{ record.real_name }}）</span>
      </template>
      <p v-if="status === 'rejected' && record?.reason" data-testid="kyc-reason">原因：{{ record.reason }}</p>
    </el-alert>

    <el-alert v-if="loadError" type="error" :title="loadError" show-icon :closable="false" data-testid="kyc-load-error">
      <el-button size="small" data-testid="kyc-retry" @click="load">重试</el-button>
    </el-alert>
    <el-skeleton v-else-if="loading" :rows="4" animated />
    <el-card v-else-if="showForm">
      <el-tabs v-model="tab">
        <el-tab-pane label="个人" name="personal">
          <el-alert v-if="rejectedMessage" type="error" :title="rejectedMessage" :closable="false" show-icon data-testid="kyc-rejected-box" />
          <el-form label-position="top" @submit.prevent="submitPersonal">
            <el-form-item label="真实姓名" :error="personalErrors.name" :show-message="false" data-testid="kyc-name-item">
              <el-input v-model="personal.name" data-testid="kyc-name" maxlength="50" />
              <span v-if="personalErrors.name" class="field-error" data-testid="kyc-name-error">{{ personalErrors.name }}</span>
            </el-form-item>
            <el-form-item label="身份证号" :error="personalErrors.idNumber" :show-message="false" data-testid="kyc-idno-item">
              <el-input v-model="personal.idNumber" data-testid="kyc-idno" maxlength="18" autocomplete="off" />
              <span v-if="personalErrors.idNumber" class="field-error" data-testid="kyc-idno-error">{{ personalErrors.idNumber }}</span>
            </el-form-item>
            <el-button type="primary" native-type="submit" :loading="submittingPersonal" data-testid="kyc-personal-submit">提交认证</el-button>
          </el-form>
        </el-tab-pane>

        <el-tab-pane label="企业" name="enterprise">
          <el-form label-position="top" @submit.prevent="submitEnterprise">
            <el-form-item label="企业名称" :error="enterpriseErrors.company" :show-message="false" data-testid="kyc-company-item">
              <el-input v-model="enterprise.company" data-testid="kyc-company" maxlength="100" />
              <span v-if="enterpriseErrors.company" class="field-error" data-testid="kyc-company-error">{{ enterpriseErrors.company }}</span>
            </el-form-item>
            <el-form-item label="统一社会信用代码" :error="enterpriseErrors.licenseNo" :show-message="false" data-testid="kyc-license-item">
              <el-input v-model="enterprise.licenseNo" data-testid="kyc-license-no" maxlength="18" />
              <span v-if="enterpriseErrors.licenseNo" class="field-error" data-testid="kyc-license-error">{{ enterpriseErrors.licenseNo }}</span>
            </el-form-item>
            <el-form-item label="营业执照（JPG / PNG / PDF，10MB 以内）" data-testid="kyc-file-item">
              <div class="upload-box">
                <el-upload data-testid="kyc-upload" :http-request="doUpload" :show-file-list="false" accept=".jpg,.jpeg,.png,.pdf,image/jpeg,image/png,application/pdf">
                  <el-button :loading="uploading">{{ enterprise.fileKey ? '重新选择' : '选择文件' }}</el-button>
                </el-upload>
                <div v-if="enterprise.fileKey" class="file">
                  <img v-if="enterprise.previewUrl" :src="enterprise.previewUrl" alt="营业执照预览" class="thumb" data-testid="kyc-file-preview" />
                  <span v-else class="pdf" data-testid="kyc-file-pdf">PDF</span>
                  <span data-testid="kyc-file-name">{{ enterprise.fileName }}</span>
                </div>
                <span v-if="uploadError" class="field-error" data-testid="kyc-upload-error">{{ uploadError }}</span>
                <span v-else-if="enterpriseErrors.fileKey" class="field-error">{{ enterpriseErrors.fileKey }}</span>
              </div>
            </el-form-item>
            <el-button type="primary" native-type="submit" :disabled="!enterprise.fileKey || uploading" :loading="submittingEnterprise" data-testid="kyc-enterprise-submit">提交审核</el-button>
          </el-form>
        </el-tab-pane>
      </el-tabs>
    </el-card>
  </div>
</template>

<style scoped>
.kyc { display: grid; gap: 16px; max-width: 640px; }
.upload-box { display: grid; gap: 8px; width: 100%; }
.field-error { color: var(--el-color-danger); font-size: 12px; padding-top: 4px; }
.file { display: flex; align-items: center; gap: 8px; }
.thumb { width: 64px; height: 64px; object-fit: cover; border: 1px solid #ebeef5; border-radius: 4px; }
.pdf { display: inline-block; padding: 2px 6px; border: 1px solid var(--el-color-danger); color: var(--el-color-danger); border-radius: 4px; font-size: 12px; }
</style>
