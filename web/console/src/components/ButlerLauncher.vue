<script setup lang="ts">
import { useRoute, useRouter } from 'vue-router'
import { openButler } from '../store/butler'
import { session } from '../store/session'

/** 桌面端右下角浮动按钮：已登录 → 打开管家抽屉；未登录 → 登录页（登录后回到当前页）。 */
const route = useRoute()
const router = useRouter()

function onClick() {
  if (session.status !== 'authed') {
    void router.push({ path: '/login', query: { redirect: route.fullPath } })
    return
  }
  openButler()
}
</script>

<template>
  <button type="button" class="launcher" data-testid="butler-launcher" @click="onClick">
    <span class="icon" aria-hidden="true">✦</span>AI 管家
  </button>
</template>

<style scoped>
.launcher {
  position: fixed; right: 24px; bottom: 24px; z-index: 1500; display: inline-flex; align-items: center; gap: 6px;
  height: 44px; padding: 0 18px; border: none; border-radius: 22px; cursor: pointer; font-size: 14px; font-weight: 600;
  color: #fff; background: var(--el-color-primary); box-shadow: 0 4px 12px rgba(0, 0, 0, 0.18);
}
.launcher:hover { filter: brightness(1.08); }
.icon { font-size: 16px; }
</style>
