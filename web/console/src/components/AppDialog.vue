<script setup lang="ts">
import { useMobile } from '@spark/shared'

// beforeClose：点右上角关闭、按 Esc、点遮罩时先调用，由调用方决定是否 done()（例如明文 Key 面板关闭前二次确认）
const props = withDefaults(defineProps<{ kind?: 'confirm' | 'form'; title?: string; width?: number; beforeClose?: (done: () => void) => void }>(), {
  kind: 'confirm',
  title: '',
  width: undefined,
  beforeClose: undefined,
})
const visible = defineModel<boolean>({ required: true })
const mobile = useMobile()
</script>

<template>
  <!-- 手机：一句话确认框底部弹出（sheet，不全屏），表单弹窗全屏；电脑：居中 420 / 560 -->
  <el-dialog
    v-model="visible"
    :title="props.title"
    :fullscreen="mobile && props.kind === 'form'"
    :class="{ sheet: mobile && props.kind === 'confirm' }"
    :width="props.width ?? (props.kind === 'confirm' ? 420 : 560)"
    :align-center="!mobile"
    :before-close="props.beforeClose"
    append-to-body
    destroy-on-close
  >
    <slot />
    <template #footer><slot name="footer" /></template>
  </el-dialog>
</template>
