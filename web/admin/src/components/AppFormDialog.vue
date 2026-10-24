<script setup lang="ts">
import { appCategoryLabel, appStatusLabel, type AdminApp, type AppCategory, type AppStatus } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { reactive, ref, watch } from 'vue'
import { api } from '../api'
import { notifyError } from '../logic/notify'

// 新建 / 编辑应用（super）。slug 创建后不可改（容器内目录名），编辑时只读；状态只在编辑时可改（新建一律 draft）。
const props = defineProps<{ modelValue: boolean; app: AdminApp | null }>()
const emit = defineEmits<{ 'update:modelValue': [boolean]; saved: [AdminApp] }>()

/** 固定图标名集合（控制台映射成图标），与后端 AppInput 校验一致。 */
const ICONS = ['app', 'llm', 'chat', 'image', 'notebook', 'tool', 'code', 'audio'] as const
const CATEGORIES: AppCategory[] = ['llm', 'image', 'notebook', 'tool']
const STATUSES: AppStatus[] = ['draft', 'published', 'retired']

interface AppForm {
  slug: string
  name: string
  summary: string
  description: string
  category: AppCategory
  icon: string
  sort: number
  status: AppStatus
}
const blank = (): AppForm => ({ slug: '', name: '', summary: '', description: '', category: 'llm', icon: 'app', sort: 0, status: 'draft' })
const form = reactive<AppForm>(blank())
const busy = ref(false)

watch(
  () => props.modelValue,
  (open) => {
    if (!open) return
    const a = props.app
    Object.assign(form, a ? { slug: a.slug, name: a.name, summary: a.summary, description: a.description, category: a.category, icon: a.icon, sort: a.sort, status: a.status } : blank())
  },
  { immediate: true },
)

async function submit() {
  if (busy.value) return
  busy.value = true
  try {
    const { status, ...rest } = form
    const saved = props.app ? await api.put<AdminApp>(`/apps/${props.app.id}`, { ...rest, status }) : await api.post<AdminApp>('/apps', rest)
    ElMessage.success(props.app ? '应用已更新' : '应用已创建')
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
    :title="app ? '编辑应用' : '新建应用'"
    width="560"
    align-center
    destroy-on-close
    data-testid="apps-dialog"
    @update:model-value="(v: boolean) => emit('update:modelValue', v)"
  >
    <el-form label-position="top" @submit.prevent="submit">
      <el-form-item label="slug（小写字母开头，2～32 位 [a-z0-9-]；创建后不可修改）">
        <el-input v-model="form.slug" :disabled="!!app" maxlength="32" data-testid="apps-slug" />
      </el-form-item>
      <el-form-item label="名称（对用户展示，1～32 字）"><el-input v-model="form.name" maxlength="32" data-testid="apps-name" /></el-form-item>
      <el-form-item label="一句话简介（≤ 80 字）"><el-input v-model="form.summary" maxlength="80" data-testid="apps-summary" /></el-form-item>
      <el-form-item label="介绍（≤ 4000 字，支持段落、列表、代码、链接）">
        <el-input v-model="form.description" type="textarea" :rows="5" maxlength="4000" data-testid="apps-description" />
      </el-form-item>
      <div class="three">
        <el-form-item label="分类">
          <el-select v-model="form.category" data-testid="apps-category">
            <el-option v-for="c in CATEGORIES" :key="c" :label="appCategoryLabel[c]" :value="c" />
          </el-select>
        </el-form-item>
        <el-form-item label="图标">
          <el-select v-model="form.icon" data-testid="apps-icon">
            <el-option v-for="i in ICONS" :key="i" :label="i" :value="i" />
          </el-select>
        </el-form-item>
        <el-form-item label="排序（小的在前）"><el-input-number v-model="form.sort" :controls="false" data-testid="apps-sort" /></el-form-item>
      </div>
      <el-form-item v-if="app" label="状态">
        <el-select v-model="form.status" data-testid="apps-status">
          <el-option v-for="s in STATUSES" :key="s" :label="appStatusLabel[s]" :value="s" />
        </el-select>
      </el-form-item>
      <p class="muted">草稿只在后台可见；已发布且至少有一个已发布版本才出现在控制台；退役后不能新部署，已有部署照常运行。</p>
      <el-button type="primary" native-type="submit" :loading="busy" data-testid="apps-submit">保存</el-button>
    </el-form>
  </el-dialog>
</template>

<style scoped>
.three { display: grid; grid-template-columns: 1fr 1fr 1fr; gap: 12px; }
.muted { color: #909399; font-size: 13px; }
</style>
