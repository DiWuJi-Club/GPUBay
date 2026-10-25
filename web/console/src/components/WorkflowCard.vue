<script setup lang="ts">
import { formatGB, type WorkflowCatalogItem } from '@spark/shared'
import { computed } from 'vue'
import WorkflowCover from './WorkflowCover.vue'

/** 工作流市场的卡片：封面（无则分类占位）、名称、简介、标签、下载量、节点数、快速出图标记、「一键安装」。 */
const props = defineProps<{ item: WorkflowCatalogItem }>()
const emit = defineEmits<{ install: [item: WorkflowCatalogItem] }>()
const to = computed(() => `/workflows/${encodeURIComponent(props.item.slug)}`)
</script>

<template>
  <el-card class="wf-card" :body-style="{ padding: '0' }" :data-testid="`wf-card-${item.slug}`">
    <!-- 封面链接与名称链接同目标：封面不进 Tab 顺序、读屏跳过 -->
    <router-link :to="to" class="cover" tabindex="-1" aria-hidden="true"><WorkflowCover :item="item" /></router-link>
    <div class="body">
      <router-link :to="to" class="name" data-testid="wf-card-name">{{ item.name }}</router-link>
      <p class="summary">{{ item.summary }}</p>
      <div class="tags">
        <el-tag v-for="t in item.tags" :key="t" size="small" effect="plain">{{ t }}</el-tag>
        <el-tag v-if="item.version.quick_run" size="small" type="success" data-testid="wf-card-quickrun">支持快速出图</el-tag>
      </div>
      <div class="meta">
        <span data-testid="wf-card-size">需下载约 {{ formatGB(item.version.download_gb) }}</span>
        <span v-if="item.version.nodes > 0" data-testid="wf-card-nodes">含 {{ item.version.nodes }} 个自定义节点</span>
      </div>
      <el-button type="primary" class="install" data-testid="wf-install-btn" @click="emit('install', item)">一键安装</el-button>
    </div>
  </el-card>
</template>

<style scoped>
.wf-card { display: flex; flex-direction: column; }
.cover { display: block; }
.body { padding: 12px 16px 16px; display: flex; flex-direction: column; gap: 8px; }
.name { font-weight: 600; color: #303133; text-decoration: none; }
.name:hover { color: var(--el-color-primary); }
.summary { margin: 0; color: #606266; font-size: 13px; overflow: hidden; text-overflow: ellipsis; display: -webkit-box; -webkit-line-clamp: 2; line-clamp: 2; -webkit-box-orient: vertical; }
.tags { display: flex; flex-wrap: wrap; gap: 4px; }
.meta { display: flex; flex-wrap: wrap; gap: 12px; color: #909399; font-size: 12px; }
.install { width: 100%; }
</style>
