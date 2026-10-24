<script setup lang="ts">
import type { WorkflowInput } from '@spark/shared'
import type { UploadRequestOptions, UploadRawFile } from 'element-plus'
import { onBeforeUnmount, reactive } from 'vue'
import { inputRequired, quickRunErrorText, validateValue, validateValues, type QuickValue, type QuickValues } from '../logic/quickrun'

/**
 * 快速出图表单：按工作流版本的 inputs 渲染（规格 §4）。textarea / text → el-input，int / float → el-input-number，bool → switch，
 * enum → select，seed → 文本框（留空 = 随机，D11），image → 上传框（调 …/uploads，显示本地缩略图）。
 * 校验镜像后端 BuildPrompt（logic/quickrun.ts），服务器仍会再校验。
 */
const props = defineProps<{ inputs: WorkflowInput[]; upload: (file: File) => Promise<string>; disabled?: boolean }>()
const model = defineModel<QuickValues>({ required: true })

const MAX_UPLOAD = 16 << 20 // D10：上传图片 ≤ 16MB
const IMAGE_TYPES = ['image/png', 'image/jpeg', 'image/webp']

const errors = reactive<Record<string, string>>({})
const previews = reactive<Record<string, string>>({})
const uploading = reactive<Record<string, boolean>>({})

const val = (i: WorkflowInput) => model.value[i.key]
const numVal = (i: WorkflowInput) => (typeof val(i) === 'number' ? (val(i) as number) : undefined)
const strVal = (i: WorkflowInput) => (typeof val(i) === 'string' ? (val(i) as string) : '')
const hasDefault = (i: WorkflowInput) => i.default !== undefined && i.default !== null
let disposed = false // 上传返回前表单已卸载（抽屉关闭）：丢弃结果，不再创建缩略图 URL

function set(key: string, v: QuickValue) {
  model.value = { ...model.value, [key]: v }
  delete errors[key]
}
function setNum(i: WorkflowInput, n: number | null | undefined) {
  set(i.key, n == null || Number.isNaN(n) ? null : n)
}
/** 失焦时即时提示该项（提交前再整体校验）。 */
function check(i: WorkflowInput) {
  const r = validateValue(i, val(i))
  if (r) errors[i.key] = r
  else delete errors[i.key]
}

function placeholder(i: WorkflowInput): string {
  if (i.type === 'seed') return '留空或 -1 = 随机'
  if (hasDefault(i)) return `默认 ${String(i.default)}`
  return ''
}
function rangeHelp(i: WorkflowInput): string {
  if (i.type !== 'int' && i.type !== 'float') return ''
  const parts: string[] = []
  if (i.min != null && i.max != null) parts.push(`${i.min}～${i.max}`)
  else if (i.min != null) parts.push(`≥ ${i.min}`)
  else if (i.max != null) parts.push(`≤ ${i.max}`)
  if (i.type === 'int' && i.step) parts.push(`步长 ${i.step}`)
  return parts.join('，')
}

function beforeUpload(i: WorkflowInput, f: UploadRawFile): boolean {
  if (!IMAGE_TYPES.includes(f.type)) {
    errors[i.key] = '只支持 PNG / JPG / WebP 图片'
    return false
  }
  if (f.size > MAX_UPLOAD) {
    errors[i.key] = '图片不能超过 16MB'
    return false
  }
  return true
}
async function doUpload(i: WorkflowInput, opt: UploadRequestOptions) {
  uploading[i.key] = true
  delete errors[i.key]
  try {
    const name = await props.upload(opt.file)
    if (disposed) return
    if (previews[i.key]) URL.revokeObjectURL(previews[i.key])
    previews[i.key] = URL.createObjectURL(opt.file)
    set(i.key, name)
  } catch (e) {
    if (!disposed) errors[i.key] = `上传失败：${quickRunErrorText(e)}`
  } finally {
    if (!disposed) uploading[i.key] = false
  }
}
onBeforeUnmount(() => {
  disposed = true
  for (const u of Object.values(previews)) URL.revokeObjectURL(u)
})

/** 整体预校验，错误显示在各项下；全部通过返回 true。 */
function validate(): boolean {
  const r = validateValues(props.inputs, model.value)
  for (const i of props.inputs) {
    if (r.errors[i.key]) errors[i.key] = r.errors[i.key]
    else delete errors[i.key]
  }
  return r.ok
}
defineExpose({ validate })
</script>

<template>
  <el-form label-position="top" class="qr-form" data-testid="qr-form" @submit.prevent>
    <p v-if="!inputs.length" class="help">这个工作流没有可调参数，将按默认设置运行。</p>
    <el-form-item v-for="i in inputs" :key="i.key" :label="i.label || i.key" :required="inputRequired(i)" :class="{ 'is-error': !!errors[i.key] }">
      <el-input
        v-if="i.type === 'textarea'" type="textarea" :autosize="{ minRows: 3, maxRows: 10 }" :model-value="strVal(i)" :placeholder="placeholder(i)"
        :disabled="disabled" :data-testid="`qr-input-${i.key}`" @update:model-value="(v: string) => set(i.key, v)" @blur="check(i)"
      />
      <el-input-number
        v-else-if="i.type === 'int' || i.type === 'float'" :model-value="numVal(i)" :min="i.min ?? -Infinity" :max="i.max ?? Infinity"
        :step="i.type === 'int' ? i.step || 1 : 0.1" :precision="i.type === 'int' ? 0 : undefined" :placeholder="placeholder(i)"
        controls-position="right" :disabled="disabled" :data-testid="`qr-input-${i.key}`"
        @update:model-value="(n: number | null | undefined) => setNum(i, n)" @blur="check(i)"
      />
      <el-switch v-else-if="i.type === 'bool'" :model-value="val(i) === true" :disabled="disabled" :data-testid="`qr-input-${i.key}`" @update:model-value="(v: string | number | boolean) => set(i.key, v === true)" />
      <el-select
        v-else-if="i.type === 'enum'" :model-value="strVal(i)" :clearable="hasDefault(i)" :placeholder="placeholder(i)" :disabled="disabled"
        :data-testid="`qr-input-${i.key}`" @update:model-value="(v: string | undefined) => set(i.key, v ?? '')"
      >
        <el-option v-for="o in i.options ?? []" :key="o" :value="o" :label="o" />
      </el-select>
      <div v-else-if="i.type === 'seed'" class="seed">
        <el-input
          :model-value="strVal(i)" inputmode="numeric" :placeholder="placeholder(i)" clearable :disabled="disabled" :data-testid="`qr-input-${i.key}`"
          @update:model-value="(v: string) => set(i.key, v)" @blur="check(i)"
        />
        <el-button :disabled="disabled || strVal(i) === ''" :data-testid="`qr-input-${i.key}-random`" @click="set(i.key, '')">随机</el-button>
      </div>
      <div v-else-if="i.type === 'image'" class="image">
        <el-upload
          :show-file-list="false" accept="image/png,image/jpeg,image/webp" :disabled="disabled || uploading[i.key]"
          :before-upload="(f: UploadRawFile) => beforeUpload(i, f)" :http-request="(o: UploadRequestOptions) => doUpload(i, o)" :data-testid="`qr-input-${i.key}`"
        >
          <el-button :loading="uploading[i.key]" :disabled="disabled">{{ strVal(i) ? '重新上传' : '上传图片' }}</el-button>
        </el-upload>
        <img v-if="previews[i.key] && strVal(i)" :src="previews[i.key]" alt="" class="thumb" :data-testid="`qr-input-${i.key}-preview`">
        <span v-if="strVal(i)" class="help" :data-testid="`qr-input-${i.key}-name`">已上传</span>
        <span v-else class="help">PNG / JPG / WebP，不超过 16MB</span>
      </div>
      <el-input
        v-else :model-value="strVal(i)" :placeholder="placeholder(i)" :disabled="disabled" :data-testid="`qr-input-${i.key}`"
        @update:model-value="(v: string) => set(i.key, v)" @blur="check(i)"
      />
      <div v-if="errors[i.key]" class="err" :data-testid="`qr-input-${i.key}-error`">{{ errors[i.key] }}</div>
      <div v-else-if="rangeHelp(i)" class="help">{{ rangeHelp(i) }}</div>
    </el-form-item>
  </el-form>
</template>

<style scoped>
.seed { display: flex; gap: 8px; width: 100%; }
.image { display: flex; align-items: center; gap: 12px; flex-wrap: wrap; width: 100%; }
.thumb { width: 72px; height: 72px; object-fit: cover; border-radius: 4px; border: 1px solid var(--el-border-color-lighter); }
.err { color: var(--el-color-danger); font-size: 12px; line-height: 1.5; width: 100%; }
.help { color: #909399; font-size: 12px; line-height: 1.5; }
</style>
