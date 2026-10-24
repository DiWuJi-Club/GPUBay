<script setup lang="ts">
import type { AppCatalogItem } from '@spark/shared'

/**
 * 应用小卡片列表（图标、名称、summary）。disabled 返回灰显原因（null = 可选）；原因直接显示在卡片里（手机没有悬停）。
 * testid 为 data-testid 前缀：卡片 `<testid>-<slug>`、原因 `<testid>-<slug>-reason`。
 */
const props = withDefaults(defineProps<{ apps: AppCatalogItem[]; disabled?: (app: AppCatalogItem) => string | null; testid?: string }>(), {
  disabled: () => null,
  testid: 'app-pick',
})
const model = defineModel<number | null>({ required: true })

function pick(a: AppCatalogItem) {
  if (props.disabled(a)) return
  model.value = a.id
}
</script>

<template>
  <div class="app-picker">
    <button
      v-for="a in apps" :key="a.id" type="button" class="app" :class="{ picked: a.id === model, off: !!disabled(a) }"
      :aria-pressed="a.id === model" :aria-disabled="disabled(a) ? 'true' : undefined" :title="disabled(a) ?? a.summary" :data-testid="`${testid}-${a.slug}`" @click="pick(a)"
    >
      <span class="icon" :class="`cat-${a.category}`" aria-hidden="true">{{ a.name.slice(0, 1) }}</span>
      <span class="body">
        <span class="name">{{ a.name }}</span>
        <span class="summary">{{ a.summary }}</span>
        <span v-if="disabled(a)" class="reason" :data-testid="`${testid}-${a.slug}-reason`">{{ disabled(a) }}</span>
      </span>
    </button>
  </div>
</template>

<style scoped>
.app-picker { display: grid; gap: 8px; grid-template-columns: repeat(auto-fill, minmax(180px, 1fr)); }
.app { display: flex; gap: 8px; align-items: flex-start; text-align: left; padding: 8px; border: 1px solid #dcdfe6; border-radius: 6px; background: #fff; cursor: pointer; font: inherit; color: inherit; }
.app:hover:not(.off) { border-color: var(--el-color-primary); }
.app.picked { border-color: var(--el-color-primary); background: var(--el-color-primary-light-9); }
.app.off { cursor: not-allowed; background: #f5f7fa; color: #c0c4cc; }
.icon { flex: none; width: 28px; height: 28px; border-radius: 6px; display: flex; align-items: center; justify-content: center; font-weight: 600; color: #fff; background: #909399; }
.cat-llm { background: #409eff; }
.cat-image { background: #e6a23c; }
.cat-notebook { background: #67c23a; }
.cat-tool { background: #909399; }
.app.off .icon { opacity: 0.5; }
.body { display: flex; flex-direction: column; min-width: 0; }
.name { font-weight: 600; font-size: 13px; }
.summary { font-size: 12px; color: #909399; overflow: hidden; text-overflow: ellipsis; display: -webkit-box; -webkit-line-clamp: 2; line-clamp: 2; -webkit-box-orient: vertical; }
.reason { font-size: 12px; color: var(--el-color-warning); }
</style>
