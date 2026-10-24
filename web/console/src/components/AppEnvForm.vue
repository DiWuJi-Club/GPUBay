<script setup lang="ts">
import type { AppEnvSpec } from '@spark/shared'
import { reactive } from 'vue'
import { canGenerate, generateSecret, validateEnvValue } from '../logic/apps'

/**
 * 应用变量表单（预装 / 添加 / 重新部署共用）。只渲染 el-form-item，由调用方包在 el-form（label-position="top"）里。
 * secret 从不回显：重新部署时 secretSet 标出已设置的项，占位「已设置，留空沿用」，留空即沿用。
 */
const props = defineProps<{ spec: AppEnvSpec[]; secretSet?: Record<string, boolean> }>()
const model = defineModel<Record<string, string>>({ required: true })

const errors = reactive<Record<string, string>>({})
const revealed = reactive<Record<string, boolean>>({}) // 「生成」后明文显示，便于复制

const val = (e: AppEnvSpec) => model.value[e.name] ?? ''
const isSet = (e: AppEnvSpec) => e.type === 'secret' && !!props.secretSet?.[e.name]
const mustFill = (e: AppEnvSpec) => !!e.required && !e.default && !isSet(e)
const intVal = (e: AppEnvSpec) => (val(e) === '' || Number.isNaN(Number(val(e))) ? undefined : Number(val(e)))

function set(name: string, v: string) {
  model.value = { ...model.value, [name]: v }
  delete errors[name]
}
function setInt(name: string, n: number | null | undefined) {
  set(name, n == null ? '' : String(n))
}
function gen(e: AppEnvSpec) {
  const s = generateSecret(e)
  if (!s) return
  set(e.name, s)
  revealed[e.name] = true
}
function placeholder(e: AppEnvSpec): string {
  if (isSet(e)) return '已设置，留空沿用'
  if (e.default) return `默认 ${e.default}`
  return e.required ? '' : '可留空'
}

/** 逐项预校验（服务器仍会再校验），错误显示在该项下；全部通过返回 true。 */
function validate(): boolean {
  let ok = true
  for (const e of props.spec) {
    const v = val(e)
    const err = v === '' && isSet(e) ? null : validateEnvValue(e, v)
    if (err) {
      errors[e.name] = err
      ok = false
    } else {
      delete errors[e.name]
    }
  }
  return ok
}
defineExpose({ validate })
</script>

<template>
  <div class="app-env-form" data-testid="app-env-form">
    <el-form-item v-for="e in spec" :key="e.name" :label="e.label || e.name" :required="mustFill(e)" :class="{ 'is-error': !!errors[e.name] }">
      <el-select v-if="e.type === 'enum'" :model-value="val(e)" :clearable="!e.required" :placeholder="placeholder(e)" :data-testid="`app-env-${e.name}`" @update:model-value="(v: string | undefined) => set(e.name, v ?? '')">
        <el-option v-for="o in e.options ?? []" :key="o" :value="o" :label="o" />
      </el-select>
      <el-input-number
        v-else-if="e.type === 'int'" :model-value="intVal(e)" :min="e.min ?? -Infinity" :max="e.max ?? Infinity" :step="1" :precision="0"
        :placeholder="placeholder(e)" controls-position="right" :data-testid="`app-env-${e.name}`" @update:model-value="(n: number | null | undefined) => setInt(e.name, n)"
      />
      <el-switch v-else-if="e.type === 'bool'" :model-value="val(e)" active-value="true" inactive-value="false" :data-testid="`app-env-${e.name}`" @update:model-value="(v: string | number | boolean) => set(e.name, String(v))" />
      <div v-else-if="e.type === 'secret'" class="secret">
        <el-input
          :model-value="val(e)" :type="revealed[e.name] ? 'text' : 'password'" :show-password="!revealed[e.name]" autocomplete="new-password"
          :placeholder="placeholder(e)" :data-testid="`app-env-${e.name}`" @update:model-value="(v: string) => set(e.name, v)"
        />
        <el-button v-if="canGenerate(e)" :data-testid="`app-env-${e.name}-gen`" @click="gen(e)">生成</el-button>
      </div>
      <el-input v-else :model-value="val(e)" :placeholder="placeholder(e)" clearable :data-testid="`app-env-${e.name}`" @update:model-value="(v: string) => set(e.name, v)" />
      <div v-if="errors[e.name]" class="err" :data-testid="`app-env-${e.name}-error`">{{ errors[e.name] }}</div>
      <div v-if="revealed[e.name] && val(e)" class="help">已生成，请复制保存；部署后不再显示</div>
      <div v-if="e.help" class="help">{{ e.help }}</div>
    </el-form-item>
  </div>
</template>

<style scoped>
.secret { display: flex; gap: 8px; width: 100%; }
.err { color: var(--el-color-danger); font-size: 12px; line-height: 1.5; width: 100%; }
.help { color: #909399; font-size: 12px; line-height: 1.5; width: 100%; }
</style>
