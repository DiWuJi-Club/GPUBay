<script setup lang="ts">
import { fmtTime, type AdminKYCRecord, type Paged } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { onMounted, reactive } from 'vue'
import { api } from '../api'
import IdLink from '../components/IdLink.vue'
import { usePaged } from '../composables/usePaged'
import { notifyError } from '../logic/notify'

const list = usePaged<AdminKYCRecord>((page, size) => api.get<Paged<AdminKYCRecord>>('/kyc/pending', { page, page_size: size }))
onMounted(list.load)

const review = reactive({ visible: false, record: null as AdminKYCRecord | null, imgFailed: false, approveConfirm: false, rejectMode: false, reason: '', error: '', busy: false })
/** 营业执照只能经后台接口读取（需要运维 / 超管会话），控制台没有读取上传文件的接口 */
const fileUrl = (key: string) => `/admin/api/v1/files/${key}`
const isPdf = (r: AdminKYCRecord) => !!r.license_file?.toLowerCase().endsWith('.pdf')
function open(r: AdminKYCRecord) {
  Object.assign(review, { visible: true, record: r, imgFailed: false, approveConfirm: false, rejectMode: false, reason: '', error: '' })
}
function openInNewWindow() {
  if (review.record?.license_file) window.open(fileUrl(review.record.license_file), '_blank', 'noopener')
}
async function submit(approve: boolean) {
  if (!review.record || review.busy) return // 同步守卫：连点只发一次
  if (!approve && !review.reason.trim()) {
    review.error = '请填写驳回原因'
    return
  }
  review.error = ''
  review.busy = true
  try {
    await api.post(`/kyc/${review.record.id}/review`, { approve, reason: approve ? '' : review.reason.trim() })
    ElMessage.success(approve ? '已通过' : '已驳回')
    review.visible = false
    await list.load()
  } catch (e) {
    notifyError(e) // 弹窗保留，驳回原因不丢
  } finally {
    review.busy = false
  }
}
</script>

<template>
  <el-card>
    <el-alert v-if="list.error.value" type="error" :title="list.error.value" show-icon :closable="false" class="error-alert" data-testid="kyc-error">
      <el-button size="small" :loading="list.loading.value" data-testid="kyc-retry" @click="list.load()">重试</el-button>
    </el-alert>
    <el-table :data="list.items.value" v-loading="list.loading.value" row-key="id">
      <el-table-column label="企业名称" min-width="200"><template #default="{ row }"><span data-testid="kyc-company">{{ row.real_name }}</span></template></el-table-column>
      <el-table-column label="统一社会信用代码" width="200"><template #default="{ row }"><span class="mono" data-testid="kyc-license-no">{{ row.license_no }}</span></template></el-table-column>
      <el-table-column label="用户" width="100"><template #default="{ row }"><span data-testid="kyc-user"><IdLink kind="user" :id="row.user_id" /></span></template></el-table-column>
      <el-table-column label="提交时间" width="160"><template #default="{ row }"><span data-testid="kyc-submitted">{{ fmtTime(row.created_at) }}</span></template></el-table-column>
      <el-table-column label="操作" width="100"><template #default="{ row }"><el-button size="small" type="primary" data-testid="kyc-review-btn" @click="open(row as AdminKYCRecord)">审核</el-button></template></el-table-column>
    </el-table>
    <div class="pager-row">
      <span class="muted" data-testid="kyc-total">共 {{ list.total.value }} 条待审核</span>
      <el-pagination v-model:current-page="list.page.value" v-model:page-size="list.size.value" :total="list.total.value" layout="prev, pager, next" data-testid="kyc-pager" />
    </div>

    <el-dialog v-model="review.visible" title="企业认证审核" width="860" align-center data-testid="kyc-review-dialog">
      <div v-if="review.record" class="review">
        <el-descriptions :column="1" border>
          <el-descriptions-item label="企业名称"><span data-testid="kyc-review-company">{{ review.record.real_name }}</span></el-descriptions-item>
          <el-descriptions-item label="信用代码"><span class="mono" data-testid="kyc-review-license-no">{{ review.record.license_no }}</span></el-descriptions-item>
          <el-descriptions-item label="用户"><span data-testid="kyc-review-user"><IdLink kind="user" :id="review.record.user_id" /></span></el-descriptions-item>
          <el-descriptions-item label="提交时间">{{ fmtTime(review.record.created_at) }}</el-descriptions-item>
        </el-descriptions>
        <div class="preview">
          <el-button v-if="isPdf(review.record)" data-testid="kyc-open-pdf" @click="openInNewWindow">在新窗口打开 PDF</el-button>
          <img v-else-if="!review.imgFailed" :src="fileUrl(review.record.license_file ?? '')" alt="营业执照" class="license" data-testid="kyc-preview-img" @error="review.imgFailed = true" />
          <div v-else class="img-failed">
            <p class="muted">图片无法预览</p>
            <el-button data-testid="kyc-open-file" @click="openInNewWindow">在新窗口打开</el-button>
          </div>
        </div>
      </div>
      <div v-if="review.rejectMode" class="reject">
        <el-input v-model="review.reason" placeholder="驳回原因（必填，会展示给用户）" data-testid="kyc-reject-reason" />
        <p v-if="review.error" class="error" data-testid="kyc-reject-error">{{ review.error }}</p>
      </div>
      <template #footer>
        <template v-if="review.approveConfirm">
          <span class="confirm">确认通过该企业认证？</span>
          <el-button data-testid="kyc-approve-cancel" @click="review.approveConfirm = false">取消</el-button>
          <el-button type="primary" :loading="review.busy" data-testid="kyc-approve-ok" @click="submit(true)">确认通过</el-button>
        </template>
        <template v-else-if="review.rejectMode">
          <el-button data-testid="kyc-reject-cancel" @click="Object.assign(review, { rejectMode: false, error: '' })">取消</el-button>
          <el-button type="danger" :loading="review.busy" data-testid="kyc-reject-submit" @click="submit(false)">确认驳回</el-button>
        </template>
        <template v-else>
          <el-button type="danger" plain data-testid="kyc-reject" @click="review.rejectMode = true">驳回</el-button>
          <el-button type="primary" data-testid="kyc-approve" @click="review.approveConfirm = true">通过</el-button>
        </template>
      </template>
    </el-dialog>
  </el-card>
</template>

<style scoped>
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.muted { color: #909399; }
.error-alert { margin-bottom: 12px; }
.pager-row { display: flex; justify-content: space-between; align-items: center; margin-top: 12px; }
.review { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; }
.preview { display: flex; align-items: center; justify-content: center; min-height: 240px; background: #f5f7fa; border-radius: 8px; }
.img-failed { text-align: center; }
.license { max-width: 100%; max-height: 420px; }
.reject { margin-top: 12px; }
.confirm { margin-right: 12px; }
.error { color: var(--el-color-danger); font-size: 13px; }
</style>
