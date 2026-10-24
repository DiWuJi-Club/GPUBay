<script setup lang="ts">
import { fmtTime, type ConversationBrief } from '@spark/shared'

/** /butler 页的历史对话列表（桌面左栏；手机放在「历史」抽屉里）。 */
defineProps<{ items: ConversationBrief[]; current: string | null; loading: boolean }>()
const emit = defineEmits<{ select: [id: string]; remove: [item: ConversationBrief] }>()
</script>

<template>
  <div class="list" data-testid="butler-list">
    <p v-if="!loading && !items.length" class="muted" data-testid="butler-list-empty">还没有对话</p>
    <div
      v-for="c in items" :key="c.id" class="item" :class="{ active: c.id === current }" data-testid="butler-list-item" :data-id="c.id"
      @click="emit('select', c.id)"
    >
      <div class="main">
        <div class="t">{{ c.title || '新对话' }}</div>
        <div class="muted small">{{ fmtTime(c.last_message_at ?? c.created_at) }}</div>
      </div>
      <el-button link type="danger" size="small" data-testid="butler-list-delete" @click.stop="emit('remove', c)">删除</el-button>
    </div>
  </div>
</template>

<style scoped>
.list { display: flex; flex-direction: column; }
.item { display: flex; align-items: center; gap: 8px; padding: 10px 12px; cursor: pointer; border-bottom: 1px solid #f2f3f5; min-height: 44px; box-sizing: border-box; }
.item:hover { background: #f5f7fa; }
.item.active { background: #ecf5ff; }
.main { flex: 1; min-width: 0; }
.t { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: 14px; }
.muted { color: #909399; padding: 0 12px; }
.small { font-size: 12px; padding: 0; }
</style>
