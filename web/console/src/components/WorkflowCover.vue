<script setup lang="ts">
import type { WorkflowCatalogItem } from '@spark/shared'
import { computed } from 'vue'
import { coverOrPlaceholder } from '../logic/workflows'

/** 工作流封面（16:9）：有 cover_url 用图片，否则按分类的渐变占位（市场卡片与详情页共用）。 */
const props = defineProps<{ item: Pick<WorkflowCatalogItem, 'cover_url' | 'category'> }>()
const cover = computed(() => coverOrPlaceholder(props.item))
</script>

<template>
  <span class="wf-cover">
    <img v-if="cover.kind === 'image'" :src="cover.url" alt="" loading="lazy" />
    <span v-else class="placeholder" :class="`cat-${cover.category}`" data-testid="wf-cover-placeholder">{{ cover.label }}</span>
  </span>
</template>

<style scoped>
.wf-cover { display: block; width: 100%; aspect-ratio: 16 / 9; background: #f5f7fa; overflow: hidden; }
.wf-cover img { width: 100%; height: 100%; object-fit: cover; display: block; }
.placeholder { width: 100%; height: 100%; display: flex; align-items: center; justify-content: center; color: #fff; font-size: 20px; font-weight: 600; background: #909399; }
.cat-txt2img { background: linear-gradient(135deg, #409eff, #79bbff); }
.cat-img2img { background: linear-gradient(135deg, #e6a23c, #f3d19e); }
.cat-video { background: linear-gradient(135deg, #f56c6c, #fab6b6); }
.cat-upscale { background: linear-gradient(135deg, #67c23a, #b3e19d); }
.cat-other { background: linear-gradient(135deg, #909399, #c8c9cc); }
</style>
