<script setup lang="ts">
import type { Guide } from '../composables/useGuide'
import AppDialog from './AppDialog.vue'

const props = defineProps<{ guide: Guide }>()
</script>

<template>
  <AppDialog v-model="props.guide.state.visible" kind="confirm" :title="props.guide.state.title">
    <p data-testid="guide-dialog">{{ props.guide.state.message }}</p>
    <template #footer>
      <el-button data-testid="guide-cancel" @click="props.guide.state.visible = false">取消</el-button>
      <el-button type="primary" data-testid="guide-action" @click="props.guide.go()">{{ props.guide.state.action }}</el-button>
    </template>
  </AppDialog>
</template>
