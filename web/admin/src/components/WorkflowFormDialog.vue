<script setup lang="ts">
import { appStatusLabel, messageOf, workflowCategoryLabel, type AdminWorkflow, type AdminWorkflowInput, type WorkflowCategory, type WorkflowStatus } from '@spark/shared'
import { ElMessage, type UploadFile } from 'element-plus'
import { computed, onBeforeUnmount, reactive, ref, watch } from 'vue'
import { api } from '../api'
import { COVER_TYPES, coverIssue, LIMITS, parseTags, WORKFLOW_CATEGORIES, workflowIssue } from '../logic/workflowManifest'
import { notifyError } from '../logic/notify'

// 新建 / 编辑工作流（super，计划 11 规格 §12）：slug 创建后不可改（容器内文件名 spark-<slug>.json）；状态只在编辑时可改。
// 封面（D13：PNG / JPG / WebP ≤ 2MB）先在本地选好预览，保存基本信息后再上传（新建时要先有工作流 id）。
const props = defineProps<{ modelValue: boolean; workflow: AdminWorkflow | null }>()
const emit = defineEmits<{ 'update:modelValue': [boolean]; saved: [AdminWorkflow] }>()

const STATUSES: WorkflowStatus[] = ['draft', 'published', 'retired']

interface Form {
  slug: string
  name: string
  summary: string
  description: string
  category: WorkflowCategory
  tags: string
  sort: number
  status: WorkflowStatus
}
const blank = (): Form => ({ slug: '', name: '', summary: '', description: '', category: 'txt2img', tags: '', sort: 0, status: 'draft' })
const form = reactive<Form>(blank())
const busy = ref(false)
const touched = ref(false)
const cover = reactive({ file: null as File | null, url: '', error: '' })
const issue = computed(() => workflowIssue({ ...form, sort: Number(form.sort ?? 0) }, !props.workflow))
const preview = computed(() => cover.url || props.workflow?.cover_url || '')

function clearCover() {
  if (cover.url) URL.revokeObjectURL(cover.url)
  Object.assign(cover, { file: null, url: '', error: '' })
}
watch(
  () => props.modelValue,
  (open) => {
    clearCover()
    touched.value = false
    if (!open) return
    const w = props.workflow
    Object.assign(form, w
      ? { slug: w.slug, name: w.name, summary: w.summary, description: w.description, category: w.category, tags: w.tags.join(', '), sort: w.sort, status: w.status }
      : blank())
  },
  { immediate: true },
)
onBeforeUnmount(clearCover)

function onCoverChange(f: UploadFile) {
  const raw = f.raw
  if (!raw) return
  const err = coverIssue(raw)
  clearCover()
  if (err) {
    cover.error = err
    return
  }
  Object.assign(cover, { file: raw, url: URL.createObjectURL(raw) })
}

async function submit() {
  touched.value = true
  if (busy.value || issue.value) return
  busy.value = true
  try {
    const body: AdminWorkflowInput = {
      slug: form.slug.trim(), name: form.name.trim(), summary: form.summary.trim(), description: form.description, category: form.category,
      tags: parseTags(form.tags), sort: Number(form.sort ?? 0),
    }
    let saved = props.workflow ? await api.put<AdminWorkflow>(`/workflows/${props.workflow.id}`, { ...body, status: form.status }) : await api.post<AdminWorkflow>('/workflows', body)
    if (cover.file) {
      try {
        saved = await api.upload<AdminWorkflow>(`/workflows/${saved.id}/cover`, cover.file)
      } catch (e) {
        if (props.workflow) {
          // 基本信息已保存：弹窗保留以便换一张封面重试
          cover.error = `封面上传失败：${messageOf(e)}`
          emit('saved', saved)
          return
        }
        ElMessage.warning(`工作流已创建，但封面上传失败（${messageOf(e)}），可在详情页「编辑」重新上传`)
        emit('update:modelValue', false)
        emit('saved', saved)
        return
      }
    }
    ElMessage.success(props.workflow ? '工作流已更新' : '工作流已创建')
    emit('update:modelValue', false)
    emit('saved', saved)
  } catch (e) {
    notifyError(e) // duplicate（slug 已存在）/ bad_request 原文；弹窗保留
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <el-dialog
    :model-value="modelValue"
    :title="workflow ? '编辑工作流' : '新建工作流'"
    width="620"
    align-center
    destroy-on-close
    data-testid="adm-wf-dialog"
    @update:model-value="(v: boolean) => emit('update:modelValue', v)"
  >
    <el-form label-position="top" @submit.prevent="submit">
      <div class="top">
        <div class="fields">
          <el-form-item label="slug（小写字母开头，2～48 位 [a-z0-9-]；创建后不可修改）">
            <el-input v-model="form.slug" :disabled="!!workflow" maxlength="48" class="mono" data-testid="adm-wf-slug" />
          </el-form-item>
          <el-form-item :label="`名称（对用户展示，1～${LIMITS.name} 字）`"><el-input v-model="form.name" :maxlength="LIMITS.name" data-testid="adm-wf-name" /></el-form-item>
        </div>
        <div class="cover">
          <div class="cover-box">
            <img v-if="preview" :src="preview" alt="封面" data-testid="adm-wf-cover-preview" />
            <span v-else class="muted">无封面</span>
          </div>
          <el-upload
            :auto-upload="false"
            :show-file-list="false"
            :accept="COVER_TYPES.join(',')"
            :on-change="onCoverChange"
            data-testid="adm-wf-cover-upload"
          >
            <el-button size="small">{{ preview ? '更换封面' : '选择封面' }}</el-button>
          </el-upload>
          <span class="muted small">PNG / JPG / WebP，≤ 2MB</span>
        </div>
      </div>
      <div v-if="cover.error" class="field-error" data-testid="adm-wf-cover-error">{{ cover.error }}</div>
      <el-form-item :label="`一句话简介（≤ ${LIMITS.summary} 字）`"><el-input v-model="form.summary" :maxlength="LIMITS.summary" data-testid="adm-wf-summary" /></el-form-item>
      <el-form-item :label="`介绍（≤ ${LIMITS.description} 字，支持段落、列表、链接）`">
        <el-input v-model="form.description" type="textarea" :rows="5" :maxlength="LIMITS.description" data-testid="adm-wf-description" />
      </el-form-item>
      <div class="three">
        <el-form-item label="分类">
          <el-select v-model="form.category" data-testid="adm-wf-category">
            <el-option v-for="c in WORKFLOW_CATEGORIES" :key="c" :label="workflowCategoryLabel[c]" :value="c" />
          </el-select>
        </el-form-item>
        <el-form-item label="标签（逗号分隔，≤ 8 个，每个 ≤ 16 字）"><el-input v-model="form.tags" placeholder="SDXL, 入门" data-testid="adm-wf-tags" /></el-form-item>
        <el-form-item label="排序（小的在前）"><el-input-number v-model="form.sort" :min="-10000" :max="10000" :controls="false" data-testid="adm-wf-sort" /></el-form-item>
      </div>
      <el-form-item v-if="workflow" label="状态">
        <el-select v-model="form.status" data-testid="adm-wf-status">
          <el-option v-for="s in STATUSES" :key="s" :label="appStatusLabel[s]" :value="s" />
        </el-select>
      </el-form-item>
      <p class="muted">草稿只在后台可见；已发布且至少有一个已发布版本才出现在控制台工作流市场；退役后不能新安装，已装的照常使用。</p>
      <div v-if="touched && issue" class="field-error" data-testid="adm-wf-form-error">{{ issue }}</div>
      <el-button type="primary" native-type="submit" :loading="busy" data-testid="adm-wf-submit">保存</el-button>
    </el-form>
  </el-dialog>
</template>

<style scoped>
.top { display: grid; grid-template-columns: 1fr 180px; gap: 16px; }
.cover { display: flex; flex-direction: column; align-items: center; gap: 6px; }
.cover-box { width: 180px; height: 110px; border: 1px dashed var(--el-border-color); border-radius: 4px; display: flex; align-items: center; justify-content: center; overflow: hidden; background: var(--el-fill-color-lighter); }
.cover-box img { width: 100%; height: 100%; object-fit: cover; }
.three { display: grid; grid-template-columns: 1fr 1.6fr 1fr; gap: 12px; }
.muted { color: #909399; font-size: 13px; }
.small { font-size: 12px; }
.mono :deep(input) { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.field-error { color: var(--el-color-danger); font-size: 12px; line-height: 1.5; margin: 0 0 8px; }
</style>
