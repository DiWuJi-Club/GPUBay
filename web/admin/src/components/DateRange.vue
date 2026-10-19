<script setup lang="ts">
const model = defineModel<{ from: string; to: string }>({ required: true })
defineProps<{ prefix: string }>()
</script>

<template>
  <!-- 原生日期输入：用户选的日期按 YYYY-MM-DD 原样发给后端（后端按北京时间解释，to 含当天） -->
  <span class="range">
    <input v-model="model.from" type="date" class="el-input__inner native-date" :data-testid="`${prefix}-from`" :max="model.to || undefined" />
    <span class="sep">至</span>
    <input v-model="model.to" type="date" class="el-input__inner native-date" :data-testid="`${prefix}-to`" :min="model.from || undefined" />
  </span>
</template>

<style scoped>
.range { display: inline-flex; align-items: center; gap: 6px; }
.native-date { height: 32px; border: 1px solid #dcdfe6; border-radius: 4px; padding: 0 8px; font-size: 14px; background: #fff; }
.sep { color: #909399; }
</style>
