<script setup lang="ts">
import { butler, consumeContext } from '../store/butler'
import ButlerChat from './ButlerChat.vue'

/** 桌面端 AI 管家抽屉（420px）：状态在 store/butler，关闭抽屉不清对话、草稿与上下文；首次打开后聊天区常驻（不随关闭销毁）。 */
function setConversation(id: string | null) {
  butler.conversationId = id
}
function setDraft(t: string) {
  butler.draft = t
}
</script>

<template>
  <el-drawer v-model="butler.open" title="AI 管家" size="420px" append-to-body class="butler-drawer" data-testid="butler-drawer">
    <ButlerChat
      :conversation-id="butler.conversationId" :context="butler.pendingContext" :initial-draft="butler.draft"
      @update:conversation-id="setConversation"
      @update:draft="setDraft"
      @context-used="consumeContext()"
    />
  </el-drawer>
</template>

<style>
/* 抽屉挂在 body 下，不能用 scoped */
.butler-drawer .el-drawer__header { margin-bottom: 0; padding: 12px 16px; border-bottom: 1px solid #ebeef5; }
.butler-drawer .el-drawer__body { padding: 0; display: flex; flex-direction: column; overflow: hidden; }
</style>
