<script setup lang="ts">
import { useMobile, type ConversationBrief } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { onMounted, ref } from 'vue'
import { api } from '../api'
import AppDialog from '../components/AppDialog.vue'
import ButlerChat from '../components/ButlerChat.vue'
import ButlerHistory from '../components/ButlerHistory.vue'
import { notifyError } from '../logic/notify'
import { butler, consumeContext } from '../store/butler'

/**
 * /butler 全页（D13）：桌面左侧历史对话 + 右侧聊天；手机只有聊天区，顶部「历史」按钮弹出列表。
 * 当前对话、草稿、实例上下文与抽屉共用 store/butler（本页不显示抽屉与浮动按钮）。
 */
const mobile = useMobile()
const items = ref<ConversationBrief[]>([])
const loading = ref(false)
const historyOpen = ref(false)
let seq = 0

butler.open = false

async function loadList() {
  const mine = ++seq
  loading.value = true
  try {
    const res = await api.get<{ items: ConversationBrief[] }>('/butler/conversations', { limit: 50 })
    if (mine === seq) items.value = res.items ?? []
  } catch (e) {
    if (mine === seq) notifyError(e)
  } finally {
    if (mine === seq) loading.value = false
  }
}
onMounted(loadList)

function setDraft(t: string) {
  butler.draft = t
}

function select(id: string) {
  butler.conversationId = id
  historyOpen.value = false
}

function onConversation(id: string | null) {
  butler.conversationId = id
  if (id && !items.value.some((c) => c.id === id)) void loadList() // 新建的对话出现在列表里
}

// ---- 删除 ----
const removeTarget = ref<ConversationBrief | null>(null)
const removing = ref(false)
async function confirmRemove() {
  const c = removeTarget.value
  if (!c || removing.value) return
  removing.value = true
  try {
    await api.del<{ ok: boolean }>(`/butler/conversations/${c.id}`)
    if (butler.conversationId === c.id) butler.conversationId = null
    ElMessage.success('已删除')
  } catch (e) {
    notifyError(e)
  } finally {
    removing.value = false
    removeTarget.value = null
    await loadList()
  }
}
</script>

<template>
  <div class="butler-page" :class="{ mobile }" data-testid="butler-page">
    <aside v-if="!mobile" class="side">
      <ButlerHistory :items="items" :current="butler.conversationId" :loading="loading" @select="select" @remove="(c) => (removeTarget = c)" />
    </aside>
    <section class="chat-col">
      <div v-if="mobile" class="mbar">
        <el-button size="small" data-testid="butler-history" @click="historyOpen = true">历史</el-button>
      </div>
      <ButlerChat
        :conversation-id="butler.conversationId" :context="butler.pendingContext" :initial-draft="butler.draft"
        @update:conversation-id="onConversation"
        @update:draft="setDraft"
        @context-used="consumeContext()"
        @settled="loadList"
      />
    </section>

    <el-drawer v-if="mobile" v-model="historyOpen" title="历史对话" direction="ltr" size="85%" append-to-body data-testid="butler-history-drawer">
      <ButlerHistory :items="items" :current="butler.conversationId" :loading="loading" @select="select" @remove="(c) => (removeTarget = c)" />
    </el-drawer>

    <AppDialog
      :model-value="!!removeTarget" kind="confirm" title="删除对话" data-testid="butler-delete-dialog"
      @update:model-value="(v: boolean) => { if (!v) removeTarget = null }"
    >
      <p>删除后无法恢复：「{{ removeTarget?.title || '新对话' }}」</p>
      <template #footer>
        <el-button :disabled="removing" data-testid="butler-delete-cancel" @click="removeTarget = null">取消</el-button>
        <el-button type="danger" :loading="removing" data-testid="butler-delete-ok" @click="confirmRemove">删除</el-button>
      </template>
    </AppDialog>
  </div>
</template>

<style scoped>
/* 顶栏 56px + 内容区上下内边距 32px */
.butler-page { display: flex; height: calc(100vh - 56px - 32px); height: calc(100dvh - 56px - 32px); min-height: 420px; background: #fff; border: 1px solid #ebeef5; border-radius: 8px; overflow: hidden; }
.side { width: 260px; flex-shrink: 0; border-right: 1px solid #ebeef5; overflow-y: auto; }
.chat-col { flex: 1; min-width: 0; display: flex; flex-direction: column; }
.mbar { padding: 6px 12px; border-bottom: 1px solid #ebeef5; }
/* 手机：顶栏 56 + 内容区上内边距 12 + 底部 tabbar 与安全区（由布局的下内边距占位） */
.butler-page.mobile { height: calc(100vh - 56px - 12px - var(--sc-tabbar-height) - 16px - env(safe-area-inset-bottom)); height: calc(100dvh - 56px - 12px - var(--sc-tabbar-height) - 16px - env(safe-area-inset-bottom)); min-height: 360px; border-radius: 0; border-left: none; border-right: none; }
</style>
