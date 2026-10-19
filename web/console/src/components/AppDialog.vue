<script setup lang="ts">
import { useMobile } from '@spark/shared'

const props = withDefaults(defineProps<{ kind?: 'confirm' | 'form'; title?: string; width?: number }>(), { kind: 'confirm', title: '', width: undefined })
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
    append-to-body
    destroy-on-close
  >
    <slot />
    <template #footer><slot name="footer" /></template>
  </el-dialog>
</template>
